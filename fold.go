package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotSeeded means the broker has no refresh token yet — caller must POST
// /init. Distinguished from transient refresh failures so callers can show a
// precise message.
var ErrNotSeeded = errors.New("broker not initialised: POST /init with {refresh_token, device_hash}")

// ErrBadInput marks errors caused by malformed client input (bad JWT shape,
// missing fields, etc). It's wrapped into more specific errors via %w and
// unwrapped in the HTTP layer to choose the right status code (400 vs 502).
var ErrBadInput = errors.New("bad input")

// ErrRefreshRejected means Fold said our refresh token is no longer valid
// (401 with code 2001/2002/etc). Recovery is manual re-seed.
type ErrRefreshRejected struct {
	Code    int
	Message string
}

func (e *ErrRefreshRejected) Error() string {
	return fmt.Sprintf("fold rejected refresh token (code=%d): %s", e.Code, e.Message)
}

// TokenBundle mirrors the shape of `data` inside a successful /tokens/refresh
// response. Fold rotates the refresh token on every successful call, so we
// must persist the new one.
type TokenBundle struct {
	TokenType    string    `json:"token_type"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// FoldClient talks to api.fold.money. It's the *only* place we send requests
// upstream, which makes it the single place to reason about device headers,
// request IDs, and rate-limit handling.
type FoldClient struct {
	BaseURL string        // e.g. https://api.fold.money/api
	HTTP    *http.Client
	UA      string
}

// NewFoldClient returns a client with sensible timeouts. Callers can swap
// HTTP for a stubbed transport in tests.
func NewFoldClient(baseURL string, timeout time.Duration) *FoldClient {
	return &FoldClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: timeout},
		UA:      "texas-fold-em/1.0 (+https://github.com/rounakdatta/texas-fold-em)",
	}
}

// apiEnvelope matches Fold's {meta, data, error} wrapper.
type apiEnvelope struct {
	Meta  json.RawMessage `json:"meta"`
	Data  json.RawMessage `json:"data"`
	Error *apiError       `json:"error"`
}

type apiError struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	HowToFix string `json:"how_to_fix"`
}

// RefreshTokens calls POST /v1/auth/tokens/refresh. On success Fold returns
// a new refresh_token (rotation) and a fresh access_token.
//
// This function does NOT touch any persistent state — the caller (Broker)
// is responsible for persisting the rotated tokens. Separating API I/O from
// persistence is what keeps the rotation race small and testable.
func (c *FoldClient) RefreshTokens(ctx context.Context, refreshToken, deviceHash string) (TokenBundle, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/auth/tokens/refresh", bytes.NewReader(body))
	if err != nil {
		return TokenBundle{}, fmt.Errorf("build refresh request: %w", err)
	}
	c.setDeviceHeaders(req, deviceHash)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return TokenBundle{}, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return TokenBundle{}, fmt.Errorf("read refresh response: %w", err)
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return TokenBundle{}, fmt.Errorf("decode refresh envelope (status=%d): %w", resp.StatusCode, err)
	}

	if env.Error != nil {
		// 401 on refresh typically means the refresh token has been revoked
		// or rotated out. Signal that distinctly so operators know to re-seed.
		if resp.StatusCode == http.StatusUnauthorized {
			return TokenBundle{}, &ErrRefreshRejected{Code: env.Error.Code, Message: env.Error.Message}
		}
		return TokenBundle{}, fmt.Errorf("fold refresh error %d (status=%d): %s — %s",
			env.Error.Code, resp.StatusCode, env.Error.Message, env.Error.HowToFix)
	}
	if resp.StatusCode/100 != 2 {
		return TokenBundle{}, fmt.Errorf("fold refresh non-2xx (status=%d): %s", resp.StatusCode, string(raw))
	}

	var bundle TokenBundle
	if err := json.Unmarshal(env.Data, &bundle); err != nil {
		return TokenBundle{}, fmt.Errorf("decode refresh data: %w", err)
	}
	if bundle.AccessToken == "" || bundle.RefreshToken == "" {
		return TokenBundle{}, fmt.Errorf("fold refresh response missing tokens")
	}
	return bundle, nil
}

// setDeviceHeaders applies the four headers Fold mandates on every API call.
func (c *FoldClient) setDeviceHeaders(req *http.Request, deviceHash string) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Device-Hash", deviceHash)
	req.Header.Set("X-Device-Type", "Web")
	req.Header.Set("X-Device-Location", "India")
	req.Header.Set("X-Request-ID", newRequestID())
	req.Header.Set("User-Agent", c.UA)
}

// newRequestID returns a fresh UUIDv4-looking string per request. We don't
// need crypto-grade randomness here — Fold uses this for tracing only — but
// crypto/rand is already imported for atomicity elsewhere, and this is
// cheap.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback; should essentially never happen.
		return fmt.Sprintf("reqid-%x", time.Now().UnixNano())
	}
	// RFC 4122 v4 bits
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// SubjectOfJWT returns the `sub` claim from a JWT without verifying the
// signature. Used to derive the Fold user UUID from a refresh token.
//
// We do not verify the signature because:
//  1. We don't have Fold's HMAC secret (and shouldn't).
//  2. The token has already been validated server-side on every call that
//     uses it; if Fold accepted it for /refresh, the `sub` is real.
//  3. The `sub` is also returned inside /users/me, so worst case (a maliciously
//     crafted token planted via /init) produces a wrong user_uuid that fails
//     Fold auth on the next data call and surfaces as a 401 — no security
//     impact on the broker.
func SubjectOfJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("not a JWT (expected 3 parts, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Fall back to standard base64 in case of odd padding in the wild.
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return "", fmt.Errorf("decode JWT payload: %w", err)
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("decode JWT claims: %w", err)
	}
	if claims.Sub == "" {
		return "", errors.New("JWT has no sub claim")
	}
	return claims.Sub, nil
}
