package firefly

// THIS FILE IS THE WRITE SIDE OF THE FIREFLY CLIENT.
//
// Read-only contract recap (see client.go for the long version):
// the firefly client surfaces only GET methods AND a single
// CreateTransaction method. There is NO PATCH, NO PUT, NO DELETE
// codepath in this package. The split between client.go (reads) and
// write.go (this file, the single create method) is by file, not just
// by method-naming convention — anyone reviewing a future change has
// to physically open this file to introduce a write operation, which
// makes such changes visible at code-review time.
//
// External ID + idempotency:
// every transaction we create carries external_id = <fold_uuid>. The
// Pusher in push.go calls SearchByExternalID first; if a hit comes
// back, we skip the POST and mark our staging row as already_pushed.
// This means even if the staged_fold_txns table somehow loses its
// pushed_at column, the firefly side won't double-write on a retry.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CreateTransactionRequest is what we send to POST /api/v1/transactions.
// firefly's API expects a transaction GROUP wrapping one or more
// JOURNALS. We always send exactly one journal per group — splits are
// not modelled in fold staging today.
type CreateTransactionRequest struct {
	GroupTitle   string                  `json:"group_title,omitempty"`
	ErrorIfDup   bool                    `json:"error_if_duplicate_hash"`
	Transactions []CreateTransactionLine `json:"transactions"`
}

