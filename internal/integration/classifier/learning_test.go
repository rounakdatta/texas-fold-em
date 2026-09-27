package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// All fictional: a bookshop, a café, a streaming service, a person paid by
// UPI, a trip abroad.

func learnDB(t *testing.T) *sql.DB {
	t.Helper()
	idb, err := integration.Open(context.Background(), filepath.Join(t.TempDir(), "learn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })
	db := idb.DB
	mustExec(t, db, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES
		(1, 'Orchid Bank Card', 'asset', 1, '{}'),
		(40, 'Lantern Books, Indiranagar', 'expense', 1, '{}'),
		(41, 'Tea Trail Cafe, Koramangala', 'expense', 1, '{}'),
		(42, 'Streamly', 'expense', 1, '{}'),
		(43, 'Ravi Kumar', 'expense', 1, '{}')`)
	mustExec(t, db, `INSERT INTO firefly_categories (firefly_id, name) VALUES (5, 'Books'), (6, 'Eating out'), (7, 'Subscriptions'), (8, 'Household help')`)
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q[:min(len(q), 60)], err)
	}
}

// fireflyRow adds a firefly journal the way the mirror sync stores one.
func fireflyRow(t *testing.T, db *sql.DB, id int64, date time.Time, dstID int64, dst, cat, desc, notes, tags string, paise int64) {
	t.Helper()
	catID := map[string]int64{"Books": 5, "Eating out": 6, "Subscriptions": 7, "Household help": 8}[cat]
	mustExec(t, db, `INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		source_account_id, source_account_name, destination_account_id, destination_account_name, destination_account_name_normalized,
		category_id, category_name, description, notes, tags_json)
		VALUES (?, ?, 'withdrawal', ?, 'INR', ?, 1, 'Orchid Bank Card', ?, ?, LOWER(?), NULLIF(?, 0), NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''))`,
		id, id, paise, date, dstID, dst, dst, catID, cat, desc, notes, tags)
}

// stagedAt stages a fold row with its timestamp bound as a time.Time — the
// shape fold sync writes ("2026-09-20 06:30:00 +0000 UTC"), which SQLite's
// own date functions cannot read.
func stagedAt(t *testing.T, db *sql.DB, uuid, typ, narration, merchant string, paise int64, at time.Time, status string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration, merchant_extracted, status)
		VALUES (?, '{}', ?, 'INR', ?, 'UPI', ?, ?, ?, ?)`, uuid, paise, at, typ, narration, merchant, status)
}

func propose(t *testing.T, db *sql.DB, uuid string, dst int64, cat int64, title, tags string) {
	t.Helper()
	mustExec(t, db, `UPDATE staged_fold_txns SET proposed_txn_type = 'withdrawal', proposed_source_account_id = 1,
		proposed_destination_account_id = ?, proposed_category_id = ?, proposed_description = ?, proposed_tags_json = NULLIF(?, '') WHERE fold_uuid = ?`,
		dst, cat, title, tags, uuid)
}

func recordAt(t *testing.T, db *sql.DB, uuid, action, merchant, narration string, sug, cho feedback.ReviewValues, note, at string) {
	t.Helper()
	sj, _ := json.Marshal(sug)
	cj, _ := json.Marshal(cho)
	mustExec(t, db, `INSERT INTO review_feedback (fold_uuid, at, action, merchant_key, narration, direction, amount_paise, suggested_json, chosen_json, note)
		VALUES (?, ?, ?, ?, ?, 'OUTGOING', 45000, ?, ?, ?)`, uuid, at, action, merchant, narration, string(sj), string(cj), note)
}

var ist = time.FixedZone("IST", 19800)

func TestHandleTokensFindWhoWasPaid(t *testing.T) {
	cases := map[string][]string{
		"UPI/ravi.kumar42@oksbi/Payment from phone/CANARA":                  {"ravi.kumar42@oksbi"},
		"UPI/Q12345678@ybl/UPI/YES BANK":                                    {"q12345678@ybl"},
		"CARD/3a4b5c6d7e8f9a0b/TEA TRAIL CAFE/Rs./450.00/OUTGOING/20-09-26": {"/TEA TRAIL CAFE/"},
		"NEFT/ACME PAYROLL/SALARY SEP":                                      nil,
		"UPI/aa@okx/UPI/bb@okx/UPI/cc@okx/UPI/dd@okx":                       {"aa@okx", "bb@okx", "cc@okx"},
		"CARD/5d2c4b3a29181706/shop.123@okbank/Rs./64.00/OUTGOING/14-09-26": {"shop.123@okbank"},
		"CARD/7c1e0a5b9d3f2e11//USD/42.50/INCOMING/is credited back to you": nil,
	}
	for narration, want := range cases {
		got := handleTokens(narration)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("handleTokens(%q) = %q, want %q", narration, got, want)
		}
	}
}

