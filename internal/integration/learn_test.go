package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// TestLearningLoop_Reinforces: a real Pusher with a real Classifier as
// learner. After a successful push, merchant_lookup gets the new merchant.
//
// This proves the end-to-end loop the user described:
//   "Whenever the user confirms 'Cake Palace = Snacks' once, the next
//    Cake Palace transaction should auto-classify."
func TestLearningLoop_Reinforces(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed firefly_txns with destination/category/source name lookups
	// so learn.go's name-resolution sub-queries find friendly strings.
	_, _ = db.DB.Exec(`
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name,
		    destination_account_id, destination_account_name, destination_account_name_normalized,
		    category_id, category_name, description, tags_json)
		VALUES (901, 9001, 'withdrawal', 5000, 'INR', '2026-04-01',
		    1, 'HDFC Card',
		    99, 'Cake Palace', 'cake palace',
		    6, 'Snacks', 'sample', '[]')
	`)

	// Seed staged_fold_txns ready_to_push for "cake palace" (no entry
	// in merchant_lookup yet — first time we're learning it).
	_, _ = db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    confirmed_destination_account_id, confirmed_source_account_id, confirmed_category_id,
		    confirmed_description)
		VALUES ('cake-1', '{}', 5000, 'INR', '2026-05-08T20:00:00Z',
		    'CARD', 'OUTGOING', 'CARD/x/Cake Palace/Rs/50.00/OUTGOING', 'cake palace', 'ready_to_push',
		    99, 1, 6, 'birthday cake')
	`)

	// Fake firefly that succeeds on create.
	fakeFF := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/search/transactions"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/transactions"):
			_, _ = w.Write([]byte(`{"data":{"id":"7950","attributes":{"transactions":[{"transaction_journal_id":"7951"}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fakeFF.Close)

	cls := classifier.New(db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)),
		classifier.DefaultConfidenceThreshold, 10)

	pusher := NewPusher(db, firefly.NewClient(fakeFF.URL, "p", fakeFF.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	pusher.SetLearner(cls)

	report, err := pusher.Push(context.Background(), "cake-1", true)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if report.Action != "created" {
		t.Fatalf("action=%q, want created", report.Action)
	}

	// Reinforcement should have inserted a merchant_lookup row for
	// "cake palace".
	var (
		destID    int64
		categoryID int64
		sample     int
		conf       float64
	)
	err = db.DB.QueryRow(`
		SELECT modal_destination_account_id, modal_category_id, sample_size, confidence
		FROM merchant_lookup WHERE merchant_normalized='cake palace'
	`).Scan(&destID, &categoryID, &sample, &conf)
	if err != nil {
		t.Fatalf("lookup row missing: %v", err)
	}
	if destID != 99 {
		t.Errorf("destination_account_id=%d, want 99", destID)
	}
	if categoryID != 6 {
		t.Errorf("category_id=%d, want 6", categoryID)
	}
	if sample != 1 {
		t.Errorf("sample_size=%d, want 1 on first reinforcement", sample)
	}
	if conf < 0.99 {
		t.Errorf("confidence=%f, want ~1.0", conf)
	}
}

// TestLearningLoop_ReinforcesMatchingLabels: when the same merchant
// gets pushed AGAIN with the SAME labels, sample_size bumps to 2 and
// confidence stays high.
func TestLearningLoop_ReinforcesMatchingLabels(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Seed an existing merchant_lookup row.
	_, _ = db.DB.Exec(`
		INSERT INTO merchant_lookup (merchant_normalized,
		    modal_destination_account_id, modal_destination_account_name,
		    modal_source_account_id, modal_source_account_name,
		    modal_category_id, modal_category_name,
		    modal_budget_id, modal_budget_name,
		    sample_size, confidence, last_seen)
		VALUES ('cake palace', 99, 'Cake Palace', 1, 'HDFC', 6, 'Snacks', NULL, NULL, 1, 1.0, '2026-05-01')
	`)
	// Push another cake palace with same labels.
	_, _ = db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    confirmed_destination_account_id, confirmed_source_account_id, confirmed_category_id,
		    confirmed_description)
		VALUES ('cake-2', '{}', 6000, 'INR', '2026-05-10T20:00:00Z',
		    'CARD', 'OUTGOING', 'CARD/x/Cake Palace/Rs/60/OUTGOING', 'cake palace', 'pushed',
		    99, 1, 6, 'another cake')
	`)
	cls := classifier.New(db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)),
		classifier.DefaultConfidenceThreshold, 10)
	if err := cls.LearnFromPushed(context.Background(), "cake-2"); err != nil {
		t.Fatalf("LearnFromPushed: %v", err)
	}
	var sample int
	var conf float64
	if err := db.DB.QueryRow(`SELECT sample_size, confidence FROM merchant_lookup WHERE merchant_normalized='cake palace'`).
		Scan(&sample, &conf); err != nil {
		t.Fatal(err)
	}
	if sample != 2 {
		t.Errorf("sample_size=%d, want 2", sample)
	}
	if conf < 0.99 {
		t.Errorf("confidence=%f, want stable at 1.0 with matching labels", conf)
	}
}

// TestLearningLoop_LabelsChanged: when a previously-zomato=Eating-outside
// row is pushed with category=Groceries instead, confidence drops to
// 0.85 (signals "the modal is in flux"). The next periodic firefly
// sync will recompute the true modal precisely.
func TestLearningLoop_LabelsChanged(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Existing lookup says zomato → category 5 (eating outside).
	_, _ = db.DB.Exec(`
		INSERT INTO merchant_lookup (merchant_normalized,
		    modal_destination_account_id, modal_destination_account_name,
		    modal_category_id, modal_category_name,
		    sample_size, confidence, last_seen)
		VALUES ('zomato', 11, 'Zomato', 5, 'Eating outside', 10, 1.0, '2026-04-01')
	`)
	// Newly-pushed row with DIFFERENT category id (40 = Groceries).
	_, _ = db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    confirmed_destination_account_id, confirmed_category_id, confirmed_description)
		VALUES ('zoma-divergent', '{}', 5000, 'INR', '2026-05-12T20:00:00Z',
		    'CARD', 'OUTGOING', 'CARD/x/Zomato/Rs/50/OUTGOING', 'zomato', 'pushed',
		    11, 40, 'groceries via zomato hypermart')
	`)
	cls := classifier.New(db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)),
		classifier.DefaultConfidenceThreshold, 10)
	if err := cls.LearnFromPushed(context.Background(), "zoma-divergent"); err != nil {
		t.Fatalf("LearnFromPushed: %v", err)
	}
	var conf float64
	if err := db.DB.QueryRow(`SELECT confidence FROM merchant_lookup WHERE merchant_normalized='zomato'`).Scan(&conf); err != nil {
		t.Fatal(err)
	}
	if conf > 0.86 {
		t.Errorf("confidence=%f, expected drop to 0.85 on label change", conf)
	}
}

