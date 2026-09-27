package classifier

// creditback.go — a card's "credited back" alert is usually not money received.
//
// When a merchant takes a card authorisation and then lets it go (a ticket
// site holding seats, an airport kiosk, a one-dollar card check), the bank
// sends an alert that the amount "is credited back to your Card", naming no
// merchant. fold.money records it as money in. But the statement shows neither
// side: the charge was never billed, so its release is not a credit either.
// Sent to firefly, a credit-back books money that never arrived. Every one
// seen so far was exactly that, and each had to be caught by hand during a
// statement reconciliation.
//
// So the refund tier still books a credit-back the way a real refund would be
// (a deposit into the card, category Refund, linked to the charge it gives
// back, titled after it) and fold puts it on hold with the reason: the owner
// checks the statement once and skips both, or releases the hold when the
// statement shows both. A hold fold set is marked as fold's (hold_by), and one
// a person released is never set again (a 'release' in review_feedback).

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/refundref"
)

// creditBackLookback bounds how long before its credit-back the charge was.
// Authorisations are released within days; a month covers a slow hotel.
const creditBackLookback = 30 * 24 * time.Hour

// isCreditBack reports whether a staged row is a card's "credited back"
// alert with no merchant in it — a released authorisation, not a refund a
// merchant sent (those name the merchant, or say "refund").
func isCreditBack(staged StagedRow) bool {
	if staged.Type != "INCOMING" {
		return false
	}
	n := strings.ToLower(staged.Narration)
	if !strings.Contains(n, "credited back") || strings.Contains(n, "refund") {
		return false
	}
	if strings.TrimSpace(parseRefundPayload(staged.RawPayload).Merchant.Name) != "" {
		return false
	}
	// CARD/<ref>/<merchant>/<currency>/<amount>/INCOMING/<text>: the merchant
	// slot is empty ("CARD/7c1e…//USD/18.00/…").
	parts := strings.Split(strings.TrimSpace(staged.Narration), "/")
	return len(parts) >= 3 && strings.EqualFold(parts[0], "CARD") && strings.TrimSpace(parts[2]) == ""
}

// creditBackCharge is the charge a credit-back gives back.
type creditBackCharge struct {
	cand   RefundCandidate
	label  string // "USD 18.00" or "₹1,529.40"
	payee  string // who it was paid to, for the hold's reason
	status string
}

// findCreditBackCharge looks for the charge a credit-back releases: money
// out on the same card, for the same amount (the foreign amount when there is
// one — the rupee side of the two can differ by the day's rate), within the
// month before. The latest such charge not already given back wins.
func (c *Classifier) findCreditBackCharge(ctx context.Context, staged StagedRow, foldAccountID string, when time.Time) *creditBackCharge {
	if foldAccountID == "" || when.IsZero() {
		return nil
	}
	var fAmt sql.NullInt64
	var fCur sql.NullString
	_ = c.db.QueryRowContext(ctx, `SELECT foreign_amount_paise, foreign_currency FROM staged_fold_txns WHERE fold_uuid = ?`,
		staged.FoldUUID).Scan(&fAmt, &fCur)
	foreign := fAmt.Valid && fAmt.Int64 > 0 && fCur.Valid && strings.TrimSpace(fCur.String) != "" && !strings.EqualFold(strings.TrimSpace(fCur.String), "INR")

	match := `COALESCE(s.confirmed_amount_paise, s.amount_paise) = ?`
	args := []any{staged.AmountPaise}
	if foreign {
		match = `UPPER(COALESCE(s.foreign_currency, '')) = ? AND COALESCE(s.confirmed_foreign_amount_paise, s.foreign_amount_paise) = ?`
		args = []any{strings.ToUpper(strings.TrimSpace(fCur.String)), fAmt.Int64}
	}
	claimed, _ := refundClaims(ctx, c.db, staged.FoldUUID)
	fromDay, toDay := dayRange(when.Add(-creditBackLookback), when)
	rows, err := c.db.QueryContext(ctx, `
		SELECT s.fold_uuid, s.status, `+effAmountSQL+`, `+effWhenSQL+`, `+effDescSQL+`, `+effDstNameSQL+`,
		       COALESCE(s.firefly_txn_id, 0), `+effTagsSQL+`, COALESCE(s.narration, '')
		FROM staged_fold_txns s
		WHERE s.type = 'OUTGOING' AND s.fold_uuid <> ? AND `+jsonAccountSQL+` = ?
		  AND `+match+`
		  AND substr(`+effWhenSQL+`, 1, 10) BETWEEN ? AND ?`,
		append(append([]any{staged.FoldUUID, foldAccountID}, args...), fromDay, toDay)...)
	if err != nil {
		return nil
	}
	var found []creditBackCharge
	for rows.Next() {
		var (
			ch                     creditBackCharge
			uuid, whenStr, tagsJS  string
			narration, merchantRaw string
			journal                int64
		)
		if rows.Scan(&uuid, &ch.status, &ch.cand.AmountPaise, &whenStr, &ch.cand.Description, &merchantRaw,
			&journal, &tagsJS, &narration) != nil {
			continue
		}
		t, ok := parseAnyTime(whenStr)
		if !ok || t.After(when) || when.Sub(t) > creditBackLookback {
			continue
		}
		ref := refundref.Fold(uuid)
		if claimed[ref] >= ch.cand.AmountPaise {
			continue // another refund already gives this one back in full
		}
		ch.cand.Ref = ref
		ch.cand.JournalID = journal
		ch.cand.Merchant = merchantRaw
		ch.cand.Status = ch.status
		ch.cand.When = t
		ch.cand.Tags = parseTags(tagsJS)
		ch.cand.Exact = true
		ch.cand.RemainingPaise = ch.cand.AmountPaise - claimed[ref]
		ch.cand.DaysBefore = int(when.Sub(ch.cand.When).Hours() / 24)
		ch.payee = strings.TrimSpace(merchantRaw)
		if ch.payee == "" {
			// Not classified yet: the bank's own words for the merchant.
			if parts := strings.Split(narration, "/"); len(parts) >= 3 {
				ch.payee = strings.TrimSpace(parts[2])
			}
		}
		if foreign {
			ch.label = fmt.Sprintf("%s %.2f", strings.ToUpper(strings.TrimSpace(fCur.String)), float64(fAmt.Int64)/100)
		} else {
			ch.label = rupees(ch.cand.AmountPaise)
		}
		found = append(found, ch)
	}
	rows.Close()
	if len(found) == 0 {
		return nil
	}
	// The latest charge before the credit-back is the one it releases.
	sort.Slice(found, func(i, j int) bool {
		return found[i].cand.When.After(found[j].cand.When) ||
			(found[i].cand.When.Equal(found[j].cand.When) && found[i].cand.Ref < found[j].cand.Ref)
	})
	return &found[0]
}

