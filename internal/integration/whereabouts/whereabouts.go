// Package whereabouts says where the owner was when they paid — at home, or
// on a trip, and in which time zone — so a payment made abroad reads at the
// time it was where it was made.
//
// A foreign currency alone says nothing: a subscription billed in dollars
// from home at noon was paid at noon at home. What says the owner was there
// is the rest: payments in that currency close together, at places their
// payees name the way the owner names places away ("Lantern Noodles,
// Chinatown, Singapore"). That is a trip. Its time zone comes from the
// currency when the currency has one (Singapore dollars are Singapore's),
// and from the places when it doesn't: dollars span four zones, and a diner
// in a town beside New York is New York's while a taquería in San Francisco
// is California's. A payment that
// names no place — a ticket bought online mid-trip — is read in the zone of
// the places around it, when they agree. Anything less certain stays at home
// time: a wrong local time is worse than none.
package whereabouts

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Home is the owner's currency: its payments are at home.
const Home = "INR"

const (
	tripGap    = 72 * time.Hour // payments further apart are separate trips
	tripMargin = 12 * time.Hour // before a trip's first place or after its last is not the trip
	edgeReach  = 24 * time.Hour // a payment at a trip's edge takes the zone of a place this near
	nearReach  = time.Hour      // nobody pays in two zones within the hour: not even a flight is that quick
	minPlaced  = 2              // places a trip must name: one could be a coincidence
	maxContext = 60             // places shown to a Resolver for context
)

// Row is one foreign payment, as Locate reads it.
type Row struct {
	UUID     string
	At       time.Time
	Currency string // ISO, any case
	Payee    string // the other side's name: "Lantern Noodles, Chinatown, Singapore"
}

// Local is where a payment was made: its time zone, and what to call it.
type Local struct {
	Zone  *time.Location
	Place string // "Singapore", "New York": the zone by the city the trip names most in it
}

// Place is a place a trip's payees name that no table here knows, for a
// Resolver to place.
type Place struct {
	Key     string // what it is kept under: "maple hollow"
	Name    string // as the payees write it: "Maple Hollow"
	Context string // the trip's places in order, which say which region a name is
}

// Resolver finds the time zones of places no table here knows — "Maple
// Hollow", beside New York's places, is America/New_York. It answers with what it knows now
// (IANA names by Place.Key) and may look the rest up for next time.
type Resolver interface {
	PlaceZones(ctx context.Context, currency string, places []Place) map[string]string
}

// Timeline is where each foreign payment was made, for those it can say.
type Timeline struct {
	local map[string]Local
}

// For says where a payment was made, when it was made on a trip.
func (t *Timeline) For(uuid string) (Local, bool) {
	if t == nil {
		return Local{}, false
	}
	l, ok := t.local[uuid]
	return l, ok
}

// Len is how many payments it places.
func (t *Timeline) Len() int {
	if t == nil {
		return 0
	}
	return len(t.local)
}

// Build reads every foreign payment fold has — sent, waiting or skipped:
// each is evidence of where the owner was — and places them.
func Build(ctx context.Context, db *sql.DB, r Resolver) (*Timeline, error) {
	rs, err := db.QueryContext(ctx, `
		SELECT s.fold_uuid, COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp), UPPER(TRIM(s.foreign_currency)), s.type,
		       `+dstNameSQL+`, `+srcNameSQL+`
		FROM staged_fold_txns s
		WHERE UPPER(COALESCE(TRIM(s.foreign_currency), '')) NOT IN ('', '`+Home+`')`)
	if err != nil {
		return nil, fmt.Errorf("whereabouts: read: %w", err)
	}
	defer rs.Close()
	var rows []Row
	for rs.Next() {
		var uuid, ts, cur, typ, dst, src string
		if rs.Scan(&uuid, &ts, &cur, &typ, &dst, &src) != nil {
			continue
		}
		at, ok := parseTime(ts)
		if !ok {
			continue
		}
		payee := dst
		if typ == "INCOMING" {
			payee = src // money back from a merchant is the merchant's
		}
		rows = append(rows, Row{UUID: uuid, At: at, Currency: cur, Payee: payee})
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("whereabouts: read: %w", err)
	}
	return Locate(ctx, rows, r), nil
}

