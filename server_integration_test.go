package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
	"github.com/rounakdatta/texas-fold-em/internal/integration/fold"
)

// TestServer_FireflySyncRoute_NotRegisteredWithoutSyncer guards against
// the route showing up by accident on a stock (non-integration) deploy.
// 404 is the explicit signal that the integration is OFF.
func TestServer_FireflySyncRoute_NotRegisteredWithoutSyncer(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv.Handler(), "POST", "/admin/firefly/sync", "admin-key", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when integration disabled, got %d", w.Code)
	}
}

// TestServer_FireflySync_RequiresAdminKey confirms the endpoint is
// gated by the admin key — not the broker key (different blast radius).
func TestServer_FireflySync_RequiresAdminKey(t *testing.T) {
	srv := newTestServerWithIntegration(t)
	tests := []struct {
		name string
		auth string
		code int
	}{
		{"no auth", "", http.StatusUnauthorized},
		{"broker key (wrong key for this route)", "broker-key", http.StatusUnauthorized},
		{"correct admin key", "admin-key", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(t, srv.Handler(), "POST", "/admin/firefly/sync", tt.auth, nil)
			if w.Code != tt.code {
				t.Errorf("status=%d, want %d. body: %s", w.Code, tt.code, w.Body.String())
			}
		})
	}
}

// TestServer_FireflySync_ReturnsReport asserts the happy-path response
// body shape matches what operators + the future UI will parse. The
// underlying SQL paths are exercised in sync_test.go; here we only
// assert the HTTP-layer wiring is correct.
func TestServer_FireflySync_ReturnsReport(t *testing.T) {
	srv := newTestServerWithIntegration(t)
	w := do(t, srv.Handler(), "POST", "/admin/firefly/sync", "admin-key", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d. body: %s", w.Code, w.Body.String())
	}
	var report integration.SyncReport
	if err := json.NewDecoder(w.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.TransactionJournals == 0 {
		t.Errorf("expected non-zero journals in test fixture, got %+v", report)
	}
}

// TestServer_FoldSyncRoute_NotRegisteredWithoutSyncer mirrors the
// firefly variant — asserts a stock deploy returns 404.
func TestServer_FoldSyncRoute_NotRegisteredWithoutSyncer(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv.Handler(), "POST", "/admin/fold/sync", "admin-key", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when fold syncer absent, got %d", w.Code)
	}
}

// TestServer_FoldSync_RequiresAdminKey covers the auth gating.
func TestServer_FoldSync_RequiresAdminKey(t *testing.T) {
	srv := newTestServerWithIntegration(t)
	tests := []struct {
		name string
		auth string
		code int
	}{
		{"no auth", "", http.StatusUnauthorized},
		{"broker key (wrong key)", "broker-key", http.StatusUnauthorized},
		{"correct admin key", "admin-key", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(t, srv.Handler(), "POST", "/admin/fold/sync", tt.auth, nil)
			if w.Code != tt.code {
				t.Errorf("status=%d, want %d. body: %s", w.Code, tt.code, w.Body.String())
			}
		})
	}
}

// TestServer_FoldSync_RejectsBadLimit guards against the limit param
// going outside the upstream's accepted range.
func TestServer_FoldSync_RejectsBadLimit(t *testing.T) {
	srv := newTestServerWithIntegration(t)
	for _, bad := range []string{"0", "-1", "abc", "9999"} {
		t.Run(bad, func(t *testing.T) {
			w := do(t, srv.Handler(), "POST", "/admin/fold/sync?limit="+bad, "admin-key", nil)
			if w.Code != http.StatusBadRequest {
				t.Errorf("limit=%s expected 400, got %d", bad, w.Code)
			}
		})
	}
}

