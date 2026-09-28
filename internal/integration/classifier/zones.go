package classifier

// zones.go — where the owner was, for the deck and the prompts.
//
// whereabouts finds the trips (payments in a currency, close together, at
// places named the owner's way) and knows the time zones of the big cities.
// The towns no table lists are placed here: a model is asked
// once per town and currency, in the background, shown the trip's places in
// order so that a name found in several regions is read as the trip's; its
// answer is kept, and the next reading of the deck has them.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
	"github.com/rounakdatta/texas-fold-em/internal/integration/whereabouts"
)

const (
	zoneKeptFor   = 180 * 24 * time.Hour // towns don't move
	zoneUnsureFor = 7 * 24 * time.Hour   // an unsure answer is asked again
	zonesPerAsk   = 40
	whereKeptFor  = time.Minute // how long a reading of the trips is reused
)

// whereState keeps the latest reading of the trips, and which currencies'
// towns are being placed (guarded by Classifier.whereMu).
type whereState struct {
	tl      *whereabouts.Timeline
	at      time.Time
	placing map[string]bool
}

// Whereabouts is where the owner was for each foreign payment: a reading of
// the trips, reused for a minute, and read afresh once a model has placed a
// town.
func (c *Classifier) Whereabouts(ctx context.Context) *whereabouts.Timeline {
	c.whereMu.Lock()
	tl, fresh := c.where.tl, time.Since(c.where.at) < whereKeptFor
	c.whereMu.Unlock()
	if tl != nil && fresh {
		return tl
	}
	built, err := whereabouts.Build(ctx, c.db, c)
	if err != nil {
		if c.log != nil {
			c.log.Warn("whereabouts: read failed", "err", err)
		}
		if tl != nil {
			return tl // the last reading beats none
		}
		return &whereabouts.Timeline{}
	}
	c.whereMu.Lock()
	c.where.tl, c.where.at = built, time.Now()
	c.whereMu.Unlock()
	return built
}

// PlaceZones (whereabouts.Resolver): the zones of towns a trip names, as
// kept; the ones not kept yet are placed in the background, for next time.
func (c *Classifier) PlaceZones(ctx context.Context, currency string, places []whereabouts.Place) map[string]string {
	out := map[string]string{}
	var ask []whereabouts.Place
	for _, p := range places {
		var zone, at string
		if c.db.QueryRowContext(ctx, `SELECT zone, resolved_at FROM place_zones WHERE place_key = ? AND currency = ?`, p.Key, currency).Scan(&zone, &at) == nil {
			if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
				if zone != "" && time.Since(t) < zoneKeptFor {
					out[p.Key] = zone
					continue
				}
				if zone == "" && time.Since(t) < zoneUnsureFor {
					continue // unsure, and asked lately
				}
			}
		}
		ask = append(ask, p)
	}
	if len(ask) > 0 {
		c.placeLater(currency, ask)
	}
	return out
}

// placeLater asks a model for the zones of towns, once per currency at a
// time; what is still unknown after is asked on a later reading.
func (c *Classifier) placeLater(currency string, places []whereabouts.Place) {
	model := c.zonesModel()
	if model == nil {
		return
	}
	c.whereMu.Lock()
	if c.where.placing == nil {
		c.where.placing = map[string]bool{}
	}
	if c.where.placing[currency] {
		c.whereMu.Unlock()
		return
	}
	c.where.placing[currency] = true
	c.whereMu.Unlock()
	if len(places) > zonesPerAsk {
		places = places[:zonesPerAsk]
	}
	go func() {
		defer func() {
			c.whereMu.Lock()
			delete(c.where.placing, currency)
			c.whereMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		zones, err := askZones(ctx, model, currency, places)
		if err != nil {
			if c.log != nil {
				c.log.Warn("whereabouts: placing towns failed", "currency", currency, "err", err)
			}
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, p := range places {
			zone := zones[p.Key] // "" when it couldn't say: kept as unsure
			_, _ = c.db.Exec(`INSERT INTO place_zones (place_key, currency, zone, resolved_at) VALUES (?, ?, ?, ?)
				ON CONFLICT(place_key, currency) DO UPDATE SET zone = excluded.zone, resolved_at = excluded.resolved_at`,
				p.Key, currency, zone, now)
		}
		c.forgetWhereabouts() // read the trips afresh, with the towns placed
	}()
}

// forgetWhereabouts drops the latest reading of the trips: the next one is
// read afresh.
func (c *Classifier) forgetWhereabouts() {
	c.whereMu.Lock()
	c.where.tl = nil
	c.whereMu.Unlock()
}

// zonesModel places towns: the most knowing model there is — accuracy over
// speed, for an answer kept for months.
func (c *Classifier) zonesModel() *llm.Client {
	for _, m := range []*llm.Client{c.lookup, c.llm, c.fast} {
		if m != nil {
			return m
		}
	}
	return nil
}

const zonesSystemPrompt = `You name the time zone of places. Each place below is where someone paid during a trip, in the currency given. The trip's places, in order, show which region a name found in several regions is ("Springfield" among Illinois towns is Illinois's).

Reply with the IANA time zone of each place ("America/New_York"), or "" when you can't be sure. Never guess a country's usual zone for a place you don't know.

Reply with the JSON object only: {"zones":{"<key>":"<IANA zone or empty>"}}`

// askZones asks a model for the zones of places; only real zones come back.
func askZones(ctx context.Context, model *llm.Client, currency string, places []whereabouts.Place) (map[string]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "CURRENCY: %s\n", currency)
	if len(places) > 0 && places[0].Context != "" {
		fmt.Fprintf(&b, "THE TRIP'S PLACES, IN ORDER: %s\n", places[0].Context)
	}
	b.WriteString("\nPLACES TO NAME (key: as written):\n")
	for _, p := range places {
		fmt.Fprintf(&b, "- %s: %q\n", p.Key, p.Name)
	}
	b.WriteString("\nReturn the JSON object now.")
	out, err := model.GenerateJSON(ctx, zonesSystemPrompt, b.String())
	if err != nil {
		return nil, err
	}
	var r struct {
		Zones map[string]string `json:"zones"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(out)), &r); err != nil {
		return nil, fmt.Errorf("unreadable answer: %.120s", out)
	}
	zones := map[string]string{}
	keys := make([]string, 0, len(r.Zones))
	for k := range r.Zones {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		z := strings.TrimSpace(r.Zones[k])
		if z == "" {
			continue
		}
		if _, err := time.LoadLocation(z); err == nil && strings.Contains(z, "/") {
			zones[k] = z
		}
	}
	return zones, nil
}
