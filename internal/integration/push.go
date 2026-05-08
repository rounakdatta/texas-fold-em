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
	FoldUUID            string    `json:"fold_uuid"`
	Action              string    `json:"action"`               // "preview" | "created" | "deduped"
	FireflyTxnID        int64     `json:"firefly_txn_id,omitempty"`
	FireflyGroupID      int64     `json:"firefly_group_id,omitempty"`
	PushedAt            time.Time `json:"pushed_at,omitempty"`
	PreviewBody         any       `json:"preview_body,omitempty"` // present when Action="preview"
}

// Pusher orchestrates: validate staged row → check firefly for an
// existing transaction with the same external_id → POST → audit →
// update staged_fold_txns. Read-only mode short-circuits before any
// write.
type Pusher struct {
	db       *DB
	fc       *firefly.Client
	log      *slog.Logger
	readOnly bool
}

// NewPusher constructs a Pusher.
func NewPusher(db *DB, fc *firefly.Client, log *slog.Logger, readOnly bool) *Pusher {
	return &Pusher{db: db, fc: fc, log: log.With("component", "pusher"), readOnly: readOnly}
}

// pushableRow is the slim view of staged_fold_txns we read for a push.
type pushableRow struct {
	FoldUUID                  string
	AmountPaise               int64
	Currency                  string
	TxnTimestamp              time.Time
	Type                      string // INCOMING | OUTGOING
	Status                    string

	ConfirmedSourceAccountID      sql.NullInt64
	ConfirmedDestinationAccountID sql.NullInt64
	ConfirmedCategoryID           sql.NullInt64
	ConfirmedBudgetID             sql.NullInt64
	ConfirmedDescription          sql.NullString
	ConfirmedTagsJSON             sql.NullString

	// Fall-through: when confirmed_* is null we use proposed_* (auto-classified).
	ProposedSourceAccountID      sql.NullInt64
	ProposedDestinationAccountID sql.NullInt64
	ProposedCategoryID           sql.NullInt64
	ProposedBudgetID             sql.NullInt64
	ProposedDescription          sql.NullString

	// Default fallback for description: the original narration.
	Narration string
	FireflyTxnID sql.NullInt64
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

	body := p.buildCreateRequest(row)

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
		SELECT fold_uuid, amount_paise, currency, txn_timestamp, type, status,
		       confirmed_source_account_id, confirmed_destination_account_id,
		       confirmed_category_id, confirmed_budget_id,
		       confirmed_description, confirmed_tags_json,
		       proposed_source_account_id, proposed_destination_account_id,
		       proposed_category_id, proposed_budget_id, proposed_description,
		       narration, firefly_txn_id
		FROM staged_fold_txns
		WHERE fold_uuid = ?
	`, foldUUID).Scan(
		&r.FoldUUID, &r.AmountPaise, &r.Currency, &r.TxnTimestamp, &r.Type, &r.Status,
		&r.ConfirmedSourceAccountID, &r.ConfirmedDestinationAccountID,
		&r.ConfirmedCategoryID, &r.ConfirmedBudgetID,
		&r.ConfirmedDescription, &r.ConfirmedTagsJSON,
		&r.ProposedSourceAccountID, &r.ProposedDestinationAccountID,
		&r.ProposedCategoryID, &r.ProposedBudgetID, &r.ProposedDescription,
		&r.Narration, &r.FireflyTxnID,
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
func (p *Pusher) buildCreateRequest(row pushableRow) firefly.CreateTransactionRequest {
	srcID := pickInt64(row.ConfirmedSourceAccountID, row.ProposedSourceAccountID)
	destID := pickInt64(row.ConfirmedDestinationAccountID, row.ProposedDestinationAccountID)
	catID := pickInt64(row.ConfirmedCategoryID, row.ProposedCategoryID)
	budID := pickInt64(row.ConfirmedBudgetID, row.ProposedBudgetID)
	desc := pickString(row.ConfirmedDescription, row.ProposedDescription)
	if desc == "" {
		// Last resort — we don't want to send an empty description to firefly.
		desc = row.Narration
	}

	var tags []string
	if row.ConfirmedTagsJSON.Valid && row.ConfirmedTagsJSON.String != "" {
		_ = json.Unmarshal([]byte(row.ConfirmedTagsJSON.String), &tags)
	}

	line := firefly.CreateTransactionLine{
		Type:          foldTypeToFireflyType(row.Type),
		Date:          row.TxnTimestamp.Format(time.RFC3339),
		Amount:        paiseToDecimal(row.AmountPaise),
		CurrencyCode:  row.Currency,
		Description:   desc,
		ExternalID:    row.FoldUUID,
		Tags:          tags,
		Notes:         "",
	}
	if srcID != 0 {
		line.SourceID = strconv.FormatInt(srcID, 10)
	}
	if destID != 0 {
		line.DestinationID = strconv.FormatInt(destID, 10)
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
func (p *Pusher) markPushed(ctx context.Context, foldUUID string, journalID, groupID int64) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET status = 'pushed',
		    firefly_txn_id = ?,
		    pushed_at = CURRENT_TIMESTAMP,
		    updated_at = CURRENT_TIMESTAMP
		WHERE fold_uuid = ?
	`, journalID, foldUUID)
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
