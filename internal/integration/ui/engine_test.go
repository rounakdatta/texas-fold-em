package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// withEngine wires a classifier whose model always answers reply.
func (rh *reviewHarness) withEngine(t *testing.T, reply string) *classifier.Classifier {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": reply}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(fake.Close)
	cls := classifier.New(rh.db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)), classifier.DefaultConfidenceThreshold, 10)
	cls.SetLLM(llm.NewClient("k", "claude-opus-5-5", fake.URL+"/v1", fake.Client()))
	rh.h.SetClassifier(cls)
	return cls
}

func (rh *reviewHarness) getJSON(t *testing.T, path string, v any) int {
	t.Helper()
	resp, err := http.Get(rh.srv.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(v)
	return resp.StatusCode
}

func TestEngine_StatusSaysWhatMakesTheSuggestions(t *testing.T) {
	rh := newReviewHarness(t)
	var none classifier.EngineStatus
	if code := rh.getJSON(t, "/api/engine", &none); code != http.StatusOK || none.Configured {
		t.Errorf("with no classifier: %d %+v; want 200, not configured", code, none)
	}

	rh.withEngine(t, `{}`)
	rh.stage(t, "old", "needs_review", "Masala chai", 20000, "2025-12-29T07:51:00Z")
	rh.stage(t, "held", "needs_review", "x", 100, "2025-12-29T08:51:00Z", `UPDATE staged_fold_txns SET hold_reason = 'check', hold_by = 'fold' WHERE fold_uuid = ?`)
	if err := feedback.RecordDecision(t.Context(), rh.db.DB, "old", feedback.FeedbackHold, "why"); err != nil {
		t.Fatal(err)
	}
	var s classifier.EngineStatus
	rh.getJSON(t, "/api/engine", &s)
	if !s.Configured || s.ModelName != "Claude Opus 5.5" || !strings.HasPrefix(s.Host, "127.0.0.1:") || s.Reasoning != "none" || !s.Health.Available {
		t.Errorf("status = %+v", s)
	}
	if s.Feedback["hold"] != 1 || s.Waiting.Total != 2 || s.Waiting.Older != 1 || s.Waiting.HeldByFold != 1 {
		t.Errorf("feedback %v, waiting %+v; want 1 hold, 2 waiting, 1 older unheld, 1 held by fold", s.Feedback, s.Waiting)
	}
}

func TestEngine_AnEvaluationStartsOnlyFromTheUI(t *testing.T) {
	rh := newReviewHarness(t)
	rh.withEngine(t, `{}`)
	resp, _ := http.Post(rh.srv.URL+"/api/engine/eval", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a POST without the UI's header = %d, want 403", resp.StatusCode)
	}
	code, a := rh.post(t, "engine/eval", `{"limit": 5}`)
	if code != http.StatusBadRequest || !strings.Contains(a.Message, "no sent transactions") {
		t.Errorf("with nothing sent yet: %d %q; want a 400 that says why", code, a.Message)
	}
	var rep classifier.EvalReport
	if rh.getJSON(t, "/api/engine/eval", &rep); rep.Running {
		t.Error("a failed start left an evaluation running")
	}
}

// fold's own hold reads as fold's, and releasing it is a decision fold
// remembers — never to hold that row again.
func TestCard_FoldsHoldIsFoldsAndItsReleaseIsRemembered(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "cb", "needs_review", "Refund for ___", 17130, "2026-07-19T09:00:00Z",
		`UPDATE staged_fold_txns SET hold_reason = 'Looks like a released card hold', hold_by = 'fold' WHERE fold_uuid = ?`)
	c := rh.deck(t, "").Cards[0]
	if c.Hold == "" || c.HoldBy != "fold" {
		t.Fatalf("card hold %q by %q; want fold's", c.Hold, c.HoldBy)
	}
	if code, a := rh.post(t, "rows/cb/hold", `{"reason": ""}`); code != http.StatusOK || a.Card == nil || a.Card.Hold != "" || a.Card.HoldBy != "" {
		t.Fatalf("release = %d %+v", code, a.Card)
	}
	var n int
	_ = rh.db.DB.QueryRow(`SELECT COUNT(*) FROM review_feedback WHERE fold_uuid = 'cb' AND action = 'release'`).Scan(&n)
	if n != 1 {
		t.Errorf("release feedback rows = %d, want 1", n)
	}
}

