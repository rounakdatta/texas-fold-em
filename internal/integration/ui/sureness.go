package ui

// sureness.go — how sure fold is that a card can go as it stands, and the
// deck's Surest first order, which is built on it.
//
// A backlog in time order is a mix. Every few cards one asks for a word in
// its title or a closer look at who was paid, and the obvious ones can't
// simply go between them. Surest first puts the cards that go with one swipe,
// and that fold is sure of, first — to be sent at the pace of a swipe — then
// the ones it is less sure of, to read before sending; then the ones waiting
// on a word or a pick from you, surest first again; then what is on hold.
//
// Sureness is three bands, not the engine's number. The number is the
// model's own word (or a vote's share, or a lookup's agreement): 0.93 against
// 0.95 tells nobody anything, and ordering by it would scatter a day's
// payments across the deck for differences that aren't there. Within a band
// the deck keeps to time, newest first, so a day's payments stay together —
// half of knowing what a payment was is what else happened that day.
//
//   - sure: a suggestion the classifier would have accepted unasked — by its
//     own rules: confident (at or above its threshold), settled (not a card it
//     declined), and naming only accounts Firefly has — or a card where a
//     person chose who was paid, or a row a person added from a statement;
//   - fairly: a good guess, worth a look before it goes — and any card that
//     would make an account in Firefly (a new payee, an account of yours
//     Firefly doesn't have), however sure fold is of the rest: the name is
//     for a person to check, as the classifier itself asks;
//   - unsure: a guess, a card fold couldn't settle, or one that raises a doubt
//     of its own — a possible duplicate alert, a refund whose purchase isn't
//     picked, fold's case for holding it — which needs a decision fold can't
//     make.

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
)

// Sureness bands, surest first. Shared with static/review.js.
const (
	sureYes    = "sure"
	sureFairly = "fairly"
	sureNot    = "unsure"
)

// Why a card is in its band, when it isn't the engine's confidence alone.
const (
	sureChosen     = "chosen"      // a person chose who is on the other side
	sureManual     = "manual"      // a person added it from a statement line
	sureDuplicate  = "duplicate"   // fold.money flags it as a possible duplicate alert
	sureRefund     = "refund"      // a refund whose purchase isn't picked
	sureHold       = "hold"        // fold makes a case for holding it back
	sureUnsettled  = "unsettled"   // the engine declined it, or gave no score
	sureNewPayee   = "new-payee"   // sending it makes the other side in Firefly
	sureNewAccount = "new-account" // sending it makes one of your accounts in Firefly
)

// sureFairlyAt is where a guess becomes a good one: below it the model was
// barely committing (it declines under 0.5), as the list's old confidence
// gauge had it too.
const sureFairlyAt = 0.6

// cardSure is a card's sureness as the deck shows it.
type cardSure struct {
	Band string `json:"band"` // sure | fairly | unsure
	Why  string `json:"why,omitempty"`
	// Score is the engine's confidence, 0..1, when it gave one: the model's
	// own, a vote's share or a lookup's agreement, as the classifier
	// recorded it.
	Score *float64 `json:"score,omitempty"`
}

// sureFacts is what a card's sureness is made from, besides the card.
type sureFacts struct {
	score sql.NullFloat64 // the engine's confidence
	tier  sql.NullInt64   // the classifier's tier that made the suggestion
	// chosen: a person put someone other than fold's suggestion on the other
	// side of the move.
	chosen bool
	// newOther, newOwn: a side names an account Firefly doesn't have, so
	// sending the card makes one there — who was paid (or who paid), or one
	// of your own.
	newOther, newOwn bool
}

// sureness says how sure fold is of a card, from what the card says and how
// it came to say it. A doubt the card raises comes first: whatever the score,
// it needs a decision fold can't make. Then what a person decided; then the
// classifier's own rules for what it may accept unasked; then its
// confidence, in bands.
func sureness(c reviewCard, f sureFacts) cardSure {
	var s cardSure
	if f.score.Valid && f.score.Float64 > 0 {
		v := f.score.Float64
		s.Score = &v
	}
	band := sureNot
	switch {
	case s.Score == nil:
	case *s.Score >= classifier.DefaultConfidenceThreshold:
		band = sureYes
	case *s.Score >= sureFairlyAt:
		band = sureFairly
	}
	// a new name is for a person to check, however sure fold is of the rest
	atMostFairly := func(b string) string {
		if b == sureYes {
			return sureFairly
		}
		return b
	}
	// it declined: the model's confidence was in what it could say, not who
	unsettled := s.Score == nil || (f.tier.Valid && f.tier.Int64 == int64(classifier.TierHumanReview))
	switch {
	case c.Duplicate:
		s.Band, s.Why = sureNot, sureDuplicate
	case c.Refund != nil && c.Refund.NeedsPick:
		s.Band, s.Why = sureNot, sureRefund
	case c.Why != nil && c.Why.Hold != "":
		s.Band, s.Why = sureNot, sureHold
	case c.Manual:
		s.Band, s.Why = sureYes, sureManual
	case f.newOwn:
		// picking who was paid says nothing about your own account
		switch {
		case f.chosen:
			s.Band = sureFairly
		case unsettled:
			s.Band = sureNot
		default:
			s.Band = atMostFairly(band)
		}
		s.Why = sureNewAccount
	case f.chosen:
		s.Band, s.Why = sureYes, sureChosen
	case unsettled:
		s.Band, s.Why = sureNot, sureUnsettled
	case f.newOther:
		s.Band, s.Why = atMostFairly(band), sureNewPayee
	default:
		s.Band = band
	}
	return s
}

