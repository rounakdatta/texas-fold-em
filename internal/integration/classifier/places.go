package classifier

// places.go — naming a new payee as it is typed.
//
// A card whose payee fold couldn't name leaves a person typing one: "sn
// ref…". The payee picker matches their existing payees letter by letter,
// and for a place they have never paid that is nothing at all — no hint of
// how they write it ("Place, Area" at home, "Place, Area, City" abroad),
// which branch it was, or whether the name they half-remember exists.
//
// So as they type, a fast model offers what the place probably is: the name
// completed and put in their style, placed where the evidence says they
// were — their other payments that day, a trip in progress, the areas they
// name most — with a few words on what it is. Offered, never assumed: the
// picker shows these apart from the real payees, below them, so nothing
// already on screen moves when they arrive. A name that is one of the
// owner's payees comes back as that payee.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// PlaceSuggestion is one name offered for who was paid.
type PlaceSuggestion struct {
	Name     string `json:"name"`
	What     string `json:"what,omitempty"`     // "South Indian tiffin café"
	Existing bool   `json:"existing,omitempty"` // one of the owner's payees already
}

// PlacesResult is what the picker gets back.
type PlacesResult struct {
	Suggestions []PlaceSuggestion `json:"suggestions"`
	// Resting: the model is resting (a limit, a refused key, failures);
	// the picker shows only the real payees meanwhile.
	Resting bool `json:"resting,omitempty"`
}

// Typed text shorter than this names nothing yet; longer is a sentence.
const (
	placesMinTyped = 3
	placesMaxTyped = 80
)

// ErrNoPlaces: nothing to suggest for (no model, not money out, too short).
var ErrNoPlaces = errors.New("no place suggestions for this")

// SetFastLLM attaches the model used while someone types — quicker than the
// one that classifies. Without it, the classifying model answers.
func (c *Classifier) SetFastLLM(client *llm.Client) { c.fast = client }

func (c *Classifier) placesModel() *llm.Client {
	if c.fast != nil {
		return c.fast
	}
	return c.llm
}

