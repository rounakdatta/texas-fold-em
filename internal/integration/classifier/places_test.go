package classifier

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// placesWorld: a morning out in one area — a court booked, then a QR payment
// at a tiffin place the owner hasn't paid before — and a history of payees
// named "Place, Area".
func placesWorld(t *testing.T) (*Classifier, *capturingLLM) {
	t.Helper()
	db := learnDB(t)
	mustExec(t, db, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES
		(60, 'Court Nine, Lakeview', 'expense', 1, '{}'),
		(61, 'Lantern Books, Lakeview', 'expense', 1, '{}'),
		(62, 'Harbour Noodle, Old Town, Harbourtown', 'expense', 1, '{}'),
		(63, 'Tea Trail Express, Koramangala', 'expense', 1, '{}')`)
	at := time.Date(2026, 9, 27, 4, 25, 0, 0, time.UTC) // 9:55 am IST
	stagedAt(t, db, "court", "OUTGOING", "UPI/courtnine@okaxis/x", "court nine", 60000, at.Add(-80*time.Minute), "pushed")
	propose(t, db, "court", 60, 7, "Badminton court booking", "")
	stagedAt(t, db, "qr", "OUTGOING", "CARD/5d2c4b3a29181706/bharatpe.90000000001@fbpe/Rs./85.00/OUTGOING/27-09-26", "", 8500, at, "needs_review")
	fake := newCapturingLLM(t, `{"suggestions":[
		{"name":"sunrise tiffins, lakeview.","what":"South Indian tiffin café.","confidence":0.8},
		{"name":"Tea Trail Express, Koramangala","what":"","confidence":0.4},
		{"name":"Sunrise Tiffins, Lakeview","what":"dup","confidence":0.7},
		{"name":"Sunrise Snacks Corner","what":"a guess","confidence":0.1},
		{"name":"Lantern Books, Lakeview","what":"","confidence":0.3},
		{"name":"One Too Many","what":"","confidence":0.9}]}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "claude-opus-5-5", fake.URL, fake.Client()))
	return c, fake
}

