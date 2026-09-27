package feedback_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
)

// A small world: two of the owner's accounts, two payees, a payer, two
// categories and a budget, all fictional.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	idb, err := integration.Open(context.Background(), filepath.Join(t.TempDir(), "fb.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })
	db := idb.DB
	for _, q := range []string{
		`INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES
		   (1, 'Orchid Bank Card', 'asset', 1, '{}'),
		   (2, 'Orchid Bank Savings', 'asset', 1, '{}'),
		   (40, 'Lantern Books', 'expense', 1, '{}'),
		   (41, 'Tea Trail Cafe', 'expense', 1, '{}'),
		   (60, 'Acme Payroll', 'revenue', 1, '{}')`,
		`INSERT INTO firefly_categories (firefly_id, name) VALUES (5, 'Books'), (6, 'Eating out')`,
		`INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date, budget_id, budget_name, description)
		 VALUES (900, 900, 'withdrawal', 100, 'INR', '2026-01-01', 7, 'Monthly essentials', 'seed')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return db
}

// stage writes a row the way fold sync and the classifier leave one: the
// timestamp bound as a time.Time (the driver's own rendering), a suggestion
// in proposed_*.
func stage(t *testing.T, db *sql.DB, uuid, typ, narration, merchant string, paise int64) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration, merchant_extracted, status,
		    proposed_txn_type, proposed_description, proposed_source_account_id, proposed_destination_account_id, proposed_category_id, proposed_tags_json)
		VALUES (?, '{}', ?, 'INR', ?, 'UPI', ?, ?, ?, 'needs_review', 'withdrawal', 'Book from Lantern', 1, 40, 5, '["reading"]')`,
		uuid, paise, time.Date(2026, 9, 20, 6, 30, 0, 0, time.UTC), typ, narration, merchant); err != nil {
		t.Fatalf("stage: %v", err)
	}
}

