package ui

// review.go — the review deck.
//
// One card per transaction waiting for a human. Swipe it right (or press →)
// and it goes to firefly; swipe it left (←) and it waits in the Later pile.
// The card shows the money, what kind of move it was (a withdrawal, a
// deposit or a transfer, exactly as push will book it), who it went to, when,
// from which account, and the title and category push will send — and then
// only what is
// exceptional: a blank in the title, a missing payee, a refund whose
// purchase isn't picked, a possible duplicate, a hold. Everything a row
// arrives with (it was classified, it is unconfirmed) is normal and is not
// badged.
//
// The page is a shell (templates/review.html); the deck itself is
// static/review.js talking to the small JSON API below. Sending is the only
// firefly write, and it happens through the same Pusher as every other
// push, after the same checks — the client's undo window only delays the
// request, it never skips a check.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
	"github.com/rounakdatta/texas-fold-em/internal/integration/refundref"
)

// party is one end of a transaction as the card shows it: a merchant
// ("Chai Corner" in "Market Road") or one of the user's own
// accounts ("Tata Neu card").
type party struct {
	Name  string `json:"name"`
	Place string `json:"place,omitempty"`
	// Mine: one of the user's own asset accounts (a card, a bank account).
	Mine bool `json:"mine,omitempty"`
	// Full is the account's full firefly name when Name is shortened.
	Full string `json:"full,omitempty"`
}

// cardRefund is the refund part of a card: which purchase the money came
// back for.
type cardRefund struct {
	NeedsPick bool         `json:"needsPick"`
	Current   string       `json:"current,omitempty"`
	Label     string       `json:"label,omitempty"`
	Options   []refundPick `json:"options,omitempty"`
}

type refundPick struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// reviewCard is one card in the deck — everything the card renders, already
// formatted, so the client does no money or date arithmetic of its own.
type reviewCard struct {
	UUID   string `json:"uuid"`
	Status string `json:"status"`
	// Direction is what the bank saw: "out" (money left one of the user's
	// accounts) or "in" (money arrived in one).
	Direction string `json:"direction"`
	// Type is Firefly's kind for it — "withdrawal", "deposit" or "transfer" —
	// resolved exactly as push resolves it (integration.CheckType), so the
	// card never shows one thing while push sends another.
	Type string `json:"type"`
	// Types are the kinds the direction allows, the everyday one first:
	// money out is a withdrawal or a transfer, money in a deposit or a
	// transfer.
	Types []string `json:"types"`
	// TypeChosen: a person picked the type; push sends it as it is.
	TypeChosen bool `json:"typeChosen,omitempty"`
	// Problem is what doesn't fit between the type and the accounts (an
	// integration.Problem* code), and ProblemText says so in words.
	Problem     string   `json:"problem,omitempty"`
	ProblemText string   `json:"problemText,omitempty"`
	AmountPaise int64    `json:"amountPaise"`
	Amount      string   `json:"amount"`
	FoldAmount  string   `json:"foldAmount,omitempty"` // the alert's amount, when the statement corrected it
	Foreign     string   `json:"foreign,omitempty"`    // "AED 25", for a charge abroad
	When        string   `json:"when"`                 // RFC3339, UTC
	Day         string   `json:"day"`                  // "Today", "Tue 30 Dec", "Tue 30 Dec 2025"
	Clock       string   `json:"clock"`                // "1:21 pm" (IST, like every statement)
	From        party    `json:"from"`
	To          party    `json:"to"`
	Title       string   `json:"title"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags,omitempty"`
	// Note is the human's own note from fold.money, or a reconciliation hint
	// on a row added from a statement line — context in their own words.
	Note string `json:"note,omitempty"`
	// BankSaid is what the bank's alert or statement called the payee.
	BankSaid  string      `json:"bankSaid,omitempty"`
	Narration string      `json:"narration"`
	Manual    bool        `json:"manual,omitempty"`
	Later     bool        `json:"later,omitempty"`
	Hold      string      `json:"hold,omitempty"`
	Duplicate bool        `json:"duplicate,omitempty"`
	Refund    *cardRefund `json:"refund,omitempty"`
	// Blockers: what must change before this can be sent. The same list
	// guards the send endpoint, so the card and the server never disagree.
	Blockers []string `json:"blockers,omitempty"`
	EditURL  string   `json:"editUrl"`
}

// blocker kinds, shared with static/review.js.
const (
	blockHold       = "hold"
	blockTitleEmpty = "title-empty"
	blockTitleBlank = "title-blank"
	blockPayee      = "payee"
	blockSource     = "source"
	// the other side is one of the user's own accounts, on a withdrawal or a
	// deposit: one tap makes it the transfer it is
	blockMine = "mine"
	// the type can't carry the direction (a deposit of money that left)
	blockType = "type"
	// the two sides were saved the wrong way round: one tap swaps them
	blockSwap = "swap"
)

// titleBlank is how titles mark what the classifier (or a reconciliation)
// couldn't know: "___ for lunch from Zomato".
const titleBlank = "___"

// cardSelect reads every column a card needs, with each field resolved the
// way push will resolve it (a human's choice as a unit, else the
// classifier's). Expects the alias s.
var cardSelect = `
	SELECT s.fold_uuid, s.status, s.type, s.mode, s.narration,
	       COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp),
	       COALESCE(s.confirmed_amount_paise, s.amount_paise), s.amount_paise,
	       COALESCE(s.confirmed_foreign_amount_paise, s.foreign_amount_paise), s.foreign_currency,
	       COALESCE(NULLIF(s.confirmed_description, ''), NULLIF(s.proposed_description, ''), ''),
	       CASE WHEN s.confirmed_category_id IS NOT NULL
	            THEN COALESCE((SELECT name FROM firefly_categories WHERE firefly_id = s.confirmed_category_id), (SELECT category_name FROM firefly_txns WHERE category_id = s.confirmed_category_id LIMIT 1))
	            ELSE COALESCE((SELECT name FROM firefly_categories WHERE firefly_id = s.proposed_category_id), (SELECT category_name FROM firefly_txns WHERE category_id = s.proposed_category_id LIMIT 1)) END,
	       COALESCE(s.confirmed_tags_json, s.proposed_tags_json, ''),
	       ` + effectiveAccountIDSQL("source") + `,
	       COALESCE(NULLIF(s.confirmed_source_account_name, ''), NULLIF(s.proposed_source_account_name, ''), ''),
	       ` + effectiveAccountIDSQL("destination") + `,
	       COALESCE(NULLIF(s.confirmed_destination_account_name, ''), NULLIF(s.proposed_destination_account_name, ''), ''),
	       COALESCE(s.merchant_extracted, ''),
	       CASE WHEN json_valid(s.raw_payload) THEN COALESCE(json_extract(s.raw_payload, '$.notes'), '') ELSE '' END,
	       ` + possibleDuplicateSQL("s.raw_payload") + `,
	       COALESCE(s.hold_reason, ''), s.later_at IS NOT NULL,
	       -- refund-shaped, as the list decides it
	       s.classifier_tier = 5 OR COALESCE(NULLIF(s.confirmed_refund_of,''), s.proposed_refund_of, '') <> ''
	         OR (s.type = 'INCOMING' AND COALESCE(s.confirmed_category_id, s.proposed_category_id) IN
	             (SELECT category_id FROM firefly_txns WHERE LOWER(category_name) = 'refund')),
	       COALESCE(NULLIF(s.confirmed_refund_of,''), s.proposed_refund_of, ''),
	       COALESCE(s.confirmed_txn_type, ''), COALESCE(s.proposed_txn_type, '')
	FROM staged_fold_txns s`

