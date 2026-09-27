package feedback

// Package feedback keeps what a person did with fold's suggestions, so the
// classifier can learn from it. A leaf package (it needs only a *sql.DB):
// the pusher, the review UI and the classifier all record or read it.
//
// Until 0.20.0 the only thing fold learned from was a push (LearnFromPushed
// reinforces the merchant lookup's category and payee). What a person
// *corrected* — the title they rewrote, the payee they renamed, the tag they
// added — was stored as the row's confirmed_* values and then never looked
// at again, so the next card from the same merchant got the same wrong
// suggestion. review_feedback keeps every decision in the shape the model
// reads (names, not ids): the suggestion, what the person chose, and what
// kind of decision it was. The Tier-3 prompt shows the relevant ones back as
// the strongest evidence there is of how this person books things, and the
// re-suggest loop revisits waiting cards that a new decision could improve.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Feedback actions.
const (
	FeedbackEdit    = "edit"    // a save that changed what the card says
	FeedbackSend    = "send"    // the values that went to firefly
	FeedbackHold    = "hold"    // held back from sending, with a reason
	FeedbackSkip    = "skip"    // never to be sent
	FeedbackRelease = "release" // a hold fold set, released: it was real, never hold it again
)

// NoneValue is how an explicit "no category / no budget" reads — the person
// cleared it — as opposed to "" (nothing chosen yet).
const NoneValue = "(none)"

// ReviewValues is what a card says, in names: a suggestion and a person's
// choice are both recorded in this shape.
type ReviewValues struct {
	Type     string   `json:"type,omitempty"`
	Title    string   `json:"title,omitempty"`
	Payee    string   `json:"payee,omitempty"`
	Category string   `json:"category,omitempty"`
	Budget   string   `json:"budget,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// Equal reports whether two values say the same thing.
func (v ReviewValues) Equal(o ReviewValues) bool {
	if v.Type != o.Type || v.Title != o.Title || v.Payee != o.Payee || v.Category != o.Category || v.Budget != o.Budget || len(v.Tags) != len(o.Tags) {
		return false
	}
	for i := range v.Tags {
		if v.Tags[i] != o.Tags[i] {
			return false
		}
	}
	return true
}

// ReviewSnapshot is one staged row's suggestion and what it says now.
type ReviewSnapshot struct {
	Suggested    ReviewValues // proposed_* — what fold suggested
	Effective    ReviewValues // confirmed_* over proposed_* — what a send would book
	MerchantKey  string
	Narration    string
	Direction    string
	AmountPaise  int64
	ForeignLabel string
}

// SnapshotReview reads a row's suggestion and effective values in names.
func SnapshotReview(ctx context.Context, db *sql.DB, uuid string) (ReviewSnapshot, error) {
	var (
		s                              ReviewSnapshot
		merchant                       sql.NullString
		fxPaise                        sql.NullInt64
		fxCur                          sql.NullString
		pType, cType                   sql.NullString
		pDesc, cDesc                   sql.NullString
		pDstID, cDstID, pSrcID, cSrcID sql.NullInt64
		pDstName, cDstName             sql.NullString
		pSrcName, cSrcName             sql.NullString
		pCat, cCat, pBud, cBud         sql.NullInt64
		pTags, cTags                   sql.NullString
		confirmedAmount                sql.NullInt64
	)
	err := db.QueryRowContext(ctx, `
		SELECT merchant_extracted, narration, type, amount_paise, confirmed_amount_paise,
		       foreign_amount_paise, foreign_currency,
		       proposed_txn_type, confirmed_txn_type,
		       proposed_description, confirmed_description,
		       proposed_destination_account_id, confirmed_destination_account_id,
		       proposed_destination_account_name, confirmed_destination_account_name,
		       proposed_source_account_id, confirmed_source_account_id,
		       proposed_source_account_name, confirmed_source_account_name,
		       proposed_category_id, confirmed_category_id,
		       proposed_budget_id, confirmed_budget_id,
		       proposed_tags_json, confirmed_tags_json
		FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(
		&merchant, &s.Narration, &s.Direction, &s.AmountPaise, &confirmedAmount,
		&fxPaise, &fxCur,
		&pType, &cType, &pDesc, &cDesc,
		&pDstID, &cDstID, &pDstName, &cDstName,
		&pSrcID, &cSrcID, &pSrcName, &cSrcName,
		&pCat, &cCat, &pBud, &cBud, &pTags, &cTags)
	if err != nil {
		return s, err
	}
	s.MerchantKey = merchant.String
	if confirmedAmount.Valid {
		s.AmountPaise = confirmedAmount.Int64
	}
	if fxPaise.Valid && fxCur.Valid && fxCur.String != "" && !strings.EqualFold(fxCur.String, "INR") {
		s.ForeignLabel = fmt.Sprintf("%s %.2f", strings.ToUpper(fxCur.String), float64(fxPaise.Int64)/100)
	}
	// The other side of the move is the payee: who was paid on money out,
	// who paid on money in.
	payee := func(dstID, srcID sql.NullInt64, dstName, srcName sql.NullString) string {
		if s.Direction == "INCOMING" {
			return accountLabel(ctx, db, srcID, srcName)
		}
		return accountLabel(ctx, db, dstID, dstName)
	}
	s.Suggested = ReviewValues{
		Type:     pType.String,
		Title:    strings.TrimSpace(pDesc.String),
		Payee:    payee(pDstID, pSrcID, pDstName, pSrcName),
		Category: categoryLabel(ctx, db, pCat),
		Budget:   budgetLabel(ctx, db, pBud),
		Tags:     tagList(pTags),
	}
	eff := s.Suggested
	if cType.Valid && cType.String != "" {
		eff.Type = cType.String
	}
	if cDesc.Valid {
		eff.Title = strings.TrimSpace(cDesc.String)
	}
	confirmedSide := cDstID.Valid || (cDstName.Valid && cDstName.String != "")
	if s.Direction == "INCOMING" {
		confirmedSide = cSrcID.Valid || (cSrcName.Valid && cSrcName.String != "")
	}
	if confirmedSide {
		eff.Payee = payee(cDstID, cSrcID, cDstName, cSrcName)
	}
	if cCat.Valid {
		eff.Category = categoryLabel(ctx, db, cCat)
	}
	if cBud.Valid {
		eff.Budget = budgetLabel(ctx, db, cBud)
	}
	if cTags.Valid {
		eff.Tags = tagList(cTags)
	}
	s.Effective = eff
	return s, nil
}

