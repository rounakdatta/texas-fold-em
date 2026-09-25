package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// reviewHarness is a UI server on a fresh DB with a fake firefly that
// records every transaction it is asked to create — so a test can say
// "this reached firefly" or, as often matters more, "nothing did".
type reviewHarness struct {
	srv *httptest.Server
	db  *integration.DB

	mu      sync.Mutex
	creates []map[string]any
	failing bool // firefly answers creates with a 500
}

func newReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	idb, err := integration.Open(context.Background(), filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })
	rh := &reviewHarness{db: idb}

	// firefly's own account list and a little history.
	mustExec(t, idb, `
		INSERT INTO firefly_accounts (firefly_id, name, type, account_role, active, raw_payload) VALUES
		  (476, 'Tata Neu HDFC Bank Credit Card', 'asset', 'ccAsset', 1, '{}'),
		  (12,  'HDFC Bank', 'asset', 'savingAsset', 1, '{}'),
		  (979, 'Chai Corner, Market Road', 'expense', NULL, 1, '{}'),
		  (421, 'Daily Mart, Market Road', 'expense', NULL, 1, '{}')`)
	mustExec(t, idb, `
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name, destination_account_id, destination_account_name,
		    destination_account_name_normalized, category_id, category_name, description, tags_json, notes)
		VALUES
		  (7001, 7001, 'withdrawal', 20000, 'INR', '2025-12-24T21:09:00Z', 476, 'Tata Neu HDFC Bank Credit Card',
		   979, 'Chai Corner, Market Road', 'chai corner, market road', 3, 'Food',
		   'Masala chai and bun for dinner', '[]', 'CARD/0a0a0a0a0a0a0a01/Ramesh S/Rs./200.00/OUTGOING/24-12-25'),
		  (7002, 7002, 'withdrawal', 20000, 'INR', '2025-12-26T13:55:00Z', 476, 'Tata Neu HDFC Bank Credit Card',
		   979, 'Chai Corner, Market Road', 'chai corner, market road', 3, 'Food',
		   'Masala chai and bun for lunch', '[]', 'CARD/0a0a0a0a0a0a0a02/Ramesh S/Rs./200.00/OUTGOING/26-12-25'),
		  (7003, 7003, 'withdrawal', 20000, 'INR', '2025-12-27T13:55:00Z', 476, 'Tata Neu HDFC Bank Credit Card',
		   979, 'Chai Corner, Market Road', 'chai corner, market road', 3, 'Food',
		   'CARD/0a0a0a0a0a0a0a02/Ramesh S/Rs./200.00/OUTGOING/27-12-25', '[]', ''),
		  (7004, 7004, 'withdrawal', 51900, 'INR', '2025-12-26T19:02:00Z', 476, 'Tata Neu HDFC Bank Credit Card',
		   421, 'Daily Mart, Market Road', 'daily mart, market road', 9, 'Grocery',
		   'Fruits and curd purchase', '[]', '')`)

	ff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/search/transactions"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/transactions":
			rh.mu.Lock()
			failing := rh.failing
			if !failing {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				rh.creates = append(rh.creates, body)
			}
			rh.mu.Unlock()
			if failing {
				http.Error(w, `{"message":"firefly is down"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"8800","attributes":{"transactions":[{"transaction_journal_id":"8801"}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ff.Close)
	pusher := integration.NewPusher(idb, firefly.NewClient(ff.URL, "p", ff.Client()), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	h, err := New(idb.DB, pusher, slog.New(slog.NewTextHandler(io.Discard, nil)), "admin-key", AuthModeBypass)
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	h.SetFireflyPublicURL("https://firefly.example")
	mux := http.NewServeMux()
	h.Mount(mux)
	rh.srv = httptest.NewServer(mux)
	t.Cleanup(rh.srv.Close)
	return rh
}

func mustExec(t *testing.T, db *integration.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.DB.Exec(q, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, q)
	}
}

// stage adds one row waiting for review: a spend on the Tata Neu card.
func (rh *reviewHarness) stage(t *testing.T, uuid, status, title string, amountPaise int64, when string, extra ...string) {
	t.Helper()
	mustExec(t, rh.db, `
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration,
		    merchant_extracted, status, classifier_tier, classifier_confidence,
		    proposed_source_account_id, proposed_destination_account_id, proposed_category_id, proposed_description)
		VALUES (?, '{}', ?, 'INR', ?, 'CARD', 'OUTGOING', 'CARD/0a0a0a0a0a0a0a03/Ramesh S/Rs./200.00/OUTGOING/29-12-25',
		        'ramesh s', ?, 3, 0.7, 476, 979, 3, ?)`, uuid, amountPaise, when, status, title)
	for _, q := range extra {
		mustExec(t, rh.db, q, uuid)
	}
}

func (rh *reviewHarness) deck(t *testing.T, query string) deckResponse {
	t.Helper()
	resp, err := http.Get(rh.srv.URL + "/admin/ui/api/deck?" + query)
	if err != nil {
		t.Fatalf("deck: %v", err)
	}
	defer resp.Body.Close()
	var d deckResponse
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode deck: %v", err)
	}
	return d
}

func (rh *reviewHarness) post(t *testing.T, path, body string) (int, actionResponse) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, rh.srv.URL+"/admin/ui/api/"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fold-UI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	var a actionResponse
	_ = json.NewDecoder(resp.Body).Decode(&a)
	return resp.StatusCode, a
}

func (rh *reviewHarness) status(t *testing.T, uuid string) string {
	t.Helper()
	var s string
	if err := rh.db.DB.QueryRow(`SELECT status FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&s); err != nil {
		t.Fatalf("status: %v", err)
	}
	return s
}

