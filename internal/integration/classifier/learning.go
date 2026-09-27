package classifier

// learning.go — what the Tier-3 model reads beyond the transaction itself.
//
// Until 0.20.0 the model saw one transaction in isolation: its payload, the
// fifteen nearest firefly rows by BM25, the person's past titles for the
// same merchant. Four things a careful human reviewer uses were missing:
//
//   - what this person CORRECTED before. fold suggested "X", they chose
//     "Y" — the strongest evidence there is, and it was thrown away;
//   - what the ledger already knows about this exact UPI handle or card
//     descriptor. Since mid-2026 many alerts carry only a handle
//     ("q12…@ybl"), and the only place its owner is named is the notes of
//     past firefly rows that paid it;
//   - what happened around it: the ride to the restaurant, the meal before,
//     a trip in progress (a run of SGD charges) and the tags it carries;
//   - whether it recurs: the same amount to the same payee every month is a
//     subscription with a title the person has already written.
//
// Each piece is capped so the prompt stays focused, and each lookup takes an
// exclusion (the row being evaluated) so the shadow evaluation can test the
// engine on already-decided history without showing it the answer.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
)

// exclusion hides one transaction from every lookup (the shadow
// evaluation's target, so the engine never sees the answer it is graded on).
// accountID is its payee's account when nothing else in the ledger uses it:
// an account created by sending this very row, which the engine could not
// have known when the row arrived.
type exclusion struct {
	foldUUID  string
	fireflyID int64
	accountID int64
}

// learnedExample is one past decision shown to the model.
type learnedExample struct {
	why          string // "same merchant", "same handle", "recent"
	action       string
	at           time.Time
	narration    string
	amountPaise  int64
	foreignLabel string
	suggested    feedback.ReviewValues
	chosen       feedback.ReviewValues
	note         string
}

// handleHit summarises what the ledger says about one handle/descriptor.
type handleHit struct {
	token     string
	count     int
	latest    time.Time
	payees    []string // "Payee · Category · budget (×n, ₹lo–₹hi)", most frequent first
	titles    []string // most recent distinct titles
	txnTypes  []string
	tagsSeen  []string
	inLedger  int // firefly rows
	inDecided int // fold rows already decided
}

// neighbour is another transaction near this one in time. Its title, payee,
// category and tags are shown only when they are the owner's (owner): what
// firefly holds for a sent row, what they confirmed on a waiting one. An
// unreviewed card's values are an older engine's guesses — shown as fact,
// they taught the model the old engine's habits — so it shows as the bank
// wrote it instead.
type neighbour struct {
	at       time.Time
	dir      string
	amount   int64
	foreign  string
	title    string
	payee    string
	category string
	budget   string
	tags     []string
	state    string // "sent", "waiting", "held"
	owner    bool
	bank     string // the bank's words, for an unreviewed one
}

// ownerValues are a staged row's values as the owner decided them: firefly's
// row for a sent one, the confirmed fields (and only confirmed tags — push
// sends no others) for a waiting one. ok is false when nobody has decided
// anything on it yet.
func (c *Classifier) ownerValues(ctx context.Context, uuid, status string, snap feedback.ReviewSnapshot) (feedback.ReviewValues, bool) {
	if status == "pushed" {
		if _, _, v, ok := fireflyTruth(ctx, c.db, uuid); ok {
			return v, true
		}
	}
	var touched int
	var confirmedTags sql.NullString
	_ = c.db.QueryRowContext(ctx, `SELECT `+humanTouchedSQL+`, s.confirmed_tags_json FROM staged_fold_txns s WHERE s.fold_uuid = ?`, uuid).Scan(&touched, &confirmedTags)
	if status != "pushed" && touched == 0 {
		return feedback.ReviewValues{}, false
	}
	v := snap.Effective
	v.Tags = parseTags(confirmedTags.String)
	return v, true
}

