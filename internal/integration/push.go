package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// PushNotFoundError is returned when the requested fold_uuid isn't
// present in staged_fold_txns.
var PushNotFoundError = errors.New("push: staged transaction not found")

// PushNotReadyError is returned when the row isn't in a status that
// allows pushing (only ready_to_push is accepted by default).
var PushNotReadyError = errors.New("push: row is not in ready_to_push status")

// PushReadOnlyError is returned when the operator-facing read-only
// kill-switch is set. The push handler is not registered when this is
// true at server-start time, but the error is also surfaced from
// the orchestrator directly so a misconfigured caller gets a clear
// message rather than a confusing 401/403.
var PushReadOnlyError = errors.New("push: TEXAS_FOLDEM_FIREFLY_READONLY is set; refusing to write")

// PushReport is the structured outcome of a push attempt.
type PushReport struct {
	FoldUUID       string    `json:"fold_uuid"`
	Action         string    `json:"action"` // "preview" | "created" | "deduped"
	FireflyTxnID   int64     `json:"firefly_txn_id,omitempty"`
	FireflyGroupID int64     `json:"firefly_group_id,omitempty"`
	PushedAt       time.Time `json:"pushed_at,omitempty"`
	PreviewBody    any       `json:"preview_body,omitempty"` // present when Action="preview"
}

// Learner is the optional active-learning hook called after a
// successful push. The classifier package's Classifier satisfies it.
// Decoupled via interface so push.go doesn't need to import classifier
// directly (which would also work since they're sibling subpackages,
// but interface-coupling lets us mock learning in push_test.go).
type Learner interface {
	LearnFromPushed(ctx context.Context, foldUUID string) error
}

// Pusher orchestrates: validate staged row → check firefly for an
// existing transaction with the same external_id → POST → audit →
// update staged_fold_txns. Read-only mode short-circuits before any
// write.
type Pusher struct {
	db          *DB
	fc          *firefly.Client
	log         *slog.Logger
	readOnly    bool
	learner     Learner // optional; when set, push success triggers reinforcement
	eagerSyncer *Syncer // optional; when set, push success mirrors the just-created firefly journal into firefly_txns
}

// NewPusher constructs a Pusher with no learner.
func NewPusher(db *DB, fc *firefly.Client, log *slog.Logger, readOnly bool) *Pusher {
	return &Pusher{db: db, fc: fc, log: log.With("component", "pusher"), readOnly: readOnly}
}

// SetLearner attaches a Learner. After a successful real push (not a
// preview, not a dedup-no-op-for-already-pushed), the Pusher will call
// LearnFromPushed. Failures in the learner are logged but never
// surface to the push caller.
func (p *Pusher) SetLearner(l Learner) { p.learner = l }

// SetEagerSyncer attaches a Syncer that the Pusher uses to eagerly
// mirror the just-created firefly journal into firefly_txns
// immediately after Push succeeds. Without this, the journal lands in
// our mirror only when the next periodic /admin/firefly/sync runs
// (up to an hour later) — meaning any correction the operator made
// during review takes that long to flow back into the classifier's
// FTS index and the Tier-3 STYLE SAMPLES. With it, the next
// classification of a similar transaction sees the correction within
// seconds. Failures are logged-not-fatal so a degraded mirror never
// breaks the push itself.
func (p *Pusher) SetEagerSyncer(s *Syncer) { p.eagerSyncer = s }

