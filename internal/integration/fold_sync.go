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
	Fetched   int           `json:"fetched"`    // total returned by the fold API
	Inserted  int           `json:"inserted"`   // newly staged
	Skipped   int           `json:"skipped"`    // already present (idempotent re-run)
	Duration  time.Duration `json:"duration"`
	NewestUUID string       `json:"newest_uuid,omitempty"`
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

	for i, t := range resp.Data.Transactions {
		ok, err := s.insertStaged(ctx, stmt, t)
		if err != nil {
			return report, fmt.Errorf("insert %s: %w", t.UUID, err)
		}
		if ok {
			report.Inserted++
		} else {
			report.Skipped++
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

	s.log.Info("fold sync complete",
		"fetched", report.Fetched,
		"inserted", report.Inserted,
		"skipped", report.Skipped,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// insertStagedFoldTxnSQL inserts a staged row. INSERT OR IGNORE so a
// re-sync naturally skips already-staged UUIDs without erroring. We
// return inserted=true only when RowsAffected == 1.
const insertStagedFoldTxnSQL = `
INSERT OR IGNORE INTO staged_fold_txns (
    fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
    mode, type, narration, merchant_extracted, status
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')
`

// insertStaged returns (inserted, err). Inserted is false when the
// row already existed — i.e. the INSERT OR IGNORE was a no-op.
func (s *FoldSyncer) insertStaged(ctx context.Context, stmt *sql.Stmt, t fold.Transaction) (bool, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return false, fmt.Errorf("marshal raw payload: %w", err)
	}
	ts, err := parseFoldTimestamp(t.TxnTimestamp)
	if err != nil {
		return false, fmt.Errorf("parse timestamp %q: %w", t.TxnTimestamp, err)
	}
	merchant := NormalizeMerchant(fold.ExtractMerchant(t.Narration, t.Mode))

	res, err := stmt.ExecContext(ctx,
		t.UUID,
		string(raw),
		amountToPaise(t.SourceAmount),
		nullIfEmpty(t.SourceCurrency),
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
