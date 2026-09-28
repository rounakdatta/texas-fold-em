package classifier

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// gateway is a fake auth2api: the picker's question on /chat/completions, a
// web lookup on /messages, and what each was asked.
type gateway struct {
	*httptest.Server
	mu       sync.Mutex
	asked    []string
	searched []string
	fast     string
	lookup   func(prompt string) (int, string)
}

func newGateway(t *testing.T, fast string, lookup func(string) (int, string)) *gateway {
	t.Helper()
	g := &gateway{fast: fast, lookup: lookup}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		prompt := ""
		if n := len(body.Messages); n > 0 {
			prompt = body.Messages[n-1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/completions":
			g.mu.Lock()
			g.asked = append(g.asked, prompt)
			reply := g.fast
			g.mu.Unlock()
			b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": reply}, "finish_reason": "stop"}}})
			_, _ = w.Write(b)
		case "/messages":
			g.mu.Lock()
			g.searched = append(g.searched, prompt)
			g.mu.Unlock()
			status, text := g.lookup(prompt)
			if status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(text))
				return
			}
			b, _ := json.Marshal(map[string]any{"content": []map[string]any{
				{"type": "text", "text": "Searching."},
				{"type": "server_tool_use", "id": "s1", "name": "web_search", "input": map[string]any{"query": "q"}},
				{"type": "web_search_tool_result", "tool_use_id": "s1", "content": []any{}},
				{"type": "text", "text": text}},
				"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 100, "output_tokens": 20, "server_tool_use": map[string]any{"web_search_requests": 1}}})
			_, _ = w.Write(b)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *gateway) searches() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.searched...)
}

func found(reply string) func(string) (int, string) {
	return func(string) (int, string) { return http.StatusOK, reply }
}

// lookupWorld is placesWorld — a morning at the courts in Lakeview, then a
// QR payment at a tiffin place — with a gateway that also searches the web.
func lookupWorld(t *testing.T, fast string, lookup func(string) (int, string)) (*Classifier, *gateway) {
	t.Helper()
	c, _ := placesWorld(t)
	g := newGateway(t, fast, lookup)
	wire(c, g)
	return c, g
}

func wire(c *Classifier, g *gateway) {
	main := llm.NewClient("k", "claude-opus-5-5", g.URL, g.Client())
	c.SetLLM(main)
	c.SetFastLLM(main.WithOverrides("claude-sonnet-5", "none"))
	c.SetLookupLLM(main.WithOverrides("claude-sonnet-5", "none"))
}

const (
	unplaced = `{"suggestions":[{"name":"Sunrise Tiffins","what":"South Indian tiffin café","confidence":0.8}]}`
	branches = `{"found":true,"name":"Sunrise Tiffins","what":"South Indian breakfast","branches":[{"area":"Harbour Town","city":"Bayport"},{"area":"Lake View","city":"Bayport"}],"many":false}`
)

func names(list []PlaceSuggestion) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return strings.Join(out, " / ")
}

