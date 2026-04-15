package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *stubFold, *Broker) {
	t.Helper()
	stub := newStubFold(t)
	t.Cleanup(stub.Close)

	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	client := NewFoldClient(stub.server.URL, 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := NewBroker(store, client, log, 2*time.Minute)
	srv := NewServer(broker, "broker-key", "admin-key", log)
	return srv, stub, broker
}

func do(t *testing.T, h http.Handler, method, path, auth string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		buf, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(buf))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServer_Health_BeforeInit(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv.Handler(), "GET", "/health", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", w.Code)
	}
	var body struct {
		Service string `json:"service"`
		Seeded  bool   `json:"seeded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Service != "texas-fold-em" || body.Seeded {
		t.Fatalf("body = %+v", body)
	}
}

func TestServer_Token_RequiresAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// No auth header.
	if w := do(t, h, "GET", "/token", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth: code = %d", w.Code)
	}
	// Wrong key.
	if w := do(t, h, "GET", "/token", "wrong-key", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key: code = %d", w.Code)
	}
	// Admin key should NOT work on /token (blast-radius isolation).
	if w := do(t, h, "GET", "/token", "admin-key", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("admin-on-token: code = %d", w.Code)
	}
}

func TestServer_Init_RequiresAdminAuth(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	body := map[string]string{"refresh_token": makeJWT("u"), "device_hash": "dh"}
	if w := do(t, h, "POST", "/init", "broker-key", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("broker-on-init: code = %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestServer_Init_Then_Token(t *testing.T) {
	srv, stub, _ := newTestServer(t)
	h := srv.Handler()

	// /token before init → 409
	w := do(t, h, "GET", "/token", "broker-key", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("pre-init /token: code = %d", w.Code)
	}

	// /init
	w = do(t, h, "POST", "/init", "admin-key", map[string]string{
		"refresh_token": makeJWT("user-007"),
		"device_hash":   "dh-abc",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("/init: code=%d body=%s", w.Code, w.Body.String())
	}
	var tok AccessToken
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		t.Fatal(err)
	}
	if tok.UserUUID != "user-007" || tok.DeviceHash != "dh-abc" {
		t.Fatalf("tok = %+v", tok)
	}

	// /token returns something usable
	w = do(t, h, "GET", "/token", "broker-key", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/token: code=%d body=%s", w.Code, w.Body.String())
	}
	var tok2 AccessToken
	_ = json.Unmarshal(w.Body.Bytes(), &tok2)
	if tok2.AccessToken == "" {
		t.Fatal("empty access token")
	}

	// /health now reports seeded
	w = do(t, h, "GET", "/health", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/health after init: code=%d", w.Code)
	}

	// Init consumed one upstream hit. /token with warm cache = no hit.
	if stub.Hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", stub.Hits())
	}
}

func TestServer_Init_BadJSON(t *testing.T) {
	srv, _, _ := newTestServer(t)
	r := httptest.NewRequest("POST", "/init", bytes.NewReader([]byte("not json")))
	r.Header.Set("Authorization", "Bearer admin-key")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestServer_Init_MissingFields_IsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	w := do(t, h, "POST", "/init", "admin-key", map[string]string{"refresh_token": makeJWT("u")})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing device_hash: want 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestServer_Init_MalformedJWT_IsBadRequest(t *testing.T) {
	srv, stub, _ := newTestServer(t)
	h := srv.Handler()
	w := do(t, h, "POST", "/init", "admin-key", map[string]string{"refresh_token": "not-a-jwt", "device_hash": "dh"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JWT: want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if stub.Hits() != 0 {
		t.Fatal("malformed JWT must not reach upstream")
	}
}

func TestServer_UnknownRoute(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv.Handler(), "GET", "/nope", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestServer_Livez_AlwaysOK(t *testing.T) {
	// Even when unseeded, /livez must return 200 — k8s probes must not
	// gate on seeded-ness or we could never bootstrap via the Service.
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	w := do(t, h, "GET", "/livez", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("livez unseeded: code = %d", w.Code)
	}
	// And after seeding it stays 200.
	_ = do(t, h, "POST", "/init", "admin-key", map[string]string{"refresh_token": makeJWT("u"), "device_hash": "dh"})
	w = do(t, h, "GET", "/livez", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("livez seeded: code = %d", w.Code)
	}
}

func TestServer_Token_NoResponseCaching(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	_ = do(t, h, "POST", "/init", "admin-key", map[string]string{"refresh_token": makeJWT("u"), "device_hash": "dh"})
	w := do(t, h, "GET", "/token", "broker-key", nil)
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
}

func TestServer_ContextDeadlinePropagation(t *testing.T) {
	// Regression: the handler wraps request ctx with its own timeout; cancelling
	// the request ctx should still cancel the upstream refresh promptly.
	srv, _, _ := newTestServer(t)
	_ = do(t, srv.Handler(), "POST", "/init", "admin-key", map[string]string{"refresh_token": makeJWT("u"), "device_hash": "dh"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("GET", "/token", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer broker-key")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	// With a warm cache this will still succeed because no upstream call is made.
	// Either 200 (cache hit) or 504 (if a refresh raced) is acceptable — just
	// assert we didn't 500.
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("500 on cancelled ctx; body=%s", w.Body.String())
	}
}