// The other side's name as fold has it: what the owner confirmed, else what
// the engine proposed, by the account's own name in Firefly when it is one.
const (
	dstNameSQL = `COALESCE(
	    CASE WHEN s.confirmed_destination_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.confirmed_destination_account_id)
	         ELSE NULLIF(s.confirmed_destination_account_name,'') END,
	    CASE WHEN s.proposed_destination_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.proposed_destination_account_id)
	         ELSE NULLIF(s.proposed_destination_account_name,'') END,
	    '')`
	srcNameSQL = `COALESCE(
	    CASE WHEN s.confirmed_source_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.confirmed_source_account_id)
	         ELSE NULLIF(s.confirmed_source_account_name,'') END,
	    CASE WHEN s.proposed_source_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.proposed_source_account_id)
	         ELSE NULLIF(s.proposed_source_account_name,'') END,
	    '')`
)

// Locate places the rows. The database and the model are the callers':
// Build reads the one, a Resolver asks the other.
func Locate(ctx context.Context, rows []Row, r Resolver) *Timeline {
	tl := &Timeline{local: map[string]Local{}}
	byCur := map[string][]Row{}
	for _, row := range rows {
		cur := strings.ToUpper(strings.TrimSpace(row.Currency))
		if cur == "" || cur == Home {
			continue
		}
		row.Currency = cur
		byCur[cur] = append(byCur[cur], row)
	}
	curs := make([]string, 0, len(byCur))
	for cur := range byCur {
		curs = append(curs, cur)
	}
	sort.Strings(curs)
	for _, cur := range curs {
		rs := byCur[cur]
		sort.SliceStable(rs, func(i, j int) bool {
			return rs[i].At.Before(rs[j].At) || (rs[i].At.Equal(rs[j].At) && rs[i].UUID < rs[j].UUID)
		})
		start := 0
		for i := 1; i <= len(rs); i++ {
			if i == len(rs) || rs[i].At.Sub(rs[i-1].At) > tripGap {
				locateTrip(ctx, tl, cur, rs[start:i], r)
				start = i
			}
		}
	}
	return tl
}

// locateTrip places one run of payments in a currency, if they are a trip.
func locateTrip(ctx context.Context, tl *Timeline, cur string, trip []Row, r Resolver) {
	named := make([]place, len(trip))
	var placed []int
	for i, row := range trip {
		if p, ok := placeOf(row.Payee); ok {
			named[i], placed = p, append(placed, i)
		}
	}
	if len(placed) < minPlaced {
		return // nothing says they were there: a charge from home
	}
	zones := make([]*time.Location, len(trip))
	cities := make([]string, len(trip)) // the city a row's place is in, as the tables name it
	if z, ok := currencyZone(cur); ok {
		for i := range trip {
			zones[i] = z
		}
		for _, i := range placed {
			if c, ok := cityOf(named[i], cur); ok && c.zone == z.String() {
				cities[i] = c.name
			}
		}
	} else {
		anchored := make([]bool, len(trip))
		var ask []Place
		askFor := map[string][]int{}
		for _, i := range placed {
			if c, ok := cityOf(named[i], cur); ok {
				if loc, err := time.LoadLocation(c.zone); err == nil {
					zones[i], cities[i], anchored[i] = loc, c.name, true
					continue
				}
			}
			k := named[i].key()
			if _, asked := askFor[k]; !asked {
				ask = append(ask, Place{Key: k, Name: named[i].text()})
			}
			askFor[k] = append(askFor[k], i)
		}
		if r != nil && len(ask) > 0 {
			context := tripContext(named, placed)
			for j := range ask {
				ask[j].Context = context
			}
			for k, zone := range r.PlaceZones(ctx, cur, ask) {
				loc, err := time.LoadLocation(strings.TrimSpace(zone))
				if err != nil || zone == "" {
					continue
				}
				for _, i := range askFor[k] {
					zones[i], anchored[i] = loc, true
				}
			}
		}
		fillFromAround(trip, zones, anchored)
	}
	// The trip is its named places, and a margin either side: a charge in
	// its currency days after the last one was made from home again.
	from, to := trip[placed[0]].At.Add(-tripMargin), trip[placed[len(placed)-1]].At.Add(tripMargin)
	names := zoneNames(zones, cities)
	for i, row := range trip {
		if zones[i] == nil || row.At.Before(from) || row.At.After(to) {
			continue
		}
		tl.local[row.UUID] = Local{Zone: zones[i], Place: names[zones[i].String()]}
	}
}

