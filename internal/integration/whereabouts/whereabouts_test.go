package whereabouts

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 7, 2, 0, 0, 0, time.UTC) // a Saturday morning in Asia

func at(h float64) time.Time { return t0.Add(time.Duration(h * float64(time.Hour))) }

func row(uuid string, h float64, cur, payee string) Row {
	return Row{UUID: uuid, At: at(h), Currency: cur, Payee: payee}
}

// fakeResolver places the towns it is told about, and remembers what it was
// asked.
type fakeResolver struct {
	mu    sync.Mutex
	zones map[string]string
	asked []Place
}

func (f *fakeResolver) PlaceZones(_ context.Context, _ string, places []Place) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, places...)
	out := map[string]string{}
	for _, p := range places {
		if z, ok := f.zones[p.Key]; ok {
			out[p.Key] = z
		}
	}
	return out
}

func local(tl *Timeline, uuid string) string {
	l, ok := tl.For(uuid)
	if !ok {
		return "home"
	}
	return l.Zone.String() + " (" + l.Place + ")"
}

// A trip in a currency with one time zone: every payment in it reads at that
// zone's time — the online top-up too, since the owner was there — and a
// charge in that currency long after the trip was made from home.
func TestATripAbroadIsReadAtItsOwnTime(t *testing.T) {
	tl := Locate(context.Background(), []Row{
		row("a", 0, "SGD", "Lantern Noodles, Chinatown, Singapore"),
		row("b", 3, "SGD", "Harbour Tea, Marina Bay, Singapore"),
		row("c", 5, "sgd", "Transit Card"), // names no place
		row("d", 26, "SGD", "Kopi Stall, Singapore"),
		row("later", 26+24*10, "SGD", "Streaming Plus"), // ten days on, from home
	}, nil)
	for _, u := range []string{"a", "b", "c", "d"} {
		if got := local(tl, u); got != "Asia/Singapore (Singapore)" {
			t.Errorf("%s: %s", u, got)
		}
	}
	if got := local(tl, "later"); got != "home" {
		t.Errorf("a charge ten days after the trip: %s, want home time", got)
	}
}

// A foreign currency alone says nothing: charges from home in dollars and
// euros — even several in a day — name no place, and stay at home time.
func TestAForeignChargeFromHomeStaysAtHomeTime(t *testing.T) {
	tl := Locate(context.Background(), []Row{
		row("api", 0, "USD", "Cloud API Credits"),
		row("files", 30, "EUR", "File Host"),
		row("app1", 50, "USD", "App Store"), row("app2", 51, "USD", "Game Shop"), row("app3", 52, "USD", "Font Foundry"),
		row("inr", 53, "INR", "Chai Corner, Market Road"),
	}, nil)
	if tl.Len() != 0 {
		for _, u := range []string{"api", "files", "app1", "app2", "app3", "inr"} {
			t.Logf("%s: %s", u, local(tl, u))
		}
		t.Errorf("%d charges from home read at a foreign time", tl.Len())
	}
}

// One place is not a trip: a single payment at a named place abroad could be
// anything (a gift bought online from a shop that names its town).
func TestOnePlaceIsNotATrip(t *testing.T) {
	tl := Locate(context.Background(), []Row{row("one", 0, "EUR", "Book Barge, Paris")}, nil)
	if got := local(tl, "one"); got != "home" {
		t.Errorf("one named place: %s", got)
	}
}

