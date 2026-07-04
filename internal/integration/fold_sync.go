package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/fold"
)

// FoldSyncReport is the structured outcome of a fold staging sync.
type FoldSyncReport struct {
	Fetched      int           `json:"fetched"`               // total returned by the fold API across all pages
	Inserted     int           `json:"inserted"`              // newly staged
	Skipped      int           `json:"skipped"`               // already present (idempotent re-run)
	RawRefreshed int           `json:"raw_refreshed"`         // already-staged rows whose raw_payload we updated to the verbatim wire bytes
	Pages        int           `json:"pages"`                 // number of fold-API pages walked (>1 only in since-firefly mode)
	StoppedAt    string        `json:"stopped_at,omitempty"`  // human-readable reason the walk halted: "cutoff_reached" | "hard_cap" | "exhausted"
	CutoffDate   string        `json:"cutoff_date,omitempty"` // RFC3339 firefly cutoff used (since-firefly mode only)
	Duration     time.Duration `json:"duration"`
	NewestUUID   string        `json:"newest_uuid,omitempty"`
	OldestUUID   string        `json:"oldest_uuid,omitempty"`
}

// FoldSyncer pulls recent fold transactions and stages them in
// staged_fold_txns. Idempotent: re-running with the same fold UUIDs
// already on disk is a no-op.
type FoldSyncer struct {
	db  *DB
	fc  *fold.Client
	log *slog.Logger
}

// NewFoldSyncer constructs a FoldSyncer.
func NewFoldSyncer(db *DB, fc *fold.Client, log *slog.Logger) *FoldSyncer {
	return &FoldSyncer{db: db, fc: fc, log: log.With("component", "fold_sync")}
}