// fillFromAround gives a payment that names no place (or one nobody could
// place) the zone of the placed payments around it, when those agree — a
// payment between New York's and Chicago's was made on the way, in either —
// or, when they don't, of the one within the hour of it: a snack at the
// airport minutes before the train into town is the town's. At a trip's
// edge, the one placed payment beside it, when it is near.
func fillFromAround(trip []Row, zones []*time.Location, anchored []bool) {
	for i := range trip {
		if zones[i] != nil {
			continue
		}
		before, after := -1, -1
		for j := i - 1; j >= 0; j-- {
			if anchored[j] {
				before = j
				break
			}
		}
		for j := i + 1; j < len(trip); j++ {
			if anchored[j] {
				after = j
				break
			}
		}
		switch {
		case before >= 0 && after >= 0:
			sinceBefore, untilAfter := trip[i].At.Sub(trip[before].At), trip[after].At.Sub(trip[i].At)
			switch {
			case zones[before].String() == zones[after].String():
				zones[i] = zones[before]
			case sinceBefore <= nearReach && untilAfter > nearReach:
				zones[i] = zones[before]
			case untilAfter <= nearReach && sinceBefore > nearReach:
				zones[i] = zones[after]
			}
		case before >= 0:
			if trip[i].At.Sub(trip[before].At) <= edgeReach {
				zones[i] = zones[before]
			}
		case after >= 0:
			if trip[after].At.Sub(trip[i].At) <= edgeReach {
				zones[i] = zones[after]
			}
		}
	}
}

// zoneNames: what to call each of a trip's zones — the city it names most
// in that zone ("San Francisco", though Sunnyvale was in it too), else the
// zone's own name.
func zoneNames(zones []*time.Location, cities []string) map[string]string {
	counts := map[string]map[string]int{}
	first := map[string]int{}
	for i, z := range zones {
		if z == nil || cities[i] == "" {
			continue
		}
		zk := z.String()
		if counts[zk] == nil {
			counts[zk] = map[string]int{}
		}
		if _, seen := first[zk+"|"+cities[i]]; !seen {
			first[zk+"|"+cities[i]] = i
		}
		counts[zk][cities[i]]++
	}
	out := map[string]string{}
	for _, z := range zones {
		if z == nil {
			continue
		}
		zk := z.String()
		if _, done := out[zk]; done {
			continue
		}
		best, bestN := "", 0
		for city, n := range counts[zk] {
			if n > bestN || (n == bestN && first[zk+"|"+city] < first[zk+"|"+best]) {
				best, bestN = city, n
			}
		}
		if best == "" {
			best = zoneName(zk)
		}
		out[zk] = best
	}
	return out
}

// tripContext is a trip's places in order, each once where it repeats, for
// a Resolver to tell which region a name is.
func tripContext(named []place, placed []int) string {
	var out []string
	for _, i := range placed {
		t := named[i].text()
		if len(out) > 0 && out[len(out)-1] == t {
			continue
		}
		out = append(out, t)
		if len(out) == maxContext {
			break
		}
	}
	return strings.Join(out, " · ")
}

// place is where a payee's name says it is: the parts after the business
// ("Lantern Noodles, Chinatown, Singapore" → Chinatown, Singapore).
type place struct{ parts []string }

// placeOf reads a payee's place, in the owner's style ("Place, Area" or
// "Place, Area, City"); a name with no place in it ("Hotel Booker") has none.
func placeOf(payee string) (place, bool) {
	var parts []string
	for _, p := range strings.Split(payee, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return place{}, false
	}
	return place{parts: parts[1:]}, true
}

func (p place) text() string { return strings.Join(p.parts, ", ") }
func (p place) key() string  { return normalize(p.text()) }

// cityOf is the city a place is in, as the tables know it for the currency:
// its last part ("…, San Francisco"), else the one before.
func cityOf(p place, cur string) (city, bool) {
	for i := len(p.parts) - 1; i >= 0 && i >= len(p.parts)-2; i-- {
		if c, ok := cityIn(cur, p.parts[i]); ok {
			return c, true
		}
	}
	return city{}, false
}

// normalize compares place names as a person would: case, spacing and
// punctuation aside.
func normalize(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}), " ")
}

// parseTime reads the shapes fold stores times in.
func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
