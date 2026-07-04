package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// TestFoldSync_PreservesUnmodeledFields proves the LLM-grounding
// contract: fields fold sends that our typed Transaction does NOT
// model (account_id, merchant, category, current_balance, etc.) MUST
// land in raw_payload byte-for-byte so the classifier's synthesiser
// can ground source-account inference on them.
//
// Regression context: prior to this test, raw_payload was filled by
// json.Marshal-ing the typed struct, silently dropping account_id —
// the very signal the LLM needed to pick the right "from" account on
// rows like the c3c79fef KARAN BAHADUR SAUD transfer.
func TestFoldSync_PreservesUnmodeledFields(t *testing.T) {
	ctx := context.Background()

	// Hand-rolled JSON containing the fields fold actually returns,
	// many of which are NOT modelled in fold.Transaction.
	rich := `{
		"uuid": "rich-uuid-1",
		"amount": 70.0,
		"source_amount": 70.0,
		"currency": "INR",
		"source_currency": "INR",
		"txn_timestamp": "2026-05-08T07:00:00Z",
		"mode": "UPI",
		"type": "OUTGOING",
		"narration": "UPI to KARAN BAHADUR SAUD",
		"account_id": "fold-acc-hdfc-savings-1",
		"merchant": "karan bahadur saud",
		"category": "Transfer",
		"category_icon": "transfer-icon",
		"current_balance": 12345.67,
		"kind": "personal",
		"created_at": "2026-05-08T07:00:01Z",
		"updated_at": "2026-05-08T07:00:01Z",
		"is_valid_time": true,
		"txn_date": "2026-05-08"
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"transactions":[` + rich + `]},"meta":{}}`))
	}))
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tokens := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{
			AccessToken: "ats", DeviceHash: "dh", UserUUID: "user-1",
			ExpiresAt: time.Now().Add(15 * time.Minute),
		}, nil
	}
	syncer := NewFoldSyncer(db, fold.NewClient(srv.URL, tokens, srv.Client()), slog.Default())
	if _, err := syncer.SyncRecent(ctx, 5); err != nil {
		t.Fatalf("SyncRecent: %v", err)
	}

	var rawPayload string
	if err := db.QueryRowContext(ctx, `SELECT raw_payload FROM staged_fold_txns WHERE fold_uuid='rich-uuid-1'`).Scan(&rawPayload); err != nil {
		t.Fatalf("read raw_payload: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawPayload), &parsed); err != nil {
		t.Fatalf("raw_payload not valid JSON: %v\nbody: %s", err, rawPayload)
	}
	for _, k := range []string{"account_id", "merchant", "category", "category_icon", "current_balance", "kind", "txn_date"} {
		if _, ok := parsed[k]; !ok {
			t.Errorf("raw_payload missing %q — typed-marshal regression?", k)
		}
	}
	if parsed["account_id"] != "fold-acc-hdfc-savings-1" {
		t.Errorf("account_id = %v, want fold-acc-hdfc-savings-1", parsed["account_id"])
	}
}

