package classifier

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

func TestTitleFit(t *testing.T) {
	for _, tc := range []struct{ got, truth, want string }{
		{"Filter coffee and bun", "Filter coffee and bun", "exact"},
		{"filter coffee and bun.", "Filter Coffee and Bun", "exact"},
		{"___ from Lantern Books", "Two novels from Lantern Books", "fits"},
		{"Ride from ___ to ___", "Ride from Home to Office", "fits"},
		{"Ride from ___ to ___", "Ride home", "overlap"}, // a ride, but not the one it names
		{"Ride from ___ to ___", "Groceries for the week", "miss"},
		{"Coffee and bun at Tea Trail", "Filter coffee and bun", "overlap"},
		{"Groceries", "Rent for September", "miss"},
		{"", "Something", "miss"},
		{"", "", "exact"},
	} {
		if got := titleFit(tc.got, tc.truth); got != tc.want {
			t.Errorf("titleFit(%q, %q) = %q, want %q", tc.got, tc.truth, got, tc.want)
		}
	}
}

// evalWorld: one sent transaction whose firefly row is the answer, and a
// merchant lookup built from that row alone.
func evalWorld(t *testing.T) *sql.DB {
	t.Helper()
	db := learnDB(t)
	at := time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)
	stagedAt(t, db, "sent", "OUTGOING", "UPI/oliveroom@okaxis/Dinner", "olive room", 420000, at, "pushed")
	propose(t, db, "sent", 41, 6, "Dinner at ___", "")
	mustExec(t, db, `UPDATE staged_fold_txns SET confirmed_description = 'Anniversary dinner at Olive Room', pushed_at = '2026-09-11 09:00:00', firefly_txn_id = 7777 WHERE fold_uuid = 'sent'`)
	mustExec(t, db, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES (44, 'Olive Room, Indiranagar', 'expense', 1, '{}')`)
	fireflyRow(t, db, 7001, at, 44, "Olive Room, Indiranagar", "Eating out", "Anniversary dinner at Olive Room", "UPI/oliveroom@okaxis/Dinner", `["celebration"]`, 420000)
	mustExec(t, db, `UPDATE firefly_txns SET external_id = 'sent', group_id = 7777 WHERE firefly_id = 7001`)
	// what the push taught merchant_lookup: one sample — this very row
	mustExec(t, db, `INSERT INTO merchant_lookup (merchant_normalized, modal_destination_account_id, modal_destination_account_name,
		modal_category_id, modal_category_name, modal_description, sample_size, confidence, last_seen)
		VALUES ('olive room', 44, 'Olive Room, Indiranagar', 6, 'Eating out', 'Anniversary dinner at Olive Room', 1, 1.0, '2026-09-11')`)
	// and the owner's send, as feedback
	recordAt(t, db, "sent", "send", "olive room", "UPI/oliveroom@okaxis/Dinner", feedback.ReviewValues{Title: "Dinner at ___"},
		feedback.ReviewValues{Title: "Anniversary dinner at Olive Room"}, "", "2026-09-11 09:00:00")
	return db
}

func waitEval(t *testing.T, c *Classifier) EvalReport {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if r := c.EvalStatus(); !r.Running {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the evaluation never finished")
	return EvalReport{}
}

// The evaluation grades the engine on what the owner already sent — so it
// must never show the engine that answer: not the firefly row, not the
// send, not a lookup learned from nothing but that row.
func TestTheEvaluationNeverShowsTheAnswer(t *testing.T) {
	db := evalWorld(t)
	fake := newCapturingLLM(t, `{"reasoning":"a dinner","txn_type":"withdrawal","destination_account_id":44,"category_id":6,
		"tags":["celebration"],"description_suggestion":"___ dinner at Olive Room","unknowns":["occasion"],"confidence":0.8}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "claude-opus-5-5", fake.URL, fake.Client()))

	if _, err := c.StartEval(EvalOptions{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	rep := waitEval(t, c)
	if rep.Done != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v; want one graded row", rep)
	}
	p := fake.last()
	if strings.Contains(p, "Anniversary") {
		i := strings.Index(p, "Anniversary")
		t.Fatalf("the prompt showed the engine its answer: …%s…", p[max(0, i-200):min(len(p), i+80)])
	}
	if !strings.Contains(p, "Tier 1: no merchant_lookup match.") {
		t.Error("a merchant lookup learned from the graded row alone was shown as a hint")
	}
	if !strings.Contains(p, "Tier 2: no FTS5 modal vote.") {
		t.Error("the graded row's own firefly journal voted in Tier 2")
	}
	if strings.Contains(p, "firefly_id=7001") {
		t.Error("the graded row's own firefly journal was a historical example")
	}
	row := rep.Rows[0]
	for field, want := range map[string]bool{"type": true, "payee": true, "category": true, "tags": true, "title": false, "titleFits": true} {
		if row.Right[field] != want {
			t.Errorf("%s graded %v, want %v (got %+v, truth %+v)", field, row.Right[field], want, row.Got, row.Truth)
		}
	}
	if row.TitleFit != "fits" || strings.Join(row.Unknowns, ",") != "occasion" {
		t.Errorf("title fit %q unknowns %v", row.TitleFit, row.Unknowns)
	}
	// the old suggestion, same row, is the baseline: it had the wrong payee
	if rep.Baseline["payee"].Right != 0 || rep.Baseline["payee"].Total != 1 || rep.Engine["payee"].Rate != 1 {
		t.Errorf("payee: engine %+v, baseline %+v", rep.Engine["payee"], rep.Baseline["payee"])
	}
	if rep.Model != "claude-opus-5-5" || rep.Reasoning != "none" {
		t.Errorf("report engine = %q/%q", rep.Model, rep.Reasoning)
	}
	// nothing was written
	var title, status string
	_ = db.QueryRow(`SELECT proposed_description, status FROM staged_fold_txns WHERE fold_uuid = 'sent'`).Scan(&title, &status)
	if title != "Dinner at ___" || status != "pushed" {
		t.Errorf("the evaluation changed the row it graded: %q %q", title, status)
	}
}

