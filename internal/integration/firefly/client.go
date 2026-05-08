// Package firefly is a thin typed Go client over the subset of the
// Firefly III REST API that the texas-fold-em integration needs.
//
// Non-destructive contract — load-bearing for the entire feature:
// THIS PACKAGE EXPOSES ONLY READ METHODS (GET). The write surface
// (POST /api/v1/transactions for the eventual push step) lives in a
// separate file `write.go` that lands in PR F. The split is enforced
// by file rather than method-naming convention so a careless edit can't
// silently introduce a destructive call into the read-side code path.
//
// No PATCH/PUT/DELETE codepath exists in this package, ever.
package firefly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout is what NewClient uses when given a nil http.Client.
// Tunable via NewClient if a caller needs a different bound.
const DefaultTimeout = 30 * time.Second

// Client is a typed HTTP client over Firefly III's API. Construct via
// NewClient — fields are unexported because mutation post-construction
// would be racy.
type Client struct {
	base       string
	pat        string
	httpClient *http.Client
}

// NewClient returns a configured client. base is the firefly host URL
// (no trailing slash, no /api suffix — we add the path); pat is a
// Firefly III Personal Access Token issued in the firefly UI. If
// httpClient is nil, a default with DefaultTimeout is used.
//
// The returned Client is safe for concurrent use.
func NewClient(base, pat string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		base:       strings.TrimRight(base, "/"),
		pat:        pat,
		httpClient: httpClient,
	}
}

// AboutUser hits GET /api/v1/about/user. Mostly used as a connectivity
// + auth sanity check; we don't store anything from the response.
func (c *Client) AboutUser(ctx context.Context) (User, error) {
	var env aboutUserEnvelope
	if err := c.get(ctx, "/api/v1/about/user", nil, &env); err != nil {
		return User{}, err
	}
	return env.Data.Attributes, nil
}

// ListTransactions returns one page of transactions. page is 1-indexed
// (firefly's convention). limit must be in [1, 100].
func (c *Client) ListTransactions(ctx context.Context, page, limit int) (TransactionListResponse, error) {
	q := url.Values{}
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("limit", fmt.Sprintf("%d", limit))
	var resp TransactionListResponse
	if err := c.get(ctx, "/api/v1/transactions", q, &resp); err != nil {
		return TransactionListResponse{}, err
	}
	return resp, nil
}

// ListAccounts returns one page of accounts (all types: asset, expense,
// revenue, etc.). Filtering by type happens client-side in the sync
// orchestrator since we want them all anyway for the classifier corpus.
func (c *Client) ListAccounts(ctx context.Context, page, limit int) (AccountListResponse, error) {
	q := url.Values{}
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("limit", fmt.Sprintf("%d", limit))
	var resp AccountListResponse
	if err := c.get(ctx, "/api/v1/accounts", q, &resp); err != nil {
		return AccountListResponse{}, err
	}
	return resp, nil
}

// ListCategories returns one page of categories.
func (c *Client) ListCategories(ctx context.Context, page, limit int) (CategoryListResponse, error) {
	q := url.Values{}
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("limit", fmt.Sprintf("%d", limit))
	var resp CategoryListResponse
	if err := c.get(ctx, "/api/v1/categories", q, &resp); err != nil {
		return CategoryListResponse{}, err
	}
	return resp, nil
}

// ListBudgets returns one page of budgets.
func (c *Client) ListBudgets(ctx context.Context, page, limit int) (BudgetListResponse, error) {
	q := url.Values{}
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("limit", fmt.Sprintf("%d", limit))
	var resp BudgetListResponse
	if err := c.get(ctx, "/api/v1/budgets", q, &resp); err != nil {
		return BudgetListResponse{}, err
	}
	return resp, nil
}

// get is the single point through which all HTTP calls flow. It is the
// reason this package's read-only contract is enforced: nothing in this
// file calls http.Client.Do directly with a non-GET method.
func (c *Client) get(ctx context.Context, path string, query url.Values, dst any) error {
	if c.base == "" {
		return errors.New("firefly: base URL is empty")
	}
	if c.pat == "" {
		return errors.New("firefly: PAT is empty")
	}

	u := c.base + path
	if query != nil {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("firefly: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("User-Agent", "texas-fold-em-integration/1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("firefly: %s %s: %w", req.Method, path, err)
	}
	defer resp.Body.Close()

	// Cap the body we read so a misconfigured proxy returning HTML can't
	// blow up our memory. 16 MiB is generous for paginated JSON.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("firefly: read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{
			Status: resp.StatusCode,
			Path:   path,
			Body:   string(body),
		}
	}

	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("firefly: decode %s: %w", path, err)
	}
	return nil
}

// Error is returned for any non-2xx response. It carries enough context
// for an operator to diagnose without having to attach a debugger; the
// body is included verbatim because firefly's error envelopes are JSON
// with useful messages.
type Error struct {
	Status int
	Path   string
	Body   string
}

func (e *Error) Error() string {
	preview := e.Body
	if len(preview) > 256 {
		preview = preview[:256] + "…"
	}
	return fmt.Sprintf("firefly: HTTP %d on %s: %s", e.Status, e.Path, preview)
}