// reviewable: rows a human still has to decide about. pending rows haven't
// been classified yet; pushed and skipped rows are decided.
const reviewable = `s.status IN ('needs_review', 'ready_to_push')`

// cardRow is one card's columns as read, before any lookup. The pool has a
// single connection (SQLite, see integration.Open), so every row is read
// and the cursor closed before the second query that names its accounts —
// resolving inside the rows loop would wait on itself forever.
type cardRow struct {
	uuid, status, typ, mode, narration string
	tsStr                              string
	amountPaise, foldPaise             int64
	fxPaise                            sql.NullInt64
	fxCur                              sql.NullString
	title                              string
	category                           sql.NullString
	tagsJSON                           string
	srcID, dstID                       sql.NullInt64
	srcName, dstName                   string
	merchant, notes                    string
	dup                                int
	hold                               string
	later                              bool
	isRefund                           sql.NullBool
	refundRef                          string
	confType, propType                 string
}

func scanCardRow(rows interface{ Scan(...any) error }) (cardRow, error) {
	var r cardRow
	err := rows.Scan(&r.uuid, &r.status, &r.typ, &r.mode, &r.narration, &r.tsStr, &r.amountPaise, &r.foldPaise,
		&r.fxPaise, &r.fxCur, &r.title, &r.category, &r.tagsJSON, &r.srcID, &r.srcName, &r.dstID, &r.dstName,
		&r.merchant, &r.notes, &r.dup, &r.hold, &r.later, &r.isRefund, &r.refundRef, &r.confType, &r.propType)
	return r, err
}

// buildCard turns a read row into the card the deck shows.
func (h *Handler) buildCard(ctx context.Context, r cardRow) reviewCard {
	c := reviewCard{UUID: r.uuid, Status: r.status, AmountPaise: r.amountPaise, Title: r.title, Hold: r.hold, Later: r.later}
	c.Narration = r.narration
	c.Manual = r.mode == manualMode
	c.Duplicate = r.dup == 1
	c.Amount = formatINR(r.amountPaise)
	if r.amountPaise != r.foldPaise && !c.Manual {
		c.FoldAmount = formatINR(r.foldPaise)
	}
	if r.fxCur.Valid && r.fxCur.String != "" && r.fxPaise.Valid {
		c.Foreign = formatForeign(r.fxPaise.Int64, r.fxCur.String)
	}
	if t, ok := parseDBTime(r.tsStr); ok {
		c.When = t.UTC().Format(time.RFC3339)
		c.Day, c.Clock = spokenDay(t, time.Now()), spokenClock(t)
	}
	if r.category.Valid {
		c.Category = r.category.String
	}
	if r.tagsJSON != "" {
		_ = json.Unmarshal([]byte(r.tagsJSON), &c.Tags)
	}

	// Name each side. An id resolves to firefly's own account name (the
	// accounts mirror first: it has every account, including ones with no
	// history yet); a name-only side is a merchant or payer push will create.
	src := h.accountParty(ctx, r.srcID, r.srcName)
	dst := h.accountParty(ctx, r.dstID, r.dstName)
	incoming := r.typ == "INCOMING"
	c.Direction = "out"
	if incoming {
		c.Direction = "in"
	}
	// The kind of move, as push will book it.
	tc := integration.CheckType(ctx, h.db, r.typ, r.confType, r.propType, cardSide(r.srcID, r.srcName), cardSide(r.dstID, r.dstName))
	c.Type, c.Types, c.TypeChosen, c.Problem = tc.Type, integration.TypesFor(r.typ), tc.Explicit, tc.Problem
	if tc.Problem == integration.ProblemSwapped {
		// saved the wrong way round: the card reads the right way — the
		// banner says what is stored — and one tap stores it that way
		src, dst = dst, src
		tc.OwnAsset, tc.OtherAsset = tc.OtherAsset, 0
	}
	// An own account shows as the account, even where a same-named payee
	// account stands in for it; so does a transfer's other side.
	own, other := &src, &dst
	if incoming {
		own, other = &dst, &src
	}
	if tc.OwnAsset != 0 {
		*own = h.accountParty(ctx, sql.NullInt64{Int64: tc.OwnAsset, Valid: true}, "")
	}
	if tc.Type == integration.TypeTransfer && tc.OtherAsset != 0 {
		*other = h.accountParty(ctx, sql.NullInt64{Int64: tc.OtherAsset, Valid: true}, "")
	}
	c.From, c.To = src, dst
	// A side counts as filled only if it resolved to a real name (firefly's
	// "(no name)" placeholder doesn't).
	hasDest, hasSource := dst.Name != "", src.Name != ""
	if c.To.Name == "" && !incoming && c.Type == integration.TypeWithdrawal && r.merchant != "" && r.merchant != "(no name)" {
		// Nothing proposed yet: show what the alert called the merchant, so
		// the card still reads "to …" — it stays a blocker until confirmed.
		c.To = party{Name: titleCase(r.merchant)}
	}
	c.ProblemText = problemText(tc.Problem, c)

	c.BankSaid = bankSaid(r.narration, r.mode)
	c.Note = humanNote(r.notes)
	if c.Manual {
		if hint := manualHint(r.narration); hint != "" {
			c.Note = hint
		}
	}

	if r.isRefund.Valid && r.isRefund.Bool {
		kind, _, _ := refundref.Parse(r.refundRef)
		decided := kind == refundref.KindFold || kind == refundref.KindJournal || kind == refundref.KindNone
		c.Refund = &cardRefund{NeedsPick: !decided && c.Status != "skipped", Current: r.refundRef}
		if decided && kind != refundref.KindNone {
			c.Refund.Label = h.refLabel(ctx, r.refundRef)
		}
	}

	c.Blockers = cardBlockers(c, r.typ, hasSource, hasDest)
	// what doesn't fit between the type and the accounts
	switch tc.Problem {
	case integration.ProblemOtherIsMine:
		c.Blockers = insertBlocker(c.Blockers, blockMine)
	case integration.ProblemDirection:
		c.Blockers = insertBlocker(c.Blockers, blockType)
	case integration.ProblemSwapped:
		c.Blockers = insertBlocker(c.Blockers, blockSwap)
	case integration.ProblemOtherNotMine, integration.ProblemSameAccount, integration.ProblemOtherKind:
		c.Blockers = insertBlocker(c.Blockers, blockPayee)
	case integration.ProblemOwnNotMine:
		c.Blockers = insertBlocker(c.Blockers, blockSource)
	}
	c.EditURL = "/admin/ui/staged/" + c.UUID + "?back=" + url.QueryEscape("/admin/ui/review")
	return c
}