// The strongest evidence of how the owner books a merchant is how they
// corrected fold on it before. Those corrections come first; an edit that
// changed nothing isn't one; and the row being graded never sees itself.
func TestCorrectionsForTheSameMerchantComeFirst(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	sug := feedback.ReviewValues{Title: "Book from Lantern", Payee: "Lantern Books", Category: "Books"}
	cho := feedback.ReviewValues{Title: "Two novels for the trip", Payee: "Lantern Books, Indiranagar", Category: "Books"}
	recordAt(t, db, "old-edit", "edit", "lantern books", "UPI/lantern.books@okbank/x", sug, cho, "", "2026-08-01 10:00:00")
	recordAt(t, db, "noop-edit", "edit", "lantern books", "UPI/lantern.books@okbank/y", cho, cho, "", "2026-08-02 10:00:00")
	recordAt(t, db, "elsewhere", "edit", "tea trail cafe", "UPI/teatrail@okaxis/z",
		feedback.ReviewValues{Title: "Coffee"}, feedback.ReviewValues{Title: "Filter coffee and bun"}, "", "2026-08-03 10:00:00")
	recordAt(t, db, "sent", "send", "lantern books", "UPI/lantern.books@okbank/w", cho, cho, "", "2026-08-04 10:00:00")
	recordAt(t, db, "target", "edit", "lantern books", "UPI/lantern.books@okbank/t",
		sug, feedback.ReviewValues{Title: "THE ANSWER"}, "", "2026-08-05 10:00:00")

	staged := StagedRow{FoldUUID: "new", Narration: "UPI/lantern.books@okbank/Books", MerchantExtracted: "lantern books", Type: "OUTGOING"}
	corr, dec := c.learnedExamples(context.Background(), staged, exclusion{foldUUID: "target"})
	if len(corr) != 2 || corr[0].why != "same merchant" || corr[0].chosen.Title != "Two novels for the trip" {
		t.Fatalf("corrections = %+v; want the same-merchant one first, then the recent one elsewhere", corr)
	}
	if corr[1].chosen.Title != "Filter coffee and bun" || corr[1].why != "recent" {
		t.Errorf("second correction = %+v; want the recent one from another merchant", corr[1])
	}
	for _, e := range corr {
		if e.chosen.Title == "THE ANSWER" {
			t.Fatal("the row under evaluation was shown its own correction")
		}
		if e.suggested.Equal(e.chosen) {
			t.Errorf("an edit that changed nothing was shown as a correction: %+v", e)
		}
	}
	if len(dec) != 1 || dec[0].action != "send" {
		t.Errorf("decisions = %+v; want the one send", dec)
	}
}

// Only a few corrections from unrelated merchants make it in: style drift,
// not noise.
func TestUnrelatedCorrectionsAreCapped(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	for i := range 12 {
		recordAt(t, db, "e"+string(rune('a'+i)), "edit", "merchant "+string(rune('a'+i)), "n",
			feedback.ReviewValues{Title: "x"}, feedback.ReviewValues{Title: "y" + string(rune('a'+i))}, "", "2026-08-01 10:00:00")
	}
	corr, _ := c.learnedExamples(context.Background(), StagedRow{FoldUUID: "new", MerchantExtracted: "unseen", Narration: "n"}, exclusion{})
	if len(corr) != maxGlobalCorrections {
		t.Errorf("unrelated corrections shown = %d, want %d", len(corr), maxGlobalCorrections)
	}
}