// pushableRow is the slim view of staged_fold_txns we read for a push.
type pushableRow struct {
	FoldUUID     string
	AmountPaise  int64
	Currency     string
	// Foreign side of a cross-currency charge (e.g. AED 5.99) — set when the
	// original charge currency differs from the home currency. NULL for
	// domestic transactions.
	ForeignAmountPaise sql.NullInt64
	ForeignCurrency    sql.NullString
	TxnTimestamp time.Time
	Type         string // INCOMING | OUTGOING
	Status       string

	ConfirmedSourceAccountID        sql.NullInt64
	ConfirmedDestinationAccountID   sql.NullInt64
	ConfirmedDestinationAccountName sql.NullString
	ConfirmedCategoryID             sql.NullInt64
	ConfirmedBudgetID               sql.NullInt64
	ConfirmedDescription            sql.NullString
	ConfirmedTagsJSON               sql.NullString

	ConfirmedTxnType sql.NullString

	// Fall-through: when confirmed_* is null we use proposed_* (auto-classified).
	ProposedSourceAccountID        sql.NullInt64
	ProposedDestinationAccountID   sql.NullInt64
	ProposedDestinationAccountName sql.NullString
	ProposedCategoryID             sql.NullInt64
	ProposedBudgetID               sql.NullInt64
	ProposedDescription            sql.NullString
	ProposedTxnType                sql.NullString

	// Narration is the raw fold-side string ("CARD/.../MERCHANT/Rs./AMT/...").
	// Used as firefly's notes field on every push so the operator has the
	// underlying ground truth available alongside the friendlier title.
	Narration string

	// MerchantExtracted is the normalised merchant string (e.g. "zomato",
	// "neon market cafe"). Used as the description fallback when both
	// confirmed_description and proposed_description are empty — cleaner
	// than raw narration, which now lives in notes anyway.
	MerchantExtracted string

	FireflyTxnID sql.NullInt64
	// FireflyGroupID is the transaction GROUP id — needed to UPDATE an
	// already-pushed transaction in place (PUT /transactions/{group}).
	// Resolved from the stored column, falling back to the firefly_txns
	// mirror for rows pushed before we persisted it.
	FireflyGroupID sql.NullInt64
}

