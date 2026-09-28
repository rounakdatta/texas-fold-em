package classifier

// places_lookup.go — looking a place up when the model can't place it.
//
// A model knows the famous places — a landmark café has one address the
// world knows — but not a neighbourhood tiffin room with two branches, and it
// rightly won't guess which one it was. So when the picker's answer is a business it
// couldn't place, fold looks it up: a model searches the web for that name
// where the payment was made and says which branches it found. Only the name
// and where leave fold — nothing about the payment — and a name that may be
// a person's is never searched. A business it did place is looked up too,
// without a word to the owner: a branch it didn't know is added below.
//
// The branches come back as suggestions in the owner's style — each area
// spelt as they spell it, the areas they were in that day and are most often
// in first — and are kept a month, so the next card that names the place
// has them at once.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// PlaceFacts is what a web search found about a place.
type PlaceFacts struct {
	Found    bool          `json:"found"`
	Name     string        `json:"name,omitempty"`
	What     string        `json:"what,omitempty"`
	Branches []PlaceBranch `json:"branches,omitempty"`
	// Many: a chain with more branches there than it lists — which one a
	// payment was at can't be told from the name.
	Many bool `json:"many,omitempty"`
}

// PlaceBranch is one branch, by the neighbourhood locals name it.
type PlaceBranch struct {
	Area string `json:"area"`
	City string `json:"city,omitempty"`
}

// SetLookupLLM attaches the model that searches the web for a place.
// Without one, places are not looked up.
func (c *Classifier) SetLookupLLM(client *llm.Client) { c.lookup = client }

const (
	lookupFoundTTL   = 30 * 24 * time.Hour // places don't move often
	lookupMissTTL    = 3 * 24 * time.Hour  // a new place gets listed
	lookupFailTTL    = 10 * time.Minute    // a search that failed is tried again soon
	lookupTimeout    = 40 * time.Second
	lookupSearches   = 2
	lookupRefusedFor = 6 * time.Hour // a host without search is asked again after
	lookupBranches   = 6
	groundedMax      = 3
	// lookupVersion is part of what a lookup is kept under, with the model
	// that searched: a better prompt or model looks a place up afresh.
	lookupVersion = 3
)

// lookupStore keeps what searches found, and the ones under way.
type lookupStore struct {
	mu       sync.Mutex
	mem      map[string]lookupEntry
	inflight map[string]chan struct{}
	offUntil time.Time // the host refused search: not asked again until then
}

type lookupEntry struct {
	facts  PlaceFacts
	at     time.Time
	failed bool
}

func (e lookupEntry) fresh() bool {
	ttl := lookupFoundTTL
	switch {
	case e.failed:
		ttl = lookupFailTTL
	case !e.facts.Found:
		ttl = lookupMissTTL
	}
	return time.Since(e.at) < ttl
}

// placeWhere is where a payment was made, as far as fold can tell.
type placeWhere struct {
	key  string // for the cache: "home", "city:<name>", "abroad:<CUR>[:<city>]"
	text string // for the model
	home bool
	city string // the city, when it isn't home
}

// whereFor: abroad when the payment was in another currency; in a city the
// owner's payees around it name ("…, Area, City"); else at home — which the
// model tells from the areas the owner's payees name. (fold never names the
// home city itself.)
func (c *Classifier) whereFor(ctx context.Context, staged StagedRow, ev placesEvidence) placeWhere {
	cities := map[string]int{}
	for _, n := range ev.neighbours {
		if !n.owner {
			continue
		}
		if parts := strings.Split(n.payee, ", "); len(parts) >= 3 {
			cities[strings.TrimSpace(parts[len(parts)-1])]++
		}
	}
	city := topKey(cities)
	cur := strings.ToUpper(strings.TrimSpace(foreignCurrencyOf(staged.RawPayload)))
	if cur == "" || cur == "INR" {
		var fc sql.NullString
		_ = c.db.QueryRowContext(ctx, `SELECT foreign_currency FROM staged_fold_txns WHERE fold_uuid = ?`, staged.FoldUUID).Scan(&fc)
		cur = strings.ToUpper(strings.TrimSpace(fc.String))
	}
	if cur != "" && cur != "INR" {
		w := placeWhere{key: "abroad:" + strings.ToLower(cur), text: "abroad — the payment was in " + cur, city: city}
		if city != "" {
			w.key += ":" + placeKey(city)
			w.text += ", in " + city
		}
		return w
	}
	if city != "" {
		return placeWhere{key: "city:" + placeKey(city), text: "in " + city, city: city}
	}
	var top []string
	for _, a := range rankedCounts(ownerAreas(ev.accounts, ev.usage)) {
		if len(top) == 6 {
			break
		}
		top = append(top, a)
	}
	text := "at home"
	if len(top) > 0 {
		text += ", in the city whose neighbourhoods include " + strings.Join(top, ", ") +
			" (they only say which city: the place may be anywhere in it)"
	}
	return placeWhere{key: "home", text: text, home: true}
}