// creditBackHold is the reason fold holds a credit-back with: what it
// looks like, the charge it gives back, and what to do about each outcome
// of checking the statement.
func creditBackHold(staged StagedRow, ch *creditBackCharge, when time.Time) string {
	const looks = "Looks like a released card hold, not money in"
	if ch == nil {
		return looks + ": the bank credited it back naming no merchant, and fold found no charge it gives back. If it isn't on the statement, skip it"
	}
	charge := "the " + ch.label + " charge"
	if ch.payee != "" {
		charge += " at " + ch.payee
	}
	charge += " on " + sayDay(ch.cand.When, when)
	switch ch.status {
	case "pushed":
		return looks + ": it gives back " + charge + ", already in Firefly. If neither is on the statement, skip this and delete that one there; if both are, release the hold"
	case "skipped":
		return looks + ": it gives back " + charge + ", which you skipped as never billed. Skip this too"
	}
	return looks + ": it gives back " + charge + ". If neither is on the statement, skip both; if both are, release the hold"
}

// sayDay is a day the way the deck says it: "Tue 11 Aug", "Mon 14 Sept",
// with the year only when it isn't the reference's.
func sayDay(t, ref time.Time) string {
	ist := time.FixedZone("IST", 5*3600+1800)
	t = t.In(ist)
	s := strings.Replace(t.Format("Mon 2 Jan"), " Sep", " Sept", 1)
	if t.Year() != ref.In(ist).Year() {
		s += " " + t.Format("2006")
	}
	return s
}

// rupees formats paise the Indian way: ₹2,384.75, ₹1,23,456.00.
func rupees(p int64) string {
	neg := p < 0
	if neg {
		p = -p
	}
	whole, frac := p/100, p%100
	s := fmt.Sprintf("%d", whole)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var groups []string
		for len(head) > 2 {
			groups = append([]string{head[len(head)-2:]}, groups...)
			head = head[:len(head)-2]
		}
		if head != "" {
			groups = append([]string{head}, groups...)
		}
		s = strings.Join(groups, ",") + "," + tail
	}
	out := fmt.Sprintf("₹%s.%02d", s, frac)
	if neg {
		out = "−" + out
	}
	return out
}

// rupeesShort is a rupee amount the way a range reads: "₹38", "₹1,250.50".
func rupeesShort(p int64) string {
	return strings.TrimSuffix(rupees(p), ".00")
}
