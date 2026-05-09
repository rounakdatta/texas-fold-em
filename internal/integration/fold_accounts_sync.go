package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/fold"
)

// FoldAccountsSyncReport is the structured outcome of a fold-accounts mirror.
type FoldAccountsSyncReport struct {
	Fetched  int           `json:"fetched"`
	Upserted int           `json:"upserted"`
	Duration time.Duration `json:"duration"`
}

// FoldAccountsSyncer pulls the user's fold-side asset registry (credit
// cards + bank accounts) and mirrors it into fold_accounts. The
// classifier's Tier-3 prompt joins this table on
// raw_payload.account_id to resolve "this transaction came from
// account X" → "X is your 'HDFC Tata Neu Plus ****8943' card".
type FoldAccountsSyncer struct {
	db  *DB
	fc  *fold.Client
	log *slog.Logger
}

// NewFoldAccountsSyncer constructs a FoldAccountsSyncer.
func NewFoldAccountsSyncer(db *DB, fc *fold.Client, log *slog.Logger) *FoldAccountsSyncer {
	return &FoldAccountsSyncer{db: db, fc: fc, log: log.With("component", "fold_accounts_sync")}
}

// Sync fetches every fold account and upserts the row by
// fold_account_id. The mirror is the source of truth — accounts that
// disappear from fold (consents revoked, cards closed) are left in
// place with their last-known data; we surface is_closed but never
// hard-delete, so historical transactions can still be explained.
func (s *FoldAccountsSyncer) Sync(ctx context.Context) (FoldAccountsSyncReport, error) {
	start := time.Now()
	report := FoldAccountsSyncReport{}

	accounts, err := s.fc.ListAccounts(ctx)
	if err != nil {
		return report, fmt.Errorf("list fold accounts: %w", err)
	}
	report.Fetched = len(accounts)

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

	stmt, err := tx.PrepareContext(ctx, upsertFoldAccountSQL)
	if err != nil {
		return report, fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	for _, acc := range accounts {
		raw := string(acc.Raw)
		if raw == "" {
			b, err := json.Marshal(acc)
			if err != nil {
				return report, fmt.Errorf("marshal fallback: %w", err)
			}
			raw = string(b)
		}
		closedInt := 0
		if acc.IsClosed {
			closedInt = 1
		}
		_, err := stmt.ExecContext(ctx,
			acc.ID,
			string(acc.Kind),
			acc.Name,
			nullIfEmpty(acc.Provider),
			nullIfEmpty(acc.Network),
			nullIfEmpty(acc.LastFour),
			nullIfEmpty(acc.Nickname),
			nullIfEmpty(acc.HolderName),
			raw,
			closedInt,
		)
		if err != nil {
			return report, fmt.Errorf("upsert %s: %w", acc.ID, err)
		}
		report.Upserted++
	}

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("commit: %w", err)
	}
	committed = true
	report.Duration = time.Since(start)

	s.log.Info("fold accounts sync complete",
		"fetched", report.Fetched,
		"upserted", report.Upserted,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// upsertFoldAccountSQL writes a fold_accounts row, refreshing every
// human-readable field on each sync (fold sometimes renames or
// reissues — a cycle date update or nickname change should propagate).
// updated_at is bumped on every conflict; created_at is preserved.
const upsertFoldAccountSQL = `
INSERT INTO fold_accounts (
    fold_account_id, kind, name, provider, network, last_four, nickname,
    holder_name, raw_payload, is_closed, last_synced_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(fold_account_id) DO UPDATE SET
    kind            = excluded.kind,
    name            = excluded.name,
    provider        = excluded.provider,
    network         = excluded.network,
    last_four       = excluded.last_four,
    nickname        = excluded.nickname,
    holder_name     = excluded.holder_name,
    raw_payload     = excluded.raw_payload,
    is_closed       = excluded.is_closed,
    last_synced_at  = CURRENT_TIMESTAMP,
    updated_at      = CURRENT_TIMESTAMP
`