func count(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM review_feedback WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The suggestion and what a send would book are read in the names the
// model sees; the payee is the other side of the move.
func TestSnapshotReadsTheSuggestionAndWhatWouldBeSent(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	stage(t, db, "u1", "OUTGOING", "UPI/lantern.books@okbank/Books", "lantern books", 45000)

	snap, err := feedback.SnapshotReview(ctx, db, "u1")
	if err != nil {
		t.Fatal(err)
	}
	want := feedback.ReviewValues{Type: "withdrawal", Title: "Book from Lantern", Payee: "Lantern Books", Category: "Books", Tags: []string{"reading"}}
	if !snap.Suggested.Equal(want) || !snap.Effective.Equal(want) {
		t.Fatalf("untouched row: suggested %+v, effective %+v; want both %+v", snap.Suggested, snap.Effective, want)
	}
	if snap.MerchantKey != "lantern books" || snap.Direction != "OUTGOING" || snap.AmountPaise != 45000 {
		t.Errorf("snapshot context = %q %q %d", snap.MerchantKey, snap.Direction, snap.AmountPaise)
	}

	// The owner corrects the title, the payee (a name-only new one), clears
	// the category and sets a budget.
	if _, err := db.Exec(`UPDATE staged_fold_txns SET confirmed_description = 'Two novels for the trip', confirmed_destination_account_name = 'Lantern Books, Indiranagar',
	    confirmed_category_id = 0, confirmed_budget_id = 7, confirmed_tags_json = '[]' WHERE fold_uuid = 'u1'`); err != nil {
		t.Fatal(err)
	}
	snap, _ = feedback.SnapshotReview(ctx, db, "u1")
	got := snap.Effective
	if got.Title != "Two novels for the trip" || got.Payee != "Lantern Books, Indiranagar" || got.Category != feedback.NoneValue ||
		got.Budget != "Monthly essentials" || len(got.Tags) != 0 {
		t.Errorf("after the corrections, effective = %+v", got)
	}
	if !snap.Suggested.Equal(want) {
		t.Errorf("the suggestion moved with the corrections: %+v", snap.Suggested)
	}
}

// Money in: the payee is who paid, from the source side.
func TestSnapshotPayeeOfMoneyInIsThePayer(t *testing.T) {
	db := openDB(t)
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration, status,
		    proposed_txn_type, proposed_source_account_id, proposed_destination_account_id, foreign_amount_paise, foreign_currency)
		VALUES ('in1', '{}', 5000000, 'INR', '2026-09-01 04:00:00', 'NEFT', 'INCOMING', 'NEFT/ACME PAYROLL', 'needs_review', 'deposit', 60, 2, 60000, 'usd')`); err != nil {
		t.Fatal(err)
	}
	snap, err := feedback.SnapshotReview(context.Background(), db, "in1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Suggested.Payee != "Acme Payroll" {
		t.Errorf("payee of money in = %q, want the payer", snap.Suggested.Payee)
	}
	if snap.ForeignLabel != "USD 600.00" {
		t.Errorf("foreign label = %q, want USD 600.00", snap.ForeignLabel)
	}
}

// Only a save that changed what the card says is a correction; re-posting
// the same values records nothing.
func TestAnEditIsRecordedOnlyWhenItChangedSomething(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	stage(t, db, "u1", "OUTGOING", "UPI/lantern.books@okbank/Books", "lantern books", 45000)

	before, _ := feedback.SnapshotReview(ctx, db, "u1")
	if err := feedback.RecordEdit(ctx, db, "u1", before); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `1=1`); n != 0 {
		t.Fatalf("a save that changed nothing recorded %d rows", n)
	}

	before, _ = feedback.SnapshotReview(ctx, db, "u1")
	if _, err := db.Exec(`UPDATE staged_fold_txns SET confirmed_description = 'Two novels for the trip' WHERE fold_uuid = 'u1'`); err != nil {
		t.Fatal(err)
	}
	if err := feedback.RecordEdit(ctx, db, "u1", before); err != nil {
		t.Fatal(err)
	}
	var action, merchant, sj, cj string
	if err := db.QueryRow(`SELECT action, merchant_key, suggested_json, chosen_json FROM review_feedback`).Scan(&action, &merchant, &sj, &cj); err != nil {
		t.Fatal(err)
	}
	var sug, cho feedback.ReviewValues
	_ = json.Unmarshal([]byte(sj), &sug)
	_ = json.Unmarshal([]byte(cj), &cho)
	if action != feedback.FeedbackEdit || merchant != "lantern books" || sug.Title != "Book from Lantern" || cho.Title != "Two novels for the trip" {
		t.Errorf("recorded %s for %q: suggested %q → chose %q", action, merchant, sug.Title, cho.Title)
	}
}

func TestDecisionsAreRecordedWithTheRowAsItStands(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	stage(t, db, "u1", "OUTGOING", "n", "lantern books", 45000)
	for _, a := range []string{feedback.FeedbackSend, feedback.FeedbackHold, feedback.FeedbackSkip, feedback.FeedbackRelease} {
		if err := feedback.RecordDecision(ctx, db, "u1", a, "why "+a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"send", "hold", "skip", "release"} {
		if n := count(t, db, `action = ? AND note = ?`, a, "why "+a); n != 1 {
			t.Errorf("%s recorded %d times", a, n)
		}
	}
	if err := feedback.RecordDecision(ctx, db, "no-such-row", feedback.FeedbackSend, ""); err == nil {
		t.Error("a decision on a row that doesn't exist was recorded")
	}
}

// The first start after the upgrade seeds months of history — each past
// decision once, at its own time — and never again. A hold fold set is
// fold's, not the owner's, so it is not seeded as their decision.
func TestBackfillSeedsPastDecisionsOnce(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	for _, u := range []string{"sent", "skipped", "held", "foldheld", "edited", "untouched", "reviewedsame"} {
		stage(t, db, u, "OUTGOING", "n "+u, "lantern books", 100)
	}
	for _, q := range []string{
		`UPDATE staged_fold_txns SET status = 'pushed', pushed_at = '2026-09-02 10:00:00', confirmed_description = 'A cookbook' WHERE fold_uuid = 'sent'`,
		`UPDATE staged_fold_txns SET status = 'skipped', updated_at = '2026-09-03 10:00:00' WHERE fold_uuid = 'skipped'`,
		`UPDATE staged_fold_txns SET hold_reason = 'never billed' WHERE fold_uuid = 'held'`,
		`UPDATE staged_fold_txns SET hold_reason = 'looks like a released card hold', hold_by = 'fold' WHERE fold_uuid = 'foldheld'`,
		`UPDATE staged_fold_txns SET reviewed_at = '2026-09-04 10:00:00', confirmed_category_id = 6 WHERE fold_uuid = 'edited'`,
		`UPDATE staged_fold_txns SET reviewed_at = '2026-09-05 10:00:00' WHERE fold_uuid = 'reviewedsame'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	n, err := feedback.BackfillFeedback(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("backfilled %d rows, want 4 (sent, skipped, held, edited)", n)
	}
	for uuid, want := range map[string]string{"sent": "send", "skipped": "skip", "held": "hold", "edited": "edit"} {
		if c := count(t, db, `fold_uuid = ? AND action = ?`, uuid, want); c != 1 {
			t.Errorf("%s: %d %q rows, want 1", uuid, c, want)
		}
	}
	for _, uuid := range []string{"foldheld", "untouched", "reviewedsame"} {
		if c := count(t, db, `fold_uuid = ?`, uuid); c != 0 {
			t.Errorf("%s was seeded as a decision (%d rows)", uuid, c)
		}
	}
	var at string
	_ = db.QueryRow(`SELECT at || '' FROM review_feedback WHERE fold_uuid = 'sent'`).Scan(&at)
	if at != "2026-09-02 10:00:00" {
		t.Errorf("the send was seeded at %q, want its own time", at)
	}
	var chosen string
	_ = db.QueryRow(`SELECT chosen_json FROM review_feedback WHERE fold_uuid = 'sent'`).Scan(&chosen)
	if !json.Valid([]byte(chosen)) || !strings.Contains(chosen, "A cookbook") {
		t.Errorf("the send's chosen values = %s, want what was sent", chosen)
	}

	if again, err := feedback.BackfillFeedback(ctx, db); err != nil || again != 0 {
		t.Errorf("a second backfill wrote %d rows (%v); it must run once", again, err)
	}
}