// The model is asked with what the payment says and where the owner was —
// the evidence that places a name nobody wrote down.
func TestPlacesAreAskedWithWhereYouWere(t *testing.T) {
	c, fake := placesWorld(t)
	if _, err := c.SuggestPlaces(context.Background(), "qr", "sunrise  ti"); err != nil {
		t.Fatal(err)
	}
	p := fake.last()
	for _, want := range []string{
		`TYPED: "sunrise ti"`,
		"money out, ₹85.00 on Sun 27 Sept 2026 at 9:55 am IST",
		"a QR code payment: the handle names the payment app, not the shop",
		"YOUR OTHER PAYMENTS AROUND IT", `Court Nine, Lakeview  ("Badminton court booking")`,
		"YOUR AREAS", "Lakeview ×", "Cities beyond home: Harbourtown",
		`YOUR STYLE:`, `"Harbour Noodle, Old Town, Harbourtown"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the places prompt lacks %q:\n%s", want, p)
		}
	}
}

// At most three, each once, the unlikely dropped, cleaned — and one that is
// already a payee comes back as exactly that payee.
func TestPlaceSuggestionsAreCleanedAndMatchedToPayees(t *testing.T) {
	expense := []AccountRef{{ID: 63, Name: "Tea Trail Express, Koramangala"}, {ID: 61, Name: "Lantern Books, Lakeview"}}
	out := `{"suggestions":[
		{"name":"sunrise tiffins, lakeview.","what":"South Indian tiffin café.","confidence":0.8},
		{"name":"tea trail  express, koramangala","what":"","confidence":0.4},
		{"name":"Sunrise Tiffins, Lakeview","what":"dup","confidence":0.7},
		{"name":"Sunrise Snacks Corner","what":"a guess","confidence":0.1},
		{"name":"Lantern Books, Lakeview","what":"","confidence":0.3},
		{"name":"One Too Many","what":"","confidence":0.9}]}`
	var got []string
	for _, s := range parsePlaces(out, expense) {
		got = append(got, s.Name+"|"+s.What+"|"+map[bool]string{true: "existing", false: "new"}[s.Existing])
	}
	want := []string{"sunrise tiffins, lakeview|South Indian tiffin café|new", "Tea Trail Express, Koramangala||existing", "Lantern Books, Lakeview||existing"}
	if strings.Join(got, " / ") != strings.Join(want, " / ") {
		t.Errorf("suggestions = %q\nwant          %q", got, want)
	}
	// end to end, only what fits the typed text reaches the picker
	c, _ := placesWorld(t)
	res, err := c.SuggestPlaces(context.Background(), "qr", "sunrise ti")
	if err != nil || len(res.Suggestions) != 1 || res.Suggestions[0].Name != "sunrise tiffins, lakeview" {
		t.Errorf("SuggestPlaces = %+v, %v; want only the fitting name", res.Suggestions, err)
	}
}

// Asked twice for the same card and text, the model answers once.
func TestPlaceSuggestionsAreRemembered(t *testing.T) {
	c, fake := placesWorld(t)
	for range 3 {
		if _, err := c.SuggestPlaces(context.Background(), "qr", "Sunrise Ti"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.SuggestPlaces(context.Background(), "qr", "sunrise ti"); err != nil { // same text, any case
		t.Fatal(err)
	}
	if n := len(fake.prompts); n != 1 {
		t.Errorf("the model was asked %d times", n)
	}
}

// Nothing to ask about: too few letters, money in, or no model at all.
func TestNoPlacesWhenThereIsNothingToAsk(t *testing.T) {
	c, fake := placesWorld(t)
	stagedAt(t, c.db, "in", "INCOMING", "NEFT/ACME PAYROLL", "acme", 100, time.Now(), "needs_review")
	for _, tc := range []struct{ uuid, q string }{{"qr", "su"}, {"qr", "   "}, {"in", "acme payroll"}, {"nope", "sunrise ti"}} {
		if _, err := c.SuggestPlaces(context.Background(), tc.uuid, tc.q); !errors.Is(err, ErrNoPlaces) {
			t.Errorf("SuggestPlaces(%q, %q) = %v, want ErrNoPlaces", tc.uuid, tc.q, err)
		}
	}
	if len(fake.prompts) != 0 {
		t.Errorf("the model was asked %d times for nothing", len(fake.prompts))
	}
	bare := New(c.db, slog.Default(), DefaultConfidenceThreshold, 10)
	if _, err := bare.SuggestPlaces(context.Background(), "qr", "sunrise ti"); !errors.Is(err, ErrNoPlaces) {
		t.Errorf("with no model: %v", err)
	}
}

// The fast model answers when there is one; the classifying one otherwise.
func TestPlacesUseTheFastModel(t *testing.T) {
	c, slow := placesWorld(t)
	quick := newCapturingLLM(t, `{"suggestions":[{"name":"Sunrise Tiffins, Lakeview","what":"","confidence":0.9}]}`)
	c.SetFastLLM(llm.NewClient("k", "claude-haiku-4-5", quick.URL, quick.Client()))
	if _, err := c.SuggestPlaces(context.Background(), "qr", "sunrise ti"); err != nil {
		t.Fatal(err)
	}
	if len(quick.prompts) != 1 || len(slow.prompts) != 0 {
		t.Errorf("fast model asked %d times, classifying model %d", len(quick.prompts), len(slow.prompts))
	}
}

// While the model rests, the picker is told so at once — no request.
func TestPlacesWaitOutARestingModel(t *testing.T) {
	c, _ := placesWorld(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "refused", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	fast := llm.NewClient("bad", "m", srv.URL, srv.Client())
	_, _ = fast.GenerateJSON(context.Background(), "s", "u") // the refusal rests it
	c.SetFastLLM(fast)
	res, err := c.SuggestPlaces(context.Background(), "qr", "sunrise ti")
	if err != nil || !res.Resting || len(res.Suggestions) != 0 || hits.Load() != 1 {
		t.Errorf("while resting: %+v, %v, %d requests; want resting, empty, no new request", res, err, hits.Load())
	}
}

// A suggestion is what was typed, completed or corrected — never a
// different name the model reached for.
func TestOnlySuggestionsThatFitWhatWasTypedReachThePicker(t *testing.T) {
	list := []PlaceSuggestion{{Name: "Sunrise Tiffins, Lakeview"}, {Name: "Shankar's, Spice Market"}, {Name: "Starbucks, Lakeview"}, {Name: "Sunshine Tea House"},
		{Name: "Harbour Noodle, Maxfield, Harbourtown"}, {Name: "Maxfield Food Centre, Harbourtown"}, {Name: "Sunrise Tiffins, J.P. Lakeview"}}
	for typed, want := range map[string]string{
		"sunrise ti":     "Sunrise Tiffins, Lakeview|Sunrise Tiffins, J.P. Lakeview",
		"su":             "Sunrise Tiffins, Lakeview|Sunshine Tea House|Sunrise Tiffins, J.P. Lakeview",
		"starbuks":       "Starbucks, Lakeview",                                      // a slip in a longer word
		"sn ti":          "",                                                         // not what any says
		"sunrise lak":    "Sunrise Tiffins, Lakeview|Sunrise Tiffins, J.P. Lakeview", // an area typed too
		"tea sun":        "Sunshine Tea House",                                       // words in any order within the place
		"stbx":           "",
		"maxfield":       "Maxfield Food Centre, Harbourtown", // the place, not another place in that area
		"sunrise ti jp":  "Sunrise Tiffins, J.P. Lakeview",    // initials run together
		"sunrisetiffins": "Sunrise Tiffins, Lakeview|Sunrise Tiffins, J.P. Lakeview",
	} {
		var got []string
		for _, s := range fitting(list, typed) {
			got = append(got, s.Name)
		}
		if strings.Join(got, "|") != want {
			t.Errorf("fitting(%q) = %q, want %q", typed, got, want)
		}
	}
}

// Choosing the fast model on evidence: each model answers each case, fresh,
// with how long it took.
func TestModelsCanBeComparedOnThePickersQuestion(t *testing.T) {
	c, fake := placesWorld(t)
	runs, err := c.ComparePlaces(context.Background(), []PlacesCase{{UUID: "qr", Typed: "sunrise ti"}}, []string{"claude-haiku-4-5", "claude-sonnet-5"})
	if err != nil || len(runs) != 2 || runs[0].Model != "claude-haiku-4-5" || runs[1].Model != "claude-sonnet-5" {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	for _, r := range runs {
		if r.Error != "" || len(r.Suggestions) != 1 {
			t.Errorf("run %+v", r)
		}
	}
	if len(fake.prompts) != 2 {
		t.Errorf("asked %d times; a comparison bypasses the cache", len(fake.prompts))
	}
	if _, err := c.ComparePlaces(context.Background(), make([]PlacesCase, 7), []string{"a", "b"}); err == nil {
		t.Error("14 runs were accepted; at most 12")
	}
}
