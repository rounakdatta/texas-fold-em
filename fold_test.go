package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubjectOfJWT(t *testing.T) {
	// Hand-roll a JWT payload with a sub claim. Signature is irrelevant
	// because SubjectOfJWT intentionally doesn't verify it.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"2f0e7bec-43b3-49b0-88bd-9d04832eb4d8","exp":1784037336}`))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig-bytes"))
	jwt := header + "." + payload + "." + sig

	got, err := SubjectOfJWT(jwt)
	if err != nil {
		t.Fatalf("SubjectOfJWT: %v", err)
	}
	if got != "2f0e7bec-43b3-49b0-88bd-9d04832eb4d8" {
		t.Fatalf("sub: got %q", got)
	}
}

func TestSubjectOfJWT_Rejects(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"empty", ""},
		{"one-dot", "a.b"},
		{"non-base64", "a.$$$.c"},
		{"no-sub", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := SubjectOfJWT(tc.in); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestFoldClient_RefreshTokens_Success(t *testing.T) {
	var gotHeaders http.Header
	var gotBody struct {
		RefreshToken string `json:"refresh_token"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/tokens/refresh" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		gotHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"meta": {"request_id": "rid", "timestamp": "2026-04-15T14:00:00Z"},
			"data": {
				"token_type": "Bearer",
				"access_token": "new-access",
				"refresh_token": "new-refresh",
				"expires_at": "2026-04-15T14:15:00Z"
			},
			"error": null
		}`))
	}))
	defer ts.Close()

	c := NewFoldClient(ts.URL, 5*time.Second)
	bundle, err := c.RefreshTokens(context.Background(), "old-refresh", "dh-123")
	if err != nil {
		t.Fatalf("RefreshTokens: %v", err)
	}
	if bundle.AccessToken != "new-access" || bundle.RefreshToken != "new-refresh" {
		t.Fatalf("bundle: %+v", bundle)
	}

	// Request-level assertions.
	if gotBody.RefreshToken != "old-refresh" {
		t.Errorf("body.refresh_token = %q", gotBody.RefreshToken)
	}
	for _, h := range []string{"X-Device-Hash", "X-Device-Type", "X-Device-Location", "X-Request-ID", "Content-Type", "User-Agent"} {
		if gotHeaders.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if gotHeaders.Get("X-Device-Hash") != "dh-123" {
		t.Errorf("X-Device-Hash = %q", gotHeaders.Get("X-Device-Hash"))
	}
	if gotHeaders.Get("X-Device-Type") != "Web" {
		t.Errorf("X-Device-Type = %q", gotHeaders.Get("X-Device-Type"))
	}
	if gotHeaders.Get("X-Device-Location") != "India" {
		t.Errorf("X-Device-Location = %q", gotHeaders.Get("X-Device-Location"))
	}
	if !strings.Contains(gotHeaders.Get("User-Agent"), "texas-fold-em") {
		t.Errorf("UA = %q", gotHeaders.Get("User-Agent"))
	}
}

func TestFoldClient_RefreshTokens_Rejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{
			"meta": {"request_id": "rid", "timestamp": "2026-04-15T14:00:00Z"},
			"data": null,
			"error": {"code": 2001, "message": "refresh token invalid", "how_to_fix": "re-authenticate"}
		}`))
	}))
	defer ts.Close()

	c := NewFoldClient(ts.URL, 5*time.Second)
	_, err := c.RefreshTokens(context.Background(), "bad", "dh")
	var rej *ErrRefreshRejected
	if !errors.As(err, &rej) {
		t.Fatalf("want ErrRefreshRejected, got %T: %v", err, err)
	}
	if rej.Code != 2001 {
		t.Errorf("code = %d", rej.Code)
	}
}

func TestFoldClient_RefreshTokens_MissingDeviceHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{
			"meta": {"request_id": "rid", "timestamp": "2026-04-15T14:00:00Z"},
			"data": null,
			"error": {"code": 2007, "message": "missing device headers", "how_to_fix": "Add device headers to request"}
		}`))
	}))
	defer ts.Close()

	c := NewFoldClient(ts.URL, 5*time.Second)
	_, err := c.RefreshTokens(context.Background(), "rt", "dh")
	if err == nil {
		t.Fatal("expected error")
	}
	var rej *ErrRefreshRejected
	if errors.As(err, &rej) {
		t.Fatalf("422 should not map to ErrRefreshRejected (that's only for 401)")
	}
	if !strings.Contains(err.Error(), "missing device headers") {
		t.Errorf("want descriptive error, got %v", err)
	}
}

func TestNewRequestID_IsUUIDv4Shape(t *testing.T) {
	id := newRequestID()
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("shape: %q", id)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("lengths: %q", id)
	}
	// Version nibble should be 4.
	if parts[2][0] != '4' {
		t.Fatalf("version nibble: %q", id)
	}
}
