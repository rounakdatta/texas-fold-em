package ui

// months.go — the deck's month filter: review one month's cards, then the
// next, then the rest.
//
// A card belongs to the month its day falls in, the day the card shows:
// where the payment was made, on a trip, and at home (IST) otherwise. So a
// dinner at 7:30 pm on 31 January in New York — 6 am on 1 February at home —
// is January's, because its card says 31 Jan. A month never holds a card
// dated outside it.
//
// That is why months are worked out here and not in SQL: a trip's day needs
// its zone (whereabouts), and fold keeps its times in more than one shape
// (parseDBTime). The rows waiting for a decision are few — a backlog of a
// thousand-odd — and reading their times is cheap.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

var monthRe = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// parseMonth is a month filter as the deck asks for it ("2026-09"), or ""
// when s isn't one: anything else shows every month.
func parseMonth(s string) string {
	if monthRe.MatchString(s) {
		return s
	}
	return ""
}

// cardMonth is the month a card shows its day in: zone is where the payment
// was made (nil at home, which is IST).
func cardMonth(t time.Time, zone *time.Location) string {
	if zone == nil {
		zone = ist
	}
	return t.In(zone).Format("2006-01")
}

// monthRow is one row waiting for a decision, as the month filter and its
// counts see it.
type monthRow struct {
	uuid, month string // month "" when its time can't be read: it shows under every month only
	later       bool
	src, dst    sql.NullInt64 // each side's account, as the account filter reads it
}

// inAccount: the account filter's rule (either side), for counting in Go.
func (m monthRow) inAccount(id int64, ok bool) bool {
	return !ok || (m.src.Valid && m.src.Int64 == id) || (m.dst.Valid && m.dst.Int64 == id)
}

// waitingMonths reads every row still waiting for a decision, in either
// pile, with the month its card shows.
func (h *Handler) waitingMonths(ctx context.Context) ([]monthRow, error) {
	// where the trips were, read before the rows: the pool has one
	// connection, and whereabouts may need it (see cardRow)
	tl := h.whereabouts(ctx)
	rows, err := h.db.QueryContext(ctx, `SELECT s.fold_uuid, COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp),
		s.later_at IS NOT NULL, `+effectiveAccountIDSQL("source")+`, `+effectiveAccountIDSQL("destination")+`
		FROM staged_fold_txns s WHERE `+reviewable)
	if err != nil {
		return nil, fmt.Errorf("could not read the months: %w", err)
	}
	defer rows.Close()
	var out []monthRow
	for rows.Next() {
		var m monthRow
		var ts string
		if err := rows.Scan(&m.uuid, &ts, &m.later, &m.src, &m.dst); err != nil {
			return nil, fmt.Errorf("could not read a month: %w", err)
		}
		if t, ok := parseDBTime(ts); ok {
			var zone *time.Location
			if l, ok := tl.For(m.uuid); ok {
				zone = l.Zone
			}
			m.month = cardMonth(t, zone)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// monthClause narrows a deck query to one month's rows (of the ones read).
func monthClause(rows []monthRow, month string) (string, any) {
	ids := []string{}
	for _, m := range rows {
		if m.month == month {
			ids = append(ids, m.uuid)
		}
	}
	js, _ := json.Marshal(ids)
	return "s.fold_uuid IN (SELECT value FROM json_each(?))", string(js)
}

// monthCounts counts one pile's rows per month, under the account filter
// (not the month's: these are what the month picker offers), and in every
// month — which takes in a row whose time can't be read, as the deck does.
func monthCounts(rows []monthRow, later bool, account int64, hasAccount bool) (int, map[string]int) {
	all, by := 0, map[string]int{}
	for _, m := range rows {
		if m.later != later || !m.inAccount(account, hasAccount) {
			continue
		}
		all++
		if m.month != "" {
			by[m.month]++
		}
	}
	return all, by
}