// TestLearningLoop_NoMerchantNoOp: when the staged row has empty
// merchant_extracted, learn is a no-op (no key to insert under).
func TestLearningLoop_NoMerchantNoOp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	_, _ = db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    confirmed_destination_account_id, confirmed_category_id)
		VALUES ('blank-1', '{}', 100, 'INR', '2026-05-08T00:00:00Z',
		    'OTHERS', 'OUTGOING', 'something weird', '', 'pushed', 11, 5)
	`)
	cls := classifier.New(db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)),
		classifier.DefaultConfidenceThreshold, 10)
	if err := cls.LearnFromPushed(context.Background(), "blank-1"); err != nil {
		t.Errorf("LearnFromPushed should be a no-op, got error: %v", err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM merchant_lookup`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("merchant_lookup should be empty when merchant_extracted is blank, got %d rows", n)
	}
}

// TestLearningLoop_StatusGuard: if status != 'pushed', learn is a
// no-op (defensive).
func TestLearningLoop_StatusGuard(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	_, _ = db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    confirmed_destination_account_id, confirmed_category_id)
		VALUES ('not-pushed', '{}', 100, 'INR', '2026-05-08T00:00:00Z',
		    'CARD', 'OUTGOING', 'CARD/x/Anything/.../OUTGOING', 'anything', 'ready_to_push',
		    11, 5)
	`)
	cls := classifier.New(db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)),
		classifier.DefaultConfidenceThreshold, 10)
	if err := cls.LearnFromPushed(context.Background(), "not-pushed"); err != nil {
		t.Errorf("expected no-op, got error: %v", err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM merchant_lookup`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("merchant_lookup should be empty for non-pushed rows, got %d", n)
	}
}

// TestPushSucceedsEvenIfLearnerErrors: a flaky learner does not
// surface the failure to the push caller.
func TestPushSucceedsEvenIfLearnerErrors(t *testing.T) {
	s := newPushTestSetup(t)
	flaky := flakyLearner{err: errors.New("simulated learner failure")}

	p := NewPusher(s.db, firefly.NewClient(s.fakeFirefly.URL, "p", s.fakeFirefly.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	p.SetLearner(&flaky)

	report, err := p.Push(context.Background(), "u1", true)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if report.Action != "created" {
		t.Errorf("action=%q, want created", report.Action)
	}
	if !flaky.called {
		t.Error("expected learner to have been invoked")
	}
}

type flakyLearner struct {
	called bool
	err    error
}

func (f *flakyLearner) LearnFromPushed(_ context.Context, _ string) error {
	f.called = true
	return f.err
}
