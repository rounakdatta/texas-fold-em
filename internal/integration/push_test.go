package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// pushTestSetup spins up a real SQLite, a fake firefly that records
// each method+path, and seeds a single ready_to_push staged fold txn
// for "u1". Returns components the test cases assemble themselves.
type pushTestSetup struct {
	db          *DB
	fakeFirefly *httptest.Server
	createCalls *atomic.Int32
	searchCalls *atomic.Int32
	// fields the test can mutate before the fake responds:
	createBody          *string
	searchHits          []int64
	createReturnsStatus int
}

func newPushTestSetup(t *testing.T) *pushTestSetup {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	idb, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })

	setup := &pushTestSetup{
		db:                  idb,
		createCalls:         &atomic.Int32{},
		searchCalls:         &atomic.Int32{},
		createReturnsStatus: 200,
	}
	captured := ""
	setup.createBody = &captured

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search/transactions", func(w http.ResponseWriter, _ *http.Request) {
		setup.searchCalls.Add(1)
		w.Header().Set("Content-Type", "application/vnd.api+json")
		if len(setup.searchHits) == 0 {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		// One group containing all hit ids.
		s := `{"data":[{"attributes":{"transactions":[`
		for i, id := range setup.searchHits {
			if i > 0 {
				s += ","
			}
			s += `{"transaction_journal_id":"` + intToString(id) + `"}`
		}
		s += `]}}]}`
		_, _ = w.Write([]byte(s))
	})
	mux.HandleFunc("/api/v1/transactions", func(w http.ResponseWriter, r *http.Request) {
		setup.createCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		if setup.createReturnsStatus >= 400 {
			w.WriteHeader(setup.createReturnsStatus)
			_, _ = w.Write([]byte(`{"message":"validation failed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{"data":{"id":"7950","attributes":{"transactions":[{"transaction_journal_id":"7951"}]}}}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	setup.fakeFirefly = httptest.NewServer(mux)
	t.Cleanup(setup.fakeFirefly.Close)

	// Seed a ready-to-push row.
	if _, err := idb.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    proposed_source_account_id, proposed_destination_account_id,
		    proposed_category_id, proposed_description)
		VALUES ('u1','{}',7000,'INR','2026-05-08T12:59:18Z',
		        'CARD','OUTGOING','CARD/x/Cake Palace/Rs/70.00/OUTGOING','cake palace','ready_to_push',
		        1, 12, 6, 'Snack at cake palace')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return setup
}

func intToString(n int64) string { return strconv.FormatInt(n, 10) }

// TestPush_PreviewMode returns the firefly POST body without making
// any network call. Action="preview".
func TestPush_PreviewMode(t *testing.T) {
	s := newPushTestSetup(t)

	p := NewPusher(s.db, firefly.NewClient(s.fakeFirefly.URL, "p", s.fakeFirefly.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	report, err := p.Push(context.Background(), "u1", false)
	if err != nil {
		t.Fatalf("Push preview: %v", err)
	}
	if report.Action != "preview" {
		t.Errorf("action=%q, want preview", report.Action)
	}
	if s.createCalls.Load() != 0 || s.searchCalls.Load() != 0 {
		t.Errorf("preview should not call firefly. create=%d search=%d", s.createCalls.Load(), s.searchCalls.Load())
	}
	bodyJSON, _ := json.Marshal(report.PreviewBody)
	if !strings.Contains(string(bodyJSON), `"external_id":"u1"`) {
		t.Errorf("preview body should carry external_id=u1: %s", bodyJSON)
	}
	if !strings.Contains(string(bodyJSON), `"amount":"70.00"`) {
		t.Errorf("preview body should have amount 70.00: %s", bodyJSON)
	}
	if !strings.Contains(string(bodyJSON), `"type":"withdrawal"`) {
		t.Errorf("preview body should have type withdrawal: %s", bodyJSON)
	}

	// Row remains in ready_to_push.
	var status string
	if err := s.db.DB.QueryRow(`SELECT status FROM staged_fold_txns WHERE fold_uuid='u1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "ready_to_push" {
		t.Errorf("status=%q after preview, want ready_to_push", status)
	}
}

// TestPush_Confirmed creates the firefly transaction and updates the
// staged row's status, firefly_txn_id, and pushed_at.
func TestPush_Confirmed(t *testing.T) {
	s := newPushTestSetup(t)

	p := NewPusher(s.db, firefly.NewClient(s.fakeFirefly.URL, "p", s.fakeFirefly.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	report, err := p.Push(context.Background(), "u1", true)
	if err != nil {
		t.Fatalf("Push confirm: %v", err)
	}
	if report.Action != "created" {
		t.Errorf("action=%q, want created", report.Action)
	}
	if report.FireflyTxnID != 7951 {
		t.Errorf("firefly_txn_id=%d, want 7951", report.FireflyTxnID)
	}
	if s.searchCalls.Load() != 1 {
		t.Errorf("expected 1 search call (dedup), got %d", s.searchCalls.Load())
	}
	if s.createCalls.Load() != 1 {
		t.Errorf("expected 1 create call, got %d", s.createCalls.Load())
	}

	// DB state.
	var status string
	var fid int64
	if err := s.db.DB.QueryRow(`SELECT status, firefly_txn_id FROM staged_fold_txns WHERE fold_uuid='u1'`).Scan(&status, &fid); err != nil {
		t.Fatal(err)
	}
	if status != "pushed" || fid != 7951 {
		t.Errorf("status=%q firefly_txn_id=%d", status, fid)
	}

	// Audit log.
	var auditCount int
	if err := s.db.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE fold_uuid='u1' AND action='firefly_create'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Errorf("audit_log expected 1 firefly_create row, got %d", auditCount)
	}
}

// TestPush_DedupHit: firefly already has the transaction. We mark the
// staged row pushed pointing at the existing journal id, no create
// is issued.
func TestPush_DedupHit(t *testing.T) {
	s := newPushTestSetup(t)
	s.searchHits = []int64{42}

	p := NewPusher(s.db, firefly.NewClient(s.fakeFirefly.URL, "p", s.fakeFirefly.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	report, err := p.Push(context.Background(), "u1", true)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if report.Action != "deduped" {
		t.Errorf("action=%q, want deduped", report.Action)
	}
	if report.FireflyTxnID != 42 {
		t.Errorf("firefly_txn_id=%d, want 42", report.FireflyTxnID)
	}
	if s.createCalls.Load() != 0 {
		t.Errorf("dedup hit MUST NOT call create. create=%d", s.createCalls.Load())
	}

	var status string
	var fid int64
	if err := s.db.DB.QueryRow(`SELECT status, firefly_txn_id FROM staged_fold_txns WHERE fold_uuid='u1'`).Scan(&status, &fid); err != nil {
		t.Fatal(err)
	}
	if status != "pushed" || fid != 42 {
		t.Errorf("status=%q firefly_txn_id=%d", status, fid)
	}
}

// TestPush_AlreadyPushed: the staged row already has a firefly_txn_id.
// Re-pushing is a no-op; we don't even call firefly.
func TestPush_AlreadyPushed(t *testing.T) {
	s := newPushTestSetup(t)
	if _, err := s.db.DB.Exec(`UPDATE staged_fold_txns SET status='pushed', firefly_txn_id=99 WHERE fold_uuid='u1'`); err != nil {
		t.Fatal(err)
	}

	p := NewPusher(s.db, firefly.NewClient(s.fakeFirefly.URL, "p", s.fakeFirefly.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	// status='pushed' is not in our pushable statuses list, so this
	// should error with PushNotReadyError.
	_, err := p.Push(context.Background(), "u1", true)
	if !errors.Is(err, PushNotReadyError) {
		t.Errorf("expected PushNotReadyError, got %v", err)
	}
	if s.createCalls.Load() != 0 || s.searchCalls.Load() != 0 {
		t.Errorf("no firefly calls should happen for already-pushed rows")
	}
}

// TestPush_ReadOnly aborts with PushReadOnlyError on confirm; preview
// is allowed.
func TestPush_ReadOnly(t *testing.T) {
	s := newPushTestSetup(t)

	p := NewPusher(s.db, firefly.NewClient(s.fakeFirefly.URL, "p", s.fakeFirefly.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), true) // readOnly=true

	// Preview still works.
	if _, err := p.Push(context.Background(), "u1", false); err != nil {
		t.Errorf("preview should work in read-only: %v", err)
	}
	// Confirm is blocked.
	_, err := p.Push(context.Background(), "u1", true)
	if !errors.Is(err, PushReadOnlyError) {
		t.Errorf("expected PushReadOnlyError, got %v", err)
	}
	if s.createCalls.Load() != 0 {
		t.Errorf("read-only must not create. create=%d", s.createCalls.Load())
	}
}

// TestPush_NotFound on unknown fold_uuid.
func TestPush_NotFound(t *testing.T) {
	s := newPushTestSetup(t)
	p := NewPusher(s.db, firefly.NewClient(s.fakeFirefly.URL, "p", s.fakeFirefly.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	_, err := p.Push(context.Background(), "no-such-uuid", false)
	if !errors.Is(err, PushNotFoundError) {
		t.Errorf("expected PushNotFoundError, got %v", err)
	}
}

// TestPaiseToDecimal covers both directions of the money-string conversion.
func TestPaiseToDecimal(t *testing.T) {
	cases := map[int64]string{
		0:        "0.00",
		1:        "0.01",
		100:      "1.00",
		7000:     "70.00",
		123456:   "1234.56",
		99:       "0.99",
		-50:      "0.50", // negative absolute-valued
		1234500:  "12345.00",
	}
	for in, want := range cases {
		if got := paiseToDecimal(in); got != want {
			t.Errorf("paiseToDecimal(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestFoldTypeToFireflyType covers the (small) type mapping.
func TestFoldTypeToFireflyType(t *testing.T) {
	cases := map[string]string{
		"OUTGOING": "withdrawal",
		"INCOMING": "deposit",
		"outgoing": "withdrawal",
		"":         "withdrawal", // safety default
		"WEIRD":    "withdrawal",
	}
	for in, want := range cases {
		if got := foldTypeToFireflyType(in); got != want {
			t.Errorf("foldTypeToFireflyType(%q) = %q, want %q", in, got, want)
		}
	}
}
