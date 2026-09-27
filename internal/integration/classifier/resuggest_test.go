package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// waitingCard stages a card the old engine suggested (version 0), classified
// an hour ago, the way the deck holds it.
func waitingCard(t *testing.T, db *sql.DB, uuid, merchant, title string) {
	t.Helper()
	stagedAt(t, db, uuid, "OUTGOING", "UPI/"+strings.ReplaceAll(merchant, " ", "")+"@okaxis/x", merchant, 25000, time.Now().Add(-48*time.Hour), "needs_review")
	propose(t, db, uuid, 41, 6, title, "")
	mustExec(t, db, `UPDATE staged_fold_txns SET classifier_tier = 3, classified_at = datetime('now', '-1 hour') WHERE fold_uuid = ?`, uuid)
}

func proposedTitle(t *testing.T, db *sql.DB, uuid string) string {
	t.Helper()
	var s sql.NullString
	_ = db.QueryRow(`SELECT proposed_description FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&s)
	return s.String
}

func teaReply(title string) string {
	b, _ := json.Marshal(map[string]any{"reasoning": "your correction", "txn_type": "withdrawal", "destination_account_id": 41, "category_id": 6,
		"tags": []string{}, "description_suggestion": title, "evidence": []string{"your correction of today"}, "confidence": 0.9})
	return string(b)
}

// What may be re-suggested, and in what order: a card whose merchant got a
// correction after its suggestion was made comes first; then cards an older
// engine suggested. Nothing a person decided is ever a candidate.
func TestResuggestCandidatesNeverIncludeWhatAPersonDecided(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	// Every card shares the merchant the correction is for, so each of them
	// would be a "learned" candidate — but for what a person did to it.
	for _, u := range []string{"learned", "engine", "confirmed", "held", "manual", "sent", "recent", "current"} {
		waitingCard(t, db, u, "tea trail cafe", "Coffee")
	}
	mustExec(t, db, `UPDATE staged_fold_txns SET confirmed_category_id = 5 WHERE fold_uuid = 'confirmed'`)
	mustExec(t, db, `UPDATE staged_fold_txns SET hold_reason = 'never billed' WHERE fold_uuid = 'held'`)
	mustExec(t, db, `UPDATE staged_fold_txns SET mode = 'MANUAL' WHERE fold_uuid = 'manual'`)
	mustExec(t, db, `UPDATE staged_fold_txns SET status = 'pushed' WHERE fold_uuid = 'sent'`)
	// looked at since the correction arrived: its turn is over until the next
	mustExec(t, db, `UPDATE staged_fold_txns SET resuggested_at = datetime('now', '+1 minute') WHERE fold_uuid = 'recent'`)
	// "engine" was suggested after the correction (nothing to learn), by an
	// older engine; "current" by this one, before it (learned).
	mustExec(t, db, `UPDATE staged_fold_txns SET classified_at = datetime('now', '+1 minute') WHERE fold_uuid = 'engine'`)
	mustExec(t, db, `UPDATE staged_fold_txns SET classifier_version = ?, classified_at = datetime('now', '+1 minute') WHERE fold_uuid = 'current'`, EngineVersion)
	// the correction that makes "learned" worth another look
	recordAt(t, db, "elsewhere", "edit", "tea trail cafe", "n", feedback.ReviewValues{Title: "Coffee"}, feedback.ReviewValues{Title: "Filter coffee"}, "",
		time.Now().UTC().Format("2006-01-02 15:04:05"))

	cands, err := c.resuggestCandidates(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, cd := range cands {
		got = append(got, cd.uuid+":"+cd.reason)
	}
	if strings.Join(got, ",") != "learned:learned,engine:engine" {
		t.Errorf("candidates = %v; want learned first, then the older engine's — never a confirmed, held, manual, sent, recently revisited or current one", got)
	}
}

// A correction made BEFORE the card was suggested is already in its
// suggestion: no reason to look again.
func TestAnOlderCorrectionIsNoReasonToLookAgain(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	waitingCard(t, db, "card", "tea trail cafe", "Coffee")
	mustExec(t, db, `UPDATE staged_fold_txns SET classifier_version = ? WHERE fold_uuid = 'card'`, EngineVersion)
	recordAt(t, db, "elsewhere", "edit", "tea trail cafe", "n", feedback.ReviewValues{Title: "Coffee"}, feedback.ReviewValues{Title: "Filter coffee"}, "",
		time.Now().Add(-3*time.Hour).UTC().Format("2006-01-02 15:04:05"))
	if cands, _ := c.resuggestCandidates(context.Background(), 10); len(cands) != 0 {
		t.Errorf("candidates = %+v; the correction predates the suggestion", cands)
	}
}

// The loop end to end: a correction on one card improves its siblings.
func TestACorrectionImprovesTheWaitingSiblings(t *testing.T) {
	db := learnDB(t)
	for _, u := range []string{"s1", "s2"} {
		waitingCard(t, db, u, "tea trail cafe", "Coffee")
		mustExec(t, db, `UPDATE staged_fold_txns SET classifier_version = ? WHERE fold_uuid = ?`, EngineVersion, u)
	}
	recordAt(t, db, "first", "edit", "tea trail cafe", "UPI/teatrailcafe@okaxis/x", feedback.ReviewValues{Title: "Coffee"},
		feedback.ReviewValues{Title: "Filter coffee and bun"}, "", time.Now().UTC().Format("2006-01-02 15:04:05"))

	fake := newCapturingLLM(t, teaReply("Filter coffee and bun"))
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "claude-opus-5-5", fake.URL, fake.Client()))
	rep, err := c.Resuggest(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Examined != 2 || rep.Changed != 2 || rep.Reasons["learned"] != 2 {
		t.Errorf("report = %+v; want both siblings re-suggested and changed, because of the correction", rep)
	}
	if !strings.Contains(fake.last(), `you chose:      "Filter coffee and bun"`) {
		t.Error("the re-suggestion's prompt didn't carry the correction that prompted it")
	}
	for _, u := range []string{"s1", "s2"} {
		var title, reason, model string
		var changed, version int
		_ = db.QueryRow(`SELECT proposed_description, resuggest_reason, classifier_model, resuggest_changed, classifier_version FROM staged_fold_txns WHERE fold_uuid = ?`, u).
			Scan(&title, &reason, &model, &changed, &version)
		if title != "Filter coffee and bun" || reason != "learned" || changed != 1 || model != "claude-opus-5-5" || version != EngineVersion {
			t.Errorf("%s: title %q reason %q changed %d model %q v%d", u, title, reason, changed, model, version)
		}
	}
	if last := c.LastResuggest(); last == nil || last.Changed != 2 {
		t.Errorf("last report = %+v", last)
	}
	// and the next pass leaves them alone
	if rep, _ := c.Resuggest(context.Background(), 10); rep.Examined != 0 {
		t.Errorf("a second pass re-examined %d cards straight away", rep.Examined)
	}
}

// When the model fails, the card keeps what it says. (The classify path
// resets a failed row to pending — right for a new arrival, wrong here:
// it would blank a card someone may be looking at.)
func TestAFailedResuggestionKeepsTheOldSuggestion(t *testing.T) {
	db := learnDB(t)
	waitingCard(t, db, "card", "tea trail cafe", "Coffee")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", srv.URL, srv.Client()))
	rep, _ := c.Resuggest(context.Background(), 10)
	if rep.Failed != 1 {
		t.Errorf("report = %+v; want one failure", rep)
	}
	var status, title string
	_ = db.QueryRow(`SELECT status, proposed_description FROM staged_fold_txns WHERE fold_uuid = 'card'`).Scan(&status, &title)
	if status != "needs_review" || title != "Coffee" {
		t.Errorf("after a failed re-suggestion: status %q, title %q; want the card as it was", status, title)
	}
	var at sql.NullString
	_ = db.QueryRow(`SELECT resuggested_at FROM staged_fold_txns WHERE fold_uuid = 'card'`).Scan(&at)
	if !at.Valid {
		t.Error("a failed attempt wasn't marked, so the next pass would hammer it again")
	}
}

// A person deciding a card while the model thinks wins.
func TestAPersonsDecisionDuringTheCallWins(t *testing.T) {
	db := learnDB(t)
	waitingCard(t, db, "card", "tea trail cafe", "Coffee")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// the owner saves a title while the model is answering
		mustExec(t, db, `UPDATE staged_fold_txns SET confirmed_description = 'Mine' WHERE fold_uuid = 'card'`)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": teaReply("The model's")}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", srv.URL, srv.Client()))
	_, _ = c.Resuggest(context.Background(), 10)
	if got := proposedTitle(t, db, "card"); got != "Coffee" {
		t.Errorf("the suggestion under a person's decision changed to %q", got)
	}
}

// While the model rests, a pass does nothing and puts nothing back.
func TestAPassWaitsWhileTheModelRests(t *testing.T) {
	db := learnDB(t)
	waitingCard(t, db, "card", "tea trail cafe", "Coffee")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "refused", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	client := llm.NewClient("bad", "m", srv.URL, srv.Client())
	_, _ = client.GenerateJSON(context.Background(), "s", "u") // the refusal rests it
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(client)
	rep, err := c.Resuggest(context.Background(), 10)
	if err != nil || rep.Examined != 0 || hits.Load() != 1 {
		t.Errorf("a pass while resting: %+v, %v, %d requests; want nothing at all", rep, err, hits.Load())
	}
	var at sql.NullString
	_ = db.QueryRow(`SELECT resuggested_at FROM staged_fold_txns WHERE fold_uuid = 'card'`).Scan(&at)
	if at.Valid {
		t.Error("a card was marked as looked at while the model rested; it would wait hours for no reason")
	}
}

// A new model decision is the whole suggestion: when it says no tags, the
// old engine's tags go.
func TestAResuggestionReplacesTheTagsToo(t *testing.T) {
	db := learnDB(t)
	waitingCard(t, db, "card", "tea trail cafe", "Coffee")
	mustExec(t, db, `UPDATE staged_fold_txns SET proposed_tags_json = '["wrong-trip"]' WHERE fold_uuid = 'card'`)
	fake := newCapturingLLM(t, teaReply("Filter coffee"))
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", fake.URL, fake.Client()))
	_, _ = c.Resuggest(context.Background(), 10)
	var tags sql.NullString
	_ = db.QueryRow(`SELECT proposed_tags_json FROM staged_fold_txns WHERE fold_uuid = 'card'`).Scan(&tags)
	if tags.Valid && tags.String != "" && tags.String != "[]" {
		t.Errorf("tags after the re-suggestion = %s; want the old engine's gone", tags.String)
	}
}

// Newest first, within the limit.
func TestResuggestTakesTheNewestFirst(t *testing.T) {
	db := learnDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	for i, u := range []string{"old", "mid", "new"} {
		waitingCard(t, db, u, "lantern books", "Book")
		mustExec(t, db, `UPDATE staged_fold_txns SET txn_timestamp = ? WHERE fold_uuid = ?`, time.Date(2026, 9, 1+i, 6, 0, 0, 0, time.UTC), u)
	}
	cands, _ := c.resuggestCandidates(context.Background(), 2)
	var got []string
	for _, cd := range cands {
		got = append(got, cd.uuid)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "mid,new" {
		t.Errorf("with a limit of 2 took %v; want the two newest", got)
	}
}

// A correction made in the very instant the card was suggested may not be
// in its suggestion: it is worth one more look, and only one.
func TestACorrectionInTheSameInstantStillCounts(t *testing.T) {
	db := learnDB(t)
	waitingCard(t, db, "card", "tea trail cafe", "Coffee")
	mustExec(t, db, `UPDATE staged_fold_txns SET classifier_version = ?, classified_at = '2026-09-27 10:08:09.250' WHERE fold_uuid = 'card'`, EngineVersion)
	recordAt(t, db, "sibling", "edit", "tea trail cafe", "n", feedback.ReviewValues{Title: "Coffee"}, feedback.ReviewValues{Title: "Filter coffee"}, "", "2026-09-27 10:08:09.250")
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	cands, _ := c.resuggestCandidates(context.Background(), 10)
	if len(cands) != 1 || cands[0].reason != "learned" {
		t.Fatalf("candidates = %+v; want the card, learned", cands)
	}
	// a look a moment later is the one look
	mustExec(t, db, `UPDATE staged_fold_txns SET resuggested_at = '2026-09-27 10:08:09.400' WHERE fold_uuid = 'card'`)
	if cands, _ := c.resuggestCandidates(context.Background(), 10); len(cands) != 0 {
		t.Errorf("candidates after the look = %+v", cands)
	}
}