// SuggestPlaces offers up to three names for who was paid on a card, from
// what is typed and what is known about the payment. Answers are cached per
// card and text for a quarter of an hour; a request abandoned mid-way (the
// person typed on) is cancelled with its context.
func (c *Classifier) SuggestPlaces(ctx context.Context, uuid, typed string) (PlacesResult, error) {
	model := c.placesModel()
	typed = strings.Join(strings.Fields(typed), " ")
	if model == nil || len([]rune(typed)) < placesMinTyped {
		return PlacesResult{}, ErrNoPlaces
	}
	if r := []rune(typed); len(r) > placesMaxTyped {
		typed = string(r[:placesMaxTyped])
	}
	key := uuid + "|" + strings.ToLower(typed)
	if res, ok := c.places.get(key); ok {
		return res, nil
	}
	if !model.Health().Available {
		return PlacesResult{Resting: true, Suggestions: []PlaceSuggestion{}}, nil
	}
	rows, err := c.fetchStagedForClassify(ctx, `fold_uuid = ?`, uuid)
	if err != nil || len(rows) != 1 {
		return PlacesResult{}, ErrNoPlaces
	}
	staged := rows[0]
	if staged.Type != "OUTGOING" {
		return PlacesResult{}, ErrNoPlaces
	}
	expense, _ := listExpenseAccounts(ctx, c.db)
	prompt := c.placesPrompt(ctx, staged, typed, expense)

	// At most a few at once: someone typing fast fires several, and the
	// browser cancels the stale ones.
	select {
	case c.placesSlots <- struct{}{}:
		defer func() { <-c.placesSlots }()
	case <-ctx.Done():
		return PlacesResult{}, ctx.Err()
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := model.GenerateJSON(callCtx, placesSystemPrompt, prompt)
	if err != nil {
		if errors.Is(err, llm.ErrUnavailable) {
			return PlacesResult{Resting: true, Suggestions: []PlaceSuggestion{}}, nil
		}
		return PlacesResult{}, err
	}
	res := PlacesResult{Suggestions: parsePlaces(out, expense)}
	c.places.put(key, res)
	return res, nil
}

const placesSystemPrompt = `You help someone name who they paid, in their personal-finance ledger, while they type the name. From what they have typed and what is known about the payment, suggest up to 3 names for the place or business they mean.

- Complete and correct what they typed: the business's usual name, spelled and capitalised properly, in their style — "<Place>, <Area>" in their home city (the one YOUR AREAS are in), "<Place>, <Area>, <City>" anywhere else (see YOUR STYLE). No legal suffixes (Pvt Ltd, LLP), store codes, or payment-processor prefixes (TST*, SQ *, UEP*, PAYU*, …).
- Add an area or a city only when something supports it: the typed text, the payment itself, where they were that day (YOUR OTHER PAYMENTS AROUND IT), a trip in progress, or a business you know has one location. Never invent a branch.
- A payee they already have that is this place (YOUR PAYEES THAT SHARE A WORD) is the best suggestion: repeat its exact name.
- Only real places: a business you know, or the typed name made proper. Fewer is better than wrong — when the typed text is all there is to go on, one suggestion, it made proper, is right.
- "what" is 2–6 words on what the place is, from what you know or plainly from its name ("South Indian tiffin café", "chocolate shop, airport"); "" when you can't tell.
- "confidence" is 0..1 that this is the place they mean.

Reply with the JSON object only: {"suggestions":[{"name":"…","what":"…","confidence":0.8}]}`

// qrHandleRe: a UPI QR code's handle names the payment app, not the shop.
var qrHandleRe = regexp.MustCompile(`(?i)(bharatpe|paytmqr|vyapar|@ptys|@fbpe|@ptybl|^q\d{6,}@ybl|/q\d{6,}@ybl)`)

func (c *Classifier) placesPrompt(ctx context.Context, staged StagedRow, typed string, expense []AccountRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TYPED: %q\n\n", typed)

	b.WriteString("THE PAYMENT: money out")
	if staged.AmountPaise > 0 {
		b.WriteString(", " + rupees(staged.AmountPaise))
	}
	if t, ok := parseTxnTime(staged.TxnTimestamp); ok {
		ist := t.In(istLocation())
		b.WriteString(" on " + strings.Replace(ist.Format("Mon 2 Jan 2006 at 3:04 pm"), " Sep ", " Sept ", 1) + " IST")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  the bank's words: %s\n", shortNarration(staged.Narration))
	for _, h := range handleTokens(staged.Narration) {
		if qrHandleRe.MatchString(h) {
			b.WriteString("  (a QR code payment: the handle names the payment app, not the shop — the shop's name isn't in it)\n")
			break
		}
	}
	var p struct {
		Merchant struct {
			Name string `json:"name"`
		} `json:"merchant"`
		Notes          string  `json:"notes"`
		SourceAmount   float64 `json:"source_amount"`
		SourceCurrency string  `json:"source_currency"`
	}
	_ = json.Unmarshal([]byte(staged.RawPayload), &p)
	if m := strings.TrimSpace(p.Merchant.Name); m != "" {
		fmt.Fprintf(&b, "  fold.money's guess at the merchant (often wrong): %q\n", m)
	}
	if note := upiNote(staged.Narration); note != "" {
		fmt.Fprintf(&b, "  the note on the payment (their own words): %q\n", note)
	}
	if n := strings.TrimSpace(p.Notes); n != "" && n != "{}" {
		fmt.Fprintf(&b, "  their note: %q\n", shortNarration(n))
	}
	if cur := foreignCurrencyOf(staged.RawPayload); cur != "" {
		fmt.Fprintf(&b, "  charged in %s", cur)
		if p.SourceAmount > 0 {
			fmt.Fprintf(&b, " (%s %.2f)", cur, p.SourceAmount)
		}
		b.WriteString(" — a charge abroad\n")
	}

	// Where they were: the payees of their other payments that day.
	var around []string
	for _, n := range c.neighbours(ctx, staged, exclusion{}) {
		if !n.owner || strings.TrimSpace(n.payee) == "" {
			continue
		}
		line := "  " + n.at.In(istLocation()).Format("Mon 2 Jan 3:04 pm") + "  " + n.payee
		if n.title != "" {
			line += fmt.Sprintf("  (%q)", n.title)
		}
		around = append(around, line)
		if len(around) >= 8 {
			break
		}
	}
	if len(around) > 0 {
		b.WriteString("\nYOUR OTHER PAYMENTS AROUND IT (as you named them):\n" + strings.Join(around, "\n") + "\n")
	}
	if trip := c.tripContext(ctx, staged, exclusion{}); trip != "" {
		b.WriteString("\nA TRIP IN PROGRESS: " + trip + "\n")
	}

	usage := accountUsage(ctx, c.db, "destination", time.Now().AddDate(-2, 0, 0))
	words := map[string]bool{}
	for _, w := range nameWords(typed) {
		if len(w) >= 3 {
			words[w] = true
		}
	}
	var related []string
	for _, a := range rankedAccounts(expense, usage, words, 8) {
		if sharesWord(a.Name, words) {
			related = append(related, fmt.Sprintf("%q", a.Name))
		}
	}
	if len(related) > 0 {
		b.WriteString("\nYOUR PAYEES THAT SHARE A WORD: " + strings.Join(related, ", ") + "\n")
	}

	areas, cities := map[string]int{}, map[string]int{}
	type used struct {
		name string
		n    int
	}
	var styled []used
	for _, a := range expense {
		parts := strings.Split(a.Name, ", ")
		if len(parts) < 2 {
			continue
		}
		n := usage[a.ID] + 1
		areas[strings.TrimSpace(parts[1])] += n
		if len(parts) >= 3 {
			cities[strings.TrimSpace(parts[len(parts)-1])] += n
		}
		styled = append(styled, used{a.Name, n})
	}
	if len(areas) > 0 {
		b.WriteString("\nYOUR AREAS (as your payees name them, most used first): " + topCounts(areas, 14) + "\n")
	}
	if len(cities) > 0 {
		b.WriteString("Cities beyond home: " + topCounts(cities, 6) + "\n")
	}
	sort.Slice(styled, func(i, j int) bool {
		return styled[i].n > styled[j].n || (styled[i].n == styled[j].n && styled[i].name < styled[j].name)
	})
	var style []string
	for i, s := range styled {
		if i >= 6 {
			break
		}
		style = append(style, fmt.Sprintf("%q", s.name))
	}
	for _, s := range styled {
		if len(style) >= 8 {
			break
		}
		if strings.Count(s.name, ", ") >= 2 && !containsString(style, fmt.Sprintf("%q", s.name)) {
			style = append(style, fmt.Sprintf("%q", s.name))
		}
	}
	if len(style) > 0 {
		b.WriteString("YOUR STYLE: " + strings.Join(style, " · ") + "\n")
	}
	b.WriteString("\nReturn the JSON object now.")
	return b.String()
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// parsePlaces reads the model's answer: at most three names, cleaned, each
// once, the unlikely dropped, and one that is already a payee returned as
// exactly that payee.
func parsePlaces(out string, expense []AccountRef) []PlaceSuggestion {
	var r struct {
		Suggestions []struct {
			Name       string  `json:"name"`
			What       string  `json:"what"`
			Confidence float64 `json:"confidence"`
		} `json:"suggestions"`
	}
	if json.Unmarshal([]byte(extractJSONObject(out)), &r) != nil {
		return []PlaceSuggestion{}
	}
	byName := map[string]string{}
	for _, a := range expense {
		byName[placeKey(a.Name)] = a.Name
	}
	res := []PlaceSuggestion{}
	seen := map[string]bool{}
	for _, s := range r.Suggestions {
		name := strings.Trim(strings.Join(strings.Fields(s.Name), " "), " .,;")
		if name == "" || len([]rune(name)) > placesMaxTyped || s.Confidence < 0.25 {
			continue
		}
		k := placeKey(name)
		if seen[k] {
			continue
		}
		seen[k] = true
		ps := PlaceSuggestion{Name: name, What: strings.TrimRight(strings.TrimSpace(s.What), ".")}
		if exact, ok := byName[k]; ok {
			ps.Name, ps.Existing = exact, true
		}
		if r := []rune(ps.What); len(r) > 48 {
			ps.What = string(r[:47]) + "…"
		}
		res = append(res, ps)
		if len(res) == 3 {
			break
		}
	}
	return res
}

// placeKey compares names the way a person would: case, spacing and
// punctuation aside.
func placeKey(s string) string { return strings.Join(nameWords(s), " ") }

// placesCache remembers answers per card and typed text.
type placesCache struct {
	mu      sync.Mutex
	entries map[string]placesEntry
	order   []string
}

type placesEntry struct {
	at  time.Time
	res PlacesResult
}

const (
	placesTTL     = 15 * time.Minute
	placesEntries = 400
)

func (pc *placesCache) get(k string) (PlacesResult, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	e, ok := pc.entries[k]
	if !ok || time.Since(e.at) > placesTTL {
		return PlacesResult{}, false
	}
	return e.res, true
}

func (pc *placesCache) put(k string, res PlacesResult) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.entries == nil {
		pc.entries = map[string]placesEntry{}
	}
	if _, ok := pc.entries[k]; !ok {
		pc.order = append(pc.order, k)
	}
	pc.entries[k] = placesEntry{at: time.Now(), res: res}
	for len(pc.order) > placesEntries {
		delete(pc.entries, pc.order[0])
		pc.order = pc.order[1:]
	}
}