// SyncRecent fetches the latest `limit` fold transactions and inserts
// any that aren't already staged. Existing rows are left untouched —
// this is a stage-only operation; it will not re-run the classifier
// or push anything to firefly.
//
// Why we don't UPDATE existing rows: fold transactions don't change.
// Once a UUID has been issued, the underlying values (amount, mode,
// narration) are stable. If we ever need to fix a malformed staged
// row, that's a manual operation against the SQLite directly — not
// something this sync should be doing implicitly.
func (s *FoldSyncer) SyncRecent(ctx context.Context, limit int) (FoldSyncReport, error) {
	start := time.Now()
	report := FoldSyncReport{}

	resp, err := s.fc.ListTransactions(ctx, limit)
	if err != nil {
		return report, fmt.Errorf("list fold transactions: %w", err)
	}
	report.Fetched = len(resp.Data.Transactions)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.PrepareContext(ctx, insertStagedFoldTxnSQL)
	if err != nil {
		return report, fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	refreshStmt, err := tx.PrepareContext(ctx, refreshRawPayloadSQL)
	if err != nil {
		return report, fmt.Errorf("prepare refresh: %w", err)
	}
	defer refreshStmt.Close()

	for i, t := range resp.Data.Transactions {
		var raw json.RawMessage
		if i < len(resp.Data.RawTransactions) {
			raw = resp.Data.RawTransactions[i]
		}
		ok, err := s.insertStaged(ctx, stmt, t, raw)
		if err != nil {
			return report, fmt.Errorf("insert %s: %w", t.UUID, err)
		}
		if ok {
			report.Inserted++
		} else {
			report.Skipped++
			refreshed, err := s.refreshRawPayload(ctx, refreshStmt, t.UUID, raw)
			if err != nil {
				return report, fmt.Errorf("refresh raw_payload for %s: %w", t.UUID, err)
			}
			if refreshed {
				report.RawRefreshed++
			}
		}
		if i == 0 {
			// Response is newest-first.
			report.NewestUUID = t.UUID
		}
	}

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("commit: %w", err)
	}
	committed = true
	report.Duration = time.Since(start)

	report.Pages = 1
	report.StoppedAt = "exhausted"
	s.log.Info("fold sync complete",
		"fetched", report.Fetched,
		"inserted", report.Inserted,
		"skipped", report.Skipped,
		"raw_refreshed", report.RawRefreshed,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// SyncSinceFirefly walks fold transactions backwards from "now" until
// it crosses the most-recent date already on the firefly side, capped
// hard at maxTotal transactions across all pages.
//
// Pagination uses fold's `after=<base64>` cursor (built locally from
// the oldest transaction on the previous page). The walk stops at the
// first of three conditions:
//
//   - cutoff_reached  — every transaction on the most-recent page is
//     ≤ the firefly cutoff. We're caught up; no need to go further.
//   - hard_cap        — total transactions touched ≥ maxTotal. Safety
//     belt against a runaway loop or a misconfigured cutoff.
//   - exhausted       — fold returned an empty page. We've reached the
//     start of the user's fold history.
//
// The hard cap is per-call rather than per-day. A maxTotal of ~2000
// covers >18 months of typical activity (~150 txns/month observed) and
// returns in seconds; tune up only if you genuinely need to backfill
// further than that in one shot.
//
// If firefly_txns is empty (no mirror yet), we treat the cutoff as the
// zero time, which means "fetch everything within maxTotal." That's
// the correct behaviour for first-time setup.
func (s *FoldSyncer) SyncSinceFirefly(ctx context.Context, maxTotal int) (FoldSyncReport, error) {
	start := time.Now()
	report := FoldSyncReport{}

	if maxTotal <= 0 {
		return report, fmt.Errorf("maxTotal must be positive, got %d", maxTotal)
	}

	cutoff, err := latestFireflyTxnDate(ctx, s.db.DB)
	if err != nil {
		return report, fmt.Errorf("read firefly cutoff: %w", err)
	}
	if !cutoff.IsZero() {
		report.CutoffDate = cutoff.UTC().Format(time.RFC3339)
	}

	const pageSize = 100 // fold's hard ceiling
	var (
		afterCursor string
		stoppedAt   string
	)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	insStmt, err := tx.PrepareContext(ctx, insertStagedFoldTxnSQL)
	if err != nil {
		return report, fmt.Errorf("prepare insert: %w", err)
	}
	defer insStmt.Close()
	refStmt, err := tx.PrepareContext(ctx, refreshRawPayloadSQL)
	if err != nil {
		return report, fmt.Errorf("prepare refresh: %w", err)
	}
	defer refStmt.Close()

pages:
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.Pages++

		resp, err := s.fc.ListTransactionsAfter(ctx, pageSize, afterCursor)
		if err != nil {
			return report, fmt.Errorf("list page %d: %w", report.Pages, err)
		}
		txns := resp.Data.Transactions
		if len(txns) == 0 {
			stoppedAt = "exhausted"
			break
		}
		report.Fetched += len(txns)

		// Track newest seen across the whole walk (first txn of first page).
		if report.NewestUUID == "" {
			report.NewestUUID = txns[0].UUID
		}

		// Walk this page newest-first; insert anything strictly newer
		// than the cutoff. The moment we hit a txn at-or-before cutoff,
		// we know the rest of the page (and every later page) is also
		// at-or-before — fold returns newest-first within and across pages.
		pageCrossedCutoff := false
		for i, t := range txns {
			ts, err := parseFoldTimestamp(t.TxnTimestamp)
			if err != nil {
				return report, fmt.Errorf("parse timestamp %q for %s: %w", t.TxnTimestamp, t.UUID, err)
			}
			if !cutoff.IsZero() && !ts.After(cutoff) {
				pageCrossedCutoff = true
				break
			}

			var raw json.RawMessage
			if i < len(resp.Data.RawTransactions) {
				raw = resp.Data.RawTransactions[i]
			}
			ok, err := s.insertStaged(ctx, insStmt, t, raw)
			if err != nil {
				return report, fmt.Errorf("insert %s: %w", t.UUID, err)
			}
			if ok {
				report.Inserted++
			} else {
				report.Skipped++
				refreshed, err := s.refreshRawPayload(ctx, refStmt, t.UUID, raw)
				if err != nil {
					return report, fmt.Errorf("refresh raw_payload for %s: %w", t.UUID, err)
				}
				if refreshed {
					report.RawRefreshed++
				}
			}
			report.OldestUUID = t.UUID

			// Hard cap is checked AFTER the insert so the report
			// counts everything we've actually written.
			if report.Inserted+report.Skipped >= maxTotal {
				stoppedAt = "hard_cap"
				break pages
			}
		}

		if pageCrossedCutoff {
			stoppedAt = "cutoff_reached"
			break
		}

		// Echo fold's own pagination cursor back. Building one locally
		// from a timestamp doesn't work — fold's cursor format includes
		// the txn UUID for tie-breaking and the API rejects shorter
		// forms with HTTP 400.
		if resp.Data.After == "" {
			stoppedAt = "exhausted"
			break
		}
		afterCursor = resp.Data.After
	}

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("commit: %w", err)
	}
	committed = true
	report.StoppedAt = stoppedAt
	report.Duration = time.Since(start)

	s.log.Info("fold sync since firefly complete",
		"fetched", report.Fetched,
		"inserted", report.Inserted,
		"skipped", report.Skipped,
		"raw_refreshed", report.RawRefreshed,
		"pages", report.Pages,
		"stopped_at", report.StoppedAt,
		"cutoff_date", report.CutoffDate,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// latestFireflyTxnDate returns the most-recent date in firefly_txns,
// or the zero time when the mirror is empty (first-time bootstrap).
func latestFireflyTxnDate(ctx context.Context, db *sql.DB) (time.Time, error) {
	var maxDate sql.NullString
	err := db.QueryRowContext(ctx, `SELECT MAX(date) FROM firefly_txns`).Scan(&maxDate)
	if err != nil {
		return time.Time{}, err
	}
	if !maxDate.Valid || maxDate.String == "" {
		return time.Time{}, nil
	}
	// firefly_txns.date is stored as DATETIME; SQLite returns it as
	// text. We've observed two surface forms in the existing data:
	// "2026-05-08 06:44:00 +0000 UTC" (Go's default) and RFC3339.
	for _, layout := range []string{"2006-01-02 15:04:05 -0700 MST", time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, maxDate.String); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised firefly date format: %q", maxDate.String)
}

// insertStagedFoldTxnSQL inserts a staged row. INSERT OR IGNORE so a
// re-sync naturally skips already-staged UUIDs without erroring. We
// return inserted=true only when RowsAffected == 1.
const insertStagedFoldTxnSQL = `
INSERT OR IGNORE INTO staged_fold_txns (
    fold_uuid, raw_payload, amount_paise, currency,
    foreign_amount_paise, foreign_currency, txn_timestamp,
    mode, type, narration, merchant_extracted, status
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')
`

// refreshRawPayloadSQL refreshes only raw_payload on an existing row.
// Touches no classifier-owned columns (status, proposed_*, confirmed_*).
// The WHERE raw_payload <> ? guard means it's a no-op when the wire
// bytes are byte-identical to what we have on disk — so re-running
// fold/sync against unchanged upstream data won't churn rows or thrash
// the audit trail.
//
// Why this exists: prior to PR-Q the raw_payload column was filled by
// json.Marshal-ing our typed fold.Transaction (9 fields). Fold actually
// returns ~16 fields, including account_id and merchant — both of which
// the classifier's LLM uses for source-account inference. Older rows
// have a thin payload; this refresh path gets them up to the verbatim
// wire shape on the next sync without disturbing classifier state.
const refreshRawPayloadSQL = `
UPDATE staged_fold_txns
   SET raw_payload = ?
 WHERE fold_uuid = ?
   AND raw_payload <> ?
`

// insertStaged returns (inserted, err). Inserted is false when the
// row already existed — i.e. the INSERT OR IGNORE was a no-op.
//
// rawPayload is the verbatim per-transaction JSON bytes from fold's
// response. Falls back to a minimal envelope if the caller couldn't
// supply it (defensive — should not occur with the current client).
func (s *FoldSyncer) insertStaged(ctx context.Context, stmt *sql.Stmt, t fold.Transaction, rawPayload json.RawMessage) (bool, error) {
	rawStr, err := rawPayloadString(t, rawPayload)
	if err != nil {
		return false, err
	}
	ts, err := parseFoldTimestamp(t.TxnTimestamp)
	if err != nil {
		return false, fmt.Errorf("parse timestamp %q: %w", t.TxnTimestamp, err)
	}
	merchant := NormalizeMerchant(fold.ExtractMerchant(t.Narration, t.Mode))

	// fold gives BOTH the home-currency amount (Amount/Currency = INR) and
	// the original charge (SourceAmount/SourceCurrency). The primary firefly
	// amount must be the home (INR) side; the foreign side is recorded
	// separately and only when it genuinely differs (an abroad charge).
	// Degenerate older payloads without a home currency fall back to the
	// source side so we still store something.
	primaryAmt, primaryCur := t.Amount, t.Currency
	if primaryCur == "" {
		primaryAmt, primaryCur = t.SourceAmount, t.SourceCurrency
	}
	var foreignPaise, foreignCur any
	if t.SourceCurrency != "" && t.SourceCurrency != primaryCur {
		foreignPaise = amountToPaise(t.SourceAmount)
		foreignCur = t.SourceCurrency
	}

	res, err := stmt.ExecContext(ctx,
		t.UUID,
		rawStr,
		amountToPaise(primaryAmt),
		nullIfEmpty(primaryCur),
		foreignPaise,
		foreignCur,
		ts,
		t.Mode,
		t.Type,
		t.Narration,
		nullIfEmpty(merchant),
	)
	if err != nil {
		return false, fmt.Errorf("exec insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// refreshRawPayload updates only raw_payload on an already-staged row,
// when the wire bytes differ from what we have. Returns true when the
// row was actually updated.
func (s *FoldSyncer) refreshRawPayload(ctx context.Context, stmt *sql.Stmt, foldUUID string, rawPayload json.RawMessage) (bool, error) {
	rawStr, err := rawPayloadString(fold.Transaction{UUID: foldUUID}, rawPayload)
	if err != nil {
		return false, err
	}
	res, err := stmt.ExecContext(ctx, rawStr, foldUUID, rawStr)
	if err != nil {
		return false, fmt.Errorf("exec refresh: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

// rawPayloadString returns the verbatim wire bytes when available, or
// falls back to marshalling the typed struct. The fallback exists for
// callers (mainly tests) that construct a ListTransactionsResponse
// without going through UnmarshalJSON; in production the client always
// goes through UnmarshalJSON and rawPayload is non-empty.
func rawPayloadString(t fold.Transaction, rawPayload json.RawMessage) (string, error) {
	if len(rawPayload) > 0 {
		return string(rawPayload), nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("marshal raw payload fallback: %w", err)
	}
	return string(b), nil
}

// amountToPaise converts a fold-side float-amount to integer paise.
// Round-half-up at the 1-paise boundary so a wire value of 70.005 →
// 7001 paise; we never want to truncate and lose a paisa silently.
//
// Fold's amounts are observed to be either integer-valued (70, 480) or
// 2-decimal (978.33). We don't see weird precision issues in practice,
// but this is defensive against future fold API changes.
func amountToPaise(a float64) int64 {
	if a < 0 {
		// Stored as absolute value; the `type` column carries direction.
		a = -a
	}
	return int64(math.Round(a * 100))
}

// parseFoldTimestamp tolerates the variants we've observed.
func parseFoldTimestamp(s string) (time.Time, error) {
	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised fold timestamp: %q", s)
}