// lookupPlan is the lookup a picker's answer calls for, with what naming
// the branches it finds needs: how the owner names areas, and the areas
// they were in that day.
type lookupPlan struct {
	key   string
	name  string
	where placeWhere
	quiet bool // the model placed it already: a branch found is added, unannounced
	owner map[string]int
	day   map[string]bool
}

// planLookup: the first new business the answer names, by its name without
// an area. A name that may be a person's is never searched, nor a word on
// the way to a name: the model must have said what the business is — or the
// payment went to one (a card, a shop's QR code) and a whole name was typed.
// Nothing is looked up when the answer is one of the owner's payees, or on
// money in.
func (c *Classifier) planLookup(ctx context.Context, staged StagedRow, typed string, sugg []PlaceSuggestion, ev placesEvidence) *lookupPlan {
	if c.lookup == nil || staged.Type != "OUTGOING" {
		return nil
	}
	business, whole := paidABusiness(staged), wholeName(typed)
	name, placed := "", false
	for _, s := range sugg {
		if s.Existing {
			return nil
		}
		base, _, cut := strings.Cut(s.Name, ",")
		base = strings.TrimSpace(base)
		if s.What == "" && !(business && whole && len(nameWords(base)) >= 2) {
			continue
		}
		name, placed = base, cut
		break
	}
	if name == "" && len(sugg) == 0 && business && whole {
		name = tidyName(typed) // nothing named: what they typed, read as a whole name
	}
	if len([]rune(name)) < placesMinTyped {
		return nil
	}
	where := c.whereFor(ctx, staged, ev)
	key := fmt.Sprintf("%s|%s|v%d|%s", placeKey(name), where.key, lookupVersion, c.lookup.Model())
	return &lookupPlan{key: key, name: name, where: where, quiet: placed, owner: ownerAreas(ev.accounts, ev.usage), day: dayAreas(ev.neighbours)}
}

// dayAreas: the areas of the owner's payments around a card.
func dayAreas(ns []neighbour) map[string]bool {
	day := map[string]bool{}
	for _, n := range ns {
		if n.owner {
			if parts := strings.Split(n.payee, ", "); len(parts) >= 2 {
				day[areaKey(parts[1])] = true
			}
		}
	}
	return day
}

// wholeName: typed as a name, not a word on the way to one — two words or
// more, the last of four letters or more ("sunrise t" is on its way).
func wholeName(typed string) bool {
	w := nameWords(typed)
	return len(w) >= 2 && len(w[len(w)-1]) >= 4
}

// paidABusiness: a card payment, or a UPI payment to a shop's QR code.
func paidABusiness(staged StagedRow) bool {
	if strings.EqualFold(staged.Mode, "CARD") || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(staged.Narration)), "CARD/") {
		return true
	}
	for _, h := range handleTokens(staged.Narration) {
		if qrHandleRe.MatchString(h) {
			return true
		}
	}
	return qrHandleRe.MatchString(staged.Narration)
}

// tidyName capitalises each word, the way a name is written.
func tidyName(s string) string {
	w := strings.Fields(s)
	for i, x := range w {
		r := []rune(x)
		if len(r) > 0 {
			w[i] = strings.ToUpper(string(r[0])) + string(r[1:])
		}
	}
	return strings.Join(w, " ")
}