// A handle the owner paid before is the same counterparty: the ledger rows
// whose notes carry it say who that is — never counting the row graded.
func TestHandleHistoryNamesWhoTheLedgerSaysItIs(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	d := time.Date(2026, 6, 1, 4, 0, 0, 0, time.UTC)
	fireflyRow(t, db, 501, d, 43, "Ravi Kumar", "Household help", "Cleaning, June", "UPI/ravi.kumar42@oksbi/June", `["home"]`, 300000)
	fireflyRow(t, db, 502, d.AddDate(0, 1, 0), 43, "Ravi Kumar", "Household help", "Cleaning, July", "UPI/ravi.kumar42@oksbi/July", `["home"]`, 300000)
	fireflyRow(t, db, 503, d.AddDate(0, 2, 0), 43, "Ravi Kumar", "Household help", "Cleaning, August", "UPI/ravi.kumar42@oksbi/Aug", `["home"]`, 300000)
	fireflyRow(t, db, 504, d, 41, "Tea Trail Cafe, Koramangala", "Eating out", "Coffee", "UPI/teatrail@okaxis/x", "", 25000)

	staged := StagedRow{FoldUUID: "new", Narration: "UPI/ravi.kumar42@oksbi/Payment from phone", Type: "OUTGOING", AmountPaise: 300000}
	hits := c.handleHistory(context.Background(), staged, exclusion{fireflyID: 503})
	if len(hits) != 1 {
		t.Fatalf("hits = %+v; want one handle", hits)
	}
	h := hits[0]
	// the payee's budget habit rides along — here, none, stated as a fact
	if h.count != 2 || len(h.payees) != 1 || !strings.HasPrefix(h.payees[0], "Ravi Kumar · Household help · no budget (×2)") {
		t.Errorf("handle history = %+v; want Ravi Kumar ×2 (the excluded row not counted)", h)
	}
	for _, title := range h.titles {
		if title == "Cleaning, August" {
			t.Error("the excluded row's title leaked into the handle history")
		}
	}
	if len(h.tagsSeen) != 1 || !strings.HasPrefix(h.tagsSeen[0], "home") {
		t.Errorf("tags = %v, want home", h.tagsSeen)
	}
}

// What happened around a transaction — found from timestamps as fold sync
// stores them. (Written with SQLite's datetime(), this matched nothing in
// production, where every stored timestamp reads "… +0000 UTC".)
func TestNeighboursAreFoundAroundTheTransaction(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	at := time.Date(2026, 9, 20, 7, 0, 0, 0, time.UTC) // 12:30 IST
	stagedAt(t, db, "ride", "OUTGOING", "UPI/ridely@ok", "ridely", 18000, at.Add(-50*time.Minute), "pushed")
	propose(t, db, "ride", 42, 7, "Ride to Tea Trail", "")
	stagedAt(t, db, "coffee-later", "OUTGOING", "UPI/teatrail@okaxis", "tea trail cafe", 26000, at.Add(3*time.Hour), "needs_review")
	propose(t, db, "coffee-later", 41, 6, "Filter coffee", "")
	stagedAt(t, db, "skipped", "OUTGOING", "UPI/x@ok", "x", 100, at.Add(time.Hour), "skipped")
	stagedAt(t, db, "too-far", "OUTGOING", "UPI/y@ok", "y", 100, at.Add(-40*time.Hour), "pushed")
	stagedAt(t, db, "graded", "OUTGOING", "UPI/z@ok", "z", 100, at.Add(2*time.Hour), "pushed")
	mustExec(t, db, `UPDATE staged_fold_txns SET hold_reason = 'check the statement' WHERE fold_uuid = 'coffee-later'`)

	staged := StagedRow{FoldUUID: "lunch", TxnTimestamp: at.Format(time.RFC3339), Type: "OUTGOING"}
	ns := c.neighbours(context.Background(), staged, exclusion{foldUUID: "graded"})
	var got []string
	for _, n := range ns {
		if n.owner {
			got = append(got, n.title+"["+n.state+"]")
		} else {
			got = append(got, "unreviewed "+n.bank+"["+n.state+"]")
		}
	}
	// the coffee is held, but nobody has reviewed its title: an older
	// engine's "Filter coffee" is not the owner's word
	if strings.Join(got, ", ") != "Ride to Tea Trail[sent], unreviewed UPI/teatrail@okaxis[held]" {
		t.Errorf("neighbours = %v; want the ride before and the held coffee after, oldest first — not the skipped, the far or the graded row", got)
	}
}