// A hold a person sets is theirs, even on a row fold held first.
func TestCard_APersonsHoldIsTheirs(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "cb", "needs_review", "Refund for ___", 17130, "2026-07-19T09:00:00Z",
		`UPDATE staged_fold_txns SET hold_reason = 'fold''s words', hold_by = 'fold' WHERE fold_uuid = ?`)
	rh.post(t, "rows/cb/hold", `{"reason": "Checked: not on the July statement"}`)
	var by, reason string
	_ = rh.db.DB.QueryRow(`SELECT COALESCE(hold_by, ''), hold_reason FROM staged_fold_txns WHERE fold_uuid = 'cb'`).Scan(&by, &reason)
	if by != "" || reason != "Checked: not on the July statement" {
		t.Errorf("hold %q by %q; want the person's, unattributed to fold", reason, by)
	}
	var releases, holds int
	_ = rh.db.DB.QueryRow(`SELECT COUNT(*) FILTER (WHERE action = 'release'), COUNT(*) FILTER (WHERE action = 'hold') FROM review_feedback`).Scan(&releases, &holds)
	if releases != 0 || holds != 1 {
		t.Errorf("feedback: %d releases, %d holds; want one hold", releases, holds)
	}
}

// The full editor sends the stored hold back on every save: that is no
// decision, and fold's hold stays fold's. Clearing it there is a release.
func TestEditor_ASaveKeepsFoldsHoldUntilItIsCleared(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "cb", "needs_review", "Refund for ___", 17130, "2026-07-19T09:00:00Z",
		`UPDATE staged_fold_txns SET hold_reason = 'Looks like a released card hold', hold_by = 'fold' WHERE fold_uuid = ?`)
	save := func(hold string) {
		resp, err := http.PostForm(rh.srv.URL+"/transactions/cb/save", url.Values{
			"description": {"Refund for concert tickets"}, "destination_name": {"Chai Corner, Market Road"},
			"source_name": {"Tata Neu HDFC Bank Credit Card"}, "hold_reason": {hold}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	save("Looks like a released card hold")
	var by string
	_ = rh.db.DB.QueryRow(`SELECT COALESCE(hold_by, '') FROM staged_fold_txns WHERE fold_uuid = 'cb'`).Scan(&by)
	if by != "fold" {
		t.Errorf("after a save that re-sent the same hold, hold_by = %q; want still fold's", by)
	}
	save("")
	var n int
	var reason *string
	_ = rh.db.DB.QueryRow(`SELECT hold_reason FROM staged_fold_txns WHERE fold_uuid = 'cb'`).Scan(&reason)
	_ = rh.db.DB.QueryRow(`SELECT COUNT(*) FROM review_feedback WHERE fold_uuid = 'cb' AND action = 'release'`).Scan(&n)
	if reason != nil || n != 1 {
		t.Errorf("after clearing the hold in the editor: hold %v, %d release rows; want released and remembered", reason, n)
	}
}

// The engine says what each blank stands for — while the title is its own.
func TestCard_BlanksAreNamedByTheEngine(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "b", "needs_review", "___ from Daily Mart for ___", 51900, "2026-09-20T09:00:00Z",
		`UPDATE staged_fold_txns SET classifier_evidence_json = '{"unknowns":["Items bought","who with"]}' WHERE fold_uuid = ?`)
	c := rh.deck(t, "").Cards[0]
	if c.Why == nil || strings.Join(c.Why.Blanks, "|") != "items bought|who with" {
		t.Fatalf("why = %+v; want the two blanks named, lower case", c.Why)
	}
	mustExec(t, rh.db, `UPDATE staged_fold_txns SET confirmed_description = 'Curd from Daily Mart for ___' WHERE fold_uuid = 'b'`)
	if c := rh.deck(t, "").Cards[0]; c.Why != nil && len(c.Why.Blanks) > 0 {
		t.Errorf("blanks %v named on a title the person wrote", c.Why.Blanks)
	}
	// counts that don't line up say nothing rather than something wrong
	mustExec(t, rh.db, `UPDATE staged_fold_txns SET confirmed_description = NULL, classifier_evidence_json = '{"unknowns":["items"]}' WHERE fold_uuid = 'b'`)
	if c := rh.deck(t, "").Cards[0]; c.Why != nil && len(c.Why.Blanks) > 0 {
		t.Errorf("one unknown was mapped onto two blanks: %v", c.Why.Blanks)
	}
}

// "Learned" appears only where it is true: a re-suggestion after your
// corrections changed this card, and you haven't touched it since.
func TestCard_LearnedOnlyWhenACorrectionChangedIt(t *testing.T) {
	rh := newReviewHarness(t)
	ev := `UPDATE staged_fold_txns SET classifier_evidence_json = '{"signals":["your correction of 12 Sep","handle paid 4 times before","third"]}', resuggest_reason = 'learned', resuggest_changed = 1 WHERE fold_uuid = ?`
	rh.stage(t, "l", "needs_review", "Masala chai", 20000, "2026-09-20T09:00:00Z", ev)
	if c := rh.deck(t, "").Cards[0]; c.Why == nil || c.Why.Learned != "your correction of 12 Sep · handle paid 4 times before" {
		t.Fatalf("why = %+v; want the first two signals", c.Why)
	}
	for _, q := range []string{
		`UPDATE staged_fold_txns SET resuggest_changed = 0 WHERE fold_uuid = 'l'`,
		`UPDATE staged_fold_txns SET resuggest_changed = 1, resuggest_reason = 'engine' WHERE fold_uuid = 'l'`,
		`UPDATE staged_fold_txns SET resuggest_reason = 'learned', confirmed_category_id = 9 WHERE fold_uuid = 'l'`,
	} {
		mustExec(t, rh.db, q)
		if c := rh.deck(t, "").Cards[0]; c.Why != nil && c.Why.Learned != "" {
			t.Errorf("after %q the card still says learned: %q", q, c.Why.Learned)
		}
	}
}

// The engine's case for a hold is offered — never on a held card.
func TestCard_TheEngineCanSuggestAHold(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "h", "needs_review", "x", 17130, "2026-09-20T09:00:00Z",
		`UPDATE staged_fold_txns SET classifier_evidence_json = '{"hold_suggestion":"A released authorisation, not money in"}' WHERE fold_uuid = ?`)
	if c := rh.deck(t, "").Cards[0]; c.Why == nil || c.Why.Hold != "A released authorisation, not money in" {
		t.Fatalf("why = %+v", c.Why)
	}
	mustExec(t, rh.db, `UPDATE staged_fold_txns SET hold_reason = 'held' WHERE fold_uuid = 'h'`)
	if c := rh.deck(t, "").Cards[0]; c.Why != nil && c.Why.Hold != "" {
		t.Errorf("a held card still offers a hold: %+v", c.Why)
	}
}