func (rh *reviewHarness) createCount() int {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	return len(rh.creates)
}

func uuids(cards []reviewCard) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.UUID
	}
	return out
}

func TestReview_DeckHoldsOnlyWhatIsWaitingForAHuman(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "a-review", "needs_review", "Masala chai and bun for dinner", 20000, "2025-12-29T07:51:00Z")
	rh.stage(t, "b-ready", "ready_to_push", "Masala chai and bun for lunch", 20000, "2025-12-29T08:51:00Z")
	rh.stage(t, "c-pending", "pending", "not classified yet", 20000, "2025-12-29T09:51:00Z")
	rh.stage(t, "d-pushed", "pushed", "already in firefly", 20000, "2025-12-29T10:51:00Z")
	rh.stage(t, "e-skipped", "skipped", "decided against", 20000, "2025-12-29T11:51:00Z")

	d := rh.deck(t, "")
	got := strings.Join(uuids(d.Cards), ",")
	if got != "b-ready,a-review" {
		t.Fatalf("deck = %q, want the ready and needs-review rows, newest first", got)
	}
	if d.Counts.Review != 2 || d.Counts.Later != 0 {
		t.Errorf("counts = %+v", d.Counts)
	}
	if old := rh.deck(t, "order=oldest"); strings.Join(uuids(old.Cards), ",") != "a-review,b-ready" {
		t.Errorf("oldest first = %v", uuids(old.Cards))
	}
}

func TestReview_CardReadsTheWayAPersonSaysIt(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "r1", "needs_review", "Masala chai and bun for dinner", 123456789, "2025-12-29T07:51:00Z")
	c := rh.deck(t, "").Cards[0]
	if c.Amount != "₹12,34,567.89" {
		t.Errorf("amount = %q, want Indian grouping", c.Amount)
	}
	if c.Clock != "1:21 pm" {
		t.Errorf("clock = %q, want IST as the statements show it", c.Clock)
	}
	if !strings.HasPrefix(c.Day, "Mon 29 Dec") {
		t.Errorf("day = %q", c.Day)
	}
	if c.From.Name != "Tata Neu card" || !c.From.Mine || c.From.Full != "Tata Neu HDFC Bank Credit Card" {
		t.Errorf("from = %+v, want the card by the name a person uses", c.From)
	}
	if c.To.Name != "Chai Corner" || c.To.Place != "Market Road" {
		t.Errorf("to = %+v, want the payee and its place apart", c.To)
	}
	if c.Direction != "out" || c.Category != "Food" || c.BankSaid != "Ramesh S" {
		t.Errorf("card = %+v", c)
	}
	// Nothing about it is exceptional, so nothing is flagged.
	if len(c.Blockers) != 0 || c.Duplicate || c.Refund != nil || c.Hold != "" {
		t.Errorf("an ordinary card carries flags: %+v", c)
	}
}

