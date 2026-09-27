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
	c, _ := placesWorld(t)
	res, err := c.SuggestPlaces(context.Background(), "qr", "sunrise ti")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range res.Suggestions {
		got = append(got, s.Name+"|"+s.What+"|"+map[bool]string{true: "existing", false: "new"}[s.Existing])
	}
	want := []string{"sunrise tiffins, lakeview|South Indian tiffin café|new", "Tea Trail Express, Koramangala||existing", "Lantern Books, Lakeview||existing"}
	if strings.Join(got, " / ") != strings.Join(want, " / ") {
		t.Errorf("suggestions = %q\nwant          %q", got, want)
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