// The full editor names the engine and what decided its suggestion.
func TestEditor_SaysHowItWasSuggested(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "e", "needs_review", "Masala chai", 20000, "2026-09-20T09:00:00Z",
		`UPDATE staged_fold_txns SET classifier_model = 'claude-opus-5-5', hold_reason = 'Looks like a released card hold', hold_by = 'fold',
		   resuggest_reason = 'learned', resuggest_changed = 1, resuggested_at = '2026-09-27 05:00:00',
		   classifier_evidence_json = '{"note":"The handle was paid four times before.","signals":["handle paid 4 times before"],"unknowns":["who with"],"learned":{"corrections":1,"ledger_rows":4}}' WHERE fold_uuid = ?`)
	resp, err := http.Get(rh.srv.URL + "/transactions/e")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	for _, want := range []string{"Held by fold:", "How it was suggested", "Claude Opus 5.5 · Tier 3 (LLM)", "handle paid 4 times before",
		"Drew on 1 correction of yours · 4 Firefly rows naming the same payee.", "Left blank for you: who with.",
		"after your corrections to similar transactions — and it changed.", "The handle was paid four times before."} {
		if !strings.Contains(page, want) {
			t.Errorf("the editor lacks %q", want)
		}
	}
}

// The picker asks as a name is typed, and always gets a list back — empty
// when there is nothing to offer — never an error to show for a hint.
func TestPlaces_ThePickerGetsSuggestionsOrNothing(t *testing.T) {
	rh := newReviewHarness(t)
	rh.withEngine(t, `{"suggestions":[{"name":"Chai Corner, Market Road","what":"tea stall","confidence":0.9},{"name":"Chai Point, Market Road","what":"tea chain","confidence":0.7}]}`)
	rh.stage(t, "p", "needs_review", "Masala chai", 20000, "2026-09-20T09:00:00Z")
	var res classifier.PlacesResult
	if code := rh.getJSON(t, "/api/rows/p/places?q=chai", &res); code != http.StatusOK || len(res.Suggestions) != 2 {
		t.Fatalf("places = %d %+v", code, res)
	}
	if s := res.Suggestions[0]; !s.Existing || s.Name != "Chai Corner, Market Road" || res.Suggestions[1].Existing {
		t.Errorf("suggestions = %+v; want the known payee marked as one, the other as new", res.Suggestions)
	}
	for _, q := range []string{"ch", ""} {
		var empty classifier.PlacesResult
		if code := rh.getJSON(t, "/api/rows/p/places?q="+q, &empty); code != http.StatusOK || empty.Suggestions == nil || len(empty.Suggestions) != 0 {
			t.Errorf("q=%q: %d %+v; want 200 and an empty list", q, code, empty)
		}
	}
}

