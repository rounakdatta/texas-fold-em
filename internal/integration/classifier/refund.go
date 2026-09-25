package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/refundref"
)

// Refunds.
//
// A card refund reaches fold as an INCOMING row ("CARD/…/Zomato/₹/486.20/
// INCOMING/Refund Received!"). In firefly — the ledger — the user books it
// as a DEPOSIT: the merchant's REVENUE twin (an account with the same name as
// the merchant's expense account) pays the card that was charged, category
// "Refund", no budget, titled "Refund for <what it refunds>". Firefly also
// has a native way to tie a refund to its purchase: a transaction link of the
// built-in type "Refund" ("(partially) refunds" / "is (partially) refunded
// by"). This file decides the deposit and finds the purchase; the Pusher
// creates the link once both sides are in firefly.
//
// Refunds get their own deterministic tier and never reach Tier 1/2/3:
//   - the structure is fully determined (deposit, merchant → card, Refund),
//     so there is nothing for an LLM to judge;
//   - Tier 1's merchant_lookup is built from WITHDRAWALS, so applying it to
//     a refund books the money going OUT to the merchant — the bug that
//     filed six Zomato refunds as spends.

// TierRefund marks a decision made by the refund tier.
const TierRefund Tier = 5

// RefundCandidate is one purchase a refund could be giving money back for.
type RefundCandidate struct {
	Ref         string    `json:"ref"`
	When        time.Time `json:"when"`
	AmountPaise int64     `json:"amount_paise"`
	Description string    `json:"description"`
	// Merchant is the purchase's payee (its expense account name) — the
	// refund comes from that name's revenue twin.
	Merchant string `json:"merchant"`
	// Status is the purchase's fold status, or "firefly" for a journal fold
	// never staged.
	Status string `json:"status"`
	// JournalID is the purchase's firefly journal, when it is in firefly.
	JournalID int64    `json:"journal_id,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	// Exact: the purchase was for exactly the refunded amount.
	Exact bool `json:"exact"`
	// GroupMatch: fold.money itself groups the two (refund_group_id).
	GroupMatch bool `json:"group_match,omitempty"`
	// RemainingPaise is the purchase amount not yet claimed by other refunds.
	RemainingPaise int64 `json:"remaining_paise"`
	// DaysBefore: how long before the refund the purchase was made.
	DaysBefore int `json:"days_before"`

	foldAccountID string
	cardAssetID   int64
}

// refundProbe is the refund side of a match, in effective values (the
// human's corrections win over what fold saw).
type refundProbe struct {
	FoldUUID      string
	FoldAccountID string // raw_payload.account_id — the card the refund landed on
	CardAssetID   int64  // that card as a firefly asset, when known
	Merchant      string // normalised merchant, as in merchant_extracted
	AmountPaise   int64
	When          time.Time
	GroupID       string // raw_payload.refund_group_id
}

// refundLookback bounds how old a purchase a refund may point at.
const refundLookback = 180 * 24 * time.Hour

// How recent a purchase must be for a match to be CONFIDENT. A food or
// grocery refund lands within days; an exact amount found months earlier is
// as likely a coincidence (a regular order at a regular price) as the
// purchase being refunded — the 31 Dec Zomato ₹602.75 refund matched a
// 5 Nov order of the same price. Past these windows a match is only a
// suggestion for review.
const (
	exactConfidentWithin   = 30 * 24 * time.Hour
	partialSuggestedWithin = 7 * 24 * time.Hour
)

// refundPayload is the part of fold.money's raw transaction the refund logic
// reads. fold.md documents refund_group_id / refund / remaining_refund_amount
// on every transaction; when fold.money has grouped a purchase with its
// refund, the shared refund_group_id is the strongest signal there is.
type refundPayload struct {
	AccountID     string `json:"account_id"`
	RefundGroupID string `json:"refund_group_id"`
	Merchant      struct {
		Name string `json:"name"`
	} `json:"merchant"`
}

func parseRefundPayload(raw string) refundPayload {
	var p refundPayload
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	p.AccountID = strings.TrimSpace(p.AccountID)
	p.RefundGroupID = strings.TrimSpace(p.RefundGroupID)
	return p
}

// refundSignal reports whether a staged row is a refund of a purchase, from
// what fold sent. Money coming back on a CARD rail is a refund or reversal;
// on any rail, a narration that says so is too. A card's bill payment,
// cashback, interest or reward credit is NOT a refund of a purchase — those
// stay with the normal tiers.
func refundSignal(staged StagedRow) (bool, string) {
	if staged.Type != "INCOMING" {
		return false, ""
	}
	n := strings.ToLower(staged.Narration)
	for _, not := range []string{"payment", "cashback", "cash back", "interest", "reward", "salary", "dividend"} {
		if strings.Contains(n, not) {
			return false, ""
		}
	}
	switch {
	case strings.Contains(n, "refund"):
		return true, "narration says refund"
	case strings.Contains(n, "reversal") || strings.Contains(n, "reversed"):
		return true, "narration says reversal"
	case parseRefundPayload(staged.RawPayload).RefundGroupID != "":
		return true, "fold.money refund group"
	case strings.EqualFold(staged.Mode, "CARD"):
		return true, "money back on a card"
	}
	return false, ""
}

// tierRefund classifies a refund deterministically: a deposit from the
// merchant's revenue twin into the card, category Refund, no budget, titled
// after the purchase it refunds, carrying that purchase's tags. ok=false for
// anything that isn't a refund.
func (c *Classifier) tierRefund(ctx context.Context, staged StagedRow) (Decision, bool, error) {
	isRefund, why := refundSignal(staged)
	if !isRefund {
		return Decision{}, false, nil
	}
	payload := parseRefundPayload(staged.RawPayload)
	probe := refundProbe{
		FoldUUID:      staged.FoldUUID,
		FoldAccountID: payload.AccountID,
		Merchant:      staged.MerchantExtracted,
		AmountPaise:   staged.AmountPaise,
		GroupID:       payload.RefundGroupID,
	}
	probe.When, _ = parseAnyTime(staged.TxnTimestamp)

	d := Decision{
		Tier:    TierRefund,
		TxnType: "deposit",
		Evidence: Evidence{
			Tier:               TierRefund,
			MerchantNormalized: staged.MerchantExtracted,
		},
	}
	// The card the money came back to: fold's account_id is ground truth,
	// exactly as it is for the paying card of a purchase.
	if fa, _ := lookupFoldAccountForStaged(ctx, c.db, staged.RawPayload); fa != nil && fa.Name != "" {
		assets, _ := listFireflyAssetsFromMirror(ctx, c.db)
		if id, name, ok := matchFoldCardToFireflyAsset(fa, assets); ok {
			d.DestinationAccountID, d.DestinationAccountName = &id, name
			probe.CardAssetID = id
		} else {
			d.DestinationAccountName = fa.Name // unmatched card → review
		}
	}

	cands, err := findRefundCandidates(ctx, c.db, probe)
	if err != nil {
		return Decision{}, false, err
	}
	best, conf, note := pickRefundCandidate(cands)

	merchant := refundMerchantName(ctx, c.db, staged)
	if best != nil && strings.TrimSpace(best.Merchant) != "" {
		merchant = best.Merchant
	}
	d.SourceAccountName = merchant
	if id, ok := revenueTwin(ctx, c.db, merchant); ok {
		d.SourceAccountID = &id
	}
	if id, name, ok := categoryByName(ctx, c.db, "Refund"); ok {
		d.CategoryID, d.CategoryName = &id, name
	}
	d.Description = refundDescription(best, merchant)
	if best != nil {
		d.RefundOf = best.Ref
		d.Tags = best.Tags
	}
	d.Confidence = conf
	d.Evidence.Note = why + "; " + note
	if len(cands) > 5 {
		cands = cands[:5]
	}
	d.Evidence.RefundCandidates = cands
	return d, true, nil
}

// pickRefundCandidate chooses the purchase a refund most likely gives money
// back for, with the decision's confidence and a one-line reason. Candidates
// arrive best-first (group match, then exact amount, then most recent).
//
//   - fold.money grouped them (refund_group_id)             → that one, 1.0
//   - exactly one exact-amount purchase within 30 days      → it, 1.0
//   - several identical ones within 30 days (a re-placed
//     order) — any is equivalent in the books              → the latest, 0.9
//   - an exact amount, but older than 30 days               → suggested, 0.6
//   - only a larger purchase within 7 days (items missing)  → suggested, 0.6
//   - otherwise                                             → none, 0.6
//
// Below the auto-accept threshold the row goes to review, where the human
// sees every candidate; the deposit itself is right either way.
func pickRefundCandidate(cands []RefundCandidate) (*RefundCandidate, float64, string) {
	if len(cands) == 0 {
		return nil, 0.7, "no matching purchase on this card in the last 180 days"
	}
	for i := range cands {
		if cands[i].GroupMatch {
			return &cands[i], 1.0, "fold.money groups it with this purchase"
		}
	}
	var recentExact []int
	firstExact, firstPartial := -1, -1
	for i, c := range cands {
		switch {
		case c.Exact && firstExact < 0:
			firstExact = i
		case !c.Exact && firstPartial < 0:
			firstPartial = i
		}
		if c.Exact && time.Duration(c.DaysBefore)*24*time.Hour <= exactConfidentWithin {
			recentExact = append(recentExact, i)
		}
	}
	switch {
	case len(recentExact) == 1:
		return &cands[recentExact[0]], 1.0, "one purchase for exactly this amount"
	case len(recentExact) > 1:
		return &cands[recentExact[0]], 0.9, fmt.Sprintf("%d identical purchases; picked the latest", len(recentExact))
	case firstExact >= 0:
		return &cands[firstExact], 0.6, fmt.Sprintf("an exact amount, but %d days earlier — check it", cands[firstExact].DaysBefore)
	case firstPartial >= 0 && time.Duration(cands[firstPartial].DaysBefore)*24*time.Hour <= partialSuggestedWithin:
		return &cands[firstPartial], 0.6, "no purchase for exactly this amount; suggested the latest larger one (partial refund?)"
	}
	return nil, 0.6, "no purchase for exactly this amount recently; pick one in review"
}

// refundDescription mirrors the user's own refund titles ("Refund for
// Google One verification charges", "Refund for Hevy Pro subscription"):
// "Refund for <the purchase's title>", or a placeholder when there's no
// purchase to name.
func refundDescription(best *RefundCandidate, merchant string) string {
	if best != nil {
		if d := strings.TrimSpace(best.Description); d != "" {
			return "Refund for " + d
		}
	}
	if merchant = strings.TrimSpace(merchant); merchant != "" {
		return "Refund for ___ from " + merchant
	}
	return "Refund for ___"
}

// refundMerchantName is the merchant's display name for a refund with no
// matched purchase: the payee the user files this merchant under (Tier 1's
// lookup is keyed by the same normalised string), else fold's merchant as it
// appears in the narration.
func refundMerchantName(ctx context.Context, db *sql.DB, staged StagedRow) string {
	if staged.MerchantExtracted != "" {
		var name sql.NullString
		_ = db.QueryRowContext(ctx, `SELECT modal_destination_account_name FROM merchant_lookup WHERE merchant_normalized = ?`,
			staged.MerchantExtracted).Scan(&name)
		if strings.TrimSpace(name.String) != "" {
			return strings.TrimSpace(name.String)
		}
	}
	if p := parseRefundPayload(staged.RawPayload); strings.TrimSpace(p.Merchant.Name) != "" {
		return strings.TrimSpace(p.Merchant.Name)
	}
	if parts := strings.Split(staged.Narration, "/"); strings.EqualFold(staged.Mode, "CARD") && len(parts) >= 3 {
		return strings.TrimSpace(parts[2])
	}
	return staged.MerchantExtracted
}

// revenueTwin finds the revenue account that shares a merchant's name — the
// account the user's refunds from that merchant come from (Amazon 52 ↔ 110,
// Zepto 973 ↔ 974). ok=false means firefly has none yet: push sends the name
// and firefly creates it.
func revenueTwin(ctx context.Context, db *sql.DB, merchant string) (int64, bool) {
	if strings.TrimSpace(merchant) == "" {
		return 0, false
	}
	var id sql.NullInt64
	_ = db.QueryRowContext(ctx, `
		SELECT firefly_id FROM firefly_accounts
		WHERE type = 'revenue' AND active = 1 AND LOWER(name) = LOWER(?)
		ORDER BY firefly_id LIMIT 1`, strings.TrimSpace(merchant)).Scan(&id)
	if id.Valid {
		return id.Int64, true
	}
	_ = db.QueryRowContext(ctx, `
		SELECT source_account_id FROM firefly_txns
		WHERE txn_type = 'deposit' AND LOWER(source_account_name) = LOWER(?) AND source_account_id IS NOT NULL
		ORDER BY date DESC LIMIT 1`, strings.TrimSpace(merchant)).Scan(&id)
	return id.Int64, id.Valid
}

// categoryByName resolves a category the user already uses.
func categoryByName(ctx context.Context, db *sql.DB, name string) (int64, string, bool) {
	var id sql.NullInt64
	var n sql.NullString
	_ = db.QueryRowContext(ctx, `
		SELECT category_id, category_name FROM firefly_txns
		WHERE LOWER(category_name) = LOWER(?) AND category_id IS NOT NULL
		ORDER BY date DESC LIMIT 1`, name).Scan(&id, &n)
	return id.Int64, n.String, id.Valid
}

// Effective-value SQL over staged_fold_txns (alias s), matching how the
// Pusher resolves each field: the human's value wins, as a unit.
const (
	effAmountSQL = `COALESCE(s.confirmed_amount_paise, s.amount_paise)`
	effWhenSQL   = `COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp)`
	effDescSQL   = `COALESCE(NULLIF(TRIM(s.confirmed_description),''), NULLIF(TRIM(s.proposed_description),''), '')`
	effTagsSQL   = `COALESCE(NULLIF(s.confirmed_tags_json,''), NULLIF(s.proposed_tags_json,''), '')`
	effRefSQL    = `COALESCE(NULLIF(s.confirmed_refund_of,''), s.proposed_refund_of)`
	effSrcIDSQL  = `(CASE WHEN s.confirmed_source_account_id IS NOT NULL THEN s.confirmed_source_account_id
	                      WHEN NULLIF(s.confirmed_source_account_name,'') IS NOT NULL THEN NULL
	                      ELSE s.proposed_source_account_id END)`
	effDstIDSQL = `(CASE WHEN s.confirmed_destination_account_id IS NOT NULL THEN s.confirmed_destination_account_id
	                     WHEN NULLIF(s.confirmed_destination_account_name,'') IS NOT NULL THEN NULL
	                     ELSE s.proposed_destination_account_id END)`
	effDstNameSQL = `COALESCE(
	    CASE WHEN s.confirmed_destination_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.confirmed_destination_account_id)
	         ELSE NULLIF(s.confirmed_destination_account_name,'') END,
	    CASE WHEN s.proposed_destination_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.proposed_destination_account_id)
	         ELSE NULLIF(s.proposed_destination_account_name,'') END,
	    '')`
	effSrcNameSQL = `COALESCE(
	    CASE WHEN s.confirmed_source_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.confirmed_source_account_id)
	         ELSE NULLIF(s.confirmed_source_account_name,'') END,
	    CASE WHEN s.proposed_source_account_id IS NOT NULL
	         THEN (SELECT name FROM firefly_accounts WHERE firefly_id = s.proposed_source_account_id)
	         ELSE NULLIF(s.proposed_source_account_name,'') END,
	    '')`
	jsonAccountSQL = `CASE WHEN json_valid(s.raw_payload) THEN COALESCE(json_extract(s.raw_payload,'$.account_id'),'') ELSE '' END`
	jsonGroupSQL   = `CASE WHEN json_valid(s.raw_payload) THEN COALESCE(json_extract(s.raw_payload,'$.refund_group_id'),'') ELSE '' END`
)

// findRefundCandidates lists the purchases a refund could be for, best first:
// earlier OUTGOING spends on the SAME card at the SAME merchant, within
// refundLookback, not already fully claimed by other refunds. It searches
// both fold's staged rows (pending or pushed) and firefly's own journals —
// firefly is the ledger, and a purchase from before fold existed, or entered
// by hand, lives only there.
func findRefundCandidates(ctx context.Context, db *sql.DB, p refundProbe) ([]RefundCandidate, error) {
	if p.Merchant == "" && p.GroupID == "" {
		return nil, nil
	}
	claims, err := refundClaims(ctx, db, p.FoldUUID)
	if err != nil {
		return nil, err
	}

	var out []RefundCandidate
	seenJournal := map[int64]bool{}

	rows, err := db.QueryContext(ctx, `
		SELECT s.fold_uuid, s.status, `+effAmountSQL+`, `+effWhenSQL+`, `+effDescSQL+`, `+effDstNameSQL+`,
		       COALESCE(s.firefly_txn_id, 0), `+effTagsSQL+`, `+jsonGroupSQL+`, `+jsonAccountSQL+`,
		       COALESCE(`+effSrcIDSQL+`, 0)
		FROM staged_fold_txns s
		WHERE s.type = 'OUTGOING' AND s.status <> 'skipped' AND s.fold_uuid <> ?
		  AND ((? <> '' AND (s.merchant_extracted = ? OR LOWER(`+effDstNameSQL+`) = ?))
		       OR (? <> '' AND `+jsonGroupSQL+` = ?))
	`, p.FoldUUID, p.Merchant, p.Merchant, p.Merchant, p.GroupID, p.GroupID)
	if err != nil {
		return nil, fmt.Errorf("refund candidates (fold): %w", err)
	}
	for rows.Next() {
		var (
			c                 RefundCandidate
			uuid, whenStr     string
			tagsJS, groupID   string
			journal, srcAsset int64
		)
		if err := rows.Scan(&uuid, &c.Status, &c.AmountPaise, &whenStr, &c.Description, &c.Merchant,
			&journal, &tagsJS, &groupID, &c.foldAccountID, &srcAsset); err != nil {
			rows.Close()
			return nil, err
		}
		c.Ref = refundref.Fold(uuid)
		c.JournalID = journal
		c.cardAssetID = srcAsset
		c.When, _ = parseAnyTime(whenStr)
		c.Tags = parseTags(tagsJS)
		c.GroupMatch = p.GroupID != "" && groupID == p.GroupID
		if journal != 0 {
			seenJournal[journal] = true
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Firefly's own journals: purchases fold never staged (hand-entered, or
	// from before fold). Needs the card as a firefly asset to scope by.
	if p.CardAssetID != 0 && p.Merchant != "" {
		like := ""
		if len(p.Merchant) >= 4 {
			like = "%" + p.Merchant + "%"
		}
		frows, err := db.QueryContext(ctx, `
			SELECT firefly_id, amount_paise, date, description, COALESCE(destination_account_name,''),
			       COALESCE(tags_json,''), COALESCE(external_id,'')
			FROM firefly_txns
			WHERE txn_type = 'withdrawal' AND source_account_id = ?
			  AND (destination_account_name_normalized = ? OR LOWER(destination_account_name) = ?
			       OR (? <> '' AND LOWER(COALESCE(notes,'')) LIKE ?))
		`, p.CardAssetID, p.Merchant, p.Merchant, like, like)
		if err != nil {
			return nil, fmt.Errorf("refund candidates (firefly): %w", err)
		}
		for frows.Next() {
			var (
				c               RefundCandidate
				whenStr, tagsJS string
				extID           string
			)
			if err := frows.Scan(&c.JournalID, &c.AmountPaise, &whenStr, &c.Description, &c.Merchant, &tagsJS, &extID); err != nil {
				frows.Close()
				return nil, err
			}
			if seenJournal[c.JournalID] || (extID != "" && extID == p.FoldUUID) {
				continue // the staged row already stands for this journal
			}
			c.Ref = refundref.Journal(c.JournalID)
			c.Status = "firefly"
			c.When, _ = parseAnyTime(whenStr)
			c.Tags = parseTags(tagsJS)
			c.cardAssetID = p.CardAssetID
			out = append(out, c)
		}
		frows.Close()
		if err := frows.Err(); err != nil {
			return nil, err
		}
	}

	kept := out[:0]
	for _, c := range out {
		if !sameCard(p, c) {
			continue
		}
		if !c.GroupMatch {
			// A refund follows its purchase (allow a minute of clock skew)
			// and gives back at most what was paid.
			if !p.When.IsZero() && !c.When.IsZero() &&
				(c.When.After(p.When.Add(time.Minute)) || c.When.Before(p.When.Add(-refundLookback))) {
				continue
			}
		}
		claimed := claims[c.Ref]
		if c.JournalID != 0 {
			claimed += claims[refundref.Journal(c.JournalID)]
		}
		c.RemainingPaise = c.AmountPaise - claimed
		if !c.GroupMatch && c.RemainingPaise < p.AmountPaise {
			continue
		}
		c.Exact = c.AmountPaise == p.AmountPaise
		if !p.When.IsZero() && !c.When.IsZero() && p.When.After(c.When) {
			c.DaysBefore = int(p.When.Sub(c.When).Hours() / 24)
		}
		kept = append(kept, c)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.GroupMatch != b.GroupMatch {
			return a.GroupMatch
		}
		if a.Exact != b.Exact {
			return a.Exact
		}
		return a.When.After(b.When) // most recent purchase first
	})
	return kept, nil
}

// sameCard: the purchase was paid on the card the refund landed on. fold's
// account_id is compared when both sides have one; otherwise the firefly
// asset. With nothing to compare (a refund whose card is unknown), accept.
func sameCard(p refundProbe, c RefundCandidate) bool {
	switch {
	case p.FoldAccountID != "" && c.foldAccountID != "":
		return p.FoldAccountID == c.foldAccountID
	case p.CardAssetID != 0 && c.cardAssetID != 0:
		return p.CardAssetID == c.cardAssetID
	case p.FoldAccountID == "" && p.CardAssetID == 0:
		return true
	}
	return false
}

// refundClaims sums, per purchase reference, the amounts OTHER refunds
// already point at it — so a purchase that's been refunded in full isn't
// offered again, while one refunded in part still is.
func refundClaims(ctx context.Context, db *sql.DB, excludeUUID string) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+effRefSQL+`, `+effAmountSQL+`
		FROM staged_fold_txns s
		WHERE s.fold_uuid <> ? AND s.status <> 'skipped' AND `+effRefSQL+` IS NOT NULL`, excludeUUID)
	if err != nil {
		return nil, fmt.Errorf("refund claims: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var ref string
		var amt int64
		if err := rows.Scan(&ref, &amt); err != nil {
			return nil, err
		}
		if k, _, _ := refundref.Parse(ref); k == refundref.KindFold || k == refundref.KindJournal {
			out[ref] += amt
		}
	}
	return out, rows.Err()
}

// RefundCandidatesFor lists the purchases a staged row could be refunding,
// computed from the row's EFFECTIVE values — so it works for a row the human
// corrected, and for a MANUAL deposit added from a statement line (whose
// merchant is its source and whose card is its destination). Used by the
// review UI and by MatchRefunds.
func RefundCandidatesFor(ctx context.Context, db *sql.DB, foldUUID string) ([]RefundCandidate, error) {
	p, ok, err := probeForStaged(ctx, db, foldUUID)
	if err != nil || !ok {
		return nil, err
	}
	return findRefundCandidates(ctx, db, p)
}

func probeForStaged(ctx context.Context, db *sql.DB, foldUUID string) (refundProbe, bool, error) {
	var (
		p              refundProbe
		raw, whenStr   string
		srcName        string
		dstID          int64
		merchant, mode string
	)
	err := db.QueryRowContext(ctx, `
		SELECT s.fold_uuid, s.raw_payload, s.mode, COALESCE(s.merchant_extracted,''), `+effAmountSQL+`, `+effWhenSQL+`,
		       COALESCE(`+effDstIDSQL+`, 0), `+effSrcNameSQL+`
		FROM staged_fold_txns s WHERE s.fold_uuid = ?`, foldUUID).
		Scan(&p.FoldUUID, &raw, &mode, &merchant, &p.AmountPaise, &whenStr, &dstID, &srcName)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("refund probe: %w", err)
	}
	payload := parseRefundPayload(raw)
	p.FoldAccountID, p.GroupID = payload.AccountID, payload.RefundGroupID
	p.When, _ = parseAnyTime(whenStr)
	p.Merchant = merchant
	if p.Merchant == "" {
		// A manual deposit: the payer IS the merchant.
		p.Merchant = normaliseMerchantName(srcName)
	}
	if dstID != 0 && isAssetID(ctx, db, dstID) {
		p.CardAssetID = dstID
	} else if fa, _ := lookupFoldAccountForStaged(ctx, db, raw); fa != nil {
		assets, _ := listFireflyAssetsFromMirror(ctx, db)
		if id, _, ok := matchFoldCardToFireflyAsset(fa, assets); ok {
			p.CardAssetID = id
		}
	}
	return p, true, nil
}

// MatchRefunds proposes the purchase for every refund that has none yet, when
// the match is confident —
// rows classified before refunds were understood, rows the human already
// edited (their other fields are untouched), and MANUAL deposits added from
// a statement. Only proposed_refund_of changes: no status, no other field.
// A row the human already decided (confirmed_refund_of set) is left alone.
// Cheap; runs every sync cycle, so a purchase staged after its refund is
// still found.
func (c *Classifier) MatchRefunds(ctx context.Context) (int, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT s.fold_uuid FROM staged_fold_txns s
		WHERE s.type = 'INCOMING' AND s.status IN ('needs_review','ready_to_push','pushed')
		  AND s.proposed_refund_of IS NULL AND NULLIF(s.confirmed_refund_of,'') IS NULL
		  AND (s.classifier_tier = ? OR s.mode = 'CARD'
		       OR LOWER(s.narration) LIKE '%refund%' OR LOWER(s.narration) LIKE '%revers%'
		       OR LOWER(`+effDescSQL+`) LIKE '%refund%' OR LOWER(`+effDescSQL+`) LIKE '%revers%'
		       OR COALESCE(s.confirmed_category_id, s.proposed_category_id) IN
		          (SELECT category_id FROM firefly_txns WHERE LOWER(category_name) = 'refund'))
		ORDER BY s.txn_timestamp`, int(TierRefund))
	if err != nil {
		return 0, fmt.Errorf("match refunds: %w", err)
	}
	var uuids []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return 0, err
		}
		uuids = append(uuids, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, u := range uuids {
		// One at a time, oldest first, so each proposal is visible as a
		// claim to the next (two refunds never get the same full purchase).
		cands, err := RefundCandidatesFor(ctx, c.db, u)
		if err != nil {
			return n, err
		}
		// Only a CONFIDENT match is proposed here: these rows may already be
		// ready to push, and push turns the proposal into a firefly link
		// without anyone looking. A weaker candidate is still offered in the
		// review picker, for the human to choose.
		best, conf, _ := pickRefundCandidate(cands)
		if best == nil || conf < c.threshold {
			continue
		}
		if _, err := c.db.ExecContext(ctx,
			`UPDATE staged_fold_txns SET proposed_refund_of = ?, updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ? AND proposed_refund_of IS NULL`,
			best.Ref, u); err != nil {
			return n, fmt.Errorf("match refunds: %w", err)
		}
		n++
	}
	if n > 0 {
		c.log.Info("matched refunds to their purchases", "matched", n, "examined", len(uuids))
	}
	return n, nil
}

// normaliseMerchantName mirrors integration.NormalizeMerchant (duplicated to
// avoid an import cycle): lowercase, trimmed, whitespace collapsed.
func normaliseMerchantName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func isAssetID(ctx context.Context, db *sql.DB, id int64) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM firefly_accounts WHERE firefly_id = ? AND type = 'asset'`, id).Scan(&n)
	return n > 0
}

func parseTags(js string) []string {
	if strings.TrimSpace(js) == "" {
		return nil
	}
	var t []string
	if json.Unmarshal([]byte(js), &t) != nil {
		return nil
	}
	return t
}

// parseAnyTime reads the timestamp shapes found in staged_fold_txns and
// firefly_txns (RFC3339, the driver's "…-07:00" and time.String() forms, and
// bare dates).
func parseAnyTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