// cardBlockers lists what must change before a card can be sent. Firefly is
// the ledger of record: a title with a blank in it, or a spend with nobody
// paid, is not something to write there.
//
// In the order a person would settle them: who was paid comes before what it
// was (the title suggestions are that payee's past titles).
func cardBlockers(c reviewCard, typ string, hasSource, hasDest bool) []string {
	var out []string
	if strings.TrimSpace(c.Hold) != "" {
		out = append(out, blockHold)
	}
	// the other side: who was paid, or who paid
	if (typ == "OUTGOING" && !hasDest) || (typ == "INCOMING" && !hasSource) {
		out = append(out, blockPayee)
	}
	// the user's own side — the account that paid, or that the money came
	// into — firefly can't book it without
	mine := hasSource
	if typ == "INCOMING" {
		mine = hasDest
	}
	if !mine {
		out = append(out, blockSource)
	}
	switch {
	case strings.TrimSpace(c.Title) == "":
		out = append(out, blockTitleEmpty)
	case strings.Contains(c.Title, titleBlank):
		out = append(out, blockTitleBlank)
	}
	return out
}

// insertBlocker adds a blocker once, keeping the order a person settles them
// in: a hold, the kind of move, who is on the other side, the account, the
// title.
func insertBlocker(list []string, b string) []string {
	for _, x := range list {
		if x == b {
			return list
		}
	}
	rank := map[string]int{blockHold: 0, blockType: 1, blockSwap: 1, blockMine: 2, blockPayee: 3, blockSource: 4, blockTitleEmpty: 5, blockTitleBlank: 5}
	out := make([]string, 0, len(list)+1)
	placed := false
	for _, x := range list {
		if !placed && rank[b] < rank[x] {
			out = append(out, b)
			placed = true
		}
		out = append(out, x)
	}
	if !placed {
		out = append(out, b)
	}
	return out
}

// cardSide is a side as push reads it: an id when there is one, else the
// name (an account push asks firefly to create).
func cardSide(id sql.NullInt64, name string) integration.Side {
	if id.Valid {
		return integration.Side{ID: id.Int64}
	}
	return integration.Side{Name: name}
}

// problemText says what doesn't fit, with the accounts' names, in the words
// the card and the editor show.
func problemText(problem string, c reviewCard) string {
	own, other := c.From, c.To
	if c.Direction == "in" {
		own, other = c.To, c.From
	}
	name := func(p party, fallback string) string {
		if p.Name == "" {
			return fallback
		}
		return p.Name
	}
	switch problem {
	case integration.ProblemOtherIsMine:
		return name(other, "The other side") + " is one of your accounts. Money between your own accounts is a transfer."
	case integration.ProblemOtherNotMine:
		return "A transfer goes between two of your accounts, and " + name(other, "the other side") + " isn't one of them."
	case integration.ProblemSameAccount:
		return "A transfer needs two different accounts — this one goes from " + name(own, "an account") + " to itself."
	case integration.ProblemDirection:
		if c.Direction == "in" {
			return "The money came into " + name(own, "your account") + ", so it can't be a withdrawal."
		}
		return "The money went out of " + name(own, "your account") + ", so it can't be a deposit."
	case integration.ProblemOtherKind:
		if c.Type == integration.TypeDeposit {
			return name(other, "The other side") + " is someone you pay, and a deposit comes from someone who pays you."
		}
		return name(other, "The other side") + " is someone who pays you, and a withdrawal goes to someone you pay."
	case integration.ProblemOwnNotMine:
		if c.Direction == "in" {
			return name(own, "That account") + " isn't one of your accounts, so the money can't have come into it."
		}
		return name(own, "That account") + " isn't one of your accounts, so it can't have paid."
	case integration.ProblemSwapped:
		// the card shows the sides the right way round; this says how they
		// are stored
		if c.Direction == "in" {
			return "It was saved the wrong way round, with " + name(own, "your account") + " as who paid and " +
				name(other, "the payer") + " as the account it came into."
		}
		return "It was saved the wrong way round, with " + name(own, "your account") + " as who was paid and " +
			name(other, "the payee") + " as the account that paid."
	}
	return ""
}

// accountParty resolves one side of a transaction for display.
func (h *Handler) accountParty(ctx context.Context, id sql.NullInt64, name string) party {
	var p party
	full := strings.TrimSpace(name)
	if id.Valid {
		var fname, ftype sql.NullString
		_ = h.db.QueryRowContext(ctx, `SELECT name, type FROM firefly_accounts WHERE firefly_id = ?`, id.Int64).Scan(&fname, &ftype)
		if fname.Valid && fname.String != "" {
			full = fname.String
			p.Mine = ftype.String == "asset"
		} else if n := h.lookupAccountName(ctx, id.Int64); n != "" {
			full = n
			p.Mine = h.isAssetName(ctx, n)
		}
	} else if full != "" {
		p.Mine = h.isAssetName(ctx, full)
	}
	if full == "" || full == "(no name)" {
		// firefly's placeholder for an unnamed account is not a payee
		return party{}
	}
	if p.Mine {
		p.Name = shortAccountName(full)
		if p.Name != full {
			p.Full = full
		}
		return p
	}
	p.Name, p.Place = splitPlace(full)
	return p
}

