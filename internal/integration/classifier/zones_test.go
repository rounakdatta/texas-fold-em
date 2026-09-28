package classifier

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// tripWorld: a trip in dollars — two stops in a town no table knows, then New
// York — with a model that places the town.
func tripWorld(t *testing.T, reply string) (*Classifier, *capturingLLM) {
	t.Helper()
	db := learnDB(t)
	at := time.Date(2026, 3, 7, 14, 0, 0, 0, time.UTC)
	for _, r := range []struct {
		uuid, payee string
		h           int
	}{{"town1", "Diner, Maple Hollow", 0}, {"town2", "Orchard Stand, Maple Hollow", 5}, {"ny", "Corner Bagels, New York", 40}} {
		stagedAt(t, db, r.uuid, "OUTGOING", "CARD/x", "", 50000, at.Add(time.Duration(r.h)*time.Hour), "needs_review")
		mustExec(t, db, `UPDATE staged_fold_txns SET foreign_currency = 'USD', proposed_destination_account_name = ? WHERE fold_uuid = ?`, r.payee, r.uuid)
	}
	fake := newCapturingLLM(t, reply)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "claude-opus-5-5", fake.URL, fake.Client()))
	return c, fake
}

func (f *capturingLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200 && !cond(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("timed out")
	}
}

func placedTowns(t *testing.T, c *Classifier) int {
	var n int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM place_zones`).Scan(&n)
	return n
}

// A town no table knows is placed by a model — once, in the background, shown
// the trip around it — and kept; the next reading of the trips has it.
func TestATownIsPlacedOnceAndKept(t *testing.T) {
	c, fake := tripWorld(t, `{"zones":{"maple hollow":"America/New_York"}}`)
	ctx := context.Background()
	if _, ok := c.Whereabouts(ctx).For("town1"); ok {
		t.Fatal("placed before anyone placed the town")
	}
	waitFor(t, func() bool { return placedTowns(t, c) == 1 })
	for _, u := range []string{"town1", "town2", "ny"} {
		if l, ok := c.Whereabouts(ctx).For(u); !ok || l.Zone.String() != "America/New_York" || l.Place != "New York" {
			t.Errorf("%s: %+v %v", u, l, ok)
		}
	}
	p := fake.last()
	for _, want := range []string{"CURRENCY: USD", "THE TRIP'S PLACES, IN ORDER: Maple Hollow · New York", `- maple hollow: "Maple Hollow"`} {
		if !strings.Contains(p, want) {
			t.Errorf("the question lacks %q:\n%s", want, p)
		}
	}
	c.forgetWhereabouts() // read afresh: nothing is asked again
	c.Whereabouts(ctx)
	time.Sleep(50 * time.Millisecond)
	if n := fake.count(); n != 1 {
		t.Errorf("the model was asked %d times", n)
	}
}

// A town the model isn't sure of stays at home time, and isn't asked about
// again for a week.
func TestAnUnsureTownWaitsAWeek(t *testing.T) {
	c, fake := tripWorld(t, `{"zones":{"maple hollow":""}}`)
	ctx := context.Background()
	c.Whereabouts(ctx)
	waitFor(t, func() bool { return placedTowns(t, c) == 1 })
	c.forgetWhereabouts()
	if _, ok := c.Whereabouts(ctx).For("town1"); ok {
		t.Error("an unsure town was placed")
	}
	time.Sleep(50 * time.Millisecond)
	if n := fake.count(); n != 1 {
		t.Errorf("asked %d times, want once", n)
	}
}

// Only a real zone is kept: not an invented one, nor a stand-in for no place.
func TestOnlyARealZoneIsKept(t *testing.T) {
	c, _ := tripWorld(t, `{"zones":{"a":"Mars/Olympus_Mons","b":"UTC","c":"Local","d":" America/Chicago ","e":""}}`)
	zones, err := askZones(context.Background(), c.llm, "USD", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 || zones["d"] != "America/Chicago" {
		t.Errorf("zones = %v", zones)
	}
}

// The engine reads a trip's payment at its own time — for the meal it names
// ("dinner" at 8 pm there is not a 5:30 pm snack at home) — and says so; a
// foreign charge from home keeps the old, hedged reading.
func TestTheEngineReadsATripsPaymentAtItsOwnTime(t *testing.T) {
	db := learnDB(t)
	at := time.Date(2026, 3, 7, 12, 30, 0, 0, time.UTC) // 8:30 pm in Singapore, 6:00 pm at home
	for _, r := range []struct {
		uuid, payee string
		h           int
	}{{"sg1", "Lantern Noodles, Chinatown, Singapore", -4}, {"sg2", "Harbour Tea, Marina Bay, Singapore", -2}} {
		stagedAt(t, db, r.uuid, "OUTGOING", "CARD/x", "", 50000, at.Add(time.Duration(r.h)*time.Hour), "needs_review")
		mustExec(t, db, `UPDATE staged_fold_txns SET foreign_currency = 'SGD', proposed_destination_account_name = ? WHERE fold_uuid = ?`, r.payee, r.uuid)
	}
	stagedAt(t, db, "dinner", "OUTGOING", "CARD/1a/SATAY STREET/SGD/12.00/OUTGOING/07-03-26", "", 72000, at, "needs_review")
	mustExec(t, db, `UPDATE staged_fold_txns SET foreign_currency = 'SGD' WHERE fold_uuid = 'dinner'`)

	fake := newCapturingLLM(t, `{"txn_type":"withdrawal","confidence":0.5,"reasoning":"r"}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", fake.URL, fake.Client()))
	staged := StagedRow{FoldUUID: "dinner", Type: "OUTGOING", Mode: "CARD", AmountPaise: 72000,
		Narration: "CARD/1a/SATAY STREET/SGD/12.00/OUTGOING/07-03-26", TxnTimestamp: at.Format(time.RFC3339),
		RawPayload: `{"currency":"INR","source_currency":"SGD","source_amount":12}`}
	if _, err := c.ClassifyOne(context.Background(), staged); err != nil {
		t.Fatal(err)
	}
	p := fake.last()
	for _, want := range []string{"local: 2026-03-07 20:30 Sat — Singapore time, where you were (on a trip) — dinner", "home (IST):   2026-03-07 18:00 IST Sat"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "likely-local") {
		t.Error("a placed trip's payment still gets the currency guess")
	}

	// and the picker is told the same
	if _, err := c.SuggestPlaces(context.Background(), "dinner", "satay"); err != nil {
		t.Fatal(err)
	}
	if p := fake.last(); !strings.Contains(p, "on Sat 7 Mar 2026 at 8:30 pm Singapore time (6:00 pm IST)") {
		t.Errorf("the picker's question doesn't say when it was there:\n%s", p)
	}
}