// Push runs the full push pipeline. confirm=false runs in dry-run /
// preview mode: returns the firefly POST body without making any
// network call. confirm=true performs the dedup check + create +
// audit + DB update.
//
// Idempotency: if the fold_uuid already has a firefly_txn_id recorded
// in staged_fold_txns, OR firefly already has a transaction with
// external_id=<fold_uuid>, we skip the create and return Action="deduped".
func (p *Pusher) Push(ctx context.Context, foldUUID string, confirm bool) (PushReport, error) {
	if p.readOnly && confirm {
		return PushReport{}, PushReadOnlyError
	}
	row, err := p.fetchPushableRow(ctx, foldUUID)
	if err != nil {
		return PushReport{}, err
	}
	if row.Status != "ready_to_push" && row.Status != "needs_review" {
		// Allow pushing from needs_review via UI (the human just
		// confirmed it), but not from pending (classifier hasn't run)
		// or pushed/skipped (terminal).
		return PushReport{}, fmt.Errorf("%w: status=%q", PushNotReadyError, row.Status)
	}
	if row.FireflyTxnID.Valid && row.FireflyTxnID.Int64 != 0 {
		// Already pushed in a previous run. Surface as a no-op rather
		// than re-creating.
		return PushReport{
			FoldUUID:     row.FoldUUID,
			Action:       "deduped",
			FireflyTxnID: row.FireflyTxnID.Int64,
		}, nil
	}

	body := p.buildCreateRequest(ctx, row)

	if !confirm {
		return PushReport{
			FoldUUID:    row.FoldUUID,
			Action:      "preview",
			PreviewBody: body,
		}, nil
	}

	// Idempotency check against firefly.
	existing, err := p.fc.SearchByExternalID(ctx, row.FoldUUID)
	if err != nil {
		p.audit(ctx, "firefly_search_error", row.FoldUUID, 0, body, 0, err.Error())
		return PushReport{}, fmt.Errorf("firefly dedup check: %w", err)
	}
	if len(existing) > 0 {
		// Firefly already has a transaction for this fold_uuid. Mark
		// our row as pushed pointing at the existing journal.
		journalID := existing[0]
		if err := p.markPushed(ctx, row.FoldUUID, journalID, 0); err != nil {
			return PushReport{}, fmt.Errorf("mark pushed (dedup hit): %w", err)
		}
		p.audit(ctx, "firefly_dedup_hit", row.FoldUUID, journalID, body, 200, "external_id already exists in firefly")
		return PushReport{
			FoldUUID:     row.FoldUUID,
			Action:       "deduped",
			FireflyTxnID: journalID,
			PushedAt:     time.Now().UTC(),
		}, nil
	}

	// Ensure the foreign currency exists+enabled in firefly before create.
	// An abroad charge in a currency the user's firefly has never enabled
	// (e.g. AED) would 422 otherwise; we create-or-enable it on demand, the
	// same spirit as firefly auto-creating a new expense account on push.
	if len(body.Transactions) > 0 {
		if fcur := body.Transactions[0].ForeignCurrencyCode; fcur != "" {
			if err := p.fc.EnsureCurrency(ctx, fcur); err != nil {
				p.audit(ctx, "firefly_ensure_currency_error", row.FoldUUID, 0, body, statusFromErr(err), err.Error())
				return PushReport{}, fmt.Errorf("firefly ensure currency %s: %w", fcur, err)
			}
		}
	}

	// Real create.
	resp, err := p.fc.CreateTransaction(ctx, body)
	if err != nil {
		p.audit(ctx, "firefly_create_error", row.FoldUUID, 0, body, statusFromErr(err), err.Error())
		return PushReport{}, fmt.Errorf("firefly create: %w", err)
	}
	var journalID int64
	if len(resp.JournalIDs) > 0 {
		journalID = resp.JournalIDs[0]
	}
	if err := p.markPushed(ctx, row.FoldUUID, journalID, resp.GroupID); err != nil {
		return PushReport{}, fmt.Errorf("mark pushed: %w", err)
	}
	p.audit(ctx, "firefly_create", row.FoldUUID, journalID, body, 200, "")

	// Eager mirror: pull the just-created firefly journal back into our
	// local mirror so the next classification round sees this row's
	// description / notes / category without waiting for the periodic
	// /admin/firefly/sync. Latency drops from ≤1h to ~2s. Failures are
	// logged-not-fatal so a degraded mirror never breaks the push
	// itself; the periodic sync will reconcile on its next pass.
	if p.eagerSyncer != nil && resp.GroupID > 0 {
		group, err := p.fc.GetTransaction(ctx, resp.GroupID)
		if err != nil {
			p.log.Warn("eager mirror: GetTransaction failed (push still succeeded)",
				"fold_uuid", row.FoldUUID, "group_id", resp.GroupID, "err", err)
		} else if groupID, perr := strconv.ParseInt(group.Data.ID, 10, 64); perr != nil {
			p.log.Warn("eager mirror: parse group id failed",
				"fold_uuid", row.FoldUUID, "group_id_raw", group.Data.ID, "err", perr)
		} else {
			// A split would contain several journals under the same group;
			// mirror the one that matches the id we just pushed. (For
			// the non-split common case there is exactly one journal.)
			wantID := strconv.FormatInt(journalID, 10)
			for _, j := range group.Data.Attributes.Transactions {
				if j.JournalID != wantID {
					continue
				}
				if err := p.eagerSyncer.MirrorJournal(ctx, j, groupID); err != nil {
					p.log.Warn("eager mirror: upsert failed (push still succeeded)",
						"fold_uuid", row.FoldUUID, "journal_id", journalID, "err", err)
				}
				break
			}
		}
	}

	// Active-learning reinforcement. Synchronous so tests can assert,
	// failures logged-not-fatal so a degraded learner can't break push.
	// Runs AFTER the eager mirror so the learner reads from a mirror
	// that already has this row reflected.
	if p.learner != nil {
		if err := p.learner.LearnFromPushed(ctx, row.FoldUUID); err != nil {
			p.log.Warn("learner failed; push still succeeded", "fold_uuid", row.FoldUUID, "err", err)
		}
	}

	return PushReport{
		FoldUUID:       row.FoldUUID,
		Action:         "created",
		FireflyTxnID:   journalID,
		FireflyGroupID: resp.GroupID,
		PushedAt:       time.Now().UTC(),
	}, nil
}

