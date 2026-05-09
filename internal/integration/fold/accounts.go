package fold

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Account is the normalised view of a fold-side asset (credit card or
// bank account) — exactly what the staging-side fold_accounts table
// stores, exactly what Tier-3's prompt needs to ground source-account
// inference.
//
// Both kinds collapse into the same shape so callers don't have to
// branch on kind unless they care about kind-specific fields.
type Account struct {
	ID         string          `json:"id"`            // fold's per-account UUID; matches transaction.account_id
	Kind       AccountKind     `json:"kind"`          // BANK | CREDIT_CARD
	Name       string          `json:"name"`          // e.g. "Tata Neu Plus", "HDFC Bank ****5684"
	Provider   string          `json:"provider"`      // bank/issuer (FIP) name, e.g. "HDFC", "Axis Bank"
	Network    string          `json:"network"`       // credit-card-only: "Visa", "RuPay", "Mastercard"
	LastFour   string          `json:"last_four"`     // last 4 of card or masked account number
	Nickname   string          `json:"nickname"`      // user-set, may be empty
	HolderName string          `json:"holder_name"`
	IsClosed   bool            `json:"is_closed"`
	Raw        json.RawMessage `json:"-"` // verbatim fold-side per-account JSON
}

// AccountKind is the shape of a fold asset.
type AccountKind string

const (
	AccountKindBank       AccountKind = "BANK"
	AccountKindCreditCard AccountKind = "CREDIT_CARD"
)

// ListCreditCardsResponse is the partial shape of
// GET /api/v3/users/{uid}/credit-cards. We keep just enough fields to
// build an Account; cycle/usage/payment metadata is dropped because
// the classifier doesn't use it.
type ListCreditCardsResponse struct {
	Data struct {
		Accounts []json.RawMessage `json:"accounts"`
	} `json:"data"`
}

// ListBankAccountsResponse is the partial shape of
// GET /api/v1/users/{uid}/bank_accounts. Only `data.added` is kept —
// fold's `subscribed` array carries inactive/expired consents we
// don't want to surface in the LLM prompt.
type ListBankAccountsResponse struct {
	Data struct {
		Added []json.RawMessage `json:"added"`
	} `json:"data"`
}

// ListAccounts returns all fold-side assets (credit cards + bank
// accounts) as a flat slice. Network errors on either endpoint cause
// the whole call to fail — partial mirrors are worse than no mirror
// here, since a missing entry means the LLM silently falls back to
// guessing.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	tok, err := c.tokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("fold: get access token: %w", err)
	}
	if tok.UserUUID == "" || tok.AccessToken == "" || tok.DeviceHash == "" {
		return nil, fmt.Errorf("fold: incomplete access token bundle")
	}

	cards, err := c.listCreditCards(ctx, tok)
	if err != nil {
		return nil, err
	}
	banks, err := c.listBankAccounts(ctx, tok)
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(cards)+len(banks))
	out = append(out, cards...)
	out = append(out, banks...)
	return out, nil
}

func (c *Client) listCreditCards(ctx context.Context, tok AccessToken) ([]Account, error) {
	path := fmt.Sprintf("/v3/users/%s/credit-cards", url.PathEscape(tok.UserUUID))
	var resp ListCreditCardsResponse
	if err := c.get(ctx, path, nil, tok, &resp); err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(resp.Data.Accounts))
	for _, raw := range resp.Data.Accounts {
		acc, err := parseCreditCardAccount(raw)
		if err != nil {
			return nil, fmt.Errorf("fold: parse credit card: %w", err)
		}
		out = append(out, acc)
	}
	return out, nil
}