// TestFoldSync_RefreshesStaleRawPayload exercises the second-sync
// refresh: an existing row whose raw_payload is thinner than the wire
// bytes gets its raw_payload updated, but classifier-owned columns
// (status, proposed_*, confirmed_*) are not touched.
func TestFoldSync_RefreshesStaleRawPayload(t *testing.T) {
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed a row as if a previous, thin-payload sync had staged it.
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status, proposed_destination_account_id)
		VALUES ('refresh-uuid', '{"uuid":"refresh-uuid"}', 7000, 'INR', '2026-05-08T07:00:00Z',
		    'UPI', 'OUTGOING', 'thin', 'thin', 'needs_review', 999)
	`); err != nil {
		t.Fatalf("seed thin row: %v", err)
	}

	rich := `{"uuid":"refresh-uuid","amount":70,"source_amount":70,"currency":"INR","source_currency":"INR","txn_timestamp":"2026-05-08T07:00:00Z","mode":"UPI","type":"OUTGOING","narration":"thin","account_id":"fold-acc-hdfc-savings-1","merchant":"karan"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"transactions":[` + rich + `]},"meta":{}}`))
	}))
	t.Cleanup(srv.Close)

	tokens := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{
			AccessToken: "ats", DeviceHash: "dh", UserUUID: "user-1",
			ExpiresAt: time.Now().Add(15 * time.Minute),
		}, nil
	}
	syncer := NewFoldSyncer(db, fold.NewClient(srv.URL, tokens, srv.Client()), slog.Default())
	report, err := syncer.SyncRecent(ctx, 5)
	if err != nil {
		t.Fatalf("SyncRecent: %v", err)
	}
	if report.Inserted != 0 || report.Skipped != 1 || report.RawRefreshed != 1 {
		t.Errorf("report = %+v, want inserted=0 skipped=1 raw_refreshed=1", report)
	}

	// raw_payload is now the wire bytes (contains account_id).
	var (
		raw     string
		status  string
		propDst sql.NullInt64
	)
	if err := db.QueryRowContext(ctx, `SELECT raw_payload, status, proposed_destination_account_id FROM staged_fold_txns WHERE fold_uuid='refresh-uuid'`).
		Scan(&raw, &status, &propDst); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !strings.Contains(raw, `"account_id":"fold-acc-hdfc-savings-1"`) {
		t.Errorf("raw_payload not refreshed: %s", raw)
	}
	if status != "needs_review" {
		t.Errorf("status mutated: %q, want needs_review", status)
	}
	if !propDst.Valid || propDst.Int64 != 999 {
		t.Errorf("proposed_destination_account_id mutated: %v, want 999", propDst)
	}

	// Second run with identical bytes is a true no-op (no churn).
	report2, err := syncer.SyncRecent(ctx, 5)
	if err != nil {
		t.Fatalf("SyncRecent (no-op): %v", err)
	}
	if report2.RawRefreshed != 0 {
		t.Errorf("expected raw_refreshed=0 on no-op rerun, got %d", report2.RawRefreshed)
	}
}

// TestSyncSinceFirefly_StopsAtCutoff: the gap-fill walks backward
// page-by-page and halts the moment a transaction at-or-before the
// firefly cutoff appears. Anything older than cutoff must NOT land in
// staged_fold_txns.
func TestSyncSinceFirefly_StopsAtCutoff(t *testing.T) {
	ctx := context.Background()

	// Build a deterministic timeline: 5 transactions newest-first,
	// txn3 sits on the cutoff itself. txn1+txn2 should be staged;
	// txn3+txn4+txn5 must not.
	txns := []fold.Transaction{
		{UUID: "newest-1", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-09T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "n1"},
		{UUID: "newest-2", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-08T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "n2"},
		{UUID: "at-cutoff", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-07T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "n3"},
		{UUID: "older-1", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-06T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "n4"},
		{UUID: "older-2", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-05T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "n5"},
	}
	srv := newFakeFoldAPI(t, "user-1", txns)
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed firefly_txns with one row whose date == cutoff. The gap-fill
	// reads MAX(date) from this table.
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name,
		    destination_account_id, destination_account_name, destination_account_name_normalized,
		    category_id, category_name, description, tags_json)
		VALUES (1, 1, 'withdrawal', 100, 'INR', '2026-05-07T12:00:00Z',
		    1, 'HDFC', 11, 'Zomato', 'zomato', 5, 'Eating outside', 'lunch', '[]')
	`); err != nil {
		t.Fatalf("seed firefly: %v", err)
	}

	tokens := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{
			AccessToken: "ats", DeviceHash: "dh", UserUUID: "user-1",
			ExpiresAt: time.Now().Add(15 * time.Minute),
		}, nil
	}
	syncer := NewFoldSyncer(db, fold.NewClient(srv.URL, tokens, srv.Client()), slog.Default())

	report, err := syncer.SyncSinceFirefly(ctx, 1000)
	if err != nil {
		t.Fatalf("SyncSinceFirefly: %v", err)
	}
	if report.Inserted != 2 {
		t.Errorf("inserted=%d, want 2 (newest-1 + newest-2)", report.Inserted)
	}
	if report.StoppedAt != "cutoff_reached" {
		t.Errorf("stopped_at=%q, want cutoff_reached", report.StoppedAt)
	}

	// The two staged uuids should be the two newer than cutoff.
	for _, want := range []string{"newest-1", "newest-2"} {
		var n int
		if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM staged_fold_txns WHERE fold_uuid=?`, want).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", want, err)
		}
		if n != 1 {
			t.Errorf("expected %s to be staged, got count=%d", want, n)
		}
	}
	// The cutoff and older rows must NOT be staged.
	for _, blocked := range []string{"at-cutoff", "older-1", "older-2"} {
		var n int
		if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM staged_fold_txns WHERE fold_uuid=?`, blocked).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", blocked, err)
		}
		if n != 0 {
			t.Errorf("expected %s to NOT be staged, got count=%d", blocked, n)
		}
	}
}

