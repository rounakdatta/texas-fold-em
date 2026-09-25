package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// linkFirefly is a fake firefly for refund-link tests: creates always return
// group 7950 / journal 7951; link types include "Refund" (id 2); journal
// links and the link-create status are configurable.
type linkFirefly struct {
	mu           sync.Mutex
	linkBodies   []map[string]any
	linkStatus   int
	journalLinks string // JSON array body for GET /transaction-journals/{id}/links
}

func (f *linkFirefly) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search/transactions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("/api/v1/transactions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"data":{"id":"7950","attributes":{"transactions":[{"transaction_journal_id":"7951"}]}}}`))
	})
	mux.HandleFunc("/api/v1/link-types", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"1","attributes":{"name":"Related"}},{"id":"2","attributes":{"name":"Refund","inward":"is (partially) refunded by","outward":"(partially) refunds"}}]}`))
	})
	mux.HandleFunc("/api/v1/transaction-journals/", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		body := f.journalLinks
		f.mu.Unlock()
		if body == "" {
			body = "[]"
		}
		_, _ = w.Write([]byte(`{"data":` + body + `}`))
	})
	mux.HandleFunc("/api/v1/transaction-links", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.linkBodies = append(f.linkBodies, b)
		status := f.linkStatus
		f.mu.Unlock()
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"31"}}`))
	})
	return mux
}

func newLinkSetup(t *testing.T) (*DB, *Pusher, *linkFirefly) {
	t.Helper()
	idb, err := Open(context.Background(), filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idb.Close() })
	ff := &linkFirefly{}
	srv := httptest.NewServer(ff.handler())
	t.Cleanup(srv.Close)
	p := NewPusher(idb, firefly.NewClient(srv.URL, "pat", srv.Client()), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	return idb, p, ff
}

func stageRow(t *testing.T, db *DB, uuid, typ, status string, journal int64, refundOf string) {
	t.Helper()
	var jid, ref any
	if journal != 0 {
		jid = journal
	}
	if refundOf != "" {
		ref = refundOf
	}
	src, dst := any(954), any(11) // purchase: card → Zomato
	txnType := "withdrawal"
	if typ == "INCOMING" {
		src, dst, txnType = 2011, 954, "deposit" // refund: Zomato's revenue twin → card
	}
	if _, err := db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration,
		    merchant_extracted, status, firefly_txn_id, proposed_source_account_id, proposed_destination_account_id,
		    proposed_txn_type, proposed_description, proposed_refund_of)
		VALUES (?, '{}', 48620, 'INR', '2026-03-14T11:59:14Z', 'CARD', ?, 'CARD/x/Zomato/x', 'zomato', ?, ?, ?, ?, ?, 'x', ?)`,
		uuid, typ, status, jid, src, dst, txnType, ref); err != nil {
		t.Fatal(err)
	}
}

func linkIDOf(t *testing.T, db *DB, uuid string) sql.NullInt64 {
	t.Helper()
	var id sql.NullInt64
	if err := db.DB.QueryRow(`SELECT firefly_link_id FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func auditCount(t *testing.T, db *DB, action string) int {
	t.Helper()
	var n int
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n)
	return n
}

// Pushing a refund whose purchase is already in firefly links them at once,
// with the refund as the link's inward side so firefly reads
// "<refund> (partially) refunds <purchase>".
func TestPush_RefundLinksToPushedPurchase(t *testing.T) {
	db, p, ff := newLinkSetup(t)
	stageRow(t, db, "buy-1", "OUTGOING", "pushed", 5001, "")
	stageRow(t, db, "ref-1", "INCOMING", "ready_to_push", 0, "fold:buy-1")

	if _, err := p.Push(context.Background(), "ref-1", true); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(ff.linkBodies) != 1 {
		t.Fatalf("link calls = %d, want 1", len(ff.linkBodies))
	}
	b := ff.linkBodies[0]
	if b["link_type_id"] != float64(2) || b["inward_id"] != float64(7951) || b["outward_id"] != float64(5001) {
		t.Errorf("link body = %v, want type 2, inward 7951 (the refund), outward 5001 (the purchase)", b)
	}
	if id := linkIDOf(t, db, "ref-1"); !id.Valid || id.Int64 != 31 {
		t.Errorf("firefly_link_id = %v, want 31", id)
	}
	if auditCount(t, db, "firefly_link_create") != 1 {
		t.Error("expected a firefly_link_create audit row")
	}
}

