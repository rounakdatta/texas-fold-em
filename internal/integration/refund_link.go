package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rounakdatta/texas-fold-em/internal/integration/refundref"
)

// Refund links.
//
// Firefly is the ledger, so the relationship "this deposit refunds that
// purchase" lives THERE, as a transaction link of firefly's built-in type
// "Refund". fold only stages the reference (confirmed_refund_of /
// proposed_refund_of) until both journals exist; the Pusher turns it into
// the link:
//   - pushing a refund whose purchase is already in firefly links it at once;
//   - pushing a purchase links every already-pushed refund waiting for it;
//   - the review UI's "link in firefly" retries one on demand.
//
// Linking never fails a push: the transaction itself is what matters, and a
// missing link is visible (and retryable) in the UI.

// ErrRefundOriginalNotInFirefly: the refund's purchase is still only staged.
var ErrRefundOriginalNotInFirefly = errors.New("the original purchase isn't in firefly yet — push it first")

// refundLinkNote is written on every link fold creates, so a link in firefly
// can be traced back to fold.
const refundLinkNote = "linked by texas-fold-em"

// refundLinkTypeID returns firefly's "Refund" link-type id, looked up once.
func (p *Pusher) refundLinkTypeID(ctx context.Context) (int64, error) {
	p.linkMu.Lock()
	defer p.linkMu.Unlock()
	if p.refundLinkType != 0 {
		return p.refundLinkType, nil
	}
	types, err := p.fc.ListLinkTypes(ctx)
	if err != nil {
		return 0, fmt.Errorf("list firefly link types: %w", err)
	}
	for _, t := range types {
		if strings.EqualFold(strings.TrimSpace(t.Attributes.Name), "Refund") {
			id, err := strconv.ParseInt(t.ID, 10, 64)
			if err == nil && id > 0 {
				p.refundLinkType = id
				return id, nil
			}
		}
	}
	return 0, errors.New(`firefly has no "Refund" link type`)
}

// effectiveRefundOf is the row's reference: the human's, else the classifier's.
const effectiveRefundOfSQL = `COALESCE(NULLIF(confirmed_refund_of,''), proposed_refund_of)`

// originalJournal resolves a refund reference to the purchase's firefly
// journal id. ok=false when the purchase is a staged row not pushed yet.
func (p *Pusher) originalJournal(ctx context.Context, ref string) (int64, bool, error) {
	kind, uuid, journal := refundref.Parse(ref)
	switch kind {
	case refundref.KindJournal:
		return journal, true, nil
	case refundref.KindFold:
		var id sql.NullInt64
		err := p.db.QueryRowContext(ctx,
			`SELECT firefly_txn_id FROM staged_fold_txns WHERE fold_uuid = ? AND status = 'pushed'`, uuid).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (!id.Valid || id.Int64 == 0)) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		return id.Int64, true, nil
	}
	return 0, false, nil
}

// linkRefunds creates the links a successful push or update completes, in
// both directions. Best effort: failures are logged and audited, never
// returned.
func (p *Pusher) linkRefunds(ctx context.Context, foldUUID string) {
	if p.readOnly || p.db == nil {
		return
	}
	// 1. This row is a refund whose purchase is (now) in firefly.
	if _, err := p.LinkRefund(ctx, foldUUID); err != nil &&
		!errors.Is(err, ErrRefundOriginalNotInFirefly) && !errors.Is(err, errNotARefund) && !errors.Is(err, errAlreadyLinked) {
		p.log.Warn("refund link failed (push still succeeded)", "fold_uuid", foldUUID, "err", err)
	}
	// 2. This row is a purchase that pushed refunds were waiting for.
	var journal sql.NullInt64
	_ = p.db.QueryRowContext(ctx, `SELECT firefly_txn_id FROM staged_fold_txns WHERE fold_uuid = ?`, foldUUID).Scan(&journal)
	refs := []any{refundref.Fold(foldUUID)}
	if journal.Valid && journal.Int64 != 0 {
		refs = append(refs, refundref.Journal(journal.Int64))
	}
	q := `SELECT fold_uuid FROM staged_fold_txns
	      WHERE status = 'pushed' AND firefly_link_id IS NULL AND fold_uuid <> ?
	        AND ` + effectiveRefundOfSQL + ` IN (?` + strings.Repeat(",?", len(refs)-1) + `)`
	rows, err := p.db.QueryContext(ctx, q, append([]any{foldUUID}, refs...)...)
	if err != nil {
		p.log.Warn("refund link: find waiting refunds failed", "fold_uuid", foldUUID, "err", err)
		return
	}
	var waiting []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			waiting = append(waiting, u)
		}
	}
	rows.Close()
	for _, u := range waiting {
		if _, err := p.LinkRefund(ctx, u); err != nil && !errors.Is(err, errAlreadyLinked) {
			p.log.Warn("refund link for waiting refund failed", "refund", u, "original", foldUUID, "err", err)
		}
	}
}