// TestSyncSinceFirefly_HardCap: with a small maxTotal the walk halts
// at the cap even when more transactions remain newer than cutoff.
// The report carries stopped_at=hard_cap so an operator notices.
func TestSyncSinceFirefly_HardCap(t *testing.T) {
	ctx := context.Background()

	txns := []fold.Transaction{
		{UUID: "a", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-09T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "a"},
		{UUID: "b", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-08T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "b"},
		{UUID: "c", Amount: 100, SourceAmount: 100, Currency: "INR", SourceCurrency: "INR",
			TxnTimestamp: "2026-05-07T12:00:00Z", Mode: "CARD", Type: "OUTGOING", Narration: "c"},
	}
	srv := newFakeFoldAPI(t, "user-1", txns)
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tokens := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{
			AccessToken: "ats", DeviceHash: "dh", UserUUID: "user-1",
			ExpiresAt: time.Now().Add(15 * time.Minute),
		}, nil
	}
	syncer := NewFoldSyncer(db, fold.NewClient(srv.URL, tokens, srv.Client()), slog.Default())

	// No firefly seed → cutoff is zero time → the cap is the only halt.
	report, err := syncer.SyncSinceFirefly(ctx, 2)
	if err != nil {
		t.Fatalf("SyncSinceFirefly: %v", err)
	}
	if report.Inserted != 2 {
		t.Errorf("inserted=%d, want 2 (cap)", report.Inserted)
	}
	if report.StoppedAt != "hard_cap" {
		t.Errorf("stopped_at=%q, want hard_cap", report.StoppedAt)
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
			Data: fold.ListTransactionsData{Transactions: txns},
		})
	}))
	return srv
}

// TestFoldSync_ForeignCurrency verifies the home-currency (INR) amount is
// stored as the primary while the original foreign charge (AED) is captured
// separately — and that a domestic txn gets no foreign side.
func TestFoldSync_ForeignCurrency(t *testing.T) {
	ctx := context.Background()
	transactions := []fold.Transaction{
		{
			UUID:           "u-foreign-aed",
			Amount:         143.27, // INR billed
			Currency:       "INR",
			SourceAmount:   5.99, // AED charged
			SourceCurrency: "AED",
			TxnTimestamp:   "2026-07-03T10:12:47Z",
			Mode:           "CARD",
			Type:           "OUTGOING",
			Narration:      "CARD/x/CARREFOUR CITY BURJUMA/AED/5.99/OUTGOING/03-07-2026",
		},
		{
			UUID:           "u-domestic-inr",
			Amount:         500,
			Currency:       "INR",
			SourceAmount:   500,
			SourceCurrency: "INR",
			TxnTimestamp:   "2026-07-02T10:00:00Z",
			Mode:           "CARD",
			Type:           "OUTGOING",
			Narration:      "CARD/x/BLINKIT/Rs./500/OUTGOING/02-07-2026",
		},
	}
	srv := newFakeFoldAPI(t, "user-123", transactions)
	t.Cleanup(srv.Close)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tokens := func(_ context.Context) (fold.AccessToken, error) {
		return fold.AccessToken{AccessToken: "a", DeviceHash: "d", UserUUID: "user-123", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	syncer := NewFoldSyncer(db, fold.NewClient(srv.URL, tokens, srv.Client()), slog.Default())
	if _, err := syncer.SyncRecent(ctx, 5); err != nil {
		t.Fatalf("SyncRecent: %v", err)
	}

	// Foreign row: INR primary + AED foreign.
	var amt int64
	var cur string
	var famt sql.NullInt64
	var fcur sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT amount_paise, currency, foreign_amount_paise, foreign_currency
		FROM staged_fold_txns WHERE fold_uuid='u-foreign-aed'`).Scan(&amt, &cur, &famt, &fcur); err != nil {
		t.Fatal(err)
	}
	if amt != 14327 || cur != "INR" {
		t.Errorf("primary = %d %s, want 14327 INR", amt, cur)
	}
	if !famt.Valid || famt.Int64 != 599 || !fcur.Valid || fcur.String != "AED" {
		t.Errorf("foreign = %v/%v, want 599/AED", famt, fcur)
	}

	// Domestic row: no foreign side.
	if err := db.QueryRowContext(ctx, `
		SELECT amount_paise, currency, foreign_amount_paise, foreign_currency
		FROM staged_fold_txns WHERE fold_uuid='u-domestic-inr'`).Scan(&amt, &cur, &famt, &fcur); err != nil {
		t.Fatal(err)
	}
	if amt != 50000 || cur != "INR" {
		t.Errorf("domestic primary = %d %s, want 50000 INR", amt, cur)
	}
	if famt.Valid || fcur.Valid {
		t.Errorf("domestic must have no foreign side, got %v/%v", famt, fcur)
	}
}