func (h *Handler) isAssetName(ctx context.Context, name string) bool {
	var n int
	_ = h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM firefly_accounts WHERE LOWER(name) = LOWER(?) AND type = 'asset'`, name).Scan(&n)
	return n > 0
}

// shortAccountName turns a firefly asset name into what a person calls it:
// "Tata Neu HDFC Bank Credit Card" → "Tata Neu card", "Axis Bank Ace Credit
// Card" → "Axis Ace card". Bank accounts keep their name ("HDFC Bank").
func shortAccountName(full string) string {
	lower := strings.ToLower(full)
	if !strings.HasSuffix(lower, "credit card") {
		return full
	}
	words := strings.Fields(full[:len(full)-len("credit card")])
	var keep []string
	for i, w := range words {
		lw := strings.ToLower(w)
		if lw == "bank" || lw == "federal" || (lw == "hdfc" && i > 0) {
			continue
		}
		keep = append(keep, w)
	}
	if len(keep) == 0 {
		return full
	}
	return strings.Join(keep, " ") + " card"
}

// splitPlace separates a payee's name from its locality, the way the user
// names them: "Chai Corner, Market Road".
func splitPlace(full string) (string, string) {
	name, place, ok := strings.Cut(full, ", ")
	if !ok {
		return full, ""
	}
	return strings.TrimSpace(name), strings.TrimSpace(place)
}

var (
	cardNarration = regexp.MustCompile(`^CARD/[0-9a-fA-F]+/([^/]+)/`)
	upiNarration  = regexp.MustCompile(`^UPI-([^-]+?)(?:-|$)`)
	// the payer on a bank credit: "NEFT CR-<IFSC>-<payer>-…",
	// "ACH C- <payer>-<ref>", "IMPS-<ref>-<payer>-…"
	neftNarration = regexp.MustCompile(`^NEFT CR-[A-Z]{4}0[A-Z0-9]{6}-([^-]+)-`)
	achNarration  = regexp.MustCompile(`^ACH [CD]- ?([^-]+)-`)
	impsNarration = regexp.MustCompile(`^IMPS-\d+-([^-]+)-`)
)

// bankSaid is what the bank called the other side, for the card's small
// print: the merchant on a card alert, the payee on a UPI line.
func bankSaid(narration, mode string) string {
	n := strings.TrimSpace(narration)
	if mode == manualMode {
		// "Card statement 23Nov25-22Dec25: 10/12/2025 17:49 UPI-A VENDOR 45.00 · hint"
		if _, line, ok := strings.Cut(n, ": "); ok {
			line, _, _ = strings.Cut(line, " · ")
			return "Statement: " + strings.TrimSpace(line)
		}
		return n
	}
	if m := cardNarration.FindStringSubmatch(n); m != nil {
		return titleCase(strings.TrimSpace(m[1]))
	}
	if m := upiNarration.FindStringSubmatch(n); m != nil {
		return titleCase(strings.TrimSpace(m[1]))
	}
	for _, re := range []*regexp.Regexp{neftNarration, achNarration, impsNarration} {
		if m := re.FindStringSubmatch(n); m != nil && strings.TrimSpace(m[1]) != "" {
			return titleCase(strings.TrimSpace(m[1]))
		}
	}
	if len(n) > 60 {
		return n[:57] + "…"
	}
	return n
}

// manualHint pulls a reconciliation hint out of a manual row's narration
// ("… · 45.00 was a cold coffee on 8 Dec").
func manualHint(narration string) string {
	_, rest, ok := strings.Cut(narration, " · ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(rest)
}

// humanNote reads fold.money's notes field: free text, or the structured
// {"note": …, "account": …} some notes carry.
func humanNote(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "{") {
		var v struct {
			Note string `json:"note"`
		}
		if json.Unmarshal([]byte(raw), &v) == nil {
			return strings.TrimSpace(v.Note)
		}
	}
	return raw
}

// titleCase softens an ALL-CAPS (or all-lowercase) bank string ("MUZAMMIL",
// "gauri shankar enterprises") into a name. Mixed-case input and UPI
// handles are left alone.
func titleCase(s string) string {
	if s == "" || strings.ContainsAny(s, "@.") || (s != strings.ToUpper(s) && s != strings.ToLower(s)) {
		return s
	}
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		switch w {
		case "llp", "pvt", "ltd", "upi", "hdfc", "sbi", "icici", "atm":
			words[i] = strings.ToUpper(w)
			if w == "pvt" || w == "ltd" {
				words[i] = strings.ToUpper(w[:1]) + w[1:]
			}
		default:
			r := []rune(w)
			r[0] = unicode.ToUpper(r[0])
			words[i] = string(r)
		}
	}
	return strings.Join(words, " ")
}

// formatINR renders paise the Indian way, without trailing zeros:
// 123456789 → "₹12,34,567.89", 20000 → "₹200".
func formatINR(p int64) string {
	if p < 0 {
		p = -p
	}
	return "₹" + groupIndian(p/100) + paisePart(p%100)
}

func formatForeign(p int64, cur string) string {
	if p < 0 {
		p = -p
	}
	return cur + " " + groupWestern(p/100) + paisePart(p%100)
}

func paisePart(p int64) string {
	if p == 0 {
		return ""
	}
	return fmt.Sprintf(".%02d", p)
}

// groupIndian groups the last three digits, then pairs: 1234567 → 12,34,567.
func groupIndian(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	head, tail := s[:len(s)-3], s[len(s)-3:]
	var parts []string
	for len(head) > 2 {
		parts = append([]string{head[len(head)-2:]}, parts...)
		head = head[:len(head)-2]
	}
	if head != "" {
		parts = append([]string{head}, parts...)
	}
	return strings.Join(parts, ",") + "," + tail
}

func groupWestern(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// spokenDay is a date the way a person says it, in IST: "Today",
// "Yesterday", "Tue 30 Dec", and the year only when it isn't this year.
func spokenDay(t, now time.Time) string {
	t, now = t.In(ist), now.In(ist)
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	today := time.Date(ny, nm, nd, 0, 0, 0, 0, ist)
	day := time.Date(ty, tm, td, 0, 0, 0, 0, ist)
	switch {
	case day.Equal(today):
		return "Today"
	case day.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	}
	s := t.Format("Mon 2 Jan")
	s = strings.Replace(s, " Sep", " Sept", 1)
	if ty != ny {
		s += " " + strconv.Itoa(ty)
	}
	return s
}

// spokenClock: "1:21 pm", IST.
func spokenClock(t time.Time) string {
	return strings.ToLower(t.In(ist).Format("3:04 pm"))
}

// ---- deck ------------------------------------------------------------------

// deckResponse is GET /admin/ui/api/deck.
type deckResponse struct {
	Cards []reviewCard `json:"cards"`
	// Counts are for the whole pile under the current account filter, not
	// just this page.
	Counts struct {
		Review int `json:"review"`
		Later  int `json:"later"`
		// On the first page only: this pile across every account, and per
		// account (a transfer between two of them counts for both), for
		// the account picker.
		All       *int           `json:"all,omitempty"`
		ByAccount map[string]int `json:"byAccount,omitempty"`
	} `json:"counts"`
	// Next is the cursor for the following page; empty on the last one.
	Next string `json:"next,omitempty"`
}

const deckPageSize = 40

func (h *Handler) handleDeck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pile := q.Get("pile")
	if pile != "later" {
		pile = "review"
	}
	oldest := q.Get("order") == "oldest"
	limit := parsePositiveInt(q.Get("limit"), deckPageSize)
	if limit > 200 {
		limit = 200
	}

	where := []string{reviewable}
	var args []any
	if pile == "later" {
		where = append(where, "s.later_at IS NOT NULL")
	} else {
		where = append(where, "s.later_at IS NULL")
	}
	if id, err := strconv.ParseInt(strings.TrimSpace(q.Get("account")), 10, 64); err == nil {
		where = append(where, "("+effectiveAccountIDSQL("source")+" = ? OR "+effectiveAccountIDSQL("destination")+" = ?)")
		args = append(args, id, id)
	}
	accountWhere, accountArgs := where[2:], append([]any(nil), args...)

	// Keyset cursor "<timestamp>|<uuid>": cards leave the pile as they are
	// decided, so an offset would skip some.
	order := "DESC"
	cmp := "<"
	if oldest {
		order, cmp = "ASC", ">"
	}
	if ts, id, ok := strings.Cut(q.Get("after"), "|"); ok {
		where = append(where, "(COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp) "+cmp+" ? OR "+
			"(COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp) = ? AND s.fold_uuid "+cmp+" ?))")
		args = append(args, ts, ts, id)
	}

	rows, err := h.db.QueryContext(r.Context(), cardSelect+
		" WHERE "+strings.Join(where, " AND ")+
		" ORDER BY COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp) "+order+", s.fold_uuid "+order+
		" LIMIT ?", append(args, limit+1)...)
	if err != nil {
		h.apiError(w, http.StatusInternalServerError, "could not read the deck: "+err.Error())
		return
	}
	var raw []cardRow
	for rows.Next() {
		cr, err := scanCardRow(rows)
		if err != nil {
			rows.Close()
			h.apiError(w, http.StatusInternalServerError, "could not read a card: "+err.Error())
			return
		}
		raw = append(raw, cr)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var resp deckResponse
	for _, cr := range raw {
		resp.Cards = append(resp.Cards, h.buildCard(r.Context(), cr))
	}
	if len(resp.Cards) > limit {
		resp.Cards = resp.Cards[:limit]
		last := resp.Cards[limit-1]
		resp.Next = h.cursorFor(r.Context(), last.UUID)
	}
	for i := range resp.Cards {
		h.fillRefundOptions(r.Context(), &resp.Cards[i])
	}
	if resp.Cards == nil {
		resp.Cards = []reviewCard{}
	}

	count := func(extra string) int {
		clauses := append([]string{reviewable, extra}, accountWhere...)
		var n int
		_ = h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM staged_fold_txns s WHERE `+strings.Join(clauses, " AND "), accountArgs...).Scan(&n)
		return n
	}
	resp.Counts.Review = count("s.later_at IS NULL")
	resp.Counts.Later = count("s.later_at IS NOT NULL")
	if q.Get("after") == "" {
		all, by := h.pileCounts(r.Context(), where[1])
		resp.Counts.All, resp.Counts.ByAccount = &all, by
	}
	writeJSON(w, http.StatusOK, resp)
}

