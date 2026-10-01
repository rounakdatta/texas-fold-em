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
//   - sure: a suggestion the classifier would have accepted unasked (at or
//     above its own threshold), a card where a person chose who was paid, or
//     a row a person added from a statement;
//   - fairly: a good guess, worth a look before it goes;
//   - unsure: a guess, no score at all, or a card that raises a doubt of its
//     own — a possible duplicate alert, a refund whose purchase isn't picked,
//     fold's case for holding it — which needs a decision fold can't make.

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
	sureChosen    = "chosen"    // a person chose who is on the other side
	sureManual    = "manual"    // a person added it from a statement line
	sureDuplicate = "duplicate" // fold.money flags it as a possible duplicate alert
	sureRefund    = "refund"    // a refund whose purchase isn't picked
	sureHold      = "hold"      // fold makes a case for holding it back
	sureUnscored  = "unscored"  // the engine couldn't settle it
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

// sureness says how sure fold is of a card, from what the card says and how
// it came to say it. A doubt the card raises comes first: whatever the score,
// it needs a decision fold can't make. Then a person's choice; then the
// engine's confidence, in bands.
func sureness(c reviewCard, score sql.NullFloat64, chosen bool) cardSure {
	var s cardSure
	if score.Valid && score.Float64 > 0 {
		v := score.Float64
		s.Score = &v
	}
	switch {
	case c.Duplicate:
		s.Band, s.Why = sureNot, sureDuplicate
	case c.Refund != nil && c.Refund.NeedsPick:
		s.Band, s.Why = sureNot, sureRefund
	case c.Why != nil && c.Why.Hold != "":
		s.Band, s.Why = sureNot, sureHold
	case c.Manual:
		s.Band, s.Why = sureYes, sureManual
	case chosen:
		s.Band, s.Why = sureYes, sureChosen
	case s.Score == nil:
		s.Band, s.Why = sureNot, sureUnscored
	case *s.Score >= classifier.DefaultConfidenceThreshold:
		s.Band = sureYes
	case *s.Score >= sureFairlyAt:
		s.Band = sureFairly
	default:
		s.Band = sureNot
	}
	return s
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

// storedSide is one side of a row as a pair of columns stores it: an
// account's id, or the name of an account push asks Firefly to create.
type storedSide struct {
	id   sql.NullInt64
	name string
}

// choseOther reports whether a person put someone on the other side of the
// move — who was paid, who paid, a transfer's other account — other than
// fold's suggestion. Every save writes each side whole, copying what the card
// showed, so a saved side that is the suggestion is a suggestion kept (a
// title rewritten, an amount taken from the statement), not a choice made:
// fold's own confidence still speaks for it.
func (h *Handler) choseOther(ctx context.Context, r cardRow) bool {
	saved, suggested := r.savedDst, r.suggestedDst
	if r.typ == "INCOMING" {
		saved, suggested = r.savedSrc, r.suggestedSrc
	}
	if !saved.id.Valid && strings.TrimSpace(saved.name) == "" {
		return false // never saved: the suggestion stands
	}
	if saved.id.Valid && suggested.id.Valid {
		return saved.id.Int64 != suggested.id.Int64
	}
	// A name on one side or both: the same account when the names agree (a
	// payee fold suggested by name may have been made in Firefly since).
	a, okA := h.storedName(ctx, saved)
	b, okB := h.storedName(ctx, suggested)
	if !okA || !okB {
		return false // an account nobody can name: the suggestion's word stands
	}
	return !strings.EqualFold(a, b)
}

// storedName is a stored side's name; ok is false for an id no copy of
// Firefly knows.
func (h *Handler) storedName(ctx context.Context, s storedSide) (string, bool) {
	if !s.id.Valid {
		return strings.TrimSpace(s.name), true
	}
	var name sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT name FROM firefly_accounts WHERE firefly_id = ?`, s.id.Int64).Scan(&name)
	if n := strings.TrimSpace(name.String); n != "" {
		return n, true
	}
	if n := strings.TrimSpace(h.lookupAccountName(ctx, s.id.Int64)); n != "" {
		return n, true
	}
	return "", false
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
	mirror := h.assetsMirror(ctx)
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
// — and against your own accounts as the mirror had them (which of them
// exist, by what names, open or closed), which decide whether a side is
// yours. An hour is the longest a rank lives, for what neither notices.
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

// assetsMirror is the part of the accounts mirror a rank reads: your own
// accounts, their names, and whether each is open.
func (h *Handler) assetsMirror(ctx context.Context) string {
	rows, err := h.db.QueryContext(ctx, `SELECT firefly_id, name, active FROM firefly_accounts WHERE type = 'asset' ORDER BY firefly_id`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, active int64
		var name string
		if rows.Scan(&id, &name, &active) == nil {
			fmt.Fprintf(&b, "%d\x1f%s\x1f%d\x1e", id, name, active)
		}
	}
	return b.String()
}
