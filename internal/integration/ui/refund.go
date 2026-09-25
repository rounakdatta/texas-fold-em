package ui

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
	"github.com/rounakdatta/texas-fold-em/internal/integration/refundref"
)

// refundOption is one choice in the detail page's "refund of" picker.
type refundOption struct {
	Value    string
	Label    string
	Selected bool
}

// refundView drives the detail page's "refund of" card: which purchase this
// refund gives money back for, and whether firefly already records it as a
// Refund link.
type refundView struct {
	Show    bool
	Options []refundOption
	// Current is the effective reference (the human's, else the classifier's).
	Current string
	// Confirmed: the human chose Current (vs. the classifier proposing it).
	Confirmed bool
	LinkID    int64
	// CanLink: pushed, not linked yet, and the purchase is in firefly — the
	// "link in firefly" button applies. Waiting: the purchase is still only
	// staged, so the link happens when it's pushed.
	CanLink bool
	Waiting bool
}

// refundedByRow is one refund pointing at the purchase being viewed.
type refundedByRow struct {
	FoldUUID    string
	When        string
	Amount      string
	Description string
	Status      string
	LinkID      int64
}

// refundCardFor builds the "refund of" card for a row, plus the "refunded
// by" list for a purchase. Cheap SQL; only INCOMING rows or rows that
// already carry a reference get candidates computed.
func (h *Handler) refundCardFor(ctx context.Context, uuid string) (refundView, []refundedByRow) {
	var (
		v                      refundView
		typ, status            string
		tier                   sql.NullInt64
		effRef, confRef        sql.NullString
		linkID, journal, catID sql.NullInt64
	)
	err := h.db.QueryRowContext(ctx, `
		SELECT type, status, classifier_tier,
		       COALESCE(NULLIF(confirmed_refund_of,''), proposed_refund_of), confirmed_refund_of,
		       firefly_link_id, firefly_txn_id, COALESCE(confirmed_category_id, proposed_category_id)
		FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).
		Scan(&typ, &status, &tier, &effRef, &confRef, &linkID, &journal, &catID)
	if err != nil {
		return v, nil
	}
	v.Current = strings.TrimSpace(effRef.String)
	v.Confirmed = strings.TrimSpace(confRef.String) != ""
	v.LinkID = linkID.Int64
	kind, _, _ := refundref.Parse(v.Current)
	hasRef := kind == refundref.KindFold || kind == refundref.KindJournal
	isRefundCategory := catID.Valid && strings.EqualFold(h.lookupCategoryName(ctx, catID.Int64), "Refund")

	var cands []classifier.RefundCandidate
	if typ == "INCOMING" || hasRef {
		cands, _ = classifier.RefundCandidatesFor(ctx, h.db, uuid)
	}
	v.Show = hasRef || kind == refundref.KindNone || len(cands) > 0 || isRefundCategory ||
		(tier.Valid && tier.Int64 == int64(classifier.TierRefund))

	if v.Show {
		if v.Current == "" {
			v.Options = append(v.Options, refundOption{Value: "", Label: "— not set —", Selected: true})
		}
		listed := false
		for _, c := range cands {
			sel := c.Ref == v.Current
			listed = listed || sel
			v.Options = append(v.Options, refundOption{Value: c.Ref, Label: candidateLabel(c), Selected: sel})
		}
		if hasRef && !listed {
			v.Options = append(v.Options, refundOption{Value: v.Current, Label: h.refLabel(ctx, v.Current), Selected: true})
		}
		v.Options = append(v.Options, refundOption{
			Value: refundref.None, Label: "none — not a refund of a purchase I can pick here",
			Selected: kind == refundref.KindNone,
		})
		if hasRef && status == "pushed" && v.LinkID == 0 {
			if h.refInFirefly(ctx, v.Current) {
				v.CanLink = true
			} else {
				v.Waiting = true
			}
		}
	}
	return v, h.refundedBy(ctx, uuid, journal.Int64)
}

// candidateLabel renders a purchase for the picker, e.g.
// "13 Mar 2026 · INR 486.20 · ___ from Zomato for lunch · ready to push · exact".
func candidateLabel(c classifier.RefundCandidate) string {
	date, _ := istDateTime(c.When)
	parts := []string{date, "INR " + paiseToDecimal(c.AmountPaise)}
	desc := strings.TrimSpace(c.Description)
	if desc == "" {
		desc = c.Merchant
	}
	if desc != "" {
		parts = append(parts, desc)
	}
	switch {
	case c.Status == "firefly":
		parts = append(parts, fmt.Sprintf("in firefly (journal %d)", c.JournalID))
	case c.Status == "pushed":
		parts = append(parts, "pushed")
	default:
		parts = append(parts, strings.ReplaceAll(c.Status, "_", " "))
	}
	switch {
	case c.GroupMatch:
		parts = append(parts, "grouped by fold.money")
	case c.Exact:
		parts = append(parts, "exact amount")
	default:
		parts = append(parts, "partial — INR "+paiseToDecimal(c.RemainingPaise)+" not yet refunded")
	}
	return strings.Join(parts, " · ")
}

// refLabel describes a stored reference that isn't among the current
// candidates (e.g. a purchase the human picked that has since been fully
// claimed, or a firefly journal outside the lookback).
func (h *Handler) refLabel(ctx context.Context, ref string) string {
	kind, uuid, journal := refundref.Parse(ref)
	switch kind {
	case refundref.KindFold:
		var when, desc, status string
		var amt int64
		if err := h.db.QueryRowContext(ctx, `
			SELECT COALESCE(confirmed_txn_timestamp, txn_timestamp), COALESCE(confirmed_amount_paise, amount_paise),
			       COALESCE(NULLIF(confirmed_description,''), NULLIF(proposed_description,''), narration), status
			FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&when, &amt, &desc, &status); err == nil {
			d := when
			if t, ok := parseDBTime(when); ok {
				d, _ = istDateTime(t)
			}
			return fmt.Sprintf("%s · INR %s · %s · %s", d, paiseToDecimal(amt), desc, strings.ReplaceAll(status, "_", " "))
		}
		return "staged row " + uuid + " (not found)"
	case refundref.KindJournal:
		var when, desc string
		var amt int64
		if err := h.db.QueryRowContext(ctx,
			`SELECT date, amount_paise, description FROM firefly_txns WHERE firefly_id = ?`, journal).Scan(&when, &amt, &desc); err == nil {
			d := when
			if t, ok := parseDBTime(when); ok {
				d, _ = istDateTime(t)
			}
			return fmt.Sprintf("%s · INR %s · %s · in firefly (journal %d)", d, paiseToDecimal(amt), desc, journal)
		}
		return fmt.Sprintf("firefly journal %d", journal)
	}
	return ref
}