func (c *Client) listBankAccounts(ctx context.Context, tok AccessToken) ([]Account, error) {
	path := fmt.Sprintf("/v1/users/%s/bank_accounts", url.PathEscape(tok.UserUUID))
	var resp ListBankAccountsResponse
	if err := c.get(ctx, path, nil, tok, &resp); err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(resp.Data.Added))
	for _, raw := range resp.Data.Added {
		acc, err := parseBankAccount(raw)
		if err != nil {
			return nil, fmt.Errorf("fold: parse bank account: %w", err)
		}
		out = append(out, acc)
	}
	return out, nil
}

// parseCreditCardAccount pulls the fields we care about off a single
// element of /v3/users/.../credit-cards data.accounts[]. The shape is
// documented in fold.md; here we capture exactly what Tier-3 needs.
func parseCreditCardAccount(raw json.RawMessage) (Account, error) {
	var c struct {
		UUID            string `json:"uuid"`
		HolderName      string `json:"holder_name"`
		LastFourDigits  string `json:"last_four_digits"`
		Nickname        string `json:"nickname"`
		IsClosed        bool   `json:"is_closed"`
		PaymentNetwork  struct {
			Name string `json:"name"`
		} `json:"payment_network"`
		CreditCard struct {
			Name     string `json:"name"`
			Provider struct {
				Name string `json:"name"`
			} `json:"provider"`
		} `json:"credit_card"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Account{}, err
	}
	return Account{
		ID:         c.UUID,
		Kind:       AccountKindCreditCard,
		Name:       creditCardDisplayName(c.CreditCard.Name, c.CreditCard.Provider.Name, c.LastFourDigits),
		Provider:   c.CreditCard.Provider.Name,
		Network:    c.PaymentNetwork.Name,
		LastFour:   c.LastFourDigits,
		Nickname:   c.Nickname,
		HolderName: c.HolderName,
		IsClosed:   c.IsClosed,
		Raw:        raw,
	}, nil
}

// parseBankAccount pulls the fields we care about off a single element
// of /v1/users/.../bank_accounts data.added[].
func parseBankAccount(raw json.RawMessage) (Account, error) {
	var b struct {
		UUID                string  `json:"uuid"`
		MaskedAccountNumber string  `json:"masked_account_number"`
		Nickname            string  `json:"nickname"`
		IsClosed            *bool   `json:"is_closed"`
		FIP                 struct {
			Name string `json:"name"`
		} `json:"fip"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return Account{}, err
	}
	closed := false
	if b.IsClosed != nil {
		closed = *b.IsClosed
	}
	last4 := ""
	if i := strings.LastIndex(b.MaskedAccountNumber, "*"); i >= 0 && i+1 < len(b.MaskedAccountNumber) {
		last4 = strings.TrimLeft(b.MaskedAccountNumber[i+1:], "*")
	}
	return Account{
		ID:         b.UUID,
		Kind:       AccountKindBank,
		Name:       bankDisplayName(b.FIP.Name, b.MaskedAccountNumber),
		Provider:   b.FIP.Name,
		LastFour:   last4,
		Nickname:   b.Nickname,
		IsClosed:   closed,
		Raw:        raw,
	}, nil
}

// creditCardDisplayName composes a name the LLM can match against the
// user's firefly asset list. "<provider> <product> ****<last4>"
// covers the way most firefly users name their cards (e.g. "HDFC Tata
// Neu Plus ****8943" or "HDFC Bank Tata Neu Plus 8943").
func creditCardDisplayName(product, provider, last4 string) string {
	parts := []string{}
	if provider != "" {
		parts = append(parts, provider)
	}
	if product != "" {
		parts = append(parts, product)
	}
	name := strings.Join(parts, " ")
	if last4 != "" {
		if name != "" {
			name += " "
		}
		name += "****" + last4
	}
	if name == "" {
		name = "Credit Card"
	}
	return name
}

// bankDisplayName composes a name like "HDFC Bank ****5684".
func bankDisplayName(fip, masked string) string {
	if fip == "" && masked == "" {
		return "Bank Account"
	}
	if fip == "" {
		return masked
	}
	if masked == "" {
		return fip
	}
	return fip + " " + masked
}