// withLookup adds what a lookup found to a picker's answer: at once when it
// is known, or — waiting — once the search is in. Without waiting, a search
// is started and the answer says so ("pending", or "checking" when the model
// placed the place already and nothing needs saying).
func (c *Classifier) withLookup(ctx context.Context, uuid, typed string, res PlacesResult, wait bool) PlacesResult {
	p := res.plan
	if p == nil {
		return res
	}
	facts, known := c.knownLookup(ctx, p.key)
	if !known {
		if !c.startLookup(p) {
			return res
		}
		if !wait {
			res.Lookup = "pending"
			if p.quiet {
				res.Lookup = "checking"
			}
			return res
		}
		if facts, known = c.awaitLookup(ctx, p.key); !known {
			return res
		}
	}
	expense, _ := listExpenseAccounts(ctx, c.db) // (a branch that is one of their payees is that payee)
	grounded := groundedSuggestions(typed, facts, p.where, p.owner, p.day, expense)
	res.Suggestions = mergeSuggestions(correctWhat(res.Suggestions, facts), grounded)
	res.Lookup = ""
	return res
}

// correctWhat: what the search found the place is replaces what the model
// guessed, on its name without an area ("a grocery store", for a famous
// idli room, was the fast model's). The row stays where it is; only its
// small print changes.
func correctWhat(list []PlaceSuggestion, facts PlaceFacts) []PlaceSuggestion {
	if !facts.Found || facts.What == "" {
		return list
	}
	out := append([]PlaceSuggestion(nil), list...)
	for i, s := range out {
		if !s.Existing && !strings.Contains(s.Name, ",") && placeKey(s.Name) == placeKey(facts.Name) {
			out[i].What = facts.What
		}
	}
	return out
}

// knownLookup is what is kept about a place: from memory, else the table.
// A search that failed a moment ago counts as known (nothing found), so it
// isn't asked again at once.
func (c *Classifier) knownLookup(ctx context.Context, key string) (PlaceFacts, bool) {
	c.lookups.mu.Lock()
	e, ok := c.lookups.mem[key]
	c.lookups.mu.Unlock()
	if ok && e.fresh() {
		return e.facts, true
	}
	var factsJS, at string
	if c.db.QueryRowContext(ctx, `SELECT facts_json, looked_up_at FROM place_lookups WHERE lookup_key = ?`, key).Scan(&factsJS, &at) != nil {
		return PlaceFacts{}, false
	}
	var f PlaceFacts
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || json.Unmarshal([]byte(factsJS), &f) != nil {
		return PlaceFacts{}, false
	}
	e = lookupEntry{facts: f, at: t}
	if !e.fresh() {
		return PlaceFacts{}, false
	}
	c.remember(key, e)
	return f, true
}

func (c *Classifier) remember(key string, e lookupEntry) {
	c.lookups.mu.Lock()
	defer c.lookups.mu.Unlock()
	if c.lookups.mem == nil {
		c.lookups.mem = map[string]lookupEntry{}
	}
	c.lookups.mem[key] = e
}