func TestReview_ABlankInTheTitleKeepsItOutOfFirefly(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "blank", "ready_to_push", "___ for lunch from Zomato", 58404, "2025-12-29T07:51:00Z")
	c := rh.deck(t, "").Cards[0]
	if strings.Join(c.Blockers, ",") != blockTitleBlank {
		t.Fatalf("blockers = %v, want the blank", c.Blockers)
	}
	code, a := rh.post(t, "rows/blank/send", `{}`)
	if code != http.StatusUnprocessableEntity || a.Message != "Fill in the blank first" {
		t.Fatalf("send = %d %q, want 422 and the step to take", code, a.Message)
	}
	if rh.createCount() != 0 || rh.status(t, "blank") != "ready_to_push" {
		t.Fatal("a title with a blank reached firefly")
	}

	// Filling it in makes it sendable.
	code, a = rh.post(t, "rows/blank/edit", `{"title":"Chicken biryani for lunch from Zomato"}`)
	if code != 200 || a.Card == nil || len(a.Card.Blockers) != 0 {
		t.Fatalf("edit = %d %+v", code, a.Card)
	}
	code, a = rh.post(t, "rows/blank/send", `{}`)
	if code != 200 || !a.OK || rh.createCount() != 1 || rh.status(t, "blank") != "pushed" {
		t.Fatalf("send after the fix = %d %+v (creates %d)", code, a, rh.createCount())
	}
	if a.FireflyURL != "https://firefly.example/transactions/show/8800" {
		t.Errorf("firefly url = %q", a.FireflyURL)
	}
	if n := len(rh.deck(t, "").Cards); n != 0 {
		t.Errorf("a sent card is still in the deck (%d cards)", n)
	}
}

func TestReview_ASpendWithNobodyPaidIsNotSendable(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "nopayee", "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z",
		`UPDATE staged_fold_txns SET proposed_destination_account_id = NULL WHERE fold_uuid = ?`)
	c := rh.deck(t, "").Cards[0]
	if strings.Join(c.Blockers, ",") != blockPayee {
		t.Fatalf("blockers = %v", c.Blockers)
	}
	// The card still says who the alert named, so it reads as a sentence.
	if c.To.Name != "Ramesh S" {
		t.Errorf("to = %+v, want the alert's merchant as a fallback", c.To)
	}
	if code, _ := rh.post(t, "rows/nopayee/send", `{}`); code != http.StatusUnprocessableEntity || rh.createCount() != 0 {
		t.Fatalf("send = %d, creates %d", code, rh.createCount())
	}
	// A new payee name is fine: firefly creates it on push.
	if code, a := rh.post(t, "rows/nopayee/edit", `{"payee":"Tea stall, Market Road"}`); code != 200 || len(a.Card.Blockers) != 0 {
		t.Fatalf("edit payee = %d %+v", code, a.Card)
	}
}

