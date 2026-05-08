package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/fold"
)

// TestFoldSync_HappyPath spins up a fake fold API, runs SyncRecent
// against a real SQLite DB, asserts staged_fold_txns reflects the
// upstream payload, and verifies idempotency on re-run.
func TestFoldSync_HappyPath(t *testing.T) {
	ctx := context.Background()

	// Two fold transactions, newest first (matching fold's ordering).
	transactions := []fold.Transaction{
		{
			UUID:           "uuid-1-zomato",
			Amount:         978.33,
			SourceAmount:   978.33,
			Currency:       "INR",
			SourceCurrency: "INR",
			TxnTimestamp:   "2026-05-08T06:44:00Z",
			Mode:           "CARD",
			Type:           "OUTGOING",
			Narration:      "CARD/19e0654f754c92ad/Zomato/₹/978.33/OUTGOING/08-05-2026 at 06:44",
		},
		{
			UUID:           "uuid-2-praveen",
			Amount:         16123,
			SourceAmount:   16123,
			Currency:       "INR",
			SourceCurrency: "INR",
			TxnTimestamp:   "2026-05-04T11:25:00Z",
			Mode:           "OTHERS",
			Type:           "OUTGOING",
			Narration:      "UPI-PRAVEEN KUMAR-9876@okaxis-HDFC-001",
		},
	}

	srv := newFakeFoldAPI(t, "user-123", transactions)
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tokens := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{
			AccessToken: "ats",
			DeviceHash:  "dh",
			UserUUID:    "user-123",
			ExpiresAt:   time.Now().Add(15 * time.Minute),
		}, nil
	}
	fc := fold.NewClient(srv.URL, tokens, srv.Client())
	syncer := NewFoldSyncer(db, fc, slog.Default())

	report, err := syncer.SyncRecent(ctx, 5)
	if err != nil {
		t.Fatalf("SyncRecent: %v", err)
	}
	if report.Fetched != 2 || report.Inserted != 2 || report.Skipped != 0 {
		t.Errorf("first run: got %+v", report)
	}
	if report.NewestUUID != "uuid-1-zomato" {
		t.Errorf("expected newest=uuid-1-zomato, got %q", report.NewestUUID)
	}

	// Verify the row content for the Zomato txn — covers the most fields
	// in one assertion: amount, merchant extraction, status, narration.
	var (
		amt        int64
		merchant   string
		status     string
		mode, typ  string
		rawPayload string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT amount_paise, merchant_extracted, status, mode, type, raw_payload
		FROM staged_fold_txns WHERE fold_uuid = 'uuid-1-zomato'`).
		Scan(&amt, &merchant, &status, &mode, &typ, &rawPayload); err != nil {
		t.Fatalf("read staged row: %v", err)
	}
	if amt != 97833 {
		t.Errorf("amount_paise = %d, want 97833", amt)
	}
	if merchant != "zomato" {
		t.Errorf("merchant_extracted = %q, want %q", merchant, "zomato")
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q", status, "pending")
	}
	if mode != "CARD" || typ != "OUTGOING" {
		t.Errorf("mode/type = %q/%q, want CARD/OUTGOING", mode, typ)
	}
	// raw_payload should be parseable JSON containing the original UUID.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawPayload), &parsed); err != nil {
		t.Errorf("raw_payload not valid JSON: %v", err)
	}
	if parsed["uuid"] != "uuid-1-zomato" {
		t.Errorf("raw_payload uuid = %v, want %q", parsed["uuid"], "uuid-1-zomato")
	}

	// Idempotent re-run: same fold response, all skipped this time.
	report2, err := syncer.SyncRecent(ctx, 5)
	if err != nil {
		t.Fatalf("SyncRecent (re-run): %v", err)
	}
	if report2.Fetched != 2 || report2.Inserted != 0 || report2.Skipped != 2 {
		t.Errorf("rerun: got %+v", report2)
	}

	// Total rows in the table should remain 2 — no leaking duplicates.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM staged_fold_txns`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 staged rows after rerun, got %d", n)
	}

	// PRAVEEN KUMAR row should have a non-empty merchant_extracted via
	// the UPI fallback in fold.ExtractMerchant + NormalizeMerchant.
	var praveenMerchant string
	if err := db.QueryRowContext(ctx, `SELECT merchant_extracted FROM staged_fold_txns WHERE fold_uuid='uuid-2-praveen'`).
		Scan(&praveenMerchant); err != nil {
		t.Fatalf("read praveen row: %v", err)
	}
	if praveenMerchant != "praveen kumar" {
		t.Errorf("praveen merchant_extracted = %q, want %q", praveenMerchant, "praveen kumar")
	}
}

// TestAmountToPaise covers the float-to-integer-paise conversion.
// Money math gets wrong silently; cheap insurance.
func TestAmountToPaise(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0, 0},
		{1, 100},
		{1.5, 150},
		{70, 7000},
		{978.33, 97833},
		{16123, 1612300},
		{-50, 5000},   // stored absolute
		{0.005, 1},    // half-up rounding
		{0.004, 0},    // truncates below half
	}
	for _, tc := range cases {
		got := amountToPaise(tc.in)
		if got != tc.want {
			t.Errorf("amountToPaise(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// newFakeFoldAPI stands up a fake fold server that responds to
// GET /v3/users/{userId}/transactions with the supplied transactions.
// Verifies device headers are set; bails on the test if not.
func newFakeFoldAPI(t *testing.T, userUUID string, txns []fold.Transaction) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/users/"+userUUID+"/transactions" {
			http.NotFound(w, r)
			return
		}
		// Sanity: the device headers the broker requires.
		for _, h := range []string{"X-Device-Hash", "X-Device-Type", "X-Device-Location", "X-Request-ID", "Authorization"} {
			if r.Header.Get(h) == "" {
				t.Errorf("missing header %q in request to fold", h)
				http.Error(w, "missing header", http.StatusBadRequest)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fold.ListTransactionsResponse{
			Data: struct {
				Transactions []fold.Transaction `json:"transactions"`
			}{Transactions: txns},
		})
	}))
	return srv
}