// choseWhoSQL: a person's last edit of the card put someone other than fold's
// suggestion on the other side of the move — who was paid, who paid, a
// transfer's other account. NULL when nobody has edited it.
//
// From the edit, not from the row's columns: every save writes each side
// whole, by name, so a suggestion kept while fixing the title lands in the
// confirmed columns too, sometimes as a same-named twin (a card bill's payee
// that shares the card's name saves as the card itself); and the columns move
// without a person (a reclassify rewrites the suggestion under a kept save,
// the repair swaps a backwards row). Each edit records, by name, what fold
// suggested and what the person left (feedback.RecordEdit; edits before the
// log began were backfilled from the rows).
const choseWhoSQL = `(SELECT LOWER(TRIM(COALESCE(json_extract(f.chosen_json, '$.payee'), '')))
	      <> LOWER(TRIM(COALESCE(json_extract(f.suggested_json, '$.payee'), '')))
	  FROM review_feedback f WHERE f.fold_uuid = s.fold_uuid AND f.action = 'edit'
	  ORDER BY f.at DESC, f.id DESC LIMIT 1)`

// sureFacts reads what sureness needs from a row, beyond the card built from it.
func (h *Handler) sureFacts(ctx context.Context, r cardRow, c reviewCard) sureFacts {
	f := sureFacts{score: r.score, tier: r.tier, chosen: r.choseWho.Valid && r.choseWho.Bool}
	if c.Manual {
		return f // every field a person's
	}
	other, own := storedSide{r.dstID, r.dstName}, storedSide{r.srcID, r.srcName}
	if r.typ == "INCOMING" {
		other, own = own, other
	}
	// push sends a side Firefly hasn't linked by its name, and Firefly finds or
	// makes an account of the kind the move needs there: yours an asset; the
	// other side an expense to pay, a revenue account to be paid by, or another
	// of yours
	otherKind := map[string]string{integration.TypeWithdrawal: "expense", integration.TypeDeposit: "revenue", integration.TypeTransfer: "asset"}[c.Type]
	f.newOther = h.isNewAccount(ctx, other, otherKind)
	f.newOwn = h.isNewAccount(ctx, own, "asset")
	return f
}

// storedSide is one side of a row as stored: an account's id, or the name of
// an account push asks Firefly to find or make.
type storedSide struct {
	id   sql.NullInt64
	name string
}