// newTestServerWithIntegration is the integration-enabled counterpart
// to newTestServer (in server_test.go): real SQLite + a fake firefly
// upstream wired through a real Syncer.
func newTestServerWithIntegration(t *testing.T) *Server {
	t.Helper()
	srv, _, _ := newTestServer(t)

	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := integration.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open integration db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Minimal firefly fixture: about/user + one page of one journal.
	fakeFF := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/about/user"):
			_, _ = w.Write([]byte(`{"data":{"attributes":{"email":"alice@x","role":"owner","blocked":false}}}`))
		case strings.Contains(r.URL.Path, "/transactions"):
			_, _ = w.Write([]byte(`{
				"data":[{"id":"100","type":"transactions","attributes":{"transactions":[{
					"transaction_journal_id":"100","type":"withdrawal","amount":"70.00",
					"currency_code":"INR","date":"2026-05-08T12:59:18+05:30",
					"source_id":"1","source_name":"HDFC","destination_id":"10","destination_name":"Cake Palace",
					"category_id":"5","category_name":"Eating outside","budget_id":"","budget_name":"",
					"description":"snack","tags":[],"external_id":""
				}]}}],
				"meta":{"pagination":{"total":1,"count":1,"per_page":50,"current_page":1,"total_pages":1}}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fakeFF.Close)

	// Minimal fake fold API: returns one transaction for any user.
	fakeFold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v3/users/") || !strings.HasSuffix(r.URL.Path, "/transactions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data":{"transactions":[{
				"uuid":"fake-uuid-1","amount":70.0,"source_amount":70.0,
				"currency":"INR","source_currency":"INR",
				"txn_timestamp":"2026-05-08T12:59:18Z","mode":"CARD","type":"OUTGOING",
				"narration":"CARD/x/Cake Palace/Rs/70.00/OUTGOING"
			}]},
			"meta":{}
		}`))
	}))
	t.Cleanup(fakeFold.Close)

	tokenFn := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{
			AccessToken: "ats", DeviceHash: "dh", UserUUID: "user-1",
			ExpiresAt: time.Now().Add(15 * time.Minute),
		}, nil
	}

	srv.SetIntegration(db)
	srv.SetFireflySyncer(integration.NewSyncer(db, firefly.NewClient(fakeFF.URL, "x", fakeFF.Client()), slog.New(slog.NewTextHandler(io.Discard, nil))))
	srv.SetFoldSyncer(integration.NewFoldSyncer(db, fold.NewClient(fakeFold.URL, tokenFn, fakeFold.Client()), slog.New(slog.NewTextHandler(io.Discard, nil))))
	srv.SetClassifier(classifier.New(db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)), classifier.DefaultConfidenceThreshold, 10))
	srv.SetPusher(integration.NewPusher(db, firefly.NewClient(fakeFF.URL, "x", fakeFF.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false))
	return srv
}

// TestServer_PushRoute_NotRegisteredWithoutPusher
func TestServer_PushRoute_NotRegisteredWithoutPusher(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv.Handler(), "POST", "/admin/push/anything", "admin-key", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when pusher absent, got %d", w.Code)
	}
}

// TestServer_Push_RequiresAdminKey covers gating on the new
// path-parameterised route.
func TestServer_Push_RequiresAdminKey(t *testing.T) {
	srv := newTestServerWithIntegration(t)
	for _, tc := range []struct {
		name string
		auth string
		// 404 because the staged row doesn't exist (correct admin key
		// gets through gate, hits PushNotFoundError → 404).
		// Unauthorized for everything else.
		code int
	}{
		{"no auth", "", http.StatusUnauthorized},
		{"broker key", "broker-key", http.StatusUnauthorized},
		{"correct admin key (no row)", "admin-key", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv.Handler(), "POST", "/admin/push/missing", tc.auth, nil)
			if w.Code != tc.code {
				t.Errorf("status=%d, want %d", w.Code, tc.code)
			}
		})
	}
}

// TestServer_ClassifyRoute_NotRegisteredWithoutClassifier mirrors the
// other gating tests.
func TestServer_ClassifyRoute_NotRegisteredWithoutClassifier(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := do(t, srv.Handler(), "POST", "/admin/classify", "admin-key", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when classifier absent, got %d", w.Code)
	}
}

// TestServer_Classify_RequiresAdminKey covers auth gating.
func TestServer_Classify_RequiresAdminKey(t *testing.T) {
	srv := newTestServerWithIntegration(t)
	for _, tc := range []struct {
		name string
		auth string
		code int
	}{
		{"no auth", "", http.StatusUnauthorized},
		{"broker key", "broker-key", http.StatusUnauthorized},
		{"correct admin key", "admin-key", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv.Handler(), "POST", "/admin/classify", tc.auth, nil)
			if w.Code != tc.code {
				t.Errorf("status=%d, want %d", w.Code, tc.code)
			}
		})
	}
}