// humanTouchedSQL: a person chose something on the row (alias s).
const humanTouchedSQL = `(s.confirmed_destination_account_id IS NOT NULL OR s.confirmed_destination_account_name IS NOT NULL
	OR s.confirmed_source_account_id IS NOT NULL OR s.confirmed_source_account_name IS NOT NULL
	OR s.confirmed_category_id IS NOT NULL OR s.confirmed_budget_id IS NOT NULL
	OR s.confirmed_description IS NOT NULL OR s.confirmed_tags_json IS NOT NULL
	OR s.confirmed_txn_type IS NOT NULL OR s.confirmed_refund_of IS NOT NULL)`

// learningContext bundles the four pieces for the prompt.
type learningContext struct {
	corrections []learnedExample
	decisions   []learnedExample
	handles     []handleHit
	neighbours  []neighbour
	trip        string
	recurring   string
}

const (
	maxCorrections       = 10
	maxGlobalCorrections = 5
	maxDecisions         = 8
	maxNeighbours        = 14
	neighbourWindow      = 36 * time.Hour
	tripWindow           = 21 * 24 * time.Hour
)

// gatherLearning builds the learning context for one staged row. Every part
// is best-effort: a failing lookup leaves its block out, never the call.
func (c *Classifier) gatherLearning(ctx context.Context, staged StagedRow, payeeHint *int64, ex exclusion) learningContext {
	var lc learningContext
	lc.corrections, lc.decisions = c.learnedExamples(ctx, staged, ex)
	lc.handles = c.handleHistory(ctx, staged, ex)
	lc.neighbours = c.neighbours(ctx, staged, ex)
	lc.trip = c.tripContext(ctx, staged, ex)
	lc.recurring = c.recurringContext(ctx, staged, payeeHint, lc.handles, ex)
	return lc
}

// ---- past decisions -------------------------------------------------------

func (c *Classifier) learnedExamples(ctx context.Context, staged StagedRow, ex exclusion) (corrections, decisions []learnedExample) {
	seen := map[string]bool{staged.FoldUUID: true}
	if ex.foldUUID != "" {
		seen[ex.foldUUID] = true
	}
	add := func(list []learnedExample, e learnedExample, key string) []learnedExample {
		if seen[key] {
			return list
		}
		seen[key] = true
		return append(list, e)
	}
	collect := func(why, where string, args []any, limit int) {
		rows, err := c.db.QueryContext(ctx, `
			SELECT fold_uuid, at, action, narration, amount_paise, foreign_label, suggested_json, chosen_json, note
			FROM review_feedback WHERE `+where+` ORDER BY at DESC LIMIT ?`, append(args, limit)...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e      learnedExample
				uuid   string
				at     string
				sj, cj string
			)
			if rows.Scan(&uuid, &at, &e.action, &e.narration, &e.amountPaise, &e.foreignLabel, &sj, &cj, &e.note) != nil {
				continue
			}
			e.why = why
			e.at = parseSQLiteTime(at)
			_ = json.Unmarshal([]byte(sj), &e.suggested)
			_ = json.Unmarshal([]byte(cj), &e.chosen)
			if e.action == feedback.FeedbackEdit {
				if e.suggested.Equal(e.chosen) || len(corrections) >= maxCorrections {
					continue
				}
				corrections = add(corrections, e, uuid)
			} else if len(decisions) < maxDecisions {
				decisions = add(decisions, e, uuid)
			}
		}
	}
	if m := strings.TrimSpace(staged.MerchantExtracted); m != "" {
		collect("same merchant", `merchant_key = ? AND fold_uuid <> ?`, []any{m, ex.foldUUID}, 40)
	}
	if toks := handleTokens(staged.Narration); len(toks) > 0 {
		var ors []string
		var args []any
		for _, t := range toks {
			ors = append(ors, "narration LIKE ?")
			args = append(args, "%"+t+"%")
		}
		collect("same handle", `(`+strings.Join(ors, " OR ")+`) AND fold_uuid <> ?`, append(args, ex.foldUUID), 40)
	}
	// A few of the most recent corrections anywhere: how this person's
	// style is drifting, even for a merchant never seen before.
	before := len(corrections)
	collect("recent", `action = 'edit' AND fold_uuid <> ?`, []any{ex.foldUUID}, 30)
	if extra := len(corrections) - before; extra > maxGlobalCorrections {
		corrections = corrections[:before+maxGlobalCorrections]
	}
	return corrections, decisions
}