// pileCounts counts one pile (pileClause picks it) across every account, and
// per account on either side of the transaction.
func (h *Handler) pileCounts(ctx context.Context, pileClause string) (int, map[string]int) {
	var all int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM staged_fold_txns s WHERE `+reviewable+` AND `+pileClause).Scan(&all)
	by := map[string]int{}
	rows, err := h.db.QueryContext(ctx, `SELECT acct, COUNT(DISTINCT fold_uuid) FROM (
		SELECT s.fold_uuid, `+effectiveAccountIDSQL("source")+` AS acct FROM staged_fold_txns s WHERE `+reviewable+` AND `+pileClause+`
		UNION ALL
		SELECT s.fold_uuid, `+effectiveAccountIDSQL("destination")+` AS acct FROM staged_fold_txns s WHERE `+reviewable+` AND `+pileClause+`
	) WHERE acct IS NOT NULL GROUP BY acct`)
	if err != nil {
		return all, by
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if rows.Scan(&id, &n) == nil {
			by[strconv.FormatInt(id, 10)] = n
		}
	}
	return all, by
}

// cursorFor is the keyset cursor after a card: its stored timestamp (as the
// ORDER BY sees it) and its uuid.
func (h *Handler) cursorFor(ctx context.Context, uuid string) string {
	var ts string
	_ = h.db.QueryRowContext(ctx,
		`SELECT COALESCE(confirmed_txn_timestamp, txn_timestamp) FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&ts)
	return ts + "|" + uuid
}

// fillRefundOptions adds the "refund of which purchase?" choices to a refund
// card that still needs one picked.
func (h *Handler) fillRefundOptions(ctx context.Context, c *reviewCard) {
	if c.Refund == nil || !c.Refund.NeedsPick {
		return
	}
	v, _ := h.refundCardFor(ctx, c.UUID)
	for _, o := range v.Options {
		if o.Value == "" {
			continue
		}
		c.Refund.Options = append(c.Refund.Options, refundPick{Value: o.Value, Label: o.Label})
	}
}

// loadCard reads one card by uuid (any status, so the client can show what
// happened to it).
func (h *Handler) loadCard(ctx context.Context, uuid string) (reviewCard, error) {
	cr, err := scanCardRow(h.db.QueryRowContext(ctx, cardSelect+` WHERE s.fold_uuid = ?`, uuid))
	if err != nil {
		return reviewCard{}, err
	}
	c := h.buildCard(ctx, cr)
	h.fillRefundOptions(ctx, &c)
	return c, nil
}

// ---- actions ---------------------------------------------------------------

// cardEdit is the body of POST …/edit: only the fields present change.
type cardEdit struct {
	// Type is the kind of move: "withdrawal", "deposit" or "transfer" — one
	// the bank's direction allows. Sent with the payee it needs (a transfer
	// with the other account), so a change of type never leaves a card
	// half-made. "" hands the choice back to fold.
	Type     *string `json:"type"`
	Title    *string `json:"title"`
	Category *string `json:"category"`
	// Payee is the other side: who was paid on a spend, who paid on money in.
	Payee *string `json:"payee"`
	// Account is the user's own side: the account that paid, or that the
	// money came into.
	Account  *string `json:"account"`
	RefundOf *string `json:"refundOf"`
	Tags     *string `json:"tags"`
}

type actionResponse struct {
	OK         bool        `json:"ok"`
	Card       *reviewCard `json:"card,omitempty"`
	Message    string      `json:"message,omitempty"`
	FireflyURL string      `json:"fireflyUrl,omitempty"`
	// Unresolved names that could not be matched (e.g. a category that
	// doesn't exist in firefly).
	Unresolved []string `json:"unresolved,omitempty"`
}