// startLookup searches for a place in the background — once, however many
// cards and keystrokes ask. False when there is no one to ask.
func (c *Classifier) startLookup(p *lookupPlan) bool {
	if c.lookup == nil {
		return false
	}
	c.lookups.mu.Lock()
	defer c.lookups.mu.Unlock()
	if time.Now().Before(c.lookups.offUntil) {
		return false
	}
	if _, running := c.lookups.inflight[p.key]; running {
		return true
	}
	if c.lookups.inflight == nil {
		c.lookups.inflight = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	c.lookups.inflight[p.key] = done
	go func() {
		defer func() {
			c.lookups.mu.Lock()
			delete(c.lookups.inflight, p.key)
			c.lookups.mu.Unlock()
			close(done)
		}()
		// not the request's context: the next keystroke cancels that, and
		// the answer is worth having for the next card anyway
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		facts, _, err := c.searchPlace(ctx, c.lookup, p.name, p.where)
		if err != nil {
			if errors.Is(err, llm.ErrNoWebSearch) {
				c.lookups.mu.Lock()
				c.lookups.offUntil = time.Now().Add(lookupRefusedFor)
				c.lookups.mu.Unlock()
			}
			if c.log != nil {
				c.log.Warn("places: lookup failed", "name", p.name, "err", err)
			}
			c.remember(p.key, lookupEntry{at: time.Now(), failed: true})
			return
		}
		c.keepLookup(p.key, p.name, facts)
	}()
	return true
}

// keepLookup remembers what a search found, in memory and in the table.
func (c *Classifier) keepLookup(key, name string, facts PlaceFacts) {
	c.remember(key, lookupEntry{facts: facts, at: time.Now()})
	js, _ := json.Marshal(facts)
	_, _ = c.db.Exec(`INSERT INTO place_lookups (lookup_key, name, facts_json, looked_up_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(lookup_key) DO UPDATE SET name = excluded.name, facts_json = excluded.facts_json, looked_up_at = excluded.looked_up_at`,
		key, name, string(js), time.Now().UTC().Format(time.RFC3339Nano))
}

// awaitLookup waits for a search under way, and says what it found.
func (c *Classifier) awaitLookup(ctx context.Context, key string) (PlaceFacts, bool) {
	c.lookups.mu.Lock()
	done, running := c.lookups.inflight[key]
	c.lookups.mu.Unlock()
	if running {
		select {
		case <-done:
		case <-ctx.Done():
			return PlaceFacts{}, false
		}
	}
	return c.knownLookup(ctx, key)
}

const lookupSystemPrompt = `You look up a place someone paid at, so their ledger can name the right branch. Search the web for the place named below in the whole city it says — not only near any neighbourhood named — and report only what the results show.

- "found": false when nothing by that name turns up there.
- "name": the place's usual name, as its signboard writes it — no legal suffix (Pvt Ltd, LLP), no other name in brackets, and no branch or area in it.
- "what": 2–6 words on what it is ("South Indian breakfast restaurant").
- "branches": each of its branches there, by the one neighbourhood locals name it by ("Lakeview" — not "Lakeview 3rd Block", "12th Cross" or "Market Street / Lakeview"), with its city. At most 6.
- "many": true when it is a chain with more branches there than you listed.

Reply with the JSON object only: {"found":true,"name":"…","what":"…","branches":[{"area":"…","city":"…"}],"many":false}`

// searchPlace asks a model to search the web for a place: the name and
// where, and nothing else about the payment.
func (c *Classifier) searchPlace(ctx context.Context, model *llm.Client, name string, where placeWhere) (PlaceFacts, int, error) {
	prompt := fmt.Sprintf("THE PLACE: %q\nWHERE: %s\n\nSearch, then return the JSON object.", name, where.text)
	ans, err := model.SearchJSON(ctx, lookupSystemPrompt, prompt, lookupSearches)
	if err != nil {
		return PlaceFacts{}, 0, err
	}
	facts, ok := parseFacts(ans.Text)
	if !ok {
		return PlaceFacts{}, ans.Searches, fmt.Errorf("places: unreadable lookup answer: %.120s", ans.Text)
	}
	return facts, ans.Searches, nil
}

// stripAlias drops another name in brackets after a place's own: "Sunrise
// Tiffins (ST)" is Sunrise Tiffins — and one row, not two.
func stripAlias(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		return name[:i]
	}
	return name
}

// parseFacts reads a lookup's answer, cleaned: names trimmed, each branch
// once, at most six.
func parseFacts(out string) (PlaceFacts, bool) {
	var f PlaceFacts
	if json.Unmarshal([]byte(extractJSONObject(out)), &f) != nil {
		return PlaceFacts{}, false
	}
	clean := func(s string) string { return strings.Trim(strings.Join(strings.Fields(s), " "), " .,;") }
	f.Name, f.What = clean(stripAlias(f.Name)), clean(f.What)
	if r := []rune(f.What); len(r) > 48 {
		f.What = string(r[:47]) + "…"
	}
	if !f.Found || f.Name == "" || len([]rune(f.Name)) > placesMaxTyped {
		return PlaceFacts{Found: false}, true
	}
	var branches []PlaceBranch
	seen := map[string]bool{}
	for _, b := range f.Branches {
		b.Area, _, _ = strings.Cut(b.Area, ",") // one name: "Place, Area" has room for no more
		b.Area, b.City = clean(b.Area), clean(b.City)
		if b.Area == "" || len([]rune(b.Area)) > 40 || seen[areaKey(b.Area)] {
			continue
		}
		seen[areaKey(b.Area)] = true
		branches = append(branches, b)
		if len(branches) == lookupBranches {
			break
		}
	}
	f.Branches = branches
	return f, true
}