func TestReview_AHeldRowCannotBeSentUntilReleased(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "held", "needs_review", "Maybe Happy Tickets", 480000, "2026-07-22T07:51:00Z")
	if code, a := rh.post(t, "rows/held/hold", `{"reason":"never billed by AU — skip it"}`); code != 200 || a.Card.Hold == "" {
		t.Fatalf("hold = %d %+v", code, a.Card)
	}
	code, a := rh.post(t, "rows/held/send", `{}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(a.Message, "never billed by AU") || rh.createCount() != 0 {
		t.Fatalf("send on hold = %d %q creates=%d", code, a.Message, rh.createCount())
	}
	// Released, it goes like any other.
	rh.post(t, "rows/held/hold", `{"reason":""}`)
	if code, _ := rh.post(t, "rows/held/send", `{}`); code != 200 || rh.createCount() != 1 {
		t.Fatalf("send after release = %d creates=%d", code, rh.createCount())
	}
}

func TestReview_LaterMovesACardBetweenPiles(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "l1", "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z")
	rh.stage(t, "l2", "needs_review", "Coffee", 4500, "2025-12-29T08:51:00Z")
	if code, _ := rh.post(t, "rows/l1/later", `{"later":true}`); code != 200 {
		t.Fatalf("later = %d", code)
	}
	review, later := rh.deck(t, ""), rh.deck(t, "pile=later")
	if strings.Join(uuids(review.Cards), ",") != "l2" || strings.Join(uuids(later.Cards), ",") != "l1" {
		t.Fatalf("review=%v later=%v", uuids(review.Cards), uuids(later.Cards))
	}
	if review.Counts.Review != 1 || review.Counts.Later != 1 {
		t.Errorf("counts = %+v", review.Counts)
	}
	rh.post(t, "rows/l1/later", `{"later":false}`)
	if got := uuids(rh.deck(t, "").Cards); len(got) != 2 {
		t.Errorf("after moving back: %v", got)
	}
	// Sending from Later clears the flag with the send.
	rh.post(t, "rows/l1/later", `{"later":true}`)
	rh.post(t, "rows/l1/send", `{}`)
	var later1 bool
	_ = rh.db.DB.QueryRow(`SELECT later_at IS NOT NULL FROM staged_fold_txns WHERE fold_uuid='l1'`).Scan(&later1)
	if later1 || rh.status(t, "l1") != "pushed" {
		t.Errorf("sent from later: later=%v status=%s", later1, rh.status(t, "l1"))
	}
}

// An edit to one field goes through the same save as the full editor, which
// writes every field — so it must carry the others exactly as they were,
// including a statement-corrected amount and date.
func TestReview_AnEditChangesOnlyTheFieldEdited(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "e1", "ready_to_push", "Masala chai and bun for dinner", 20000, "2025-12-29T07:51:00Z",
		`UPDATE staged_fold_txns SET confirmed_destination_account_id = 979, confirmed_source_account_id = 476,
		   confirmed_category_id = 3, confirmed_description = 'Masala chai and bun for dinner',
		   confirmed_amount_paise = 21000, confirmed_txn_timestamp = '2025-12-29T08:00:00Z', confirmed_tags_json = '["friends"]'
		 WHERE fold_uuid = ?`)
	if code, _ := rh.post(t, "rows/e1/edit", `{"title":"3 masala chai for dinner"}`); code != 200 {
		t.Fatalf("edit = %d", code)
	}
	var (
		desc, tags   string
		dst, src, ct int64
		amt          int64
		ts           time.Time
	)
	if err := rh.db.DB.QueryRow(`SELECT confirmed_description, confirmed_destination_account_id, confirmed_source_account_id,
		    confirmed_category_id, confirmed_amount_paise, confirmed_txn_timestamp, confirmed_tags_json
		  FROM staged_fold_txns WHERE fold_uuid='e1'`).Scan(&desc, &dst, &src, &ct, &amt, &ts, &tags); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if desc != "3 masala chai for dinner" {
		t.Errorf("title = %q", desc)
	}
	if dst != 979 || src != 476 || ct != 3 || amt != 21000 || !ts.Equal(time.Date(2025, 12, 29, 8, 0, 0, 0, time.UTC)) || tags != `["friends"]` {
		t.Errorf("an edit to the title disturbed other fields: dst=%d src=%d cat=%d amt=%d ts=%v tags=%s", dst, src, ct, amt, ts, tags)
	}
}

func TestReview_AnEmptyCategoryIsNotConfirmedByAnUnrelatedEdit(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "nc", "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z",
		`UPDATE staged_fold_txns SET proposed_category_id = NULL WHERE fold_uuid = ?`)
	rh.post(t, "rows/nc/edit", `{"title":"Morning tea"}`)
	var cat *int64
	_ = rh.db.DB.QueryRow(`SELECT confirmed_category_id FROM staged_fold_txns WHERE fold_uuid='nc'`).Scan(&cat)
	if cat != nil {
		t.Errorf("confirmed_category_id = %v; a title edit turned 'no suggestion yet' into an explicit 'none'", *cat)
	}
}

func TestReview_SkipLeavesTheDeckAndRestoreBringsItBack(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "s1", "needs_review", "A duplicate alert", 3500, "2025-12-29T07:51:00Z")
	if code, _ := rh.post(t, "rows/s1/skip", `{}`); code != 200 || rh.status(t, "s1") != "skipped" {
		t.Fatalf("skip = %d %s", code, rh.status(t, "s1"))
	}
	if n := len(rh.deck(t, "").Cards); n != 0 {
		t.Fatalf("skipped card still in the deck")
	}
	if code, _ := rh.post(t, "rows/s1/send", `{}`); code != http.StatusConflict || rh.createCount() != 0 {
		t.Fatalf("a skipped row could be sent: %d", code)
	}
	if code, _ := rh.post(t, "rows/s1/restore", `{}`); code != 200 || rh.status(t, "s1") != "needs_review" {
		t.Fatalf("restore = %d %s", code, rh.status(t, "s1"))
	}
}

func TestReview_AFailedSendSaysWhyAndKeepsTheCard(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "f1", "ready_to_push", "Tea", 3500, "2025-12-29T07:51:00Z")
	rh.mu.Lock()
	rh.failing = true
	rh.mu.Unlock()
	code, a := rh.post(t, "rows/f1/send", `{}`)
	// the card says what firefly said, in its words, not the HTTP envelope
	if code != http.StatusBadGateway || !strings.Contains(a.Message, "firefly is down") || strings.Contains(a.Message, "HTTP") || a.Card == nil {
		t.Fatalf("send against a failing firefly = %d %+v", code, a)
	}
	if rh.status(t, "f1") != "ready_to_push" {
		t.Errorf("status = %s after a failed send", rh.status(t, "f1"))
	}
}

// The custom header is what stops another site from driving these writes
// with the user's cookies (a cross-site request can't set it without a
// preflight this server never answers).
func TestReview_WritesRequireTheUIHeader(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "x1", "ready_to_push", "Tea", 3500, "2025-12-29T07:51:00Z")
	resp, err := http.Post(rh.srv.URL+"/admin/ui/api/rows/x1/send", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || rh.createCount() != 0 {
		t.Fatalf("a write without the header = %d (creates %d)", resp.StatusCode, rh.createCount())
	}
}

func TestReview_TheCursorPagesThroughEveryCardOnce(t *testing.T) {
	rh := newReviewHarness(t)
	// Two share a timestamp: the uuid breaks the tie, so neither is skipped.
	for _, u := range []string{"p1", "p2", "p3", "p4"} {
		rh.stage(t, u, "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z")
	}
	rh.stage(t, "p5", "needs_review", "Tea", 3500, "2025-12-30T07:51:00Z")
	var seen []string
	after := ""
	for i := 0; i < 10; i++ {
		d := rh.deck(t, "limit=2&after="+after)
		seen = append(seen, uuids(d.Cards)...)
		if d.Next == "" {
			break
		}
		after = d.Next
	}
	if strings.Join(seen, ",") != "p5,p4,p3,p2,p1" {
		t.Fatalf("paged = %v", seen)
	}
}

func TestReview_SuggestsTheUsersOwnPastTitlesAndPayees(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "sg", "needs_review", "___ for lunch", 20000, "2025-12-29T07:51:00Z")
	resp, err := http.Get(rh.srv.URL + "/admin/ui/api/rows/sg/suggest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s suggestResponse
	_ = json.NewDecoder(resp.Body).Decode(&s)
	var titles []string
	for _, x := range s.Titles {
		titles = append(titles, x.Value)
	}
	// Past titles at this payee, newest first — but never a pasted bank
	// line that once stood in for a title.
	if strings.Join(titles, " | ") != "Masala chai and bun for lunch | Masala chai and bun for dinner" {
		t.Errorf("titles = %v", titles)
	}
	if len(s.Categories) == 0 || s.Categories[0].Value != "Food" {
		t.Errorf("categories = %+v", s.Categories)
	}
	// The bank line says "Ramesh S"; last time that was Chai Corner.
	if len(s.Payees) != 1 || s.Payees[0].Value != "Chai Corner, Market Road" {
		t.Errorf("payees = %+v", s.Payees)
	}
}

func TestReview_OnlyAFlaggedDuplicateSaysDuplicate(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "d1", "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z")
	rh.stage(t, "d2", "needs_review", "Tea", 3500, "2025-12-29T07:52:00Z",
		`UPDATE staged_fold_txns SET raw_payload = '{"is_possible_duplicate": true}' WHERE fold_uuid = ?`)
	for _, c := range rh.deck(t, "").Cards {
		if c.Duplicate != (c.UUID == "d2") {
			t.Errorf("%s duplicate=%v", c.UUID, c.Duplicate)
		}
	}
}

func TestReview_TheDeckPageRendersItsShell(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "pg", "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z")
	resp, err := http.Get(rh.srv.URL + "/admin/ui/review")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	// (the nav item carries an icon before its word)
	for _, want := range []string{`id="deck-app"`, `/admin/ui/static/review.js?v=`, `aria-current="page"><svg`, `</svg>Review <span class="count num">1</span>`,
		`&#34;short&#34;:&#34;Tata Neu card&#34;`} {
		if !strings.Contains(body, want) {
			t.Errorf("review page lacks %q", want)
		}
	}
	// and its assets are served, cacheable forever at the hashed URL
	resp2, err := http.Get(rh.srv.URL + assetURL("review.js"))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.Contains(resp2.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("review.js = %d %q", resp2.StatusCode, resp2.Header.Get("Cache-Control"))
	}
}