func (h *Handler) handleCardEdit(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	var e cardEdit
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&e); err != nil {
		h.apiError(w, http.StatusBadRequest, "bad request body")
		return
	}
	if !h.isDecidable(r.Context(), w, uuid) {
		return
	}
	form, err := h.currentForm(r.Context(), uuid)
	if err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var typ string
	_ = h.db.QueryRowContext(r.Context(), `SELECT type FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&typ)
	otherSide, mySide := "destination_name", "source_name"
	if typ == "INCOMING" {
		otherSide, mySide = "source_name", "destination_name"
	}
	if e.Type != nil {
		form.Set("txn_type", strings.ToLower(strings.TrimSpace(*e.Type)))
	}
	if e.Title != nil {
		form.Set("description", strings.TrimSpace(*e.Title))
	}
	if e.Category != nil {
		form.Set("category_name", strings.TrimSpace(*e.Category))
	}
	if e.Payee != nil {
		form.Set(otherSide, strings.TrimSpace(*e.Payee))
	}
	if e.Account != nil {
		form.Set(mySide, strings.TrimSpace(*e.Account))
	}
	if e.Tags != nil {
		form.Set("tags", strings.TrimSpace(*e.Tags))
	}
	if e.RefundOf != nil {
		form.Set("refund_of", strings.TrimSpace(*e.RefundOf))
	}
	// A kind of move, and who is on the other side, must fit each other
	// before anything is saved: nothing half-right reaches the card.
	if e.Type != nil || e.Payee != nil || e.Account != nil {
		if msg := h.editFits(r.Context(), uuid, typ, form); msg != "" {
			c, _ := h.loadCard(r.Context(), uuid)
			writeJSON(w, http.StatusUnprocessableEntity, actionResponse{Card: &c, Message: msg})
			return
		}
	}
	unresolved, err := h.saveEdits(r.Context(), uuid, form)
	if err != nil {
		h.apiError(w, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}
	c, err := h.loadCard(r.Context(), uuid)
	if err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := actionResponse{OK: len(unresolved) == 0, Card: &c, Unresolved: unresolved}
	if len(unresolved) > 0 {
		resp.Message = "Couldn't match " + strings.Join(unresolved, ", ")
	}
	writeJSON(w, http.StatusOK, resp)
}

// editFits checks an edit before it is saved: the type is one the bank's
// direction allows, and the two sides fit it. It returns what doesn't fit,
// in the card's words, or "" when the edit can be saved.
func (h *Handler) editFits(ctx context.Context, uuid, dir string, form url.Values) string {
	t := strings.ToLower(strings.TrimSpace(form.Get("txn_type")))
	cur, err := h.loadCard(ctx, uuid)
	if err != nil {
		return ""
	}
	if formHas(form, "txn_type") && t != "" && !integration.TypeAllowed(dir, t) {
		cur.Type = t
		return problemText(integration.ProblemDirection, cur)
	}
	confirmed := t
	if !formHas(form, "txn_type") && cur.TypeChosen {
		confirmed = cur.Type
	}
	effType := confirmed
	if effType == "" {
		effType = cur.Type
	}
	src, dst, bad := h.resolveSides(ctx, dir, effType, strings.TrimSpace(form.Get("source_name")), strings.TrimSpace(form.Get("destination_name")))
	if effType == integration.TypeTransfer && len(bad) > 0 {
		other := strings.TrimSpace(form.Get("destination_name"))
		if dir == "INCOMING" {
			other = strings.TrimSpace(form.Get("source_name"))
		}
		return "A transfer goes between two of your accounts, and " + other + " isn't one of them."
	}
	tc := integration.CheckType(ctx, h.db, dir, confirmed, "", src, dst)
	if tc.Problem == integration.ProblemNone || tc.Problem == integration.ProblemOwnNotMine {
		return ""
	}
	// say it with the names the card would show after the edit
	next := cur
	next.Type = tc.Type
	sides := func(sd integration.Side) party {
		return h.accountParty(ctx, sql.NullInt64{Int64: sd.ID, Valid: sd.ID != 0}, sd.Name)
	}
	next.From, next.To = sides(src), sides(dst)
	return problemText(tc.Problem, next)
}

// currentForm is the row's effective values in the review form's shape, so
// an edit to one field can go through saveEdits (which writes every field)
// without disturbing the others.
func (h *Handler) currentForm(ctx context.Context, uuid string) (url.Values, error) {
	_, edit, _, err := h.fetchDetail(ctx, uuid)
	if err != nil {
		return nil, err
	}
	f := url.Values{}
	f.Set("destination_name", edit.DestinationAccountName)
	f.Set("source_name", edit.SourceAccountName)
	// An empty category or budget is sent only when the edit clears it: in
	// the review form an empty field means "explicitly none", and a card
	// edit to the title must not turn "not suggested yet" into that.
	if edit.CategoryName != "" {
		f.Set("category_name", edit.CategoryName)
	}
	if edit.BudgetName != "" {
		f.Set("budget_name", edit.BudgetName)
	}
	f.Set("description", edit.Description)
	f.Set("tags", edit.Tags)
	f.Set("amount", edit.Amount)
	if edit.ForeignCurrency != "" {
		f.Set("foreign_amount", edit.ForeignAmount)
	}
	f.Set("date", edit.Date)
	f.Set("time", edit.Time)
	return f, nil
}

// isDecidable: the row exists and is still waiting for a decision. Writes
// the API error itself when it isn't.
func (h *Handler) isDecidable(ctx context.Context, w http.ResponseWriter, uuid string) bool {
	var status string
	err := h.db.QueryRowContext(ctx, `SELECT status FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		h.apiError(w, http.StatusNotFound, "that transaction no longer exists")
		return false
	case err != nil:
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return false
	case status == "pushed":
		h.apiError(w, http.StatusConflict, "already in Firefly")
		return false
	case status == "skipped":
		h.apiError(w, http.StatusConflict, "this one was skipped")
		return false
	case status == "pending":
		h.apiError(w, http.StatusConflict, "still being classified — try again in a minute")
		return false
	}
	return true
}

// handleCardSend sends one card to firefly: the right swipe.
func (h *Handler) handleCardSend(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if h.pusher == nil {
		h.apiError(w, http.StatusServiceUnavailable, "sending is unavailable")
		return
	}
	if !h.isDecidable(r.Context(), w, uuid) {
		return
	}
	c, err := h.loadCard(r.Context(), uuid)
	if err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(c.Blockers) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, actionResponse{Card: &c, Message: blockerMessage(c)})
		return
	}
	report, err := h.pusher.Push(r.Context(), uuid, true)
	if err != nil {
		h.log.Warn("review: send refused", "fold_uuid", uuid, "err", err)
		c2, _ := h.loadCard(r.Context(), uuid)
		writeJSON(w, http.StatusBadGateway, actionResponse{Card: &c2, Message: fireflyReason(err)})
		return
	}
	_, _ = h.db.ExecContext(r.Context(), `UPDATE staged_fold_txns SET later_at = NULL WHERE fold_uuid = ?`, uuid)
	resp := actionResponse{OK: true, Message: fmt.Sprintf("sent (%s)", report.Action)}
	if h.fireflyPublicURL != "" && report.FireflyGroupID > 0 {
		resp.FireflyURL = h.fireflyPublicURL + "/transactions/show/" + strconv.FormatInt(report.FireflyGroupID, 10)
	}
	writeJSON(w, http.StatusOK, resp)
}