var (
	errNotARefund    = errors.New("this row has no original purchase to link")
	errAlreadyLinked = errors.New("already linked in firefly")
	errNotPushed     = errors.New("the refund isn't in firefly yet — push it first")
)

// LinkRefund creates (or adopts) the firefly "Refund" link for one pushed
// refund: <refund> "(partially) refunds" <purchase>. Returns the link id.
// This is a firefly WRITE; the review UI calls it only on the operator's
// click, and push calls it right after the operator's push.
func (p *Pusher) LinkRefund(ctx context.Context, foldUUID string) (int64, error) {
	if p.readOnly {
		return 0, PushReadOnlyError
	}
	var (
		status  string
		journal sql.NullInt64
		linkID  sql.NullInt64
		ref     sql.NullString
	)
	err := p.db.QueryRowContext(ctx, `
		SELECT status, firefly_txn_id, firefly_link_id, `+effectiveRefundOfSQL+`
		FROM staged_fold_txns WHERE fold_uuid = ?`, foldUUID).Scan(&status, &journal, &linkID, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", PushNotFoundError, foldUUID)
	}
	if err != nil {
		return 0, err
	}
	if kind, _, _ := refundref.Parse(ref.String); kind != refundref.KindFold && kind != refundref.KindJournal {
		return 0, errNotARefund
	}
	if linkID.Valid && linkID.Int64 != 0 {
		return linkID.Int64, errAlreadyLinked
	}
	if status != "pushed" || !journal.Valid || journal.Int64 == 0 {
		return 0, errNotPushed
	}
	original, ok, err := p.originalJournal(ctx, ref.String)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrRefundOriginalNotInFirefly
	}
	if original == journal.Int64 {
		return 0, errors.New("a refund can't refund itself")
	}
	typeID, err := p.refundLinkTypeID(ctx)
	if err != nil {
		p.audit(ctx, "firefly_link_error", foldUUID, journal.Int64, nil, statusFromErr(err), err.Error())
		return 0, err
	}

	// Idempotent: adopt a Refund link firefly already has between the two
	// (made by an earlier attempt, or by hand in firefly — either way round).
	if links, err := p.fc.ListJournalLinks(ctx, journal.Int64); err == nil {
		want := strconv.FormatInt(typeID, 10)
		r, o := strconv.FormatInt(journal.Int64, 10), strconv.FormatInt(original, 10)
		for _, l := range links {
			a := l.Attributes
			if a.LinkTypeID == want && ((a.InwardID == r && a.OutwardID == o) || (a.InwardID == o && a.OutwardID == r)) {
				if id, perr := strconv.ParseInt(l.ID, 10, 64); perr == nil && id > 0 {
					if err := p.setLinkID(ctx, foldUUID, id); err != nil {
						return 0, err
					}
					p.audit(ctx, "firefly_link_existing", foldUUID, journal.Int64, nil, 200,
						fmt.Sprintf("refund %d already linked to purchase %d (link %d)", journal.Int64, original, id))
					return id, nil
				}
			}
		}
	}

	body := map[string]any{"link_type_id": typeID, "inward_id": journal.Int64, "outward_id": original}
	id, err := p.fc.CreateTransactionLink(ctx, typeID, journal.Int64, original, refundLinkNote)
	if err != nil {
		p.audit(ctx, "firefly_link_error", foldUUID, journal.Int64, body, statusFromErr(err), err.Error())
		return 0, fmt.Errorf("firefly link: %w", err)
	}
	if err := p.setLinkID(ctx, foldUUID, id); err != nil {
		return 0, err
	}
	p.audit(ctx, "firefly_link_create", foldUUID, journal.Int64, body, 200,
		fmt.Sprintf("refund %d (partially) refunds purchase %d (link %d)", journal.Int64, original, id))
	return id, nil
}

func (p *Pusher) setLinkID(ctx context.Context, foldUUID string, linkID int64) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE staged_fold_txns SET firefly_link_id = ?, updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`, linkID, foldUUID)
	return err
}