func TestFormatINR(t *testing.T) {
	for p, want := range map[int64]string{0: "₹0", 100: "₹1", 20000: "₹200", 141287: "₹1,412.87", 12345678: "₹1,23,456.78", 1000000000: "₹1,00,00,000"} {
		if got := formatINR(p); got != want {
			t.Errorf("formatINR(%d) = %q, want %q", p, got, want)
		}
	}
	if got := formatForeign(567500, "USD"); got != "USD 5,675" {
		t.Errorf("foreign = %q", got)
	}
}

func TestShortAccountName(t *testing.T) {
	for in, want := range map[string]string{
		"Tata Neu HDFC Bank Credit Card":  "Tata Neu card",
		"Scapia Federal Bank Credit Card": "Scapia card",
		"Ixigo AU Bank Credit Card":       "Ixigo AU card",
		"Axis Bank Ace Credit Card":       "Axis Ace card",
		"HDFC Bank":                       "HDFC Bank",
		"HDFC Credit Card":                "HDFC card",
	} {
		if got := shortAccountName(in); got != want {
			t.Errorf("shortAccountName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpokenDay(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, ist)
	for when, want := range map[time.Time]string{
		time.Date(2026, 9, 25, 1, 0, 0, 0, ist):       "Today",
		time.Date(2026, 9, 24, 23, 59, 0, 0, ist):     "Yesterday",
		time.Date(2026, 9, 21, 9, 0, 0, 0, ist):       "Mon 21 Sept",
		time.Date(2025, 12, 30, 14, 9, 0, 0, ist):     "Tue 30 Dec 2025",
		time.Date(2026, 9, 24, 19, 0, 0, 0, time.UTC): "Today", // 00:30 IST on the 25th
	} {
		if got := spokenDay(when, now); got != want {
			t.Errorf("spokenDay(%v) = %q, want %q", when, got, want)
		}
	}
}

func TestBankSaid(t *testing.T) {
	cases := map[[2]string]string{
		{"CARD/0a0a0a0a0a0a0a03/Ramesh S/Rs./200.00/OUTGOING/29-12-25", "CARD"}:                                                "Ramesh S",
		{"CARD/0a0a0a0a0a0a0a4/SUNRISE FOODS LLP/Rs./380/OUTGOING", "CARD"}:                                                    "Sunrise Foods LLP",
		{"NEFT CR-ABCD0123456-EXAMPLE PAYER LTD-A PERSON-ABCDN00000000001", "NEFT"}:                                            "Example Payer Ltd",
		{"ACH C- EXAMPLE DIVIDEND CO-000000000123", "ACH"}:                                                                     "Example Dividend Co",
		{"IMPS-000000000001-EXAMPLE SENDER-BANK-XXXXXXXX0001-TEST", "IMPS"}:                                                    "Example Sender",
		{"PRIN AND INT AUTO_REDEEM 000000000001", "OTHER"}:                                                                     "PRIN AND INT AUTO_REDEEM 000000000001",
		{"UPI-EXAMPLE STORE-PAYMENTS@OKBANK-BANK0000001-0001-UPI", "UPI"}:                                                      "Example Store",
		{"Card statement 23Nov25-22Dec25: 10/12/2025 17:49 UPI-A VENDOR 45.00 · 45.00 was a cold coffee on 8 Dec", manualMode}: "Statement: 10/12/2025 17:49 UPI-A VENDOR 45.00",
	}
	for in, want := range cases {
		if got := bankSaid(in[0], in[1]); got != want {
			t.Errorf("bankSaid(%q) = %q, want %q", in[0], got, want)
		}
	}
	if got := manualHint("Tata Neu statement X: line · 45.00 was a cold coffee on 8 Dec"); got != "45.00 was a cold coffee on 8 Dec" {
		t.Errorf("manualHint = %q", got)
	}
	if got := humanNote(`{"note": "Sweets for Salkia", "account": "Bhim Nag Bowbazar"}`); got != "Sweets for Salkia" {
		t.Errorf("humanNote(json) = %q", got)
	}
}

// A refusal reads as what Firefly objected to — the field-level message
// when its validator names one — and never as a dump of the response.
func TestFireflyReason(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("firefly create: %w", e) }
	cases := []struct {
		err  error
		want string
	}{
		{wrap(&firefly.Error{Status: 422, Path: "/api/v1/transactions",
			Body: `{"message":"The given data was invalid.","errors":{"transactions.0.destination_name":["This destination account is not valid."]}}`}),
			"Firefly said: This destination account is not valid."},
		{wrap(&firefly.Error{Status: 422, Body: `{"message":"The given data was invalid."}`}), "Firefly said: The given data was invalid."},
		{wrap(&firefly.Error{Status: 401, Body: `{"message":"Unauthenticated."}`}), "Firefly turned fold's access token away — it may have expired."},
		{wrap(&firefly.Error{Status: 502, Body: `<html>bad gateway</html>`}), "Firefly had a problem of its own (HTTP 502). Try again in a minute."},
		{wrap(&url.Error{Op: "Post", URL: "http://firefly", Err: errors.New("connection refused")}), "Couldn't reach Firefly. Try again in a minute."},
		{wrap(context.DeadlineExceeded), "Firefly didn't answer in time. Try again in a minute."},
	}
	for _, c := range cases {
		if got := fireflyReason(c.err); got != c.want {
			t.Errorf("fireflyReason(%v)\n got %q\nwant %q", c.err, got, c.want)
		}
	}
}

// The account picker says how many are waiting on each account, in the pile
// being looked at, whatever account the deck is filtered to.
func TestReview_TheAccountPickerCountsWhatIsWaitingPerAccount(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "a1", "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z")
	rh.stage(t, "a2", "ready_to_push", "Tea", 3500, "2025-12-29T08:51:00Z")
	rh.stage(t, "b1", "needs_review", "Tea", 3500, "2025-12-29T09:51:00Z",
		`UPDATE staged_fold_txns SET proposed_source_account_id = 501 WHERE fold_uuid = ?`)
	rh.stage(t, "l1", "needs_review", "Tea", 3500, "2025-12-29T10:51:00Z",
		`UPDATE staged_fold_txns SET later_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`)
	rh.stage(t, "p1", "pushed", "Tea", 3500, "2025-12-29T11:51:00Z")

	d := rh.deck(t, "account=501")
	if d.Counts.All == nil || *d.Counts.All != 3 {
		t.Fatalf("all = %v, want 3 waiting across every account", d.Counts.All)
	}
	// 476 pays in three of them, 501 in one; the merchant (979) is on the
	// other side of all four
	if d.Counts.ByAccount["476"] != 2 || d.Counts.ByAccount["501"] != 1 || d.Counts.ByAccount["979"] != 3 {
		t.Errorf("by account = %v", d.Counts.ByAccount)
	}
	if d.Counts.Review != 1 {
		t.Errorf("the filtered pile counts %d, want 1 (only 501's)", d.Counts.Review)
	}
	later := rh.deck(t, "pile=later")
	if later.Counts.ByAccount["476"] != 1 || *later.Counts.All != 1 {
		t.Errorf("the Later pile's counts = %v / %v", later.Counts.ByAccount, *later.Counts.All)
	}
	// later pages don't pay for the counts again
	if next := rh.deck(t, "limit=1&after="+url.QueryEscape("2025-12-29T09:51:00Z|b1")); next.Counts.ByAccount != nil {
		t.Errorf("a later page recounted: %v", next.Counts.ByAccount)
	}
}

// Money in needs to know which of the user's accounts it came into — firefly
// can't book a deposit without — and the card's edit fills the right side:
// the account is the destination of money in, the payer its source.
func TestReview_MoneyInNeedsItsAccountAndTakesAPayer(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "in1", "needs_review", "Refund of a shirt", 50000, "2025-12-29T07:51:00Z",
		`UPDATE staged_fold_txns SET type = 'INCOMING', proposed_source_account_id = NULL, proposed_destination_account_id = NULL WHERE fold_uuid = ?`)
	d := rh.deck(t, "")
	if len(d.Cards) != 1 || strings.Join(d.Cards[0].Blockers, ",") != blockPayee+","+blockSource {
		t.Fatalf("money in with no payer and no account: blockers = %v", d.Cards)
	}
	code, a := rh.post(t, "rows/in1/edit", `{"account":"HDFC Bank","payee":"A Shop"}`)
	if code != 200 || a.Card == nil {
		t.Fatalf("edit = %d %+v", code, a)
	}
	if len(a.Card.Blockers) != 0 || a.Card.Direction != "in" || a.Card.From.Name != "A Shop" {
		t.Errorf("after the edit: blockers %v, direction %s, from %+v, to %+v", a.Card.Blockers, a.Card.Direction, a.Card.From, a.Card.To)
	}
	var src, dst string
	_ = rh.db.DB.QueryRow(`SELECT COALESCE(confirmed_source_account_name,''), COALESCE(confirmed_destination_account_name,'') ||
		COALESCE(CAST(confirmed_destination_account_id AS TEXT),'') FROM staged_fold_txns WHERE fold_uuid='in1'`).Scan(&src, &dst)
	if src != "A Shop" || dst == "" {
		t.Errorf("stored source %q, destination %q: the payer is the source, the account the destination", src, dst)
	}
}