func (p *Pusher) fetchPushableRow(ctx context.Context, foldUUID string) (pushableRow, error) {
	var r pushableRow
	err := p.db.QueryRowContext(ctx, `
		SELECT fold_uuid, amount_paise, currency, foreign_amount_paise, foreign_currency,
		       txn_timestamp, type, status,
		       confirmed_source_account_id, confirmed_destination_account_id, confirmed_destination_account_name,
		       confirmed_category_id, confirmed_budget_id,
		       confirmed_description, confirmed_tags_json, confirmed_txn_type,
		       proposed_source_account_id, proposed_destination_account_id, proposed_destination_account_name,
		       proposed_category_id, proposed_budget_id, proposed_description,
		       proposed_txn_type,
		       narration, COALESCE(merchant_extracted, ''), firefly_txn_id,
		       COALESCE(firefly_group_id,
		                (SELECT group_id FROM firefly_txns WHERE firefly_id = staged_fold_txns.firefly_txn_id LIMIT 1))
		FROM staged_fold_txns
		WHERE fold_uuid = ?
	`, foldUUID).Scan(
		&r.FoldUUID, &r.AmountPaise, &r.Currency, &r.ForeignAmountPaise, &r.ForeignCurrency,
		&r.TxnTimestamp, &r.Type, &r.Status,
		&r.ConfirmedSourceAccountID, &r.ConfirmedDestinationAccountID, &r.ConfirmedDestinationAccountName,
		&r.ConfirmedCategoryID, &r.ConfirmedBudgetID,
		&r.ConfirmedDescription, &r.ConfirmedTagsJSON, &r.ConfirmedTxnType,
		&r.ProposedSourceAccountID, &r.ProposedDestinationAccountID, &r.ProposedDestinationAccountName,
		&r.ProposedCategoryID, &r.ProposedBudgetID, &r.ProposedDescription,
		&r.ProposedTxnType,
		&r.Narration, &r.MerchantExtracted, &r.FireflyTxnID, &r.FireflyGroupID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: %s", PushNotFoundError, foldUUID)
	}
	if err != nil {
		return r, fmt.Errorf("fetch staged row: %w", err)
	}
	return r, nil
}

// buildCreateRequest is what we'd POST to firefly. Pure function, no
// side effects — used both for live creates AND dry-run previews.
//
// Field resolution: confirmed_* takes precedence; if NULL, we fall
// back to proposed_*. This means an auto-classified row (where the
// human never edited anything) is pushable directly with whatever the
// classifier proposed.
func (p *Pusher) buildCreateRequest(ctx context.Context, row pushableRow) firefly.CreateTransactionRequest {
	srcID := pickInt64(row.ConfirmedSourceAccountID, row.ProposedSourceAccountID)
	destID := pickInt64(row.ConfirmedDestinationAccountID, row.ProposedDestinationAccountID)
	catID := pickInt64(row.ConfirmedCategoryID, row.ProposedCategoryID)
	budID := pickInt64(row.ConfirmedBudgetID, row.ProposedBudgetID)
	// Description fallback ladder:
	//   1. confirmed_description (human edit) — always wins
	//   2. proposed_description (LLM's title suggestion)
	//   3. merchant_extracted (normalised, e.g. "neon market cafe")
	//   4. narration (raw fold string) — last resort
	//
	// Narration is now mirrored to firefly's notes field unconditionally,
	// so using it as the description would just duplicate. The merchant
	// fallback gives Tier-1/Tier-2 rows (where the LLM didn't fill
	// description_suggestion) a readable title without an LLM call.
	desc := pickString(row.ConfirmedDescription, row.ProposedDescription)
	if desc == "" && row.MerchantExtracted != "" {
		desc = row.MerchantExtracted
	}
	if desc == "" {
		desc = row.Narration
	}

	var tags []string
	if row.ConfirmedTagsJSON.Valid && row.ConfirmedTagsJSON.String != "" {
		_ = json.Unmarshal([]byte(row.ConfirmedTagsJSON.String), &tags)
	}

	// Type resolution: explicit override (confirmed > proposed) wins.
	// Fall back to fold-direction mapping for legacy rows. This is what
	// lets the classifier emit "transfer" — fold itself only knows
	// INCOMING/OUTGOING, but Tier 3 reasons that both endpoints are
	// the user's own asset accounts and writes "transfer" into
	// proposed_txn_type. The Pusher honours that.
	txnType := pickString(row.ConfirmedTxnType, row.ProposedTxnType)
	if txnType == "" {
		txnType = foldTypeToFireflyType(row.Type)
	}
	// asset → asset ⇒ transfer. firefly rejects an asset account as a
	// withdrawal/deposit endpoint, so a credit-card repayment (HDFC Bank →
	// Scapia CC, both the user's own assets) pushed as a "withdrawal" 422s
	// with "could not find a valid destination account for id 954". Both
	// endpoints being assets is the deterministic signal for a transfer —
	// override whatever type was proposed. The classifier's LLM transfer
	// detection is best-effort and a manual edit can change the endpoints,
	// so this push-time check is the authoritative guard.
	// asset ↔ asset ⇒ transfer. Resolve each endpoint to an asset id (itself
	// when it's already an asset, or a same-named asset twin of a duplicate
	// expense payee). Only when BOTH resolve to assets do we flip to a
	// transfer and use those asset ids — so a normal purchase (asset →
	// expense merchant, which has no asset twin) stays a withdrawal, while a
	// credit-card repayment / inter-account move (asset → asset, even when
	// the destination resolved to the card's duplicate EXPENSE twin) is
	// corrected. firefly rejects an asset as a withdrawal/deposit endpoint,
	// so this override is what lets such rows push at all.
	if sa, da := p.assetTwin(ctx, srcID), p.assetTwin(ctx, destID); sa != 0 && da != 0 {
		txnType = "transfer"
		srcID, destID = sa, da
	}

	line := firefly.CreateTransactionLine{
		Type:         txnType,
		Date:         row.TxnTimestamp.Format(time.RFC3339),
		Amount:       paiseToDecimal(row.AmountPaise),
		CurrencyCode: row.Currency,
		Description:  desc,
		ExternalID:   row.FoldUUID,
		Tags:         tags,
		// Notes carries the raw fold narration (e.g. "CARD/19b4a7.../NEON
		// MARKET CAFE/Rs./1313.00/OUTGOING/23-12-25") so the operator
		// always has the bank's ground-truth string alongside firefly's
		// friendlier description. The narration is what classification
		// was performed on; preserving it here closes the audit loop.
		Notes: row.Narration,
	}
	// Cross-currency: record the original charge (e.g. AED 5.99) as the
	// foreign amount alongside the INR primary. Only when it genuinely
	// differs from the home currency. firefly requires the foreign currency
	// to exist — Push calls EnsureCurrency before the create.
	if row.ForeignCurrency.Valid && row.ForeignCurrency.String != "" &&
		row.ForeignCurrency.String != row.Currency && row.ForeignAmountPaise.Valid {
		line.ForeignAmount = paiseToDecimal(row.ForeignAmountPaise.Int64)
		line.ForeignCurrencyCode = row.ForeignCurrency.String
	}
	if srcID != 0 {
		line.SourceID = strconv.FormatInt(srcID, 10)
	}
	if destID != 0 {
		line.DestinationID = strconv.FormatInt(destID, 10)
	} else if destName := pickString(row.ConfirmedDestinationAccountName, row.ProposedDestinationAccountName); destName != "" {
		// No existing account id — send the name so firefly finds-or-creates
		// the expense account by that name (its standard behaviour on a
		// withdrawal POST). This is the push side of the classifier's
		// "propose a new destination for a novel merchant" path.
		line.DestinationName = destName
	}
	if catID != 0 {
		line.CategoryID = strconv.FormatInt(catID, 10)
	}
	if budID != 0 {
		line.BudgetID = strconv.FormatInt(budID, 10)
	}

	return firefly.CreateTransactionRequest{
		Transactions: []firefly.CreateTransactionLine{line},
	}
}

