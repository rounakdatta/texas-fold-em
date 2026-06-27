package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// FireflyAccountsSyncReport is the structured outcome of a firefly-accounts mirror.
type FireflyAccountsSyncReport struct {
	Fetched  int           `json:"fetched"`
	Upserted int           `json:"upserted"`
	Pages    int           `json:"pages"`
	Assets   int           `json:"assets"`
	Duration time.Duration `json:"duration"`
}

// FireflyAccountsSyncer pulls firefly-iii's OWN account list (GET
// /api/v1/accounts) and mirrors it into firefly_accounts. This is the
// authoritative asset inventory — unlike the transaction-derived
// listAssetAccounts, it includes accounts the user has *created* but
// not yet transacted on. The deterministic source resolver matches a
// fold card against these rows (by last4 / name), so a freshly-added
// asset becomes resolvable the moment this sync runs — no transaction
// history required.
type FireflyAccountsSyncer struct {
	db  *DB
	fc  *firefly.Client
	log *slog.Logger
}

// NewFireflyAccountsSyncer constructs a FireflyAccountsSyncer.
func NewFireflyAccountsSyncer(db *DB, fc *firefly.Client, log *slog.Logger) *FireflyAccountsSyncer {
	return &FireflyAccountsSyncer{db: db, fc: fc, log: log.With("component", "firefly_accounts_sync")}
}

// Sync walks every page of GET /api/v1/accounts and upserts each row by
// firefly_id. Like the other mirrors it runs under a single transaction
// so a mid-pagination failure leaves the prior mirror intact. We mirror
// ALL account types (asset/expense/revenue/…) for completeness, but the
// source resolver only ever reads type='asset'.
func (s *FireflyAccountsSyncer) Sync(ctx context.Context) (FireflyAccountsSyncReport, error) {
	start := time.Now()
	report := FireflyAccountsSyncReport{}

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

	stmt, err := tx.PrepareContext(ctx, upsertFireflyAccountSQL)
	if err != nil {
		return report, fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	page := 1
	for {
		resp, err := s.fc.ListAccounts(ctx, page, pageLimit)
		if err != nil {
			return report, fmt.Errorf("list accounts page %d: %w", page, err)
		}
		report.Pages++
		for _, acc := range resp.Data {
			id, err := strconv.ParseInt(acc.ID, 10, 64)
			if err != nil {
				s.log.Warn("skip malformed account id", "raw", acc.ID, "err", err)
				continue
			}
			report.Fetched++
			raw, err := json.Marshal(acc)
			if err != nil {
				return report, fmt.Errorf("marshal account %s: %w", acc.ID, err)
			}
			at := acc.Attributes
			activeInt := 1
			if !at.Active {
				activeInt = 0
			}
			if _, err := stmt.ExecContext(ctx,
				id,
				at.Name,
				at.Type,
				nullIfEmpty(at.AccountRole),
				nullIfEmpty(at.AccountNumber),
				nullIfEmpty(at.CurrencyCode),
				activeInt,
				string(raw),
			); err != nil {
				return report, fmt.Errorf("upsert account %s: %w", acc.ID, err)
			}
			report.Upserted++
			if at.Type == "asset" {
				report.Assets++
			}
		}
		if resp.Meta.Pagination.CurrentPage >= resp.Meta.Pagination.TotalPages || resp.Meta.Pagination.TotalPages == 0 {
			break
		}
		page++
	}

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("commit: %w", err)
	}
	committed = true
	report.Duration = time.Since(start)

	s.log.Info("firefly accounts sync complete",
		"fetched", report.Fetched,
		"upserted", report.Upserted,
		"assets", report.Assets,
		"pages", report.Pages,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// upsertFireflyAccountSQL writes a firefly_accounts row, refreshing every
// field on each sync (firefly accounts get renamed, archived, or have
// their account_number filled in after creation — all should propagate).
// created_at is preserved; updated_at + last_synced_at bump on conflict.
const upsertFireflyAccountSQL = `
INSERT INTO firefly_accounts (
    firefly_id, name, type, account_role, account_number, currency_code,
    active, raw_payload, last_synced_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
ON CONFLICT(firefly_id) DO UPDATE SET
    name            = excluded.name,
    type            = excluded.type,
    account_role    = excluded.account_role,
    account_number  = excluded.account_number,
    currency_code   = excluded.currency_code,
    active          = excluded.active,
    raw_payload     = excluded.raw_payload,
    last_synced_at  = CURRENT_TIMESTAMP,
    updated_at      = CURRENT_TIMESTAMP
`