// A neighbour sent to firefly reads as firefly has it — the tags an older
// engine suggested were never sent (push sends confirmed tags only) — and a
// waiting card reads as the owner confirmed it.
func TestNeighboursReadAsTheOwnerDecided(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	at := time.Date(2026, 9, 20, 7, 0, 0, 0, time.UTC)
	stagedAt(t, db, "sent", "OUTGOING", "UPI/ridely@ok", "ridely", 18000, at.Add(-time.Hour), "pushed")
	propose(t, db, "sent", 42, 7, "Ride (guess)", `["old-engine-tag"]`)
	fireflyRow(t, db, 880, at.Add(-time.Hour), 42, "Streamly", "Subscriptions", "Ride to the office", "", "", 18000)
	mustExec(t, db, `UPDATE firefly_txns SET external_id = 'sent' WHERE firefly_id = 880`)
	stagedAt(t, db, "mine", "OUTGOING", "UPI/teatrail@okaxis", "tea trail cafe", 26000, at.Add(time.Hour), "needs_review")
	propose(t, db, "mine", 41, 6, "Coffee (guess)", `["old-engine-tag"]`)
	mustExec(t, db, `UPDATE staged_fold_txns SET confirmed_description = 'Filter coffee with Ravi' WHERE fold_uuid = 'mine'`)

	ns := c.neighbours(context.Background(), StagedRow{FoldUUID: "x", TxnTimestamp: at.Format(time.RFC3339)}, exclusion{})
	if len(ns) != 2 || ns[0].title != "Ride to the office" || ns[1].title != "Filter coffee with Ravi" {
		t.Fatalf("neighbours = %+v; want firefly's title for the sent one, the confirmed one for the waiting one", ns)
	}
	for _, n := range ns {
		if len(n.tags) != 0 {
			t.Errorf("%q carries %v: tags nobody chose, never sent", n.title, n.tags)
		}
	}
}

// Many neighbours: the closest are kept.
func TestTheClosestNeighboursAreKept(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	at := time.Date(2026, 9, 20, 7, 0, 0, 0, time.UTC)
	for i := range 20 {
		u := "n" + string(rune('a'+i))
		stagedAt(t, db, u, "OUTGOING", "n", "m", 100, at.Add(time.Duration(i+1)*time.Hour), "pushed")
		propose(t, db, u, 41, 6, u, "")
	}
	ns := c.neighbours(context.Background(), StagedRow{FoldUUID: "x", TxnTimestamp: at.Format(time.RFC3339)}, exclusion{})
	if len(ns) != maxNeighbours || ns[0].title != "na" || ns[len(ns)-1].title != "nn" {
		t.Errorf("kept %d neighbours from %q to %q; want the %d closest, na…nn", len(ns), ns[0].title, ns[len(ns)-1].title, maxNeighbours)
	}
}

// A run of charges in the same foreign currency is a trip, and the tags on
// them are the trip's tag.
func TestAForeignChargeSeesItsTrip(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	at := time.Date(2026, 9, 12, 5, 0, 0, 0, time.UTC)
	for i, u := range []string{"t1", "t2", "t3"} {
		stagedAt(t, db, u, "OUTGOING", "CARD/x/SHOP/SGD/5.00/OUTGOING", "shop", 30000, at.Add(time.Duration(i-1)*48*time.Hour), "needs_review")
		propose(t, db, u, 41, 6, "Lunch", "")
		// the owner tagged the trip themselves
		mustExec(t, db, `UPDATE staged_fold_txns SET foreign_currency = 'SGD', foreign_amount_paise = 500, confirmed_tags_json = '["far-trip-2026"]' WHERE fold_uuid = ?`, u)
	}
	// an unreviewed card whose tag an older engine guessed doesn't count
	stagedAt(t, db, "guess", "OUTGOING", "CARD/x/KIOSK/SGD/3.00/OUTGOING", "kiosk", 18000, at.Add(-24*time.Hour), "needs_review")
	propose(t, db, "guess", 41, 6, "Snack", `["guessed-tag"]`)
	mustExec(t, db, `UPDATE staged_fold_txns SET foreign_currency = 'SGD', foreign_amount_paise = 300 WHERE fold_uuid = 'guess'`)
	stagedAt(t, db, "home", "OUTGOING", "UPI/x", "x", 100, at, "pushed") // not in SGD
	stagedAt(t, db, "new", "OUTGOING", "CARD/x/HAWKER/SGD/4.50/OUTGOING", "hawker", 27000, at.Add(time.Hour), "needs_review")
	mustExec(t, db, `UPDATE staged_fold_txns SET foreign_currency = 'SGD', foreign_amount_paise = 450 WHERE fold_uuid = 'new'`)

	trip := c.tripContext(context.Background(), StagedRow{FoldUUID: "new", TxnTimestamp: at.Add(time.Hour).Format(time.RFC3339), RawPayload: `{}`}, exclusion{})
	if !strings.Contains(trip, "4 other SGD charges") || !strings.Contains(trip, "far-trip-2026 ×3") || strings.Contains(trip, "guessed-tag") {
		t.Errorf("trip context = %q; want the four SGD charges and the owner's tag — not the guessed one", trip)
	}
	if home := c.tripContext(context.Background(), StagedRow{FoldUUID: "home", TxnTimestamp: at.Format(time.RFC3339), RawPayload: `{}`}, exclusion{}); home != "" {
		t.Errorf("a rupee charge got trip context %q", home)
	}
}