// markPushed updates staged_fold_txns to terminal pushed state.
// FireflyTxnView is the current state of a firefly transaction, as names,
// for pre-filling the edit form when correcting an already-pushed row.
type FireflyTxnView struct {
	Type            string
	SourceName      string
	DestinationName string
	CategoryName    string
	BudgetName      string
	Description     string
	Tags            []string
}

// CurrentFireflyView pulls the LIVE firefly transaction for a pushed row and
// returns its current field values, so the review form can be pre-filled
// from firefly's actual state (including edits made directly in firefly)
// before the operator corrects it — the "pull remote first" half of a safe
// update. Re-mirrors the pulled journal locally too. ok=false when the row
// isn't pushed, has no group id, or the fetch fails (caller falls back to
// the stored proposal).
func (p *Pusher) CurrentFireflyView(ctx context.Context, foldUUID string) (FireflyTxnView, bool) {
	row, err := p.fetchPushableRow(ctx, foldUUID)
	if err != nil || !row.FireflyGroupID.Valid || row.FireflyGroupID.Int64 == 0 {
		return FireflyTxnView{}, false
	}
	group, err := p.fc.GetTransaction(ctx, row.FireflyGroupID.Int64)
	if err != nil || len(group.Data.Attributes.Transactions) == 0 {
		return FireflyTxnView{}, false
	}
	j := group.Data.Attributes.Transactions[0]
	if p.eagerSyncer != nil {
		_ = p.eagerSyncer.MirrorJournal(ctx, j, row.FireflyGroupID.Int64)
	}
	return FireflyTxnView{
		Type:            j.Type,
		SourceName:      j.SourceName,
		DestinationName: j.DestinationName,
		CategoryName:    j.CategoryName,
		BudgetName:      j.BudgetName,
		Description:     j.Description,
		Tags:            j.Tags,
	}, true
}