// RecordFeedback writes one decision, now.
func RecordFeedback(ctx context.Context, db *sql.DB, uuid, action string, snap ReviewSnapshot, chosen ReviewValues, note string) error {
	return recordFeedbackAt(ctx, db, uuid, action, snap, chosen, note, "")
}

// recordFeedbackAt writes one decision at a given time ("" = now), for the
// backfill of decisions made before this table existed.
func recordFeedbackAt(ctx context.Context, db *sql.DB, uuid, action string, snap ReviewSnapshot, chosen ReviewValues, note, at string) error {
	if uuid == "" || action == "" {
		return errors.New("feedback: uuid and action are required")
	}
	sj, _ := json.Marshal(snap.Suggested)
	cj, _ := json.Marshal(chosen)
	_, err := db.ExecContext(ctx, `
		INSERT INTO review_feedback (fold_uuid, at, action, merchant_key, narration, direction, amount_paise, foreign_label, suggested_json, chosen_json, note)
		VALUES (?, COALESCE(NULLIF(?, ''), strftime('%Y-%m-%d %H:%M:%f', 'now')), ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid, at, action, snap.MerchantKey, truncateRunes(snap.Narration, 240), snap.Direction, snap.AmountPaise, snap.ForeignLabel, string(sj), string(cj), strings.TrimSpace(note))
	return err
}

// RecordEdit records a save that changed what the card says. before is the
// snapshot taken just before the save; nothing is written when the save
// changed nothing (a statement-amount correction that re-posts the form,
// say), so only real decisions become examples.
func RecordEdit(ctx context.Context, db *sql.DB, uuid string, before ReviewSnapshot) error {
	after, err := SnapshotReview(ctx, db, uuid)
	if err != nil {
		return err
	}
	if after.Effective.Equal(before.Effective) {
		return nil
	}
	return RecordFeedback(ctx, db, uuid, FeedbackEdit, after, after.Effective, "")
}

// RecordDecision records a send, hold or skip with the row as it stands.
func RecordDecision(ctx context.Context, db *sql.DB, uuid, action, note string) error {
	snap, err := SnapshotReview(ctx, db, uuid)
	if err != nil {
		return err
	}
	return RecordFeedback(ctx, db, uuid, action, snap, snap.Effective, note)
}

// BackfillFeedback seeds review_feedback from the decisions already in the
// staged table — a row whose confirmed values differ from its suggestion is
// a correction, a pushed row a send, a held row a hold, a skipped one a
// skip — so the loop starts from months of history instead of from nothing.
// It runs once: a non-empty table is left alone. Returns rows written.
func BackfillFeedback(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_feedback`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT fold_uuid, status, CASE WHEN hold_by IS NULL THEN COALESCE(hold_reason, '') ELSE '' END,
		       COALESCE(pushed_at, reviewed_at, updated_at),
		       reviewed_at IS NOT NULL
		FROM staged_fold_txns
		WHERE status IN ('pushed', 'skipped') OR (hold_reason IS NOT NULL AND hold_by IS NULL) OR reviewed_at IS NOT NULL
		ORDER BY COALESCE(pushed_at, reviewed_at, updated_at)`)
	if err != nil {
		return 0, err
	}
	type item struct {
		uuid, status, hold, at string
		reviewed               bool
	}
	var items []item
	for rows.Next() {
		var it item
		var at sql.NullString
		if err := rows.Scan(&it.uuid, &it.status, &it.hold, &at, &it.reviewed); err != nil {
			rows.Close()
			return 0, err
		}
		it.at = at.String
		items = append(items, it)
	}
	rows.Close()
	written := 0
	for _, it := range items {
		snap, err := SnapshotReview(ctx, db, it.uuid)
		if err != nil {
			continue
		}
		action := ""
		switch {
		case it.status == "pushed":
			action = FeedbackSend
		case it.status == "skipped":
			action = FeedbackSkip
		case it.hold != "":
			action = FeedbackHold
		case it.reviewed && !snap.Effective.Equal(snap.Suggested):
			action = FeedbackEdit
		default:
			continue
		}
		if err := recordFeedbackAt(ctx, db, it.uuid, action, snap, snap.Effective, it.hold, it.at); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// accountLabel is an account's name: the confirmed/proposed id's firefly
// name, else the name stored beside it (a new payee not in firefly yet).
func accountLabel(ctx context.Context, db *sql.DB, id sql.NullInt64, name sql.NullString) string {
	if id.Valid && id.Int64 != 0 {
		var n string
		if db.QueryRowContext(ctx, `SELECT name FROM firefly_accounts WHERE firefly_id = ?`, id.Int64).Scan(&n) == nil && n != "" {
			return n
		}
		if db.QueryRowContext(ctx, `SELECT destination_account_name FROM firefly_txns WHERE destination_account_id = ? LIMIT 1`, id.Int64).Scan(&n) == nil && n != "" {
			return n
		}
		if db.QueryRowContext(ctx, `SELECT source_account_name FROM firefly_txns WHERE source_account_id = ? LIMIT 1`, id.Int64).Scan(&n) == nil && n != "" {
			return n
		}
	}
	return strings.TrimSpace(name.String)
}

func categoryLabel(ctx context.Context, db *sql.DB, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	if id.Int64 == 0 {
		return NoneValue
	}
	var n string
	if db.QueryRowContext(ctx, `SELECT name FROM firefly_categories WHERE firefly_id = ?`, id.Int64).Scan(&n) == nil && n != "" {
		return n
	}
	_ = db.QueryRowContext(ctx, `SELECT category_name FROM firefly_txns WHERE category_id = ? AND category_name IS NOT NULL LIMIT 1`, id.Int64).Scan(&n)
	return n
}

func budgetLabel(ctx context.Context, db *sql.DB, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	if id.Int64 == 0 {
		return NoneValue
	}
	var n string
	_ = db.QueryRowContext(ctx, `SELECT budget_name FROM firefly_txns WHERE budget_id = ? AND budget_name IS NOT NULL LIMIT 1`, id.Int64).Scan(&n)
	return n
}

func tagList(js sql.NullString) []string {
	if !js.Valid || js.String == "" {
		return nil
	}
	var tags []string
	if json.Unmarshal([]byte(js.String), &tags) != nil {
		return nil
	}
	return tags
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
