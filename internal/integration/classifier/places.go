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
	// Lookup: "pending" while fold looks the place up on the web (the picker
	// says so, then asks SuggestPlacesLooked for what it found); "checking"
	// when it looks up a place the model placed already, unannounced.
	Lookup string `json:"lookup,omitempty"`

	raw  []string    // what the model offered, before the fit guard
	plan *lookupPlan // the lookup this answer calls for, if any
}

// Typed text shorter than this names nothing yet; longer is a sentence.
const (
	placesMinTyped = 3
	placesMaxTyped = 80
)

// ErrNoPlaces: nothing to suggest for (no model, too short, a transfer).
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
	res, err := c.suggestPlacesWith(ctx, c.placesModel(), uuid, typed, true)
	if err != nil {
		return res, err
	}
	return c.withLookup(ctx, uuid, cleanTyped(typed), res, false), nil
}

// SuggestPlacesLooked is SuggestPlaces once the place's web lookup is in: it
// waits for one under way (at most lookupTimeout, or until the picker gives
// up) and adds the branches it found.
func (c *Classifier) SuggestPlacesLooked(ctx context.Context, uuid, typed string) (PlacesResult, error) {
	res, err := c.suggestPlacesWith(ctx, c.placesModel(), uuid, typed, true)
	if err != nil {
		return res, err
	}
	return c.withLookup(ctx, uuid, cleanTyped(typed), res, true), nil
}

// cleanTyped: the typed text as compared — single spaces, at most
// placesMaxTyped letters.
func cleanTyped(typed string) string {
	typed = strings.Join(strings.Fields(typed), " ")
	if r := []rune(typed); len(r) > placesMaxTyped {
		typed = string(r[:placesMaxTyped])
	}
	return typed
}