// The model knew the place, not which branch — so fold looks it up, and the
// branches come back in the owner's spelling ("Lake View" is their
// "Lakeview"), the area they were in that morning first.
func TestAPlaceTheModelCantPlaceIsLookedUp(t *testing.T) {
	c, g := lookupWorld(t, unplaced, found(branches))
	ctx := context.Background()
	res, err := c.SuggestPlaces(ctx, "qr", "sunrise tiffins")
	if err != nil {
		t.Fatal(err)
	}
	if res.Lookup != "pending" || names(res.Suggestions) != "Sunrise Tiffins" {
		t.Fatalf("first answer = %q, lookup %q; want the model's name, and a lookup under way", names(res.Suggestions), res.Lookup)
	}
	res, err = c.SuggestPlacesLooked(ctx, "qr", "sunrise tiffins")
	if err != nil {
		t.Fatal(err)
	}
	want := "Sunrise Tiffins / Sunrise Tiffins, Lakeview / Sunrise Tiffins, Harbour Town"
	if got := names(res.Suggestions); got != want || res.Lookup != "" {
		t.Errorf("after the lookup = %q (lookup %q)\nwant               %q", got, res.Lookup, want)
	}
	if res.Suggestions[1].What != "South Indian breakfast" {
		t.Errorf("what = %q, want the lookup's", res.Suggestions[1].What)
	}

	// Only the place's name and where leave fold — nothing about the payment.
	s := g.searches()
	if len(s) != 1 {
		t.Fatalf("%d searches, want 1", len(s))
	}
	for _, want := range []string{`THE PLACE: "Sunrise Tiffins"`, "WHERE: at home, in the city whose neighbourhoods include ", "Lakeview"} {
		if !strings.Contains(s[0], want) {
			t.Errorf("the lookup lacks %q:\n%s", want, s[0])
		}
	}
	for _, leak := range []string{"₹", "85", "bharatpe", "5d2c", "Court Nine", "Badminton", "sunrise tiffins"} {
		if strings.Contains(s[0], leak) {
			t.Errorf("the lookup gave away %q:\n%s", leak, s[0])
		}
	}

	// Kept: asked again, on this card or another, the branches come at once.
	if res, _ := c.SuggestPlaces(ctx, "qr", "sunrise tiffins"); res.Lookup != "" || !strings.Contains(names(res.Suggestions), "Lakeview") {
		t.Errorf("asked again: %q (lookup %q)", names(res.Suggestions), res.Lookup)
	}
	stagedAt(t, c.db, "qr2", "OUTGOING", "CARD/0a0b0c0d0e0f0a0b/paytmqr1234@paytm/Rs./60.00/OUTGOING/28-09-26", "", 6000,
		time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC), "needs_review")
	if res, _ := c.SuggestPlaces(ctx, "qr2", "sunrise"); res.Lookup != "" || !strings.Contains(names(res.Suggestions), "Harbour Town") {
		t.Errorf("another card: %q (lookup %q)", names(res.Suggestions), res.Lookup)
	}
	// …and after a restart, from the table.
	fresh := New(c.db, slog.Default(), DefaultConfidenceThreshold, 10)
	wire(fresh, g)
	if res, _ := fresh.SuggestPlaces(ctx, "qr", "sunrise tiffins"); res.Lookup != "" || !strings.Contains(names(res.Suggestions), "Lakeview") {
		t.Errorf("after a restart: %q (lookup %q)", names(res.Suggestions), res.Lookup)
	}
	if n := len(g.searches()); n != 1 {
		t.Errorf("%d searches in all, want the one", n)
	}
}

// A name that may be a person's never goes to a search engine: a UPI
// payment to someone's own handle, and a model that couldn't say what
// business it is.
func TestANameThatMayBeAPersonIsNeverSearched(t *testing.T) {
	c, g := lookupWorld(t, `{"suggestions":[{"name":"Asha Menon","what":"","confidence":0.8}]}`, found(branches))
	stagedAt(t, c.db, "friend", "OUTGOING", "UPI/asha.menon@okaxis/dinner split", "", 45000, time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC), "needs_review")
	for _, q := range []string{"asha menon", "asha"} {
		res, err := c.SuggestPlaces(context.Background(), "friend", q)
		if err != nil || res.Lookup != "" {
			t.Errorf("%q: lookup %q, %v", q, res.Lookup, err)
		}
	}
	if s := g.searches(); len(s) != 0 {
		t.Errorf("searched for a person: %q", s)
	}
}

// Who paid you is never looked up.
func TestMoneyInIsNeverSearched(t *testing.T) {
	c, g := lookupWorld(t, `{"suggestions":[{"name":"Asha Menon","what":"a friend","confidence":0.9}]}`, found(branches))
	stagedAt(t, c.db, "back", "INCOMING", "UPI-ASHA MENON-asha@okaxis-REF-1-UPI", "", 45000, time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC), "needs_review")
	if res, err := c.SuggestPlaces(context.Background(), "back", "asha"); err != nil || res.Lookup != "" {
		t.Errorf("lookup %q, %v", res.Lookup, err)
	}
	if s := g.searches(); len(s) != 0 {
		t.Errorf("searched on money in: %q", s)
	}
}

// A chain with more branches than it lists is offered only at a branch in an
// area they were in that day; anywhere else, which one it was can't be told.
func TestAChainIsOfferedOnlyWhereTheyWere(t *testing.T) {
	chain := `{"found":true,"name":"Sunrise Tiffins","what":"tiffin chain","branches":[{"area":"Harbour Town"},{"area":"Lake View"},{"area":"Old Quay"}],"many":true}`
	c, _ := lookupWorld(t, unplaced, found(chain))
	res, _ := c.SuggestPlacesLooked(context.Background(), "qr", "sunrise tiffins")
	if got := names(res.Suggestions); got != "Sunrise Tiffins / Sunrise Tiffins, Lakeview" {
		t.Errorf("suggestions = %q, want only the branch where they were", got)
	}
	// nobody's payments that day: no branch at all
	mustExec(t, c.db, `DELETE FROM staged_fold_txns WHERE fold_uuid = 'court'`)
	c.places = placesCache{}
	c.lookups = lookupStore{}
	mustExec(t, c.db, `DELETE FROM place_lookups`)
	res, _ = c.SuggestPlacesLooked(context.Background(), "qr", "sunrise tiffins")
	if got := names(res.Suggestions); got != "Sunrise Tiffins" {
		t.Errorf("suggestions = %q, want no branch guessed", got)
	}
}