// isNewAccount: a side named, not linked, with no account of that name and
// kind in Firefly — push would make one. (A payee's revenue twin is another
// account: a merchant's first refund makes it.)
func (h *Handler) isNewAccount(ctx context.Context, s storedSide, kind string) bool {
	name := strings.TrimSpace(s.name)
	if s.id.Valid || name == "" || name == "(no name)" || kind == "" {
		return false
	}
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM firefly_accounts WHERE LOWER(name) = LOWER(?) AND type = ?`, name, kind).Scan(&n)
	return n == 0
}

// sureRank is a card's place in Surest first, smallest first: the cards that
// go with a swipe, by band (0–2); then those waiting on a word or a pick from
// you, by band (3–5); then what is on hold (6).
func sureRank(c reviewCard) int {
	if slices.Contains(c.Blockers, blockHold) {
		return 6
	}
	rank := 2
	switch c.Sure.Band {
	case sureYes:
		rank = 0
	case sureFairly:
		rank = 1
	}
	if len(c.Blockers) > 0 {
		rank += 3
	}
	return rank
}

// ---- the order -----------------------------------------------------------------

// rankedRow is a waiting row with its place in Surest first.
type rankedRow struct {
	row  cardRow
	rank int
}

// before reports whether a comes before b: by rank, then newest first, then
// by uuid — the keys the cursor carries. Time and uuid compare byte by byte,
// as SQLite's ORDER BY compares them.
func (a rankedRow) before(b rankedRow) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	if a.row.tsStr != b.row.tsStr {
		return a.row.tsStr > b.row.tsStr
	}
	return a.row.uuid > b.row.uuid
}

func sureCursor(r rankedRow) string {
	return strconv.Itoa(r.rank) + "|" + r.row.tsStr + "|" + r.row.uuid
}

func parseSureCursor(s string) (rankedRow, bool) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) != 3 {
		return rankedRow{}, false
	}
	rank, err := strconv.Atoi(parts[0])
	if err != nil {
		return rankedRow{}, false
	}
	return rankedRow{row: cardRow{tsStr: parts[1], uuid: parts[2]}, rank: rank}, true
}

// surestPage is one page of the deck in Surest first, after the cursor
// "<rank>|<timestamp>|<uuid>" (a keyset, like the timeline's: cards leave as
// they are decided, so an offset would skip some), and how many of the
// cards the query selects go with a swipe and are ones fold is sure of.
//
// A rank is only known once the card is built, so every waiting card is
// ranked for every page — from sureMemo, which builds only what changed.
func (h *Handler) surestPage(ctx context.Context, where []string, args []any, after string, limit int) ([]reviewCard, string, int, error) {
	raw, err := h.readCardRows(ctx, " WHERE "+strings.Join(where, " AND ")+
		" ORDER BY COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp) DESC, s.fold_uuid DESC", args...)
	if err != nil {
		return nil, "", 0, err
	}
	now := time.Now()
	mirror := h.accountsMirror(ctx)
	built := map[string]reviewCard{}
	ranked := make([]rankedRow, len(raw))
	sure := 0
	for i, r := range raw {
		fp := rowPrint(r)
		rank, ok := h.sure.rank(r.uuid, fp, mirror, now)
		if !ok {
			c := h.buildCard(ctx, r)
			built[r.uuid] = c
			rank = sureRank(c)
			h.sure.keep(r.uuid, fp, mirror, rank, now)
		}
		ranked[i] = rankedRow{row: r, rank: rank}
		if rank == 0 {
			sure++
		}
	}
	h.sure.prune(now)
	// read newest first, so a stable sort by rank leaves each band in time
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].rank < ranked[j].rank })

	start := 0
	if cur, ok := parseSureCursor(after); ok {
		start = sort.Search(len(ranked), func(i int) bool { return cur.before(ranked[i]) })
	}
	end := min(start+limit, len(ranked))
	cards := make([]reviewCard, 0, end-start)
	for _, rr := range ranked[start:end] {
		c, ok := built[rr.row.uuid]
		if !ok {
			c = h.buildCard(ctx, rr.row)
		}
		cards = append(cards, c)
	}
	var next string
	if end < len(ranked) && end > start {
		next = sureCursor(ranked[end-1])
	}
	return cards, next, sure, nil
}

// sureMemo keeps each waiting card's rank. Building a card means the checks
// push makes, which read the accounts mirror: near a millisecond a card on
// the live box, so a second for a backlog of a thousand, on every page. A
// rank is kept against the row it was made from — a fingerprint of every
// column the card reads, so an edit, a re-suggestion or a hold makes it anew
// — and against the accounts mirror as it was read (every account's name,
// kind and whether it is open: what decides whether a side is yours, who
// someone is, and whether Firefly has them yet). An hour is the longest a
// rank lives, for what neither notices: a name only the ledger's history
// knows.
type sureMemo struct {
	mu     sync.Mutex
	mirror string
	ranks  map[string]keptRank
}

type keptRank struct {
	fp   uint64
	rank int
	at   time.Time
}

const sureMemoTTL = time.Hour

func (m *sureMemo) rank(uuid string, fp uint64, mirror string, now time.Time) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.ranks[uuid]
	if !ok || m.mirror != mirror || k.fp != fp || now.Sub(k.at) > sureMemoTTL {
		return 0, false
	}
	return k.rank, true
}

func (m *sureMemo) keep(uuid string, fp uint64, mirror string, rank int, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ranks == nil || m.mirror != mirror {
		m.mirror, m.ranks = mirror, map[string]keptRank{}
	}
	m.ranks[uuid] = keptRank{fp: fp, rank: rank, at: now}
}

// prune lets go of ranks too old to use: the cards decided since.
func (m *sureMemo) prune(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, k := range m.ranks {
		if now.Sub(k.at) > sureMemoTTL {
			delete(m.ranks, id)
		}
	}
}

// rowPrint fingerprints every column a card is built from.
func rowPrint(r cardRow) uint64 {
	f := fnv.New64a()
	fmt.Fprintf(f, "%#v", r)
	return f.Sum64()
}

// accountsMirror fingerprints the accounts mirror as a rank reads it.
func (h *Handler) accountsMirror(ctx context.Context) string {
	rows, err := h.db.QueryContext(ctx, `SELECT firefly_id, name, type, active FROM firefly_accounts ORDER BY firefly_id`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	f := fnv.New64a()
	for rows.Next() {
		var id, active int64
		var name, kind string
		if rows.Scan(&id, &name, &kind, &active) == nil {
			fmt.Fprintf(f, "%d\x1f%s\x1f%s\x1f%d\x1e", id, name, kind, active)
		}
	}
	return strconv.FormatUint(f.Sum64(), 16)
}