// Update applies a re-classified row's corrected fields to its EXISTING
// firefly transaction, in place (PUT — no duplicate). The row must already
// be pushed and carry a group id. It re-mirrors the updated group afterward
// so the local corpus stays current.
func (p *Pusher) Update(ctx context.Context, foldUUID string) (PushReport, error) {
	if p.readOnly {
		return PushReport{}, fmt.Errorf("push: read-only mode is on")
	}
	row, err := p.fetchPushableRow(ctx, foldUUID)
	if err != nil {
		return PushReport{}, err
	}
	if !row.FireflyGroupID.Valid || row.FireflyGroupID.Int64 == 0 {
		return PushReport{}, fmt.Errorf("update: row has no firefly group id")
	}
	groupID := row.FireflyGroupID.Int64

	body := p.buildCreateRequest(ctx, row)
	if len(body.Transactions) == 0 {
		return PushReport{}, fmt.Errorf("update: empty transaction body")
	}
	// Target the existing journal so firefly updates it in place.
	if row.FireflyTxnID.Valid {
		body.Transactions[0].TransactionJournalID = strconv.FormatInt(row.FireflyTxnID.Int64, 10)
	}
	if fcur := body.Transactions[0].ForeignCurrencyCode; fcur != "" {
		if err := p.fc.EnsureCurrency(ctx, fcur); err != nil {
			p.audit(ctx, "firefly_ensure_currency_error", foldUUID, 0, body, statusFromErr(err), err.Error())
			return PushReport{}, fmt.Errorf("firefly ensure currency %s: %w", fcur, err)
		}
	}
	resp, err := p.fc.UpdateTransaction(ctx, groupID, body)
	if err != nil {
		p.audit(ctx, "firefly_update_error", foldUUID, nullableInt64Value(row.FireflyTxnID), body, statusFromErr(err), err.Error())
		return PushReport{}, fmt.Errorf("firefly update: %w", err)
	}
	journalID := nullableInt64Value(row.FireflyTxnID)
	if len(resp.JournalIDs) > 0 {
		journalID = resp.JournalIDs[0]
	}
	if err := p.markPushed(ctx, foldUUID, journalID, groupID); err != nil {
		return PushReport{}, fmt.Errorf("mark updated: %w", err)
	}
	p.audit(ctx, "firefly_update", foldUUID, journalID, body, 200, "")

	if p.eagerSyncer != nil {
		if group, gerr := p.fc.GetTransaction(ctx, groupID); gerr == nil {
			for _, j := range group.Data.Attributes.Transactions {
				_ = p.eagerSyncer.MirrorJournal(ctx, j, groupID)
			}
		}
	}
	if p.learner != nil {
		_ = p.learner.LearnFromPushed(ctx, foldUUID)
	}
	return PushReport{
		FoldUUID:       foldUUID,
		Action:         "updated",
		FireflyTxnID:   journalID,
		FireflyGroupID: groupID,
		PushedAt:       time.Now().UTC(),
	}, nil
}

// nullableInt64Value returns the int64 or 0.
func nullableInt64Value(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}