// An area comes back as the owner writes it — the same area, spacing,
// punctuation or one slip aside — but never as a different area that merely
// looks alike.
func TestAreasAreSpeltTheOwnersWay(t *testing.T) {
	owner := map[string]int{"Harbortown": 3, "Q.P. Nagar": 5, "Westgate": 9, "Lake View": 2}
	for in, want := range map[string]string{
		"Harbourtown": "Harbortown", // one slip, the owner's spelling
		"QP Nagar":    "Q.P. Nagar", // punctuation aside
		"Lakeview":    "Lake View",  // spacing aside
		"Eastgate":    "Eastgate",   // not Westgate
		"Old Quay":    "Old Quay",   // not one of theirs: as found
	} {
		if got := ownerSpelling(in, owner); got != want {
			t.Errorf("ownerSpelling(%q) = %q, want %q", in, got, want)
		}
	}
}

// A host that doesn't run the search tool is asked once, not on every card.
func TestAHostWithoutSearchIsLeftAlone(t *testing.T) {
	c, g := lookupWorld(t, unplaced, func(string) (int, string) {
		return http.StatusBadRequest, `{"error":{"message":"tools.0: unknown tool type web_search_20250305"}}`
	})
	ctx := context.Background()
	if res, _ := c.SuggestPlaces(ctx, "qr", "sunrise tiffins"); res.Lookup != "pending" {
		t.Fatalf("lookup %q", res.Lookup)
	}
	if res, _ := c.SuggestPlacesLooked(ctx, "qr", "sunrise tiffins"); names(res.Suggestions) != "Sunrise Tiffins" {
		t.Errorf("a failed lookup changed the answer: %q", names(res.Suggestions))
	}
	stagedAt(t, c.db, "qr3", "OUTGOING", "CARD/0a0b0c0d0e0f0a0c/bharatpe.9@fbpe/Rs./40.00/OUTGOING/28-09-26", "", 4000, time.Date(2026, 9, 28, 5, 0, 0, 0, time.UTC), "needs_review")
	g.mu.Lock()
	g.fast = `{"suggestions":[{"name":"Morning Dosa","what":"dosa stall","confidence":0.8}]}`
	g.mu.Unlock()
	if res, _ := c.SuggestPlaces(ctx, "qr3", "morning dosa"); res.Lookup != "" {
		t.Errorf("asked a host without search again: lookup %q", res.Lookup)
	}
	if n := len(g.searches()); n != 1 {
		t.Errorf("%d searches, want 1", n)
	}
}

// A place the model placed is checked too, without a word: a branch it
// didn't know is added below its answer, which stays first.
func TestAPlacedAnswerIsCheckedQuietly(t *testing.T) {
	c, _ := lookupWorld(t, `{"suggestions":[{"name":"Sunrise Tiffins, Harbour Town","what":"tiffin café","confidence":0.8}]}`, found(branches))
	ctx := context.Background()
	if res, _ := c.SuggestPlaces(ctx, "qr", "sunrise tiffins"); res.Lookup != "checking" {
		t.Fatalf("lookup %q, want a quiet check", res.Lookup)
	}
	res, _ := c.SuggestPlacesLooked(ctx, "qr", "sunrise tiffins")
	if got := names(res.Suggestions); got != "Sunrise Tiffins, Harbour Town / Sunrise Tiffins, Lakeview" {
		t.Errorf("suggestions = %q", got)
	}
}

// Abroad, the lookup is told so, and a branch is named with its city.
func TestAbroadTheBranchCarriesItsCity(t *testing.T) {
	c, g := lookupWorld(t, unplaced, found(`{"found":true,"name":"Sunrise Tiffins","branches":[{"area":"Riverside","city":"Portvale"}]}`))
	mustExec(t, c.db, `UPDATE staged_fold_txns SET foreign_currency = 'EUR' WHERE fold_uuid = 'qr'`)
	res, _ := c.SuggestPlacesLooked(context.Background(), "qr", "sunrise tiffins")
	if got := names(res.Suggestions); got != "Sunrise Tiffins / Sunrise Tiffins, Riverside, Portvale" {
		t.Errorf("suggestions = %q", got)
	}
	if s := g.searches(); len(s) != 1 || !strings.Contains(s[0], "abroad — the payment was in EUR") {
		t.Errorf("the lookup wasn't told it was abroad: %q", s)
	}
}