// Dollars span four zones: a trip in them is placed by the cities it names —
// New York's (and a town a Resolver places), Chicago's, San Francisco's — a
// ticket bought online between two places in one city is in that city's
// zone, and a payment on the day between two cities is left at home time.
func TestDollarsArePlacedByTheirCities(t *testing.T) {
	r := &fakeResolver{zones: map[string]string{"maple hollow": "America/New_York"}}
	tl := Locate(context.Background(), []Row{
		row("ny1", 0, "USD", "Corner Bagels, New York"),
		row("ny2", 5, "USD", "Night Train, Brooklyn"),
		row("town", 20, "USD", "Orchard Stand, Maple Hollow"),
		row("tix", 22, "USD", "Theatre Tickets"), // online, between New York's places
		row("ny3", 30, "USD", "Pier Pizza, Manhattan"),
		row("flight", 40, "USD", "Sky Snacks"), // the day they flew: no place, New York's before and Chicago's after
		row("chi1", 50, "USD", "Deep Dish House, Chicago"),
		row("chi2", 60, "USD", "Lake Market, Chicago"),
		row("sf1", 80, "USD", "Fog Coffee, San Francisco"),
		row("parcel", 85, "USD", "Parcel Store"), // online, between San Francisco's places
		row("sf2", 90, "USD", "Bay Tacos, Sunnyvale"),
		row("sf3", 95, "USD", "Cable Car Ticket, San Francisco"),
	}, r)
	want := map[string]string{
		"ny1": "America/New_York (New York)", "ny2": "America/New_York (New York)", "town": "America/New_York (New York)",
		"tix": "America/New_York (New York)", "ny3": "America/New_York (New York)",
		"flight": "home",
		"chi1":   "America/Chicago (Chicago)", "chi2": "America/Chicago (Chicago)",
		"sf1": "America/Los_Angeles (San Francisco)", "parcel": "America/Los_Angeles (San Francisco)",
		"sf2": "America/Los_Angeles (San Francisco)", "sf3": "America/Los_Angeles (San Francisco)",
	}
	for u, w := range want {
		if got := local(tl, u); got != w {
			t.Errorf("%s: %s, want %s", u, got, w)
		}
	}
	// only the town was asked about, once, with the trip around it
	if len(r.asked) != 1 || r.asked[0].Key != "maple hollow" || r.asked[0].Name != "Maple Hollow" ||
		!strings.Contains(r.asked[0].Context, "New York") || !strings.Contains(r.asked[0].Context, "Chicago") {
		t.Errorf("asked %+v", r.asked)
	}
}

// A name that is a town in more than one region is the Resolver's to place,
// and it is shown the trip's places in order to tell which.
func TestANameInTwoRegionsIsPlacedByTheTripAroundIt(t *testing.T) {
	r := &fakeResolver{zones: map[string]string{}}
	tl := Locate(context.Background(), []Row{
		row("a", 0, "USD", "Deep Dish House, Chicago"),
		row("b", 10, "USD", "Corn Stand, Springfield"),
		row("c", 20, "USD", "Lake Market, Chicago"),
	}, r)
	if len(r.asked) != 1 || r.asked[0].Context != "Chicago · Springfield · Chicago" {
		t.Fatalf("asked %+v", r.asked)
	}
	// unplaced, between two of Chicago's: Chicago's, as a payment naming no place would be
	if got := local(tl, "b"); got != "America/Chicago (Chicago)" {
		t.Errorf("b: %s", got)
	}
}

// What nobody could place is left at home time — no zone is guessed for a
// trip whose places are all unknown, or for a zone that isn't one.
func TestWhatNobodyCouldPlaceStaysAtHomeTime(t *testing.T) {
	r := &fakeResolver{zones: map[string]string{"elm creek": "", "birch falls": "Mars/Olympus_Mons"}}
	tl := Locate(context.Background(), []Row{
		row("a", 0, "USD", "Diner, Elm Creek"),
		row("b", 4, "USD", "Gas Stop, Birch Falls"),
		row("c", 6, "USD", "Parcel Store"),
	}, r)
	if tl.Len() != 0 {
		t.Errorf("%d placed: a %s, b %s, c %s", tl.Len(), local(tl, "a"), local(tl, "b"), local(tl, "c"))
	}
}

// A trip is its named places and a margin: a charge in its currency two days
// after the last one — still close enough to be the same run — was made from
// home.
func TestATripEndsWithItsLastPlace(t *testing.T) {
	tl := Locate(context.Background(), []Row{
		row("a", 0, "AED", "Spice Souk Café, Dubai"),
		row("b", 8, "AED", "Metro Card, Dubai"),
		row("c", 8+11, "AED", "Airport Lounge"),   // 11 h after: the way home
		row("d", 8+48, "AED", "Desert Streaming"), // two days on
	}, nil)
	for u, w := range map[string]string{"a": "Asia/Dubai (Dubai)", "b": "Asia/Dubai (Dubai)", "c": "Asia/Dubai (Dubai)", "d": "home"} {
		if got := local(tl, u); got != w {
			t.Errorf("%s: %s, want %s", u, got, w)
		}
	}
}

