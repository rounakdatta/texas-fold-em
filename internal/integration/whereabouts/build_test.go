package whereabouts_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/whereabouts"
)

// Build reads fold's own copy of the payments: the other side by the
// Firefly account it is, else by the name confirmed, else proposed — money
// back by who sent it — every status, and never a payment at home.
func TestBuildReadsWhatFoldHas(t *testing.T) {
	db, err := integration.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES (70, 'Lantern Noodles, Chinatown, Singapore', 'expense', 1, '{}')`)
	at := time.Date(2026, 3, 7, 2, 0, 0, 0, time.UTC)
	stage := func(uuid, typ, cur, status string, h int, set string, args ...any) {
		t.Helper()
		exec(`INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration, status, foreign_currency, foreign_amount_paise)
			VALUES (?, '{}', 50000, 'INR', ?, 'CARD', ?, 'CARD/x', ?, ?, 600)`, uuid, at.Add(time.Duration(h)*time.Hour), typ, status, cur)
		if set != "" {
			exec(`UPDATE staged_fold_txns SET `+set+` WHERE fold_uuid = ?`, append(args, uuid)...)
		}
	}
	stage("by-id", "OUTGOING", "SGD", "pushed", 0, `confirmed_destination_account_id = 70`)
	stage("confirmed", "OUTGOING", "sgd", "skipped", 2, `confirmed_destination_account_name = 'Harbour Tea, Marina Bay, Singapore'`)
	stage("proposed", "OUTGOING", "SGD", "needs_review", 4, `proposed_destination_account_name = 'Kopi Stall, Singapore'`)
	stage("refund", "INCOMING", "SGD", "needs_review", 5, `proposed_source_account_name = 'Harbour Tea, Marina Bay, Singapore'`)
	stage("home", "OUTGOING", "", "needs_review", 3, `proposed_destination_account_name = 'Chai Corner, Market Road'`)
	stage("inr", "OUTGOING", "INR", "needs_review", 3, `proposed_destination_account_name = 'Book Barge, Paris'`)

	tl, err := whereabouts.Build(context.Background(), db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"by-id", "confirmed", "proposed", "refund"} {
		if l, ok := tl.For(u); !ok || l.Zone.String() != "Asia/Singapore" || l.Place != "Singapore" {
			t.Errorf("%s: %+v %v", u, l, ok)
		}
	}
	for _, u := range []string{"home", "inr"} {
		if _, ok := tl.For(u); ok {
			t.Errorf("%s: read at a foreign time", u)
		}
	}
}