// Nothing by that name: nothing added — and that is kept too, for a while.
func TestAPlaceNotFoundAddsNothing(t *testing.T) {
	c, g := lookupWorld(t, unplaced, found(`{"found":false}`))
	ctx := context.Background()
	for range 2 {
		if res, _ := c.SuggestPlacesLooked(ctx, "qr", "sunrise tiffins"); names(res.Suggestions) != "Sunrise Tiffins" {
			t.Errorf("suggestions = %q", names(res.Suggestions))
		}
	}
	if n := len(g.searches()); n != 1 {
		t.Errorf("%d searches, want 1", n)
	}
}

// A comparison learns what a model refuses once, and says so — before, each
// run paid (and timed) a refused request first.
func TestComparisonsLearnARefusalOnce(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Temperature *float64 `json:"temperature"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests++
		mu.Unlock()
		if body.Temperature != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"` + "`temperature`" + ` is deprecated for this model."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"suggestions\":[]}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c, _ := placesWorld(t)
	c.SetLLM(llm.NewClient("k", "claude-opus-5-5", srv.URL, srv.Client()))
	for i := range 2 {
		runs, err := c.ComparePlaces(context.Background(), []PlacesCase{{UUID: "qr", Typed: "sunrise"}}, []string{"claude-fable-5-1"})
		if err != nil || len(runs) != 1 || runs[0].Error != "" || !runs[0].NoTemperature {
			t.Fatalf("run %d: %+v, %v", i, runs, err)
		}
	}
	if requests != 3 {
		t.Errorf("%d requests for two runs, want 3 (one refusal, learnt)", requests)
	}
}

// A branch is named by its one neighbourhood, the way the owner names areas:
// a block or stage of it is still it, and of two names the one that is
// theirs wins.
func TestABranchGoesByItsNeighbourhood(t *testing.T) {
	owner := map[string]int{"Lakeview": 4, "Q.P. Nagar": 2, "Harbortown": 1}
	for in, want := range map[string]string{
		"Lakeview 5th Block":       "Lakeview",
		"QP Nagar 7th Phase":       "Q.P. Nagar",
		"Harbourtown Sector 2":     "Harbortown",
		"Market Street / Lakeview": "Lakeview",
		"Old Quay / Market Street": "Old Quay", // neither is theirs: the first
		"Lakeview East":            "Lakeview",
		"Phase 2":                  "Phase 2", // nothing but a subdivision: kept
		"South End":                "South End",
		"Riverside 2nd Stage":      "Riverside",
		"Westgate Extension":       "Westgate Extension", // part of the name
		"Lakeview Ph II":           "Lakeview Ph II",     // a bare numeral may be too
	} {
		if got := ownerSpelling(neighbourhood(in, owner), owner); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// A found area is one name: past a comma is an address, not an area.
func TestAFoundAreaIsOneName(t *testing.T) {
	f, ok := parseFacts(`{"found":true,"name":"Sunrise Tiffins","branches":[{"area":"Lakeview, 3rd Cross"},{"area":"Lake View"}]}`)
	if !ok || len(f.Branches) != 1 || f.Branches[0].Area != "Lakeview" {
		t.Errorf("branches = %+v (the same area twice is one)", f.Branches)
	}
}

// Two ways of writing one branch are one suggestion.
func TestABlockOfABranchIsNotAnotherBranch(t *testing.T) {
	c, _ := lookupWorld(t, unplaced, found(`{"found":true,"name":"Sunrise Tiffins","what":"tiffin café","branches":[{"area":"Lake View"},{"area":"Lakeview 5th Block"}]}`))
	res, _ := c.SuggestPlacesLooked(context.Background(), "qr", "sunrise tiffins")
	if got := names(res.Suggestions); got != "Sunrise Tiffins / Sunrise Tiffins, Lakeview" {
		t.Errorf("suggestions = %q", got)
	}
}

// What the search found the place is corrects the model's guess, on the
// name it gave — the row stays, its small print changes.
func TestTheLookupCorrectsWhatThePlaceIs(t *testing.T) {
	c, _ := lookupWorld(t, `{"suggestions":[{"name":"Sunrise Tiffins","what":"grocery store","confidence":0.8}]}`, found(branches))
	res, _ := c.SuggestPlacesLooked(context.Background(), "qr", "sunrise tiffins")
	if len(res.Suggestions) == 0 || res.Suggestions[0].Name != "Sunrise Tiffins" || res.Suggestions[0].What != "South Indian breakfast" {
		t.Errorf("first = %+v, want the name kept and what it is corrected", res.Suggestions)
	}
}

// A lookup is kept per model: one a better model makes is made afresh, not
// served from what an earlier one found.
func TestALookupIsKeptPerModel(t *testing.T) {
	c, g := lookupWorld(t, unplaced, found(branches))
	ctx := context.Background()
	c.SuggestPlacesLooked(ctx, "qr", "sunrise tiffins")
	c.SetLookupLLM(llm.NewClient("k", "claude-fable-5-1", g.URL, g.Client()))
	c.places = placesCache{} // (the picker's own answers are kept a quarter of an hour)
	if res, _ := c.SuggestPlaces(ctx, "qr", "sunrise tiffins"); res.Lookup != "pending" {
		t.Errorf("lookup %q with a new model, want a fresh search", res.Lookup)
	}
	c.SuggestPlacesLooked(ctx, "qr", "sunrise tiffins")
	if n := len(g.searches()); n != 2 {
		t.Errorf("%d searches, want 2", n)
	}
}

// A word on the way to a name isn't looked up: a pause after "sunrise" is
// not a place called Sunrise, and the model couldn't say what it is.
func TestAWordOnTheWayIsNotLookedUp(t *testing.T) {
	c, g := lookupWorld(t, `{"suggestions":[{"name":"Sunrise, Lakeview","what":"","confidence":0.5},{"name":"Sunrise","what":"","confidence":0.4}]}`, found(branches))
	for _, q := range []string{"sunrise", "sunrise t", "sunrise tif"} {
		if res, _ := c.SuggestPlaces(context.Background(), "qr", q); res.Lookup != "" {
			t.Errorf("lookup %q for %q, a name on its way", res.Lookup, q)
		}
	}
	// typed whole, on a card, it is — though the model couldn't say what it is
	g.mu.Lock()
	g.fast = `{"suggestions":[{"name":"Sunrise Tiffins","what":"","confidence":0.6}]}`
	g.mu.Unlock()
	if res, _ := c.SuggestPlaces(context.Background(), "qr", "sunrise tiffins"); res.Lookup != "pending" {
		t.Errorf("lookup %q for a whole name on a card", res.Lookup)
	}
}

// The payments around a card — dozens of queries — are gathered once per
// picker question: the prompt, the lookup's plan and the branches it offers
// all read that one gathering, and a kept answer gathers nothing.
func TestAPickerQuestionGathersTheDayOnce(t *testing.T) {
	c, _ := lookupWorld(t, unplaced, found(branches))
	var mu sync.Mutex
	n := 0
	neighboursHook = func() { mu.Lock(); n++; mu.Unlock() }
	t.Cleanup(func() { neighboursHook = nil })
	ctx := context.Background()
	c.SuggestPlaces(ctx, "qr", "sunrise tiffins")
	c.SuggestPlacesLooked(ctx, "qr", "sunrise tiffins")
	c.SuggestPlaces(ctx, "qr", "sunrise tiffins")
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Errorf("the day was gathered %d times for one question, want 1", n)
	}
}

// Another name in brackets is not part of the place's name: one place, one row.
func TestAPlaceKeepsItsOwnName(t *testing.T) {
	for in, want := range map[string]string{
		"Sunrise Tiffins (ST)":  "Sunrise Tiffins",
		"ST (Sunrise Tiffins)":  "ST",
		"Sunrise Tiffins":       "Sunrise Tiffins",
		"(ST)":                  "(ST)",
		"Sunrise (Old) Tiffins": "Sunrise (Old) Tiffins", // not at the end
	} {
		if got := stripAlias(in); got != want {
			t.Errorf("stripAlias(%q) = %q, want %q", in, got, want)
		}
	}
	c, _ := lookupWorld(t, `{"suggestions":[{"name":"Sunrise Tiffins, Lakeview","what":"tiffin café","confidence":0.8}]}`,
		found(`{"found":true,"name":"Sunrise Tiffins (ST)","branches":[{"area":"Lakeview"}]}`))
	res, _ := c.SuggestPlacesLooked(context.Background(), "qr", "sunrise tiffins")
	if got := names(res.Suggestions); got != "Sunrise Tiffins, Lakeview" {
		t.Errorf("suggestions = %q, want one row", got)
	}
}