// fireflyReason says why a send didn't go, for the card: what Firefly
// objected to in its own words, without the HTTP envelope around them. The
// whole error goes to the log for whoever needs to dig.
func fireflyReason(err error) string {
	var fe *firefly.Error
	if errors.As(err, &fe) {
		var env struct {
			Message string              `json:"message"`
			Errors  map[string][]string `json:"errors"`
		}
		_ = json.Unmarshal([]byte(fe.Body), &env)
		fields := make([]string, 0, len(env.Errors))
		for f := range env.Errors {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		detail := ""
		for _, f := range fields {
			if msgs := env.Errors[f]; len(msgs) > 0 && strings.TrimSpace(msgs[0]) != "" {
				detail = strings.TrimSpace(msgs[0])
				break
			}
		}
		switch {
		case fe.Status == http.StatusUnauthorized || fe.Status == http.StatusForbidden:
			return "Firefly turned fold's access token away — it may have expired."
		case detail != "":
			return "Firefly said: " + detail
		case strings.TrimSpace(env.Message) != "":
			return "Firefly said: " + strings.TrimSpace(env.Message)
		case fe.Status >= 500:
			return fmt.Sprintf("Firefly had a problem of its own (HTTP %d). Try again in a minute.", fe.Status)
		default:
			return fmt.Sprintf("Firefly didn't take it (HTTP %d).", fe.Status)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Firefly didn't answer in time. Try again in a minute."
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "Couldn't reach Firefly. Try again in a minute."
	}
	return "Firefly didn't take it: " + err.Error()
}

// blockerMessage says what to fix, in the words the card uses.
func blockerMessage(c reviewCard) string {
	for _, b := range c.Blockers {
		switch b {
		case blockHold:
			return "On hold: " + c.Hold
		case blockType, blockSwap, blockMine:
			return c.ProblemText
		case blockTitleEmpty:
			return "Give it a title first"
		case blockTitleBlank:
			return "Fill in the blank first"
		case blockPayee:
			if c.ProblemText != "" {
				return c.ProblemText
			}
			if c.Type == integration.TypeTransfer {
				return "Say which of your accounts it moved to (or from) first"
			}
			return "Say who was paid (or who paid) first"
		case blockSource:
			if c.ProblemText != "" {
				return c.ProblemText
			}
			return "Say which of your accounts it was first"
		}
	}
	return "Not ready to send"
}

// handleCardLater moves a card to the Later pile (or back).
func (h *Handler) handleCardLater(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	var body struct {
		Later bool `json:"later"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		h.apiError(w, http.StatusBadRequest, "bad request body")
		return
	}
	if !h.isDecidable(r.Context(), w, uuid) {
		return
	}
	q := `UPDATE staged_fold_txns SET later_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`
	if body.Later {
		q = `UPDATE staged_fold_txns SET later_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`
	}
	if _, err := h.db.ExecContext(r.Context(), q, uuid); err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, _ := h.loadCard(r.Context(), uuid)
	writeJSON(w, http.StatusOK, actionResponse{OK: true, Card: &c})
}

// handleCardHold puts a card on hold with a reason ("" releases it).
func (h *Handler) handleCardHold(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		h.apiError(w, http.StatusBadRequest, "bad request body")
		return
	}
	if !h.isDecidable(r.Context(), w, uuid) {
		return
	}
	var reason any
	if s := strings.TrimSpace(body.Reason); s != "" {
		reason = s
	}
	if _, err := h.db.ExecContext(r.Context(),
		`UPDATE staged_fold_txns SET hold_reason = ?, updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`, reason, uuid); err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, _ := h.loadCard(r.Context(), uuid)
	writeJSON(w, http.StatusOK, actionResponse{OK: true, Card: &c})
}

// handleCardSkip: never send this one (a duplicate alert, something never
// billed). Reversible with restore.
func (h *Handler) handleCardSkip(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if !h.isDecidable(r.Context(), w, uuid) {
		return
	}
	if _, err := h.db.ExecContext(r.Context(),
		`UPDATE staged_fold_txns SET status = 'skipped', later_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`, uuid); err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{OK: true})
}

// handleCardRestore brings a skipped row back for review.
func (h *Handler) handleCardRestore(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	res, err := h.db.ExecContext(r.Context(),
		`UPDATE staged_fold_txns SET status = 'needs_review', updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ? AND status = 'skipped'`, uuid)
	if err != nil {
		h.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.apiError(w, http.StatusConflict, "only a skipped transaction can be restored")
		return
	}
	c, _ := h.loadCard(r.Context(), uuid)
	writeJSON(w, http.StatusOK, actionResponse{OK: true, Card: &c})
}

// ---- suggestions -------------------------------------------------------------

type suggestion struct {
	Value string `json:"value"`
	// Hint is small print beside the value ("12×", "last used 3 Mar").
	Hint string `json:"hint,omitempty"`
}

type suggestResponse struct {
	Titles     []suggestion `json:"titles"`
	Categories []suggestion `json:"categories"`
	// Payees the user picked before for the same name on the bank line
	// ("Ramesh S" → Chai Corner).
	Payees []suggestion `json:"payees"`
	// Accounts this account usually moves money with — the likely other
	// side of a transfer, most used first ("Kestrel card · 12×").
	Accounts []suggestion `json:"accounts"`
}

// rawNarrationTitle: a firefly title that is really a pasted bank line
// (a row once pushed without a title) — not something to suggest.
var rawNarrationTitle = regexp.MustCompile(`^(CARD/|UPI[-/]|NEFT|IMPS|ACH |RTGS|NWD|BPPY)`)

// handleCardSuggest offers what the user wrote before for this payee — their
// own past titles and categories — so a blank is usually one tap.
func (h *Handler) handleCardSuggest(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	ctx := r.Context()
	var dstID sql.NullInt64
	var dstName, narration, mode string
	err := h.db.QueryRowContext(ctx, `SELECT `+effectiveAccountIDSQL("destination")+`,
		COALESCE(NULLIF(s.confirmed_destination_account_name, ''), NULLIF(s.proposed_destination_account_name, ''), ''),
		s.narration, s.mode
		FROM staged_fold_txns s WHERE s.fold_uuid = ?`, uuid).Scan(&dstID, &dstName, &narration, &mode)
	if err != nil {
		h.apiError(w, http.StatusNotFound, "not found")
		return
	}
	resp := suggestResponse{Titles: []suggestion{}, Categories: []suggestion{}, Payees: []suggestion{}, Accounts: []suggestion{}}
	resp.Payees = h.payeesForBankName(ctx, narration, mode)
	resp.Accounts = h.transferPartners(ctx, uuid)
	if !dstID.Valid && dstName != "" {
		if id := h.resolveAccountID(ctx, "destination", dstName); id != 0 {
			dstID = sql.NullInt64{Int64: id, Valid: true}
		}
	}
	if dstID.Valid {
		rows, err := h.db.QueryContext(ctx, `
			SELECT description, COUNT(*), MAX(date) FROM firefly_txns
			WHERE destination_account_id = ? AND description <> '' AND description NOT LIKE '%\_\_\_%' ESCAPE '\'
			GROUP BY description ORDER BY MAX(date) DESC LIMIT 8`, dstID.Int64)
		if err == nil {
			for rows.Next() {
				var s suggestion
				var n int
				var last string
				if rows.Scan(&s.Value, &n, &last) == nil && !rawNarrationTitle.MatchString(s.Value) {
					s.Hint = pastUseHint(n, last)
					resp.Titles = append(resp.Titles, s)
				}
			}
			rows.Close()
		}
		rows, err = h.db.QueryContext(ctx, `
			SELECT category_name, COUNT(*) FROM firefly_txns
			WHERE destination_account_id = ? AND category_name IS NOT NULL AND category_name <> ''
			GROUP BY category_name ORDER BY COUNT(*) DESC LIMIT 4`, dstID.Int64)
		if err == nil {
			for rows.Next() {
				var s suggestion
				var n int
				if rows.Scan(&s.Value, &n) == nil {
					s.Hint = fmt.Sprintf("%d× here", n)
					resp.Categories = append(resp.Categories, s)
				}
			}
			rows.Close()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// transferPartners lists the accounts the row's own account has moved money
// with before, most often first: the likely other side if it is a transfer.
// An own account the other side already names (a card's same-named payee)
// comes first of all.
func (h *Handler) transferPartners(ctx context.Context, uuid string) []suggestion {
	out := []suggestion{}
	c, err := h.loadCard(ctx, uuid)
	if err != nil {
		return out
	}
	own, other := c.From, c.To
	if c.Direction == "in" {
		own, other = c.To, c.From
	}
	ownID := integration.AssetByName(ctx, h.db, firstNonEmpty(own.Full, own.Name))
	if ownID == 0 {
		return out
	}
	seen := map[int64]bool{ownID: true}
	if id := integration.AssetByName(ctx, h.db, firstNonEmpty(other.Full, other.Name)); id != 0 && !seen[id] {
		seen[id] = true
		out = append(out, suggestion{Value: h.lookupAccountName(ctx, id), Hint: "named on this one"})
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT CASE WHEN source_account_id = ? THEN destination_account_id ELSE source_account_id END AS partner,
		       COUNT(*), MAX(date)
		FROM firefly_txns
		WHERE txn_type = 'transfer' AND (source_account_id = ? OR destination_account_id = ?)
		GROUP BY partner ORDER BY COUNT(*) DESC, MAX(date) DESC LIMIT 6`, ownID, ownID, ownID)
	if err != nil {
		return out
	}
	type hit struct {
		id   int64
		n    int
		last string
	}
	var hits []hit
	for rows.Next() {
		var x hit
		if rows.Scan(&x.id, &x.n, &x.last) == nil {
			hits = append(hits, x)
		}
	}
	rows.Close()
	for _, x := range hits {
		if seen[x.id] || integration.AssetByName(ctx, h.db, h.lookupAccountName(ctx, x.id)) != x.id {
			continue // itself, already offered, or no longer one of the user's accounts
		}
		seen[x.id] = true
		out = append(out, suggestion{Value: h.lookupAccountName(ctx, x.id), Hint: pastUseHint(x.n, x.last)})
	}
	return out
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// payeesForBankName finds the payees earlier transactions with the same
// name on their bank line went to: the notes of firefly's own history hold
// those lines verbatim.
func (h *Handler) payeesForBankName(ctx context.Context, narration, mode string) []suggestion {
	out := []suggestion{}
	name := ""
	if m := cardNarration.FindStringSubmatch(narration); m != nil {
		name = strings.TrimSpace(m[1])
	} else if m := upiNarration.FindStringSubmatch(narration); m != nil {
		name = strings.TrimSpace(m[1])
	}
	if mode == manualMode || len(name) < 4 || strings.Contains(name, "@") {
		return out
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT destination_account_name, COUNT(*) FROM firefly_txns
		WHERE txn_type = 'withdrawal' AND notes LIKE ? AND destination_account_name IS NOT NULL AND destination_account_name <> ''
		GROUP BY destination_account_name ORDER BY COUNT(*) DESC LIMIT 4`, "%"+name+"%")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s suggestion
		var n int
		if rows.Scan(&s.Value, &n) == nil {
			s.Hint = fmt.Sprintf("%d×", n)
			out = append(out, s)
		}
	}
	return out
}

func pastUseHint(n int, last string) string {
	when := ""
	if t, ok := parseDBTime(last); ok {
		when = spokenDay(t, time.Now())
	}
	switch {
	case n > 1 && when != "":
		return fmt.Sprintf("%d×, last %s", n, when)
	case when != "":
		return when
	}
	return ""
}

// handleOptions returns the pickers' full lists: categories (most used
// first) and payees (for autocomplete), fetched once per deck.
func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type account struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Short string `json:"short"`
	}
	type opts struct {
		Categories []string `json:"categories"`
		// Payees: who you have paid (firefly's expense accounts).
		Payees []string `json:"payees"`
		// Payers: who has paid you (firefly's revenue accounts).
		Payers []string `json:"payers"`
		// Accounts: your own accounts — the only thing a transfer can go
		// to or come from. Never offered as a payee or a payer.
		Accounts []account `json:"accounts"`
		// SyncedAt: when these lists last came from Firefly (RFC3339).
		SyncedAt string `json:"syncedAt"`
	}
	o := opts{Categories: []string{}, Payees: []string{}, Payers: []string{}, Accounts: []account{}, SyncedAt: rfc3339OrEmpty(h.accountsSyncedAt(ctx))}
	rows, err := h.db.QueryContext(ctx, `
		SELECT category_name FROM firefly_txns WHERE category_name IS NOT NULL AND category_name <> ''
		GROUP BY category_name ORDER BY COUNT(*) DESC, category_name`)
	if err == nil {
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				o.Categories = append(o.Categories, s)
			}
		}
		rows.Close()
	}
	// then the categories nobody has used yet (made in Firefly, or from this
	// picker a minute ago), alphabetically
	seenCat := map[string]bool{}
	for _, c := range o.Categories {
		seenCat[strings.ToLower(c)] = true
	}
	rows, err = h.db.QueryContext(ctx, `SELECT name FROM firefly_categories WHERE name <> '' ORDER BY name COLLATE NOCASE`)
	if err == nil {
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil && !seenCat[strings.ToLower(s)] {
				seenCat[strings.ToLower(s)] = true
				o.Categories = append(o.Categories, s)
			}
		}
		rows.Close()
	}
	mine := map[string]bool{}
	rows, err = h.db.QueryContext(ctx, `SELECT firefly_id, name FROM firefly_accounts WHERE type = 'asset' AND active = 1 AND name <> '' ORDER BY name COLLATE NOCASE`)
	if err == nil {
		for rows.Next() {
			var a account
			if rows.Scan(&a.ID, &a.Name) == nil {
				a.Short = shortAccountName(a.Name)
				o.Accounts = append(o.Accounts, a)
				mine[strings.ToLower(a.Name)] = true
			}
		}
		rows.Close()
	}
	payees, _ := h.listAccountsByKind(ctx, "destination")
	for _, p := range payees {
		if !mine[strings.ToLower(p.Name)] {
			o.Payees = append(o.Payees, p.Name)
		}
	}
	rows, err = h.db.QueryContext(ctx, `
		SELECT name FROM (
			SELECT source_account_name AS name FROM firefly_txns
			WHERE txn_type = 'deposit' AND source_account_name IS NOT NULL AND source_account_name NOT IN ('', '(no name)')
			UNION
			SELECT name FROM firefly_accounts WHERE type = 'revenue' AND active = 1 AND name NOT IN ('', '(no name)')
		) ORDER BY name COLLATE NOCASE`)
	if err == nil {
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil && !mine[strings.ToLower(s)] {
				o.Payers = append(o.Payers, s)
			}
		}
		rows.Close()
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	writeJSON(w, http.StatusOK, o)
}

// ---- plumbing ----------------------------------------------------------------

// withAPI guards the JSON API: same auth as the pages, plus a custom header
// on every write. A browser can't add a custom header to a cross-site
// request without a CORS preflight (which this server never answers), so
// the header is what keeps another site from driving these endpoints with
// the user's cookies.
func (h *Handler) withAPI(next http.HandlerFunc) http.Handler {
	return h.withAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Header.Get("X-Fold-UI") != "1" {
			h.apiError(w, http.StatusForbidden, "missing X-Fold-UI header")
			return
		}
		next(w, r)
	})
}

func (h *Handler) apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, actionResponse{OK: false, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleReview renders the deck's page shell; the cards arrive over the API.
func (h *Handler) handleReview(w http.ResponseWriter, r *http.Request) {
	accountOptions, _ := h.listFilterAccounts(r.Context())
	type acct struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Short string `json:"short"`
	}
	accounts := []acct{}
	for _, a := range accountOptions {
		if !h.isAssetName(r.Context(), a.Name) {
			continue
		}
		accounts = append(accounts, acct{ID: a.ID, Name: a.Name, Short: shortAccountName(a.Name)})
	}
	// Encoded here, not in the template: html/template escapes a value in an
	// attribute as text, which is not JSON.
	accountsJSON, _ := json.Marshal(accounts)
	h.render(w, h.reviewTmpl, map[string]any{
		"Title":        "Review",
		"Status":       "review",
		"Nav":          "review",
		"AccountsJSON": string(accountsJSON),
		"Flash":        flashFromCookie(r, w),
	})
}