// At a trip's edge, where a payment has placed ones on one side only, the
// nearest decides — when it is near.
func TestAtATripsEdgeTheNearestPlaceDecides(t *testing.T) {
	tl := Locate(context.Background(), []Row{
		row("taxi", 0, "EUR", "Airport Taxi"),
		row("a", 5, "EUR", "Café Lumière, Paris"),
		row("b", 9, "EUR", "Book Barge, Paris"),
		row("sim", 30, "EUR", "Travel SIM"), // 21 h after the last place — past the margin
	}, nil)
	for u, w := range map[string]string{"taxi": "Europe/Paris (Paris)", "a": "Europe/Paris (Paris)", "b": "Europe/Paris (Paris)", "sim": "home"} {
		if got := local(tl, u); got != w {
			t.Errorf("%s: %s, want %s", u, got, w)
		}
	}
}

// A zone goes by the city the trip names most in it — or, naming none the
// table knows, by the zone's own city.
func TestAZoneGoesByTheCityTheTripNamesMost(t *testing.T) {
	tl := Locate(context.Background(), []Row{
		row("a", 0, "JPY", "Ramen Bar, Gion, Kyoto"),
		row("b", 5, "JPY", "Tea House, Kyoto"),
		row("c", 30, "JPY", "Sushi Counter, Ginza, Tokyo"),
	}, nil)
	if got := local(tl, "c"); got != "Asia/Tokyo (Kyoto)" {
		t.Errorf("c: %s, want the zone called what the trip named most", got)
	}
	tl = Locate(context.Background(), []Row{
		row("a", 0, "MYR", "Satay Stall, Bukit Bintang"),
		row("b", 5, "MYR", "Night Market, Jalan Alor"),
	}, nil)
	if got := local(tl, "a"); got != "Asia/Kuala_Lumpur (Kuala Lumpur)" {
		t.Errorf("a: %s", got)
	}
}

// A place is read the owner's way: the parts after the business; a state
// after the city is no city.
func TestAPlaceIsReadTheOwnersWay(t *testing.T) {
	for payee, want := range map[string]string{
		"Harbour Tea, Marina Bay, Singapore": "SGD:Singapore",
		"Corner Bagels, New York":            "USD:New York",
		"Night Train, Brooklyn, NY":          "USD:New York", // the part before a state
		"Fog Coffee, Seattle, Washington":    "USD:Seattle",  // not the capital
		"Rooftop, Paris":                     "USD:",         // Paris in dollars is not France's
		"Hotel Booker":                       "USD:",
	} {
		cur, _, _ := strings.Cut(want, ":")
		p, ok := placeOf(payee)
		got := cur + ":"
		if ok {
			if c, found := cityOf(p, cur); found {
				got += c.name
			}
		}
		if got != want {
			t.Errorf("%q → %q, want %q", payee, got, want)
		}
	}
}

// Every zone the tables name is a real one.
func TestEveryZoneInTheTablesLoads(t *testing.T) {
	for cur, z := range singleZone {
		if _, err := time.LoadLocation(z); err != nil {
			t.Errorf("%s: %s: %v", cur, z, err)
		}
	}
	for cur, zones := range cityTable {
		for z := range zones {
			if _, err := time.LoadLocation(z); err != nil {
				t.Errorf("%s: %s: %v", cur, z, err)
			}
		}
	}
}

func TestTheShapesFoldStoresTimesInAreRead(t *testing.T) {
	want := time.Date(2026, 9, 5, 7, 39, 21, 0, time.UTC)
	for _, s := range []string{"2026-09-05T07:39:21Z", "2026-09-05 07:39:21 +0000 UTC", "2026-09-05 07:39:21"} {
		if got, ok := parseTime(s); !ok || !got.Equal(want) {
			t.Errorf("%q → %v %v", s, got, ok)
		}
	}
}

func ExampleLocate() {
	tl := Locate(context.Background(), []Row{
		row("a", 0, "SGD", "Lantern Noodles, Chinatown, Singapore"),
		row("b", 3, "SGD", "Harbour Tea, Marina Bay, Singapore"),
	}, nil)
	l, _ := tl.For("a")
	fmt.Println(at(0).In(l.Zone).Format("3:04 pm"), l.Place, "time")
	// Output: 10:00 am Singapore time
}