// Model and effort can be compared without touching the engine in use.
func TestAnEvaluationCanTryAnotherModel(t *testing.T) {
	db := evalWorld(t)
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, body.Model+"/"+body.ReasoningEffort)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"txn_type\":\"withdrawal\",\"destination_account_id\":44,\"confidence\":0.9,\"reasoning\":\"r\"}"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	base := llm.NewClient("k", "claude-opus-5-5", srv.URL, srv.Client())
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(base)
	if _, err := c.StartEval(EvalOptions{Model: "claude-sonnet-5", Reasoning: "low"}); err != nil {
		t.Fatal(err)
	}
	rep := waitEval(t, c)
	if rep.Model != "claude-sonnet-5" || rep.Reasoning != "low" {
		t.Errorf("report says %s/%s; want the overrides", rep.Model, rep.Reasoning)
	}
	if strings.Join(asked, ",") != "claude-sonnet-5/low" {
		t.Errorf("the gateway was asked for %v", asked)
	}
	if _, ok, _ := base.RecentCalls(); ok != 0 {
		t.Errorf("the engine in use counted %d evaluation calls", ok)
	}
}

func TestOneEvaluationAtATime(t *testing.T) {
	db := evalWorld(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		http.Error(w, "x", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", srv.URL, srv.Client()))
	if _, err := c.StartEval(EvalOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StartEval(EvalOptions{}); !errors.Is(err, ErrEvalRunning) {
		t.Errorf("a second evaluation while one runs: %v, want ErrEvalRunning", err)
	}
	if !c.EvalStatus().Running {
		t.Error("the running evaluation doesn't say it is running")
	}
}

func TestAnEvaluationNeedsSomethingToGrade(t *testing.T) {
	db := learnDB(t)
	fake := newCapturingLLM(t, `{}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", fake.URL, fake.Client()))
	if _, err := c.StartEval(EvalOptions{}); err == nil || !strings.Contains(err.Error(), "no sent transactions") {
		t.Errorf("an evaluation with nothing sent: %v", err)
	}
	if _, err := c.StartEval(EvalOptions{}); errors.Is(err, ErrEvalRunning) {
		t.Error("a failed start left the evaluation marked as running")
	}
}

// With a merchant seen before, the engine gets its style samples — every
// past title for the payee but the graded row's own.
func TestTheEvaluationHidesTheAnswerFromStyleSamples(t *testing.T) {
	db := evalWorld(t)
	fireflyRow(t, db, 6001, time.Date(2026, 5, 2, 8, 0, 0, 0, time.UTC), 44, "Olive Room, Indiranagar", "Eating out", "Lunch at Olive Room", "UPI/oliveroom@okaxis/Lunch", "", 180000)
	mustExec(t, db, `UPDATE merchant_lookup SET sample_size = 2, modal_description = 'Lunch at Olive Room' WHERE merchant_normalized = 'olive room'`)
	fake := newCapturingLLM(t, `{"reasoning":"r","txn_type":"withdrawal","destination_account_id":44,"confidence":0.8}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", fake.URL, fake.Client()))
	if _, err := c.StartEval(EvalOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEval(t, c)
	p := fake.last()
	if !strings.Contains(p, "== STYLE SAMPLES") || !strings.Contains(p, `"Lunch at Olive Room"`) {
		t.Fatal("the payee's other titles weren't shown; the test proves nothing without them")
	}
	if strings.Contains(p, "Anniversary") {
		t.Error("the graded row's own title was a style sample")
	}
}
