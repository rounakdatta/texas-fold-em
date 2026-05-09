// Package fold is a thin typed Go client over the Fold money
// /api/v3/users/{userId}/transactions endpoint, reusing access tokens
// from the texas-fold-em broker.
//
// Why a separate client (we already have a FoldClient in package main)?
// The broker's FoldClient is the auth-side client — its only purpose
// is hitting /v1/auth/tokens/refresh. This package is the *data-side*
// client — it consumes the access token the broker produces and uses
// it to read transactions. Keeping them separate keeps each side's
// surface tiny and the integration's blast radius small (a bug here
// can't break refresh-token rolling).
//
// Read-only contract: only GET methods are exposed. There's no write
// surface against fold's API today — and there shouldn't ever be.
package fold

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AfterCursorFromTime builds a fold pagination cursor anchored at t.
// Pass to ListTransactionsAfter to fetch transactions strictly older
// than t. Empty time → empty cursor → unpaginated newest-first.
//
// Format observed against the live API (also documented in fold.md):
// base64("DESC:::time:::<rfc3339-utc>").
func AfterCursorFromTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	raw := "DESC:::time:::" + t.UTC().Format(time.RFC3339)
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// DefaultTimeout is what NewClient uses when given a nil http.Client.
const DefaultTimeout = 30 * time.Second

// TokenFunc returns a currently-valid AccessToken bundle. The caller
// (typically the broker) is responsible for refreshing if needed.
type TokenFunc func(ctx context.Context) (AccessToken, error)

// Client talks to fold's /api/v3/users/.../transactions endpoint using
// access tokens supplied by a TokenFunc. Safe for concurrent use.
type Client struct {
	base       string
	tokens     TokenFunc
	httpClient *http.Client
}

// NewClient returns a configured client. base is the fold API base
// (e.g. "https://api.fold.money/api"). tokens is required — without
// it the client cannot authenticate.
func NewClient(base string, tokens TokenFunc, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		base:       strings.TrimRight(base, "/"),
		tokens:     tokens,
		httpClient: httpClient,
	}
}

// ListTransactions returns the latest `limit` transactions for the
// user identified by the broker's access token. limit is capped by
// the upstream — empirically the API accepts up to 100; values higher
// than that have been observed to 422.
func (c *Client) ListTransactions(ctx context.Context, limit int) (ListTransactionsResponse, error) {
	return c.ListTransactionsAfter(ctx, limit, "")
}

// ListTransactionsAfter is the cursor-aware variant. When `after` is
// empty it behaves identically to ListTransactions (returns the latest
// page). When `after` is set, it returns the page strictly older than
// the cursor — fold's transaction list is newest-first and the cursor
// is its built-in pagination anchor.
//
// Cursor format (per fold.md, observed empirically): base64(
// "DESC:::time:::<rfc3339-utc>") — sort direction, sort field, value.
// Use AfterCursorFromTime to build one for a known timestamp; that's
// what the gap-fill syncer does to walk back from "now" until it
// crosses the firefly cutoff.
func (c *Client) ListTransactionsAfter(ctx context.Context, limit int, after string) (ListTransactionsResponse, error) {
	if limit <= 0 {
		return ListTransactionsResponse{}, errors.New("fold: limit must be positive")
	}
	tok, err := c.tokens(ctx)
	if err != nil {
		return ListTransactionsResponse{}, fmt.Errorf("fold: get access token: %w", err)
	}
	if tok.UserUUID == "" || tok.AccessToken == "" || tok.DeviceHash == "" {
		return ListTransactionsResponse{}, errors.New("fold: incomplete access token bundle")
	}

	path := fmt.Sprintf("/v3/users/%s/transactions", url.PathEscape(tok.UserUUID))
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if after != "" {
		q.Set("after", after)
	}

	var resp ListTransactionsResponse
	if err := c.get(ctx, path, q, tok, &resp); err != nil {
		return ListTransactionsResponse{}, err
	}
	return resp, nil
}

// get is the single point through which all HTTP calls flow. Read-only
// contract enforced by file boundary: this file has no MethodPost/Put/
// Delete codepaths. Adding a write surface would require explicit edits
// here that any reviewer would catch.
func (c *Client) get(ctx context.Context, path string, query url.Values, tok AccessToken, dst any) error {
	u := c.base + path
	if query != nil && len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("fold: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("X-Device-Hash", tok.DeviceHash)
	req.Header.Set("X-Device-Type", "Web")
	req.Header.Set("X-Device-Location", "India")
	req.Header.Set("X-Request-ID", newRequestID())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "texas-fold-em-integration/1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fold: %s %s: %w", req.Method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("fold: read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Path: path, Body: string(body)}
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("fold: decode %s: %w", path, err)
	}
	return nil
}

// Error is returned for any non-2xx response.
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
	return fmt.Sprintf("fold: HTTP %d on %s: %s", e.Status, e.Path, preview)
}

// newRequestID generates a UUID-shaped string for the X-Request-ID
// header. Real entropy from crypto/rand; format-compatible enough that
// fold's logs accept it. We don't care about RFC4122 v4 specifically —
// fold treats it as an opaque string for correlation.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall through with zeros so we don't drop
		// the header and confuse a 400 from fold with a missing-id case.
		return "00000000-0000-0000-0000-000000000000"
	}
	// Stamp version (4) and variant (10).
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hex := hex.EncodeToString(b[:])
	return strings.ToUpper(hex[:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:])
}