// The same amount to the same payee on a steady cadence is a subscription:
// its last title is the one to reuse.
func TestARecurringChargeIsRecognised(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	d := time.Date(2026, 3, 5, 4, 0, 0, 0, time.UTC)
	for i := range 6 {
		fireflyRow(t, db, int64(600+i), d.AddDate(0, i, 0), 42, "Streamly", "Subscriptions", "Streamly premium, monthly", "CARD/x/STREAMLY", "", 64900+int64(i%2)*1000)
	}
	payee := int64(42)
	got := c.recurringContext(context.Background(), StagedRow{AmountPaise: 64900}, &payee, nil, exclusion{})
	if !strings.Contains(got, "monthly") || !strings.Contains(got, `"Streamly premium, monthly"`) || !strings.Contains(got, "6 times") {
		t.Errorf("recurring = %q; want a monthly charge, 6 times, with its title", got)
	}
	// Irregular payments aren't a subscription.
	db2 := learnDB(t)
	c2 := New(db2, slog.Default(), DefaultConfidenceThreshold, 10)
	for i, gap := range []int{0, 3, 40, 41, 90} {
		fireflyRow(t, db2, int64(700+i), d.AddDate(0, 0, gap), 42, "Streamly", "Subscriptions", "x", "", "", 64900)
	}
	if got := c2.recurringContext(context.Background(), StagedRow{AmountPaise: 64900}, &payee, nil, exclusion{}); got != "" {
		t.Errorf("irregular payments read as recurring: %q", got)
	}
}

// capturingLLM answers every request with reply and keeps each prompt.
type capturingLLM struct {
	*httptest.Server
	mu      sync.Mutex
	prompts []string
	reply   string
}

func newCapturingLLM(t *testing.T, reply string) *capturingLLM {
	t.Helper()
	cl := &capturingLLM{reply: reply}
	cl.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cl.mu.Lock()
		if n := len(body.Messages); n > 0 {
			cl.prompts = append(cl.prompts, body.Messages[n-1].Content)
		}
		reply := cl.reply
		cl.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 900, "completion_tokens": 100},
		})
	}))
	t.Cleanup(cl.Close)
	return cl
}

func (cl *capturingLLM) last() string {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.prompts) == 0 {
		return ""
	}
	return cl.prompts[len(cl.prompts)-1]
}

