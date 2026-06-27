package cron

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
	"github.com/rounakdatta/texas-fold-em/internal/integration/fold"
)

// TestPeriodicSync_StopsOnContextCancel: calling cancel returns
// promptly. Asserts the goroutine doesn't leak.
func TestPeriodicSync_StopsOnContextCancel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	idb, err := integration.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idb.Close() })

	cls := classifier.New(idb.DB, slog.New(slog.NewTextHandler(io.Discard, nil)),
		classifier.DefaultConfidenceThreshold, 10)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// foldSyncer nil to skip the fold leg; classifier nil-friendly already.
		PeriodicSync(ctx, nil, nil, nil, cls, 50*time.Millisecond, 50,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	// Let the timer fire at least once.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PeriodicSync did not stop within 2s of cancel")
	}
}

// TestPeriodicSync_RunsCycleEndToEnd: a fast tick triggers fold sync
// (fake fold API) + classify. Asserts at least one row gets staged
// and (since the merchant matches firefly_txns we also seed) gets
// auto-classified into ready_to_push.
func TestPeriodicSync_RunsCycleEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	idb, err := integration.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idb.Close() })

	// Seed a firefly txn + matching merchant_lookup so the classifier
	// has something to anchor on.
	if _, err := idb.DB.Exec(`
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name,
		    destination_account_id, destination_account_name, destination_account_name_normalized,
		    category_id, category_name, description, tags_json)
		VALUES (1001, 10001, 'withdrawal', 1000, 'INR', '2026-04-01',
		    1, 'HDFC', 11, 'Zomato', 'zomato', 5, 'Eating outside', 'lunch', '[]')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := idb.DB.Exec(`
		INSERT INTO merchant_lookup (merchant_normalized,
		    modal_destination_account_id, modal_destination_account_name,
		    modal_source_account_id, modal_source_account_name,
		    modal_category_id, modal_category_name,
		    modal_budget_id, modal_budget_name,
		    sample_size, confidence, last_seen)
		VALUES ('zomato', 11, 'Zomato', 1, 'HDFC', 5, 'Eating outside', NULL, NULL, 1, 1.0, '2026-04-01')
	`); err != nil {
		t.Fatal(err)
	}

	// Fake fold returning one txn the classifier knows.
	fakeFold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v3/users/") || !strings.HasSuffix(r.URL.Path, "/transactions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fold.ListTransactionsResponse{
			Data: fold.ListTransactionsData{
				Transactions: []fold.Transaction{{
					UUID: "cron-1", Amount: 100, SourceAmount: 100,
					Currency: "INR", SourceCurrency: "INR",
					TxnTimestamp: "2026-05-08T12:00:00Z",
					Mode:         "CARD",
					Type:         "OUTGOING",
					Narration:    "CARD/x/Zomato/Rs/100/OUTGOING",
				}},
			},
		})
	}))
	t.Cleanup(fakeFold.Close)

	tokenFn := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{
			AccessToken: "ats", DeviceHash: "dh", UserUUID: "user-1",
			ExpiresAt: time.Now().Add(15 * time.Minute),
		}, nil
	}
	foldSyncer := integration.NewFoldSyncer(idb,
		fold.NewClient(fakeFold.URL, tokenFn, fakeFold.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	cls := classifier.New(idb.DB, slog.New(slog.NewTextHandler(io.Discard, nil)),
		classifier.DefaultConfidenceThreshold, 10)

	// Run one cycle directly (rather than fight a real ticker).
	runOneCycle(context.Background(), foldSyncer, nil, nil, cls, 50,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Assert: 1 staged row, status=ready_to_push (Tier 1 classified it).
	var status string
	if err := idb.DB.QueryRow(`SELECT status FROM staged_fold_txns WHERE fold_uuid='cron-1'`).Scan(&status); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "ready_to_push" {
		t.Errorf("status=%q, want ready_to_push (cron should have classified zomato → tier 1)", status)
	}
}