// groundedSuggestions turns what a search found into names in the owner's
// style: "<Place>, <Area>" at home, "<Place>, <Area>, <City>" away, each area
// spelt the way the owner spells it; the areas they were in that day first,
// then the ones they are most often in. A chain with more branches than it
// lists offers only a branch in an area they were in that day — otherwise
// which one it was can't be told.
func groundedSuggestions(typed string, facts PlaceFacts, where placeWhere, owner map[string]int, day map[string]bool, expense []AccountRef) []PlaceSuggestion {
	if !facts.Found || facts.Name == "" || len(facts.Branches) == 0 {
		return nil
	}
	byName := map[string]string{}
	for _, a := range expense {
		byName[placeKey(a.Name)] = a.Name
	}
	type cand struct {
		s      PlaceSuggestion
		onDay  bool
		weight int
		order  int
	}
	var cands []cand
	seen := map[string]bool{}
	for i, br := range facts.Branches {
		area := ownerSpelling(neighbourhood(br.Area, owner), owner)
		name := facts.Name + ", " + area
		if !where.home {
			city := br.City
			if city == "" {
				city = where.city
			}
			if city != "" && areaKey(city) != areaKey(area) {
				name += ", " + city
			}
		}
		k := placeKey(name)
		if seen[k] {
			continue
		}
		seen[k] = true
		s := PlaceSuggestion{Name: name, What: facts.What}
		if exact, ok := byName[k]; ok {
			s.Name, s.Existing = exact, true
		}
		cands = append(cands, cand{s: s, onDay: day[areaKey(area)], weight: owner[area], order: i})
	}
	if facts.Many {
		kept := cands[:0]
		for _, x := range cands {
			if x.onDay {
				kept = append(kept, x)
			}
		}
		cands = kept
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.onDay != b.onDay {
			return a.onDay
		}
		if a.weight != b.weight {
			return a.weight > b.weight
		}
		return a.order < b.order
	})
	var out []PlaceSuggestion
	for _, x := range cands {
		if len(out) == groundedMax {
			break
		}
		out = append(out, x.s)
	}
	return fitting(out, typed)
}

// ownerAreas: how often each area is named by the owner's payees ("Place,
// Area[, City]"), weighted by how much each payee is used.
func ownerAreas(expense []AccountRef, usage map[int64]int) map[string]int {
	areas := map[string]int{}
	for _, a := range expense {
		if parts := strings.Split(a.Name, ", "); len(parts) >= 2 {
			areas[strings.TrimSpace(parts[1])] += usage[a.ID] + 1
		}
	}
	return areas
}

// areaKey compares areas spacing and punctuation aside: "J.P. Nagar",
// "JP Nagar" and "Jp nagar" are one area.
func areaKey(s string) string { return strings.Join(nameWords(s), "") }

// ownerSpelling is an area as the owner writes it: theirs when it is the
// same area — spacing and punctuation aside, or a single slip in a longer
// name ("Harbourtown" / "Harbortown") — never a different area that merely
// looks alike ("Eastgate" is not "Westgate").
func ownerSpelling(area string, owner map[string]int) string {
	k := areaKey(area)
	best, bestN := "", -1
	for a, n := range owner {
		ak := areaKey(a)
		same := ak == k
		if !same && len(k) >= 6 && len(ak) >= 6 && k[0] == ak[0] {
			same = editDistance(k, ak) <= 1
		}
		if same && (n > bestN || (n == bestN && a < best)) {
			best, bestN = a, n
		}
	}
	if best != "" {
		return best
	}
	return area
}

// subdivisionWords are the parts of an address below a neighbourhood: the
// "5th Block" of "Lakeview 5th Block", the "2nd Stage", the "7th Phase".
var subdivisionWords = map[string]bool{"block": true, "stage": true, "phase": true, "sector": true, "cross": true, "main": true,
	"east": true, "west": true, "north": true, "south": true}