// CreateTransactionLine is one journal line.
type CreateTransactionLine struct {
	Type            string   `json:"type"` // withdrawal | deposit | transfer
	Date            string   `json:"date"` // RFC3339
	Amount          string   `json:"amount"`
	CurrencyCode    string   `json:"currency_code,omitempty"`
	// ForeignAmount / ForeignCurrencyCode record the ORIGINAL charge for a
	// cross-currency transaction (e.g. AED 5.99) alongside the home-currency
	// Amount/CurrencyCode (INR). Both must be set together or neither.
	// firefly requires the foreign currency to already exist — see
	// EnsureCurrency, which the Pusher calls before create.
	ForeignAmount       string `json:"foreign_amount,omitempty"`
	ForeignCurrencyCode string `json:"foreign_currency_code,omitempty"`
	Description     string   `json:"description"`
	SourceID        string   `json:"source_id,omitempty"`
	SourceName      string   `json:"source_name,omitempty"`
	DestinationID   string   `json:"destination_id,omitempty"`
	DestinationName string   `json:"destination_name,omitempty"`
	CategoryID      string   `json:"category_id,omitempty"`
	BudgetID        string   `json:"budget_id,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	ExternalID      string   `json:"external_id,omitempty"`
	Notes           string   `json:"notes,omitempty"`
}

// CreateTransactionResponse is what firefly returns. We only surface
// the journal id of the (single) created transaction so the caller
// can write it back to staged_fold_txns.firefly_txn_id.
type CreateTransactionResponse struct {
	GroupID    int64
	JournalIDs []int64
}

type createEnvelope struct {
	Data struct {
		ID         string `json:"id"`
		Attributes struct {
			Transactions []struct {
				JournalID string `json:"transaction_journal_id"`
			} `json:"transactions"`
		} `json:"attributes"`
	} `json:"data"`
}

// CreateTransaction is the ONLY non-GET method in this package. It
// sends a POST to /api/v1/transactions and returns the firefly side
// IDs for the created journal(s).
//
// Caller is responsible for:
//   - First checking SearchByExternalID for idempotency.
//   - Validating the source/destination/category/budget IDs exist
//     (we don't pre-validate here — firefly will 422 if invalid, and
//     the caller surfaces that to the operator).
//   - Logging to audit_log on success and failure (the caller is
//     orchestrating; this client is just the wire layer).
func (c *Client) CreateTransaction(ctx context.Context, req CreateTransactionRequest) (CreateTransactionResponse, error) {
	if c.base == "" {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: base URL is empty")
	}
	if c.pat == "" {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: PAT is empty")
	}
	if len(req.Transactions) == 0 {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: at least one transaction line required")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: marshal create request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/transactions", bytes.NewReader(body))
	if err != nil {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: build create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.pat)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/vnd.api+json")
	httpReq.Header.Set("User-Agent", "texas-fold-em-integration/1")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: POST /api/v1/transactions: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: read create response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CreateTransactionResponse{}, &Error{
			Status: resp.StatusCode,
			Path:   "/api/v1/transactions",
			Body:   string(respBody),
		}
	}

	var env createEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return CreateTransactionResponse{}, fmt.Errorf("firefly: decode create response: %w", err)
	}

	out := CreateTransactionResponse{}
	if env.Data.ID != "" {
		fmt.Sscan(env.Data.ID, &out.GroupID)
	}
	for _, t := range env.Data.Attributes.Transactions {
		var jid int64
		if t.JournalID != "" {
			fmt.Sscan(t.JournalID, &jid)
		}
		if jid != 0 {
			out.JournalIDs = append(out.JournalIDs, jid)
		}
	}
	return out, nil
}

// SearchByExternalID returns the journal ids of any firefly transactions
// already carrying the given external_id. Used as the idempotency
// gate before CreateTransaction.
//
// Implementation note: firefly's search endpoint accepts the
// "external_id_is:<value>" filter. We use that rather than the more
// general /search/transactions?query= because the latter does
// substring matching that could false-match other narrations.
func (c *Client) SearchByExternalID(ctx context.Context, externalID string) ([]int64, error) {
	if externalID == "" {
		return nil, fmt.Errorf("firefly: empty external_id")
	}
	q := url.Values{}
	q.Set("query", "external_id_is:"+externalID)
	q.Set("limit", "5")
	var resp struct {
		Data []struct {
			Attributes struct {
				Transactions []struct {
					JournalID string `json:"transaction_journal_id"`
				} `json:"transactions"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v1/search/transactions", q, &resp); err != nil {
		return nil, err
	}
	var ids []int64
	for _, g := range resp.Data {
		for _, t := range g.Attributes.Transactions {
			var id int64
			if t.JournalID != "" {
				fmt.Sscan(t.JournalID, &id)
			}
			if id != 0 {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

// EnsureCurrency guarantees a currency with the given ISO code exists AND
// is enabled in firefly, so it can be used as a transaction's
// foreign_currency_code. Without it, an abroad charge in a currency the
// user's firefly has never enabled (e.g. AED) 422s on create. This mirrors
// the on-demand creation of a new expense account on push — we don't make
// the operator pre-define every currency they might travel with.
//
// Idempotent:
//   - exists + enabled  → no-op
//   - exists + disabled → POST /currencies/{code}/enable
//   - not defined (404) → POST /currencies (created enabled)
//
// This is a WRITE (see the file header). It is deliberately here in
// write.go and only ever creates/enables a currency — never edits or
// deletes one.
func (c *Client) EnsureCurrency(ctx context.Context, code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil
	}
	var cur struct {
		Data struct {
			Attributes struct {
				Enabled bool `json:"enabled"`
			} `json:"attributes"`
		} `json:"data"`
	}
	err := c.get(ctx, "/api/v1/currencies/"+code, nil, &cur)
	if err == nil {
		if cur.Data.Attributes.Enabled {
			return nil
		}
		return c.post(ctx, "/api/v1/currencies/"+code+"/enable", nil, nil)
	}
	var ffErr *Error
	if !errors.As(err, &ffErr) || ffErr.Status != http.StatusNotFound {
		return err // a real error (auth, network) — not "just not defined yet"
	}
	name, symbol, decimals := currencyMeta(code)
	return c.post(ctx, "/api/v1/currencies", map[string]any{
		"code":           code,
		"name":           name,
		"symbol":         symbol,
		"decimal_places": decimals,
		"enabled":        true,
	}, nil)
}

// post is the write-side counterpart to get: a JSON POST. body may be nil
// (for action endpoints like .../enable). Kept in write.go so every
// mutating call is physically in this file, per the read-only contract.
func (c *Client) post(ctx context.Context, path string, body any, dst any) error {
	if c.base == "" {
		return fmt.Errorf("firefly: base URL is empty")
	}
	if c.pat == "" {
		return fmt.Errorf("firefly: PAT is empty")
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("firefly: marshal post body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("firefly: build post %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("User-Agent", "texas-fold-em-integration/1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("firefly: POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Path: path, Body: string(respBody)}
	}
	if dst != nil {
		if err := json.Unmarshal(respBody, dst); err != nil {
			return fmt.Errorf("firefly: decode post %s response: %w", path, err)
		}
	}
	return nil
}

// currencyMeta returns a display name, symbol, and decimal-place count for
// an ISO currency code, used when creating it in firefly. Common travel
// currencies get proper names/symbols/decimals; anything else falls back
// to the code itself (functional, if less pretty) with 2 decimals.
func currencyMeta(code string) (name, symbol string, decimals int) {
	if m, ok := knownCurrencies[code]; ok {
		return m.name, m.symbol, m.decimals
	}
	return code, code, 2
}

var knownCurrencies = map[string]struct {
	name     string
	symbol   string
	decimals int
}{
	"AED": {"UAE Dirham", "AED", 2},
	"USD": {"US Dollar", "$", 2},
	"EUR": {"Euro", "€", 2},
	"GBP": {"British Pound", "£", 2},
	"SGD": {"Singapore Dollar", "S$", 2},
	"THB": {"Thai Baht", "฿", 2},
	"MYR": {"Malaysian Ringgit", "RM", 2},
	"IDR": {"Indonesian Rupiah", "Rp", 0},
	"JPY": {"Japanese Yen", "¥", 0},
	"KRW": {"South Korean Won", "₩", 0},
	"VND": {"Vietnamese Dong", "₫", 0},
	"AUD": {"Australian Dollar", "A$", 2},
	"CAD": {"Canadian Dollar", "C$", 2},
	"CHF": {"Swiss Franc", "CHF", 2},
	"HKD": {"Hong Kong Dollar", "HK$", 2},
	"NZD": {"New Zealand Dollar", "NZ$", 2},
	"LKR": {"Sri Lankan Rupee", "Rs", 2},
	"NPR": {"Nepalese Rupee", "Rs", 2},
	"SAR": {"Saudi Riyal", "SAR", 2},
	"QAR": {"Qatari Riyal", "QAR", 2},
	"TRY": {"Turkish Lira", "₺", 2},
	"CNY": {"Chinese Yuan", "¥", 2},
	"BHD": {"Bahraini Dinar", "BHD", 3},
	"KWD": {"Kuwaiti Dinar", "KWD", 3},
	"OMR": {"Omani Rial", "OMR", 3},
}