// Pushing the purchase links every already-pushed refund waiting for it.
func TestPush_PurchaseLinksWaitingRefund(t *testing.T) {
	db, p, ff := newLinkSetup(t)
	stageRow(t, db, "buy-2", "OUTGOING", "ready_to_push", 0, "")
	stageRow(t, db, "ref-2", "INCOMING", "pushed", 6001, "fold:buy-2")

	if _, err := p.Push(context.Background(), "buy-2", true); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(ff.linkBodies) != 1 || ff.linkBodies[0]["inward_id"] != float64(6001) || ff.linkBodies[0]["outward_id"] != float64(7951) {
		t.Fatalf("link bodies = %v, want one link inward 6001 → outward 7951", ff.linkBodies)
	}
	if id := linkIDOf(t, db, "ref-2"); !id.Valid || id.Int64 != 31 {
		t.Errorf("waiting refund's firefly_link_id = %v, want 31", id)
	}
}

// A refund pushed before its purchase waits: no link call, no error.
func TestPush_RefundWaitsForUnpushedPurchase(t *testing.T) {
	db, p, ff := newLinkSetup(t)
	stageRow(t, db, "buy-3", "OUTGOING", "ready_to_push", 0, "")
	stageRow(t, db, "ref-3", "INCOMING", "ready_to_push", 0, "fold:buy-3")

	if _, err := p.Push(context.Background(), "ref-3", true); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(ff.linkBodies) != 0 || linkIDOf(t, db, "ref-3").Valid {
		t.Errorf("linked before the purchase was in firefly: %v", ff.linkBodies)
	}
	if _, err := p.LinkRefund(context.Background(), "ref-3"); !errors.Is(err, ErrRefundOriginalNotInFirefly) {
		t.Errorf("LinkRefund err = %v, want ErrRefundOriginalNotInFirefly", err)
	}
}

// A Refund link firefly already has (either orientation) is adopted, not duplicated.
func TestLinkRefund_AdoptsExistingLink(t *testing.T) {
	db, p, ff := newLinkSetup(t)
	ff.journalLinks = `[{"id":"77","attributes":{"link_type_id":"2","inward_id":"5001","outward_id":"6002"}}]`
	stageRow(t, db, "buy-4", "OUTGOING", "pushed", 5001, "")
	stageRow(t, db, "ref-4", "INCOMING", "pushed", 6002, "fold:buy-4")

	id, err := p.LinkRefund(context.Background(), "ref-4")
	if err != nil || id != 77 {
		t.Fatalf("LinkRefund = %d, %v; want the existing link 77", id, err)
	}
	if len(ff.linkBodies) != 0 {
		t.Errorf("created a duplicate link: %v", ff.linkBodies)
	}
	if auditCount(t, db, "firefly_link_existing") != 1 {
		t.Error("expected a firefly_link_existing audit row")
	}
}

// A failing link never fails the push: the transaction is what matters.
func TestPush_LinkFailureDoesNotFailPush(t *testing.T) {
	db, p, ff := newLinkSetup(t)
	ff.linkStatus = 500
	stageRow(t, db, "buy-5", "OUTGOING", "pushed", 5001, "")
	stageRow(t, db, "ref-5", "INCOMING", "ready_to_push", 0, "fold:buy-5")

	rep, err := p.Push(context.Background(), "ref-5", true)
	if err != nil || rep.Action != "created" {
		t.Fatalf("push = %+v, %v; want created", rep, err)
	}
	if linkIDOf(t, db, "ref-5").Valid {
		t.Error("firefly_link_id set despite the link failing")
	}
	if auditCount(t, db, "firefly_link_error") != 1 {
		t.Error("expected a firefly_link_error audit row")
	}
}

// A journal reference (a purchase fold never staged) links directly.
func TestLinkRefund_JournalReference(t *testing.T) {
	db, p, ff := newLinkSetup(t)
	stageRow(t, db, "ref-6", "INCOMING", "pushed", 6003, "journal:7931")
	if id, err := p.LinkRefund(context.Background(), "ref-6"); err != nil || id != 31 {
		t.Fatalf("LinkRefund = %d, %v", id, err)
	}
	if ff.linkBodies[0]["outward_id"] != float64(7931) {
		t.Errorf("outward = %v, want the firefly journal 7931", ff.linkBodies[0]["outward_id"])
	}
}

func TestLinkRefund_ReadOnlyAndNotARefund(t *testing.T) {
	db, p, _ := newLinkSetup(t)
	stageRow(t, db, "buy-7", "OUTGOING", "pushed", 5001, "")
	if _, err := p.LinkRefund(context.Background(), "buy-7"); err == nil || !strings.Contains(err.Error(), "no original purchase") {
		t.Errorf("purchase LinkRefund err = %v, want not-a-refund", err)
	}
	p.readOnly = true
	if _, err := p.LinkRefund(context.Background(), "buy-7"); !errors.Is(err, PushReadOnlyError) {
		t.Errorf("read-only err = %v", err)
	}
}