// refInFirefly: the referenced purchase exists in firefly now.
func (h *Handler) refInFirefly(ctx context.Context, ref string) bool {
	kind, uuid, _ := refundref.Parse(ref)
	switch kind {
	case refundref.KindJournal:
		return true
	case refundref.KindFold:
		var n int
		_ = h.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM staged_fold_txns WHERE fold_uuid = ? AND status = 'pushed' AND COALESCE(firefly_txn_id,0) <> 0`, uuid).Scan(&n)
		return n > 0
	}
	return false
}

// refundedBy lists the refunds that point at this purchase.
func (h *Handler) refundedBy(ctx context.Context, uuid string, journal int64) []refundedByRow {
	refs := []any{refundref.Fold(uuid)}
	if journal != 0 {
		refs = append(refs, refundref.Journal(journal))
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT fold_uuid, COALESCE(confirmed_txn_timestamp, txn_timestamp), COALESCE(confirmed_amount_paise, amount_paise),
		       COALESCE(NULLIF(confirmed_description,''), NULLIF(proposed_description,''), narration), status,
		       COALESCE(firefly_link_id, 0)
		FROM staged_fold_txns
		WHERE fold_uuid <> ? AND status <> 'skipped'
		  AND COALESCE(NULLIF(confirmed_refund_of,''), proposed_refund_of) IN (?`+strings.Repeat(",?", len(refs)-1)+`)
		ORDER BY txn_timestamp`, append([]any{uuid}, refs...)...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []refundedByRow
	for rows.Next() {
		var r refundedByRow
		var when string
		var amt int64
		if rows.Scan(&r.FoldUUID, &when, &amt, &r.Description, &r.Status, &r.LinkID) != nil {
			continue
		}
		r.When = when
		if t, ok := parseDBTime(when); ok {
			r.When, _ = istDateTime(t)
		}
		r.Amount = paiseToDecimal(amt)
		out = append(out, r)
	}
	return out
}

// handleLink creates the firefly "Refund" link for a pushed refund — the
// "link in firefly" button. A firefly WRITE, only ever on this click (push
// does the same automatically when it completes a pair).
func (h *Handler) handleLink(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if h.pusher == nil {
		h.flashErr(w, "linking is unavailable")
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	id, err := h.pusher.LinkRefund(r.Context(), uuid)
	if err != nil && id == 0 {
		h.flashErr(w, "not linked: "+err.Error())
	} else {
		h.flashOk(w, fmt.Sprintf("linked in firefly as a refund (link %d)", id))
	}
	http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
}

// saveRefundOf stores the human's "refund of" choice when the form carried
// the picker. "" clears it (the classifier's proposal applies again);
// "none" says there is no purchase; anything else must be a well-formed
// reference.
func (h *Handler) saveRefundOf(ctx context.Context, uuid, v string) (unresolved []string, err error) {
	v = strings.TrimSpace(v)
	var ref any
	switch {
	case v == "":
		ref = nil
	case refundref.Valid(v):
		ref = v
	default:
		return []string{fmt.Sprintf("refund of %q", v)}, nil
	}
	_, err = h.db.ExecContext(ctx, `UPDATE staged_fold_txns SET confirmed_refund_of = ? WHERE fold_uuid = ?`, ref, uuid)
	return nil, err
}