// The brand travels with every page: the mark in the app bar, the favicon
// and the home-screen icons, the web manifest (fetched with the cookie,
// since the UI sits behind a login), and the font — each served with a type
// a browser will accept.
func TestReview_EveryPageCarriesTheBrand(t *testing.T) {
	rh := newReviewHarness(t)
	resp, err := http.Get(rh.srv.URL + "/admin/ui/review")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	for _, want := range []string{
		`<title>Review · texas fold ’em</title>`,
		`class="brand-mark" src="/admin/ui/static/brand-mark.svg?v=`,
		`rel="icon" href="/admin/ui/static/favicon.svg?v=`,
		`rel="apple-touch-icon" href="/admin/ui/static/apple-touch-icon.png?v=`,
		`rel="manifest" href="/admin/ui/static/manifest.webmanifest?v=`, `crossorigin="use-credentials"`,
		`font-family: "Outfit"`, `/admin/ui/static/outfit-latin.woff2?v=`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review page lacks %q", want)
		}
	}
	for file, ctype := range map[string]string{
		"favicon.svg": "image/svg+xml", "brand-mark.svg": "image/svg+xml", "cowboy.svg": "image/svg+xml",
		"favicon-32.png": "image/png", "apple-touch-icon.png": "image/png", "icon-maskable-512.png": "image/png",
		"outfit-latin.woff2": "font/woff2", "outfit-latin-ext.woff2": "font/woff2",
		"manifest.webmanifest": "application/manifest+json", "outfit-OFL.txt": "text/plain; charset=utf-8",
	} {
		r, err := http.Get(rh.srv.URL + assetURL(file))
		if err != nil {
			t.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != ctype || n == 0 {
			t.Errorf("%s: %d %q (%d bytes), want %q", file, r.StatusCode, r.Header.Get("Content-Type"), n, ctype)
		}
	}
	// the manifest names the app, starts it on the deck, and every icon it
	// lists is there
	r, _ := http.Get(rh.srv.URL + assetURL("manifest.webmanifest"))
	var m struct {
		Name     string `json:"name"`
		StartURL string `json:"start_url"`
		Display  string `json:"display"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	mb, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if err := json.Unmarshal(mb, &m); err != nil || m.Name != "texas fold ’em" || m.StartURL != "/admin/ui/review" || m.Display != "standalone" || len(m.Icons) < 3 {
		t.Errorf("manifest = %s (%v)", mb, err)
	}
	for _, ic := range m.Icons {
		ir, err := http.Get(rh.srv.URL + ic.Src)
		if err != nil {
			t.Fatal(err)
		}
		ir.Body.Close()
		if ir.StatusCode != 200 || ir.Header.Get("Content-Type") != "image/png" {
			t.Errorf("manifest icon %s: %d %s", ic.Src, ir.StatusCode, ir.Header.Get("Content-Type"))
		}
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"never billed by AU — skip it": "never billed by AU — skip it.", "done.": "done.", "why?": "why?", " ": "", "wait…": "wait…"} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

// The list's call to review counts what the deck will show — the To review
// pile, not what was put off to Later — so it agrees with the Review badge.
func TestReview_TheListInvitesYouToWhatTheDeckHolds(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "w1", "needs_review", "Tea", 3500, "2025-12-29T07:51:00Z")
	rh.stage(t, "w2", "ready_to_push", "Tea", 3500, "2025-12-29T08:51:00Z")
	rh.stage(t, "w3", "needs_review", "Tea", 3500, "2025-12-29T09:51:00Z",
		`UPDATE staged_fold_txns SET later_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`)
	resp, err := http.Get(rh.srv.URL + "/admin/ui/?status=all")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if !strings.Contains(body, `<span class="num">2</span> waiting for you`) || !strings.Contains(body, `Review <span class="count num">2</span>`) {
		t.Errorf("the call to review and the badge should both say 2: %q / %q", snippet(body, "waiting for you"), snippet(body, "count num"))
	}
}