// neighbourhood is the one name a branch's area goes by, the way the owner
// names areas: of "Market Street / Lakeview" the part that is one of their
// areas (else the first); of "Lakeview 5th Block", "Lakeview".
func neighbourhood(area string, owner map[string]int) string {
	parts := strings.Split(area, "/")
	pick := strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if _, ok := owner[ownerSpelling(p, owner)]; ok {
				pick = p
				break
			}
		}
	}
	// a trailing run of ordinals and subdivision words, with at least one
	// such word ("5th Block", "East"; a bare "II" may be part of the name)
	w := strings.Fields(pick)
	end, named := len(w), false
	for end > 0 { // (all subdivisions — "Phase 2" — is kept whole below)
		t := strings.ToLower(strings.Trim(w[end-1], ".,"))
		if subdivisionWords[t] {
			named = true
		} else if !isOrdinal(t) {
			break
		}
		end--
	}
	if !named || end == 0 {
		return pick
	}
	return strings.Join(w[:end], " ")
}

// isOrdinal: "5", "5th", "2nd", "ii".
func isOrdinal(t string) bool {
	t = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(t, "st"), "nd"), "rd"), "th")
	if t == "" {
		return false
	}
	if strings.Trim(t, "0123456789") == "" {
		return true
	}
	return strings.Trim(t, "ivx") == "" && len(t) <= 4
}

// mergeSuggestions keeps the first answer as it is and adds the others'
// names it didn't have, below it.
func mergeSuggestions(first, more []PlaceSuggestion) []PlaceSuggestion {
	out := append([]PlaceSuggestion(nil), first...)
	seen := map[string]bool{}
	for _, s := range first {
		seen[placeKey(s.Name)] = true
	}
	for _, s := range more {
		if !seen[placeKey(s.Name)] {
			seen[placeKey(s.Name)] = true
			out = append(out, s)
		}
	}
	return out
}

// rankedCounts: the keys, most counted first (ties alphabetically).
func rankedCounts(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
	return keys
}

func topKey(m map[string]int) string {
	if r := rankedCounts(m); len(r) > 0 {
		return r[0]
	}
	return ""
}

// LookupRun is one model's lookup of one place, for comparing models.
type LookupRun struct {
	Model      string            `json:"model"`
	Name       string            `json:"name"`
	Where      string            `json:"where"`
	DurationMS int64             `json:"durationMs"`
	Searches   int               `json:"searches"`
	Facts      PlaceFacts        `json:"facts"`
	Offered    []PlaceSuggestion `json:"offered,omitempty"` // as the card's picker would show them
	Error      string            `json:"error,omitempty"`
}

// CompareLookups searches for each name with each model — fresh, past what
// is kept — and says how long it took, what it found, and what the card's
// picker would offer from it. At most 6 searches, all at once.
func (c *Classifier) CompareLookups(ctx context.Context, uuid string, names, models []string) ([]LookupRun, error) {
	if c.llm == nil {
		return nil, errors.New("no model configured")
	}
	if len(names) == 0 || len(models) == 0 || len(names)*len(models) > 6 {
		return nil, errors.New("1 to 6 runs: names × models")
	}
	rows, err := c.fetchStagedForClassify(ctx, `fold_uuid = ?`, uuid)
	if err != nil || len(rows) != 1 {
		return nil, errors.New("no such card")
	}
	staged := rows[0]
	expense, _ := listExpenseAccounts(ctx, c.db)
	ev := c.placesEvidenceFor(ctx, staged, expense, "destination")
	where := c.whereFor(ctx, staged, ev)
	owner, day := ownerAreas(ev.accounts, ev.usage), dayAreas(ev.neighbours)
	var runs []LookupRun
	for _, m := range models {
		for _, n := range names {
			runs = append(runs, LookupRun{Model: m, Name: n, Where: where.text})
		}
	}
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(r *LookupRun) {
			defer wg.Done()
			start := time.Now()
			facts, searches, err := c.searchPlace(ctx, c.compareClient(r.Model), r.Name, where)
			r.DurationMS, r.Searches, r.Facts = time.Since(start).Milliseconds(), searches, facts
			if err != nil {
				r.Error = err.Error()
				return
			}
			r.Offered = groundedSuggestions(r.Name, facts, where, owner, day, expense)
		}(&runs[i])
	}
	wg.Wait()
	return runs, nil
}