// isAsset reports whether a firefly account id is one of the user's own
// asset accounts, per the firefly_accounts mirror. Used to detect
// asset→asset transfers at push time. Returns false when the id is 0, the
// mirror lacks the row, or there's no db — all of which safely keep the
// default (non-transfer) behaviour.
func (p *Pusher) isAsset(ctx context.Context, id int64) bool {
	if p.db == nil || id == 0 {
		return false
	}
	var n int
	err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM firefly_accounts WHERE firefly_id = ? AND type = 'asset' AND active = 1`, id).Scan(&n)
	return err == nil && n > 0
}

// assetTwin returns an asset-account id for a transfer endpoint: the id
// itself when it's already an asset, otherwise a same-named asset account
// (the "asset twin" of a duplicate expense/payee account), else 0 (leave
// the endpoint as-is). Case-insensitive name match.
func (p *Pusher) assetTwin(ctx context.Context, id int64) int64 {
	if id == 0 || p.db == nil {
		return 0
	}
	if p.isAsset(ctx, id) {
		return id
	}
	var name string
	if err := p.db.QueryRowContext(ctx,
		`SELECT name FROM firefly_accounts WHERE firefly_id = ?`, id).Scan(&name); err != nil || name == "" {
		return 0
	}
	var twin sql.NullInt64
	_ = p.db.QueryRowContext(ctx,
		`SELECT firefly_id FROM firefly_accounts WHERE LOWER(name) = LOWER(?) AND type = 'asset' AND active = 1 LIMIT 1`,
		name).Scan(&twin)
	if twin.Valid {
		return twin.Int64
	}
	return 0
}

func (p *Pusher) markPushed(ctx context.Context, foldUUID string, journalID, groupID int64) error {
	// Persist BOTH the journal id (firefly_txn_id) and the group id. The
	// group id backs the review UI's firefly deep-link directly, so the link
	// no longer depends on the firefly_txns mirror having caught up.
	var gid any
	if groupID > 0 {
		gid = groupID
	}
	_, err := p.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET status = 'pushed',
		    firefly_txn_id = ?,
		    firefly_group_id = ?,
		    pushed_at = CURRENT_TIMESTAMP,
		    updated_at = CURRENT_TIMESTAMP
		WHERE fold_uuid = ?
	`, journalID, gid, foldUUID)
	return err
}

// audit inserts an append-only audit_log row. Failures here are logged
// but don't fail the calling action — losing an audit row is bad but
// not as bad as failing a push that already went through.
func (p *Pusher) audit(ctx context.Context, action, foldUUID string, fireflyTxnID int64, body any, status int, notes string) {
	bodyJSON, _ := json.Marshal(body)
	hash := sha256.Sum256(bodyJSON)
	if _, err := p.db.ExecContext(ctx, `
		INSERT INTO audit_log (actor, action, fold_uuid, firefly_txn_id, payload_hash, response_status, notes)
		VALUES ('system', ?, ?, ?, ?, ?, ?)
	`,
		action,
		foldUUID,
		nullableInt64Local(fireflyTxnID),
		hex.EncodeToString(hash[:]),
		status,
		nullableStringLocal(notes),
	); err != nil {
		p.log.Warn("audit insert failed", "action", action, "fold_uuid", foldUUID, "err", err)
	}
}

func pickInt64(confirmed, proposed sql.NullInt64) int64 {
	if confirmed.Valid {
		return confirmed.Int64
	}
	if proposed.Valid {
		return proposed.Int64
	}
	return 0
}

func pickString(confirmed, proposed sql.NullString) string {
	if confirmed.Valid && confirmed.String != "" {
		return confirmed.String
	}
	if proposed.Valid {
		return proposed.String
	}
	return ""
}

// foldTypeToFireflyType maps fold's type field to firefly's. We treat
// anything we don't recognise as 'withdrawal' to be safe (most fold
// transactions are outgoing).
func foldTypeToFireflyType(foldType string) string {
	switch strings.ToUpper(foldType) {
	case "INCOMING":
		return "deposit"
	case "OUTGOING":
		return "withdrawal"
	default:
		return "withdrawal"
	}
}

// paiseToDecimal converts integer paise back to firefly's decimal-string
// wire format ("70.00", "1234.56"). Always 2 decimals.
func paiseToDecimal(paise int64) string {
	if paise < 0 {
		paise = -paise
	}
	whole := paise / 100
	frac := paise % 100
	return fmt.Sprintf("%d.%02d", whole, frac)
}

// statusFromErr extracts an HTTP status code from a firefly *Error,
// or 0 if the error is from somewhere else (network, parse, etc).
func statusFromErr(err error) int {
	var ferr *firefly.Error
	if errors.As(err, &ferr) {
		return ferr.Status
	}
	return 0
}

// nullableInt64Local + nullableStringLocal: separate names from the
// helpers in sync.go to avoid clashes if both files ever live in the
// same package symbol space (they do — same package). Kept here as
// pure-pull helpers for the audit insert.
func nullableInt64Local(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableStringLocal(s string) any {
	if s == "" {
		return nil
	}
	return s
}