// withLookupEngine is withEngine with a gateway that also searches the web
// (auth2api's /v1/messages), for the picker's second question.
func (rh *reviewHarness) withLookupEngine(t *testing.T, reply, lookup string) *classifier.Classifier {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]any{
				{"type": "server_tool_use", "id": "s1", "name": "web_search", "input": map[string]any{"query": "q"}},
				{"type": "web_search_tool_result", "tool_use_id": "s1", "content": []any{}},
				{"type": "text", "text": lookup}}, "stop_reason": "end_turn"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": reply}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(fake.Close)
	cls := classifier.New(rh.db.DB, slog.New(slog.NewTextHandler(io.Discard, nil)), classifier.DefaultConfidenceThreshold, 10)
	main := llm.NewClient("k", "claude-opus-5-5", fake.URL+"/v1", fake.Client())
	cls.SetLLM(main)
	cls.SetLookupLLM(main.WithOverrides("claude-sonnet-5", "none"))
	rh.h.SetClassifier(cls)
	return cls
}

// A place the model couldn't place: the first answer says a lookup is under
// way (and is not the browser's to keep, or asking again would bring the
// wait back); the second waits for it and brings the branches — one of them
// a payee the owner has already.
func TestPlaces_ALookupIsWaitedForAndOnlyItsAnswerKept(t *testing.T) {
	rh := newReviewHarness(t)
	rh.withLookupEngine(t, `{"suggestions":[{"name":"Chai Corner","what":"tea stall","confidence":0.9}]}`,
		`{"found":true,"name":"Chai Corner","what":"tea stall","branches":[{"area":"Market Road"},{"area":"Station Road"}]}`)
	rh.stage(t, "p", "needs_review", "Masala chai", 20000, "2026-09-20T09:00:00Z")
	get := func(path string) (classifier.PlacesResult, string) {
		t.Helper()
		resp, err := http.Get(rh.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var res classifier.PlacesResult
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&res) != nil {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		return res, resp.Header.Get("Cache-Control")
	}
	first, cc := get("/api/rows/p/places?q=chai+corner")
	if first.Lookup != "pending" || cc != "no-store" || len(first.Suggestions) != 1 {
		t.Fatalf("first answer = %+v, Cache-Control %q", first, cc)
	}
	looked, cc := get("/api/rows/p/places/lookup?q=chai+corner")
	var got []string
	for _, s := range looked.Suggestions {
		got = append(got, fmt.Sprintf("%s|%v", s.Name, s.Existing))
	}
	if strings.Join(got, " / ") != "Chai Corner|false / Chai Corner, Market Road|true / Chai Corner, Station Road|false" || looked.Lookup != "" || cc != "private, max-age=600" {
		t.Errorf("after the lookup = %q (lookup %q), Cache-Control %q", got, looked.Lookup, cc)
	}
}