// ---- the same handle / descriptor in the ledger ---------------------------

var (
	vpaRe = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9._-]{1,}@[a-z]{2,}\b`)
	// CARD/<ref>/<merchant>/<currency>/<amount>/<direction>/…, where the
	// currency is whatever the bank writes: "Rs.", "₹", "USD".
	cardMerchRe = regexp.MustCompile(`(?i)^CARD/[0-9a-f]{6,}/([^/]+)/[^/]{1,8}/[0-9.,]+/(?:OUTGOING|INCOMING|DEBIT|CREDIT)\b`)
)

// handleTokens are the strings in a narration that identify who was paid:
// UPI handles, and a card alert's merchant descriptor. They are what past
// firefly rows' notes (the raw narration, stored on push) can be matched on.
func handleTokens(narration string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range upiHandles(narration) {
		v = strings.ToLower(v)
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if m := cardMerchRe.FindStringSubmatch(strings.TrimSpace(narration)); m != nil {
		d := strings.TrimSpace(m[1])
		if len(d) >= 3 && !seen[strings.ToLower(d)] {
			out = append(out, "/"+d+"/")
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	// The note on a UPI payment is a handle of its own when it names a kind
	// of payment the owner makes again and again: "RAPIDO" ends every ride
	// paid to a Rapido driver, whoever the driver is.
	if note := upiNote(narration); len(note) >= 4 && noteWordRe.MatchString(note) && upiDashRe.MatchString(strings.TrimSpace(narration)) {
		out = append(out, "-"+strings.ToUpper(note))
	}
	return out
}

// upiHandles finds the UPI handles in a narration. In the dash format
// ("UPI-<name>-<handle>-<IFSC>-…") the handle's own dash can't be told from
// the one before it by a pattern — "RIDE DRIVER-9000000002@YBL" is a name
// and a handle — so there the handle is the dash-separated segment holding
// the "@", joined to the one before it only for a numbered handle's short
// suffix ("ASHA.M-1@OKICICI"). Elsewhere slashes delimit it.
func upiHandles(narration string) []string {
	n := strings.TrimSpace(narration)
	if !upiDashRe.MatchString(n) {
		return vpaRe.FindAllString(n, -1)
	}
	parts := strings.Split(n, "-")
	for i, p := range parts {
		local, domain, ok := strings.Cut(p, "@")
		if !ok || domain == "" || strings.ContainsAny(p, " ") {
			continue
		}
		if len(local) <= 2 && i > 0 && !strings.ContainsAny(parts[i-1], " ") {
			return []string{parts[i-1] + "-" + p}
		}
		return []string{p}
	}
	return nil
}

// noteWordRe: a note worth looking up is words, not a reference.
var noteWordRe = regexp.MustCompile(`^[A-Za-z][A-Za-z ]+$`)

func (c *Classifier) handleHistory(ctx context.Context, staged StagedRow, ex exclusion) []handleHit {
	var hits []handleHit
	for _, tok := range handleTokens(staged.Narration) {
		h := handleHit{token: strings.Trim(tok, "/")}
		type agg struct {
			n      int
			latest time.Time
			lo, hi int64 // the amounts seen
		}
		payees := map[string]*agg{}
		titleSeen := map[string]bool{}
		typeSeen := map[string]bool{}
		tagCount := map[string]int{}
		rows, err := c.db.QueryContext(ctx, `
			SELECT txn_type, COALESCE(destination_account_name, ''), COALESCE(source_account_name, ''),
			       COALESCE(category_name, ''), COALESCE(budget_name, ''), COALESCE(description, ''), COALESCE(tags_json, ''), date,
			       amount_paise
			FROM firefly_txns
			WHERE notes LIKE ? AND firefly_id <> ?
			ORDER BY date DESC LIMIT 40`, "%"+tok+"%", ex.fireflyID)
		if err != nil {
			continue
		}
		for rows.Next() {
			var typ, dst, src, cat, bud, desc, tagsJS, date string
			var amt int64
			if rows.Scan(&typ, &dst, &src, &cat, &bud, &desc, &tagsJS, &date, &amt) != nil {
				continue
			}
			h.inLedger++
			t := parseSQLiteTime(date)
			if t.After(h.latest) {
				h.latest = t
			}
			payee := dst
			if typ == "deposit" {
				payee = src
			}
			key := payee
			if cat != "" {
				key += " · " + cat
			}
			if bud != "" {
				key += " · budget " + bud
			} else {
				key += " · no budget"
			}
			if payees[key] == nil {
				payees[key] = &agg{lo: amt, hi: amt}
			}
			payees[key].n++
			payees[key].lo, payees[key].hi = min(payees[key].lo, amt), max(payees[key].hi, amt)
			if desc != "" && !titleSeen[desc] && len(h.titles) < 4 {
				titleSeen[desc] = true
				h.titles = append(h.titles, desc)
			}
			if !typeSeen[typ] {
				typeSeen[typ] = true
				h.txnTypes = append(h.txnTypes, typ)
			}
			var tags []string
			if json.Unmarshal([]byte(tagsJS), &tags) == nil {
				for _, tg := range tags {
					tagCount[tg]++
				}
			}
		}
		rows.Close()
		h.count = h.inLedger
		if h.count == 0 {
			continue
		}
		type kv struct {
			k string
			n int
		}
		var ps []kv
		for k, v := range payees {
			ps = append(ps, kv{k, v.n})
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i].n > ps[j].n || (ps[i].n == ps[j].n && ps[i].k < ps[j].k) })
		for i, p := range ps {
			if i >= 3 {
				break
			}
			// the amounts say which of two payees a new payment is, when a
			// handle is shared (a ₹38 ride is the bike, a ₹160 one the auto)
			a := payees[p.k]
			amounts := rupeesShort(a.lo)
			if a.hi != a.lo {
				amounts += "–" + rupeesShort(a.hi)
			}
			h.payees = append(h.payees, fmt.Sprintf("%s (×%d, %s)", p.k, p.n, amounts))
		}
		var ts []kv
		for k, v := range tagCount {
			ts = append(ts, kv{k, v})
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i].n > ts[j].n || (ts[i].n == ts[j].n && ts[i].k < ts[j].k) })
		for i, t := range ts {
			if i >= 4 {
				break
			}
			h.tagsSeen = append(h.tagsSeen, fmt.Sprintf("%s (×%d)", t.k, t.n))
		}
		hits = append(hits, h)
	}
	return hits
}

// ---- around this time -----------------------------------------------------

func (c *Classifier) neighbours(ctx context.Context, staged StagedRow, ex exclusion) []neighbour {
	at, ok := parseTxnTime(staged.TxnTimestamp)
	if !ok {
		return nil
	}
	fromDay, toDay := dayRange(at.Add(-neighbourWindow), at.Add(neighbourWindow))
	rows, err := c.db.QueryContext(ctx, `
		SELECT s.fold_uuid, `+effWhenSQL+`, s.status, COALESCE(s.hold_reason, '')
		FROM staged_fold_txns s
		WHERE s.fold_uuid <> ? AND s.fold_uuid <> ? AND s.status <> 'skipped'
		  AND substr(`+effWhenSQL+`, 1, 10) BETWEEN ? AND ?`, staged.FoldUUID, ex.foldUUID, fromDay, toDay)
	if err != nil {
		return nil
	}
	type pending struct {
		uuid, status, hold string
		at                 time.Time
		gap                time.Duration
	}
	var ps []pending
	for rows.Next() {
		var p pending
		var ts string
		if rows.Scan(&p.uuid, &ts, &p.status, &p.hold) != nil {
			continue
		}
		t, ok := parseTxnTime(ts)
		if !ok {
			continue
		}
		p.at, p.gap = t, t.Sub(at).Abs()
		if p.gap <= neighbourWindow {
			ps = append(ps, p)
		}
	}
	rows.Close()
	// The closest first, so the cap keeps what matters most.
	sort.Slice(ps, func(i, j int) bool {
		return ps[i].gap < ps[j].gap || (ps[i].gap == ps[j].gap && ps[i].uuid < ps[j].uuid)
	})
	if len(ps) > maxNeighbours {
		ps = ps[:maxNeighbours]
	}
	var out []neighbour
	for _, p := range ps {
		snap, err := feedback.SnapshotReview(ctx, c.db, p.uuid)
		if err != nil {
			continue
		}
		n := neighbour{at: p.at, dir: snap.Direction, amount: snap.AmountPaise, foreign: snap.ForeignLabel}
		if v, mine := c.ownerValues(ctx, p.uuid, p.status, snap); mine {
			n.owner, n.title, n.payee, n.category, n.budget, n.tags = true, v.Title, v.Payee, v.Category, v.Budget, v.Tags
		} else {
			n.bank = shortNarration(snap.Narration)
		}
		switch {
		case p.hold != "":
			n.state = "held"
		case p.status == "pushed":
			n.state = "sent"
		default:
			n.state = "waiting"
		}
		if n.owner && n.title == "" {
			n.title = shortNarration(snap.Narration)
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// ---- a trip in progress ---------------------------------------------------

func (c *Classifier) tripContext(ctx context.Context, staged StagedRow, ex exclusion) string {
	cur := strings.ToUpper(strings.TrimSpace(foreignCurrencyOf(staged.RawPayload)))
	if cur == "" || cur == "INR" {
		var fc sql.NullString
		_ = c.db.QueryRowContext(ctx, `SELECT foreign_currency FROM staged_fold_txns WHERE fold_uuid = ?`, staged.FoldUUID).Scan(&fc)
		cur = strings.ToUpper(strings.TrimSpace(fc.String))
	}
	if cur == "" || cur == "INR" {
		return ""
	}
	at, ok := parseTxnTime(staged.TxnTimestamp)
	if !ok {
		return ""
	}
	fromDay, toDay := dayRange(at.Add(-tripWindow), at.Add(tripWindow))
	rows, err := c.db.QueryContext(ctx, `
		SELECT s.fold_uuid, `+effWhenSQL+`, s.status FROM staged_fold_txns s
		WHERE UPPER(s.foreign_currency) = ? AND s.fold_uuid <> ? AND s.fold_uuid <> ? AND s.status <> 'skipped'
		  AND substr(`+effWhenSQL+`, 1, 10) BETWEEN ? AND ?`, cur, staged.FoldUUID, ex.foldUUID, fromDay, toDay)
	if err != nil {
		return ""
	}
	var uuids []string
	statusOf := map[string]string{}
	var first, last time.Time
	for rows.Next() {
		var u, ts, st string
		if rows.Scan(&u, &ts, &st) != nil {
			continue
		}
		statusOf[u] = st
		t, ok := parseTxnTime(ts)
		if !ok || t.Sub(at).Abs() > tripWindow {
			continue
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if t.After(last) {
			last = t
		}
		uuids = append(uuids, u)
	}
	rows.Close()
	if len(uuids) < 2 {
		return ""
	}
	// Tags and categories as the owner decided them — an older engine's
	// guesses on unreviewed cards would otherwise read as the trip's tag.
	tags, cats, buds := map[string]int{}, map[string]int{}, map[string]int{}
	decided := 0
	for _, u := range uuids {
		snap, err := feedback.SnapshotReview(ctx, c.db, u)
		if err != nil {
			continue
		}
		v, mine := c.ownerValues(ctx, u, statusOf[u], snap)
		if !mine {
			continue
		}
		for _, t := range v.Tags {
			tags[t]++
		}
		decided++
		if v.Category != "" && v.Category != feedback.NoneValue {
			cats[v.Category]++
		}
		bud := v.Budget
		if bud == "" || bud == feedback.NoneValue {
			bud = "none"
		}
		buds[bud]++
	}
	ist := time.FixedZone("IST", 5*3600+1800)
	s := fmt.Sprintf("%d other %s charges between %s and %s — a trip.", len(uuids), cur,
		first.In(ist).Format("Mon 2 Jan 2006"), last.In(ist).Format("Mon 2 Jan 2006"))
	if len(tags) > 0 {
		s += " Tags on them: " + topCounts(tags, 4) + " — a trip tag the person uses belongs on this charge too."
	} else {
		s += " None carries a tag yet."
	}
	if len(cats) > 0 {
		s += " Categories: " + topCounts(cats, 4) + "."
	}
	if decided > 0 {
		s += " Budgets: " + topCounts(buds, 3) + " — a new place on the trip follows the budget its other spends in that category went to."
	}
	return s
}

// ---- recurring --------------------------------------------------------------

func (c *Classifier) recurringContext(ctx context.Context, staged StagedRow, payeeHint *int64, handles []handleHit, ex exclusion) string {
	if staged.AmountPaise <= 0 {
		return ""
	}
	lo, hi := staged.AmountPaise*97/100, staged.AmountPaise*103/100
	var rows *sql.Rows
	var err error
	switch {
	case payeeHint != nil:
		rows, err = c.db.QueryContext(ctx, `
			SELECT date, amount_paise, COALESCE(description,''), COALESCE(category_name,''), COALESCE(destination_account_name,'')
			FROM firefly_txns WHERE destination_account_id = ? AND amount_paise BETWEEN ? AND ? AND firefly_id <> ?
			ORDER BY date DESC LIMIT 36`, *payeeHint, lo, hi, ex.fireflyID)
	case len(handles) > 0:
		rows, err = c.db.QueryContext(ctx, `
			SELECT date, amount_paise, COALESCE(description,''), COALESCE(category_name,''), COALESCE(destination_account_name,'')
			FROM firefly_txns WHERE notes LIKE ? AND amount_paise BETWEEN ? AND ? AND firefly_id <> ?
			ORDER BY date DESC LIMIT 36`, "%"+handles[0].token+"%", lo, hi, ex.fireflyID)
	default:
		return ""
	}
	if err != nil {
		return ""
	}
	defer rows.Close()
	var dates []time.Time
	var title, cat, payee string
	for rows.Next() {
		var d, desc, cn, pn string
		var amt int64
		if rows.Scan(&d, &amt, &desc, &cn, &pn) != nil {
			continue
		}
		dates = append(dates, parseSQLiteTime(d))
		if title == "" && desc != "" {
			title, cat, payee = desc, cn, pn
		}
	}
	if len(dates) < 3 {
		return ""
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	var gaps []float64
	for i := 1; i < len(dates); i++ {
		gaps = append(gaps, dates[i].Sub(dates[i-1]).Hours()/24)
	}
	sort.Float64s(gaps)
	med := gaps[len(gaps)/2]
	cadence := ""
	switch {
	case med >= 6 && med <= 8:
		cadence = "weekly"
	case med >= 26 && med <= 35:
		cadence = "monthly"
	case med >= 85 && med <= 95:
		cadence = "quarterly"
	case med >= 175 && med <= 190:
		cadence = "every six months"
	case med >= 350 && med <= 380:
		cadence = "yearly"
	default:
		return ""
	}
	return fmt.Sprintf("the same amount (±3%%) went to %q %s %d times since %s — a recurring charge, last titled %q (category %q).",
		payee, cadence, len(dates), dates[0].Format("Jan 2006"), title, cat)
}

// ---- rendering --------------------------------------------------------------

func (lc learningContext) render(b *strings.Builder) {
	ist := time.FixedZone("IST", 5*3600+1800)
	money := func(p int64, foreign string) string {
		s := fmt.Sprintf("₹%.2f", float64(p)/100)
		if foreign != "" {
			s += " (" + foreign + ")"
		}
		return s
	}
	vals := func(v feedback.ReviewValues) string {
		parts := []string{fmt.Sprintf("%q", v.Title)}
		if v.Payee != "" {
			parts = append(parts, "→ "+v.Payee)
		}
		if v.Category != "" {
			parts = append(parts, "· "+v.Category)
		}
		if v.Budget != "" && v.Budget != feedback.NoneValue {
			parts = append(parts, "· budget "+v.Budget)
		}
		if len(v.Tags) > 0 {
			parts = append(parts, "· tags "+strings.Join(v.Tags, ", "))
		}
		if v.Type != "" {
			parts = append(parts, "· "+v.Type)
		}
		return strings.Join(parts, " ")
	}
	// Corrections for this merchant or handle are evidence about THIS
	// transaction; recent ones elsewhere only show how the owner writes —
	// kept apart, so a model can't mistake one for the other.
	var here, elsewhere []learnedExample
	for _, e := range lc.corrections {
		if e.why == "recent" {
			elsewhere = append(elsewhere, e)
		} else {
			here = append(here, e)
		}
	}
	writeCorrection := func(e learnedExample) {
		b.WriteString(fmt.Sprintf("  [%s · %s] %s  %s\n", e.why, e.at.In(ist).Format("2 Jan 2006"), shortNarration(e.narration), money(e.amountPaise, e.foreignLabel)))
		b.WriteString("     fold suggested: " + vals(e.suggested) + "\n")
		b.WriteString("     you chose:      " + vals(e.chosen) + "\n")
	}
	if len(here) > 0 {
		b.WriteString("== HOW YOU CORRECTED FOLD BEFORE — the strongest evidence of how you book things ==\n")
		b.WriteString("(fold suggested one thing for this merchant or handle, you chose another. Follow your choices over every other signal below.)\n")
		for _, e := range here {
			writeCorrection(e)
		}
		b.WriteString("\n")
	}
	if len(elsewhere) > 0 {
		b.WriteString("== YOUR RECENT CORRECTIONS ELSEWHERE — style only, other merchants ==\n")
		b.WriteString("(How you phrase titles and pick categories lately. They are about OTHER transactions: never take a payee, a title's specifics or a category from them unless this transaction is the same kind.)\n")
		for _, e := range elsewhere {
			writeCorrection(e)
		}
		b.WriteString("\n")
	}
	if len(lc.decisions) > 0 {
		b.WriteString("== WHAT YOU DID WITH SIMILAR TRANSACTIONS ==\n")
		for _, e := range lc.decisions {
			line := fmt.Sprintf("  [%s · %s] %s  %s — ", e.why, e.at.In(ist).Format("2 Jan 2006"), shortNarration(e.narration), money(e.amountPaise, e.foreignLabel))
			switch e.action {
			case feedback.FeedbackSend:
				line += "you sent it as " + vals(e.chosen)
			case feedback.FeedbackHold:
				line += fmt.Sprintf("you HELD it (never to be sent as is): %q", e.note)
			case feedback.FeedbackSkip:
				line += "you SKIPPED it (never sent)"
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	if len(lc.handles) > 0 {
		b.WriteString("== THIS HANDLE / DESCRIPTOR IN YOUR LEDGER (firefly rows whose notes carry it) ==\n")
		for _, h := range lc.handles {
			label := fmt.Sprintf("%q", h.token)
			if strings.HasPrefix(h.token, "-") {
				label = fmt.Sprintf("the payment note %q", strings.TrimPrefix(h.token, "-"))
			}
			b.WriteString(fmt.Sprintf("  %s — %d firefly rows, latest %s\n", label, h.count, h.latest.In(ist).Format("2 Jan 2006")))
			if len(h.payees) > 0 {
				b.WriteString("     booked to: " + strings.Join(h.payees, "; ") + "\n")
			}
			if len(h.titles) > 0 {
				b.WriteString(fmt.Sprintf("     titles:    %q\n", h.titles))
			}
			if len(h.tagsSeen) > 0 {
				b.WriteString("     tags:      " + strings.Join(h.tagsSeen, ", ") + "\n")
			}
		}
		b.WriteString("(A handle the person paid before is the same counterparty: reuse that payee unless the evidence says otherwise.)\n\n")
	}
	if len(lc.neighbours) > 0 {
		b.WriteString("== AROUND THIS TIME (your other transactions within ±36h, oldest first) ==\n")
		for _, n := range lc.neighbours {
			sign := "−"
			if n.dir == "INCOMING" {
				sign = "+"
			}
			line := fmt.Sprintf("  %s  %s%s  ", n.at.In(ist).Format("Mon 2 Jan 15:04"), sign, money(n.amount, n.foreign))
			if !n.owner {
				b.WriteString(line + "not reviewed yet: " + n.bank + "  [" + n.state + "]\n")
				continue
			}
			line += fmt.Sprintf("%q", n.title)
			if n.payee != "" {
				line += " → " + n.payee
			}
			if n.category != "" {
				line += " · " + n.category
			}
			if n.budget != "" && n.budget != feedback.NoneValue {
				line += " · budget " + n.budget
			}
			if len(n.tags) > 0 {
				line += " · tags " + strings.Join(n.tags, ", ")
			}
			b.WriteString(line + "  [" + n.state + "]\n")
		}
		b.WriteString("(Titles, payees and tags here are yours — sent or confirmed; an unreviewed one shows only what the bank said. Use them for context — a ride to the place, the meal before, a split bill, a trip — never copy a neighbour's title onto an unrelated charge.)\n\n")
	}
	if lc.trip != "" {
		b.WriteString("== TRIP CONTEXT ==\n  " + lc.trip + "\n\n")
	}
	if lc.recurring != "" {
		b.WriteString("== RECURRING ==\n  " + lc.recurring + "\n\n")
	}
}

// ---- helpers ----------------------------------------------------------------

func shortNarration(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > 90 {
		return string(r[:90]) + "…"
	}
	return s
}

func topCounts(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range m {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v || (list[i].v == list[j].v && list[i].k < list[j].k) })
	var parts []string
	for i, e := range list {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", e.k, e.v))
	}
	return strings.Join(parts, ", ")
}

// parseSQLiteTime reads the timestamp shapes SQLite and firefly give us:
// CURRENT_TIMESTAMP's "2006-01-02 15:04:05", RFC3339, and the driver's own
// rendering of a time.Time, "2026-08-30 07:29:48 +0000 UTC".
func parseSQLiteTime(s string) time.Time {
	if t, ok := parseAnyTime(s); ok {
		return t
	}
	if t, err := time.Parse("2006-01-02T15:04:05", strings.TrimSpace(s)); err == nil {
		return t
	}
	return time.Time{}
}

// Time windows over staged rows. txn_timestamp is stored the way the SQLite
// driver renders a time.Time ("2026-08-30 07:29:48 +0000 UTC"), which
// SQLite's own datetime() cannot read — it returns NULL, and a window written
// with it silently matches nothing. Every stored shape starts with the date,
// so a window is a coarse day range in SQL (a day's margin either side
// covers any offset) and the exact bound in Go.
func dayRange(from, to time.Time) (string, string) {
	return from.UTC().Add(-24 * time.Hour).Format("2006-01-02"), to.UTC().Add(24 * time.Hour).Format("2006-01-02")
}

// parseTxnTime reads a staged row's txn_timestamp.
func parseTxnTime(s string) (time.Time, bool) {
	t := parseSQLiteTime(s)
	return t, !t.IsZero()
}

// LearnedCounts is what a Tier-3 decision was shown from the owner's
// history — kept in the evidence so the card can say "learned from 3 of your
// corrections", and so a test can tell "nothing relevant" from "never looked".
type LearnedCounts struct {
	Corrections int  `json:"corrections,omitempty"`
	Decisions   int  `json:"decisions,omitempty"`
	LedgerRows  int  `json:"ledger_rows,omitempty"`
	Neighbours  int  `json:"neighbours,omitempty"`
	Trip        bool `json:"trip,omitempty"`
	Recurring   bool `json:"recurring,omitempty"`
}

func (lc learningContext) summary() *LearnedCounts {
	n := &LearnedCounts{Corrections: len(lc.corrections), Decisions: len(lc.decisions), Neighbours: len(lc.neighbours),
		Trip: lc.trip != "", Recurring: lc.recurring != ""}
	for _, h := range lc.handles {
		n.LedgerRows += h.count
	}
	return n
}

// EngineVersion identifies the Tier-3 context and prompt. A suggestion made
// by an older version (or by the deterministic tiers alone, when the model
// was unreachable) is one the re-suggest loop may revisit. Bump it when the
// prompt or context change enough that every waiting suggestion deserves
// another look.
const EngineVersion = 7

// engineVersionFor is the version to record for a decision: the current one
// when the model made it — or looked and declined, or it is a deterministic
// refund, which the model would not improve — and 0 when the deterministic
// tiers stood in for a model that wasn't there.
func engineVersionFor(d Decision) int {
	if d.Tier == TierLLM || d.Tier == TierRefund || d.Engine != "" {
		return EngineVersion
	}
	return 0
}