// End to end: what the owner decided before reaches the model, in blocks
// ahead of the deterministic hints, and the decision says what it drew on.
func TestThePromptCarriesWhatTheOwnerDecidedBefore(t *testing.T) {
	db := learnDB(t)
	at := time.Date(2026, 9, 20, 7, 0, 0, 0, time.UTC)
	recordAt(t, db, "old", "edit", "ravi kumar", "UPI/ravi.kumar42@oksbi/x",
		feedback.ReviewValues{Title: "Payment to Ravi"}, feedback.ReviewValues{Title: "Cleaning, August", Payee: "Ravi Kumar", Category: "Household help"}, "", "2026-08-30 10:00:00")
	fireflyRow(t, db, 501, at.AddDate(0, -1, 0), 43, "Ravi Kumar", "Household help", "Cleaning, August", "UPI/ravi.kumar42@oksbi/Aug", "", 300000)
	stagedAt(t, db, "ride", "OUTGOING", "UPI/ridely@ok", "ridely", 18000, at.Add(-time.Hour), "pushed")
	propose(t, db, "ride", 42, 7, "Ride home", "")
	stagedAt(t, db, "new", "OUTGOING", "UPI/ravi.kumar42@oksbi/Payment from phone", "ravi kumar", 300000, at, "pending")

	fake := newCapturingLLM(t, `{"reasoning":"your correction of 30 Aug names Ravi Kumar","txn_type":"withdrawal","destination_account_id":43,
		"category_id":8,"tags":[],"description_suggestion":"Cleaning, ___","unknowns":["month"],"evidence":["your correction of 30 Aug","handle paid before"],
		"hold_suggestion":null,"confidence":0.9}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "claude-opus-5-5", fake.URL, fake.Client()))

	rows, _ := c.fetchStagedForClassify(context.Background(), `fold_uuid = 'new'`)
	d, err := c.ClassifyOne(context.Background(), rows[0])
	if err != nil {
		t.Fatal(err)
	}
	p := fake.last()
	for _, block := range []string{"== HOW YOU CORRECTED FOLD BEFORE", `you chose:      "Cleaning, August"`,
		"== THIS HANDLE / DESCRIPTOR IN YOUR LEDGER", `"ravi.kumar42@oksbi" — 1 firefly rows`,
		"== AROUND THIS TIME", `"Ride home"`} {
		if !strings.Contains(p, block) {
			t.Errorf("the prompt lacks %q", block)
		}
	}
	if strings.Index(p, "== HOW YOU CORRECTED FOLD BEFORE") > strings.Index(p, "== DETERMINISTIC HINTS") {
		t.Error("the owner's corrections come after the deterministic hints; they outrank them")
	}
	if d.Tier != TierLLM || d.Engine != "claude-opus-5-5" {
		t.Fatalf("decision tier %d engine %q; want Tier 3 by the configured model", d.Tier, d.Engine)
	}
	ev := d.Evidence
	if strings.Join(ev.Signals, "|") != "your correction of 30 Aug|handle paid before" || strings.Join(ev.Unknowns, "|") != "month" {
		t.Errorf("evidence signals %v unknowns %v", ev.Signals, ev.Unknowns)
	}
	if ev.Learned == nil || ev.Learned.Corrections != 1 || ev.Learned.LedgerRows != 1 || ev.Learned.Neighbours != 1 {
		t.Errorf("learned = %+v; want 1 correction, 1 ledger row, 1 neighbour", ev.Learned)
	}
}

// Nothing learned, nothing rendered: a first transaction's prompt has no
// empty headings.
func TestAnEmptyLearningContextRendersNothing(t *testing.T) {
	var b strings.Builder
	learningContext{}.render(&b)
	if b.Len() != 0 {
		t.Errorf("empty context rendered %q", b.String())
	}
}

func TestExtractJSONObject(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                              `{"a":1}`,
		"```json\n{\"a\":1}\n```":              `{"a":1}`,
		"Here is the JSON:\n{\"a\":{\"b\":2}}": `{"a":{"b":2}}`,
		"{\"a\":1}\nHope that helps!":          `{"a":1}`,
		"not json at all":                      "not json at all",
	} {
		if got := extractJSONObject(in); got != want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", in, got, want)
		}
	}
}

// Corrections for this merchant are facts about this transaction; recent
// corrections elsewhere are style. They sit in separate blocks, so neither
// the model nor a person reading the prompt can take one for the other.
func TestCorrectionsElsewhereAreOnlyStyle(t *testing.T) {
	lc := learningContext{corrections: []learnedExample{
		{why: "same merchant", narration: "UPI/teatrail@okaxis", suggested: feedback.ReviewValues{Title: "Coffee"}, chosen: feedback.ReviewValues{Title: "Filter coffee and bun"}},
		{why: "recent", narration: "UPI/citygas@okbank", suggested: feedback.ReviewValues{Title: "Gas"}, chosen: feedback.ReviewValues{Title: "Gas cylinder refill"}},
	}}
	var b strings.Builder
	lc.render(&b)
	out := b.String()
	here := out[strings.Index(out, "== HOW YOU CORRECTED FOLD BEFORE"):strings.Index(out, "== YOUR RECENT CORRECTIONS ELSEWHERE")]
	elsewhere := out[strings.Index(out, "== YOUR RECENT CORRECTIONS ELSEWHERE"):]
	if !strings.Contains(here, "Filter coffee and bun") || strings.Contains(here, "Gas cylinder refill") {
		t.Errorf("this merchant's block:\n%s", here)
	}
	if !strings.Contains(elsewhere, "Gas cylinder refill") || !strings.Contains(elsewhere, "never take a payee") {
		t.Errorf("the elsewhere block:\n%s", elsewhere)
	}
}