func (c *Classifier) suggestPlacesWith(ctx context.Context, model *llm.Client, uuid, typed string, cached bool) (PlacesResult, error) {
	typed = cleanTyped(typed)
	if model == nil || len([]rune(typed)) < placesMinTyped {
		return PlacesResult{}, ErrNoPlaces
	}
	key := uuid + "|" + strings.ToLower(typed)
	if res, ok := c.places.get(key); ok && cached {
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
	// Money out names who was paid — a place, mostly; money in names who
	// paid — a person or a business, whose name the bank line usually has.
	var inventory []AccountRef
	var ev placesEvidence
	var system, prompt string
	switch staged.Type {
	case "OUTGOING":
		inventory, _ = listExpenseAccounts(ctx, c.db)
		ev = c.placesEvidenceFor(ctx, staged, inventory, "destination")
		system, prompt = placesSystemPrompt, c.placesPrompt(ctx, staged, typed, ev)
	case "INCOMING":
		inventory, _ = listRevenueAccounts(ctx, c.db)
		ev = c.placesEvidenceFor(ctx, staged, inventory, "source")
		system, prompt = payersSystemPrompt, c.payersPrompt(ctx, staged, typed, ev)
	default:
		return PlacesResult{}, ErrNoPlaces
	}

	// At most a few at once: someone typing fast fires several, and the
	// browser cancels the stale ones. (A comparison runs outside the limit.)
	if cached {
		select {
		case c.placesSlots <- struct{}{}:
			defer func() { <-c.placesSlots }()
		case <-ctx.Done():
			return PlacesResult{}, ctx.Err()
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := model.GenerateJSON(callCtx, system, prompt)
	if err != nil {
		if errors.Is(err, llm.ErrUnavailable) {
			return PlacesResult{Resting: true, Suggestions: []PlaceSuggestion{}}, nil
		}
		return PlacesResult{}, err
	}
	parsed := parsePlaces(out, inventory)
	res := PlacesResult{Suggestions: fitting(parsed, typed)}
	if cached {
		res.plan = c.planLookup(ctx, staged, typed, res.Suggestions, ev)
		c.places.put(key, res)
	} else {
		for _, s := range parsed {
			res.raw = append(res.raw, s.Name)
		}
	}
	return res, nil
}

// placesEvidence is what one card's picker question draws on — the payees,
// how much each is used, the owner's payments around it — gathered once per
// question: the prompt, the lookup's plan and what the lookup offers all
// read it, and the payments around a card are dozens of queries (three
// gatherings made the picker's answer seconds slower).
type placesEvidence struct {
	accounts   []AccountRef // expense accounts for money out, revenue for money in
	usage      map[int64]int
	neighbours []neighbour
}

func (c *Classifier) placesEvidenceFor(ctx context.Context, staged StagedRow, accounts []AccountRef, side string) placesEvidence {
	return placesEvidence{accounts: accounts, usage: c.usageOf(ctx, side), neighbours: c.neighbours(ctx, staged, exclusion{})}
}

// usageOf is accountUsage over the last two years, kept two minutes: every
// card's question counts the same ledger.
func (c *Classifier) usageOf(ctx context.Context, side string) map[int64]int {
	c.usage.mu.Lock()
	e, ok := c.usage.bySide[side]
	c.usage.mu.Unlock()
	if ok && time.Since(e.at) < 2*time.Minute {
		return e.usage
	}
	u := accountUsage(ctx, c.db, side, time.Now().AddDate(-2, 0, 0))
	c.usage.mu.Lock()
	if c.usage.bySide == nil {
		c.usage.bySide = map[string]usageEntry{}
	}
	c.usage.bySide[side] = usageEntry{usage: u, at: time.Now()}
	c.usage.mu.Unlock()
	return u
}

type usageEntry struct {
	usage map[int64]int
	at    time.Time
}

// PlacesCase is one typed name on one card, for comparing models.
type PlacesCase struct {
	UUID  string `json:"uuid"`
	Typed string `json:"q"`
}

// PlacesRun is one model's answer to one case.
type PlacesRun struct {
	Model       string            `json:"model"`
	UUID        string            `json:"uuid"`
	Typed       string            `json:"q"`
	DurationMS  int64             `json:"durationMs"`
	Suggestions []PlaceSuggestion `json:"suggestions"`
	// Raw: what the model offered before the fit guard (a comparison only).
	Raw []string `json:"raw,omitempty"`
	// What the model has refused so far: having its thinking turned off
	// (so it thinks, a little), and a temperature.
	Thinks        bool   `json:"thinks,omitempty"`
	NoTemperature bool   `json:"noTemperature,omitempty"`
	Error         string `json:"error,omitempty"`
}

// ComparePlaces asks each model the picker's question for each case — fresh,
// bypassing the cache — and says how long each took and what it offered:
// how the fast model is chosen on evidence. At most 12 calls, all at once.
func (c *Classifier) ComparePlaces(ctx context.Context, cases []PlacesCase, models []string) ([]PlacesRun, error) {
	if c.llm == nil {
		return nil, errors.New("no model configured")
	}
	if len(cases) == 0 || len(models) == 0 || len(cases)*len(models) > 12 {
		return nil, errors.New("1 to 12 runs: cases × models")
	}
	runs := make([]PlacesRun, 0, len(cases)*len(models))
	for _, m := range models {
		for _, cs := range cases {
			runs = append(runs, PlacesRun{Model: m, UUID: cs.UUID, Typed: cs.Typed})
		}
	}
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(r *PlacesRun) {
			defer wg.Done()
			cl := c.compareClient(r.Model)
			start := time.Now()
			res, err := c.suggestPlacesWith(ctx, cl, r.UUID, r.Typed, false)
			r.DurationMS = time.Since(start).Milliseconds()
			r.Suggestions, r.Raw = res.Suggestions, res.raw
			r.NoTemperature, r.Thinks = cl.Adapted()
			if err != nil {
				r.Error = err.Error()
			}
		}(&runs[i])
	}
	wg.Wait()
	return runs, nil
}

// compareClient is the comparisons' client for a model. It is kept, so what
// the model refuses — a temperature, having its thinking turned off — is
// learnt once: a fresh client per comparison paid a refused request each
// time, and timed it as the model's. A model that must think thinks as
// little as it can, as the picker's does.
func (c *Classifier) compareClient(model string) *llm.Client {
	c.compare.mu.Lock()
	defer c.compare.mu.Unlock()
	if cl, ok := c.compare.clients[model]; ok {
		return cl
	}
	base := c.llm
	for _, x := range []*llm.Client{c.fast, c.lookup} {
		if x != nil && x.Model() == model {
			base = x // what it has learnt about this model already
		}
	}
	cl := base.WithOverrides(model, "none")
	cl.SetMaxTokens(600)
	cl.SetTimeout(45 * time.Second)
	cl.SetThinkingFallback("low")
	if c.compare.clients == nil {
		c.compare.clients = map[string]*llm.Client{}
	}
	c.compare.clients[model] = cl
	return cl
}

const placesSystemPrompt = `You help someone name who they paid, in their personal-finance ledger, while they type the name. From what they have typed and what is known about the payment, suggest up to 3 names for the place or business they mean. The typed text itself is already offered to them as it is; offer only what improves on it.

- Every suggestion is what they typed, completed or corrected — its words start the way the typed words do ("blue ta" can become "Blue Tokai", never "Bluestone"; "starbuks" can become "Starbucks"). Nothing else.
- Complete a half-typed word only into a real place you know. When you don't know one, don't guess the rest of the word: tidy what they typed (capitals, spacing, an area it names) or suggest nothing.
- Their style: "<Place>, <Area>" in their home city (the one YOUR AREAS are in), "<Place>, <Area>, <City>" anywhere else (see YOUR STYLE). The business's usual name — no legal suffixes (Pvt Ltd, LLP), store codes or payment-processor prefixes (TST*, SQ *, UEP*, PAYU*…).
- The area: a place you know has one location takes its real area, wherever they were that day. A chain or an unknown place takes the area where they were (YOUR OTHER PAYMENTS AROUND IT, a trip in progress) only when that is where this payment was plainly made; otherwise leave the area out. YOUR AREAS shows how they write areas — never evidence of where this payment was. Never invent a branch.
- A payee they already have that is this place (YOUR PAYEES THAT SHARE A WORD) comes first, in its exact name — but only when it is the name they typed: a different business in the same area, or with a similar word, is a different place. Then tidy what they typed instead ("sunrise tiffins lakeview" → "Sunrise Tiffins, Lakeview").
- "what" is 2–6 words on what the place is, only from what you know for certain or what its name plainly says ("egg tart bakery", "kaya toast café"); "" when unsure. A wrong "what" misleads more than none.
- "confidence" is 0..1 that this is the place they mean. Fewer and right beats more and wrong.

Reply with the JSON object only: {"suggestions":[{"name":"…","what":"…","confidence":0.8}]}`

const payersSystemPrompt = `You help someone name who paid them, in their personal-finance ledger, while they type the name. From what they have typed and what is known about the payment, suggest up to 3 names for the person or business that paid. The typed text itself is already offered to them as it is; offer only what improves on it.

- Every suggestion is what they typed, completed or corrected — its words start the way the typed words do. Nothing else.
- The bank line usually names who sent the money (a UPI, NEFT or IMPS credit carries the sender's name, often cut short). When what they typed starts that name, offer it the way a person or a business is written — "ASHA MENON" → "Asha Menon", "ACME TECHNOLOGIES PVT LTD" → "Acme Technologies" — not as the bank abbreviates it.
- A payer they already have (YOUR PAYERS THAT SHARE A WORD) comes first, in its exact name — but only when it is the one they typed: a different person with the same first name is a different payer.
- Their style (YOUR STYLE): people by their name, businesses by their usual name; no legal suffixes (Pvt Ltd, LLP), reference numbers, handles or account numbers.
- Never invent a surname or a company the evidence doesn't give. When the typed text is all there is, tidy it (capitals, spacing) or suggest nothing.
- "what" is 2–5 words on who it is, only when certain ("stockbroker", "bank interest", "employer's payroll"); "" otherwise.
- "confidence" is 0..1 that this is who they mean. Fewer and right beats more and wrong.

Reply with the JSON object only: {"suggestions":[{"name":"…","what":"…","confidence":0.8}]}`

// upiSenderRe: the sender's name on an HDFC-style UPI credit, "UPI-<name>-…".
var upiSenderRe = regexp.MustCompile(`(?i)^UPI-([A-Z][A-Z .]{1,60}?)-`)

// payersPrompt is placesPrompt's counterpart for money in: the bank line and
// the name on it, the owner's payers and how they write them.
func (c *Classifier) payersPrompt(ctx context.Context, staged StagedRow, typed string, ev placesEvidence) string {
	revenue := ev.accounts
	var b strings.Builder
	fmt.Fprintf(&b, "TYPED: %q\n\n", typed)
	b.WriteString("THE PAYMENT: money in")
	if staged.AmountPaise > 0 {
		b.WriteString(", " + rupees(staged.AmountPaise))
	}
	if t, ok := parseTxnTime(staged.TxnTimestamp); ok {
		ist := t.In(istLocation())
		b.WriteString(" on " + strings.Replace(ist.Format("Mon 2 Jan 2006 at 3:04 pm"), " Sep ", " Sept ", 1) + " IST")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  the bank's words: %s\n", shortNarration(staged.Narration))
	if m := upiSenderRe.FindStringSubmatch(strings.TrimSpace(staged.Narration)); m != nil {
		fmt.Fprintf(&b, "  the name on the bank line: %q\n", strings.TrimSpace(m[1]))
	}
	var p struct {
		Merchant struct {
			Name string `json:"name"`
		} `json:"merchant"`
		Notes string `json:"notes"`
	}
	_ = json.Unmarshal([]byte(staged.RawPayload), &p)
	if m := strings.TrimSpace(p.Merchant.Name); m != "" {
		fmt.Fprintf(&b, "  fold.money's guess at who it is (often wrong): %q\n", m)
	}
	if note := upiNote(staged.Narration); note != "" {
		fmt.Fprintf(&b, "  the note on the payment (the sender's words): %q\n", note)
	}
	if n := strings.TrimSpace(p.Notes); n != "" && n != "{}" {
		fmt.Fprintf(&b, "  their note: %q\n", shortNarration(n))
	}

	// Their titles around it can name the people they were with ("Dinner
	// with Asha"), who might be paying their share back.
	var around []string
	for _, n := range ev.neighbours {
		if !n.owner || strings.TrimSpace(n.title) == "" {
			continue
		}
		line := "  " + n.at.In(istLocation()).Format("Mon 2 Jan 3:04 pm") + fmt.Sprintf("  %q", n.title)
		if n.payee != "" {
			line += " → " + n.payee
		}
		around = append(around, line)
		if len(around) >= 8 {
			break
		}
	}
	if len(around) > 0 {
		b.WriteString("\nYOUR OTHER PAYMENTS AROUND IT (as you named them):\n" + strings.Join(around, "\n") + "\n")
	}

	usage := ev.usage
	words := map[string]bool{}
	for _, w := range nameWords(typed) {
		if len(w) >= 3 {
			words[w] = true
		}
	}
	var related []string
	for _, a := range rankedAccounts(revenue, usage, words, 8) {
		if sharesWord(a.Name, words) {
			related = append(related, fmt.Sprintf("%q", a.Name))
		}
	}
	if len(related) > 0 {
		b.WriteString("\nYOUR PAYERS THAT SHARE A WORD: " + strings.Join(related, ", ") + "\n")
	}
	style := make([]AccountRef, len(revenue))
	copy(style, revenue)
	sort.SliceStable(style, func(i, j int) bool { return usage[style[i].ID] > usage[style[j].ID] })
	var names []string
	for i, a := range style {
		if i >= 8 {
			break
		}
		names = append(names, fmt.Sprintf("%q", a.Name))
	}
	if len(names) > 0 {
		b.WriteString("YOUR STYLE (your payers, most used first): " + strings.Join(names, " · ") + "\n")
	}
	b.WriteString("\nReturn the JSON object now.")
	return b.String()
}

// qrHandleRe: a UPI QR code's handle names the payment app, not the shop.
var qrHandleRe = regexp.MustCompile(`(?i)(bharatpe|paytmqr|vyapar|@ptys|@fbpe|@ptybl|^q\d{6,}@ybl|/q\d{6,}@ybl)`)

func (c *Classifier) placesPrompt(ctx context.Context, staged StagedRow, typed string, ev placesEvidence) string {
	expense := ev.accounts
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
	for _, n := range ev.neighbours {
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

	usage := ev.usage
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

// fitting keeps the suggestions that are what was typed, completed or
// corrected: every typed word starts one of the name's words, allowing a
// slip or two in a longer word ("starbuks" for "Starbucks"). A model that
// answers "blue ta" with "Shankar's" has stopped completing and started
// inventing — that answer never reaches the picker.
//
// The first typed word names the place itself, so it must start a word in
// the name's first part — "maxwell" can become "Maxwell Food Centre", not
// another place "…, Maxwell". Initials count run together, the way people
// type them: "jp" fits "J.P. Nagar".
func fitting(list []PlaceSuggestion, typed string) []PlaceSuggestion {
	out := []PlaceSuggestion{}
	tw := nameWords(typed)
	for _, s := range list {
		place, _, _ := strings.Cut(s.Name, ",")
		ok := len(tw) > 0 && startsSome(tw[0], wordStarts(place))
		for _, t := range tw[min(1, len(tw)):] {
			if !ok {
				break
			}
			ok = startsSome(t, wordStarts(s.Name))
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// wordStarts are a name's words, and each run of them from there on written
// together ("J.P. Nagar" → j, p, nagar, jp, jpnagar, pnagar), so typed
// initials or a name typed without its spaces still start one.
func wordStarts(name string) []string {
	w := nameWords(name)
	out := append([]string(nil), w...)
	for i := range w {
		joined := w[i]
		for j := i + 1; j < len(w) && j < i+4; j++ {
			joined += w[j]
			out = append(out, joined)
		}
	}
	return out
}

// startsSome: t begins one of the words, give or take a slip — one in a word
// of four letters or more, two from seven.
func startsSome(t string, words []string) bool {
	slips := 0
	switch {
	case len(t) >= 7:
		slips = 2
	case len(t) >= 4:
		slips = 1
	}
	for _, w := range words {
		if strings.HasPrefix(w, t) {
			return true
		}
		if slips == 0 {
			continue
		}
		// against the word's start, a letter shorter to two longer
		for n := len(t) - 1; n <= len(t)+2; n++ {
			if n > 0 && n <= len(w) && editDistance(t, w[:n]) <= slips {
				return true
			}
		}
	}
	return false
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
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
