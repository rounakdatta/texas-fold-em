package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// FireflyAccountsSyncReport is the structured outcome of a firefly-accounts mirror.
type FireflyAccountsSyncReport struct {
	Fetched  int `json:"fetched"`
	Upserted int `json:"upserted"`
	Pages    int `json:"pages"`
	Assets   int `json:"assets"`
	// Categories: how many of Firefly's categories the mirror now holds.
	// CategoriesError says why they could not be read this time (the old
	// copy stays); it never fails the accounts.
	Categories      int           `json:"categories"`
	CategoriesError string        `json:"categoriesError,omitempty"`
	Duration        time.Duration `json:"duration"`
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

	if n, err := s.syncCategories(ctx); err != nil {
		report.CategoriesError = err.Error()
		s.log.Warn("firefly categories sync failed; keeping the previous copy", "err", err)
	} else {
		report.Categories = n
	}
	report.Duration = time.Since(start)

	s.log.Info("firefly accounts sync complete",
		"fetched", report.Fetched,
		"upserted", report.Upserted,
		"assets", report.Assets,
		"categories", report.Categories,
		"pages", report.Pages,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// syncCategories mirrors Firefly's categories into firefly_categories: every
// page of GET /api/v1/categories, then the rows Firefly no longer has are
// dropped. It runs in its own transaction after the accounts', so a category
// list that can't be read leaves the previous copy and never holds the
// accounts back. A category created from the deck while this runs is stamped
// later than the sync began, so the clean-up leaves it alone.
func (s *FireflyAccountsSyncer) syncCategories(ctx context.Context) (int, error) {
	// millisecond stamps (the same shape SQLite's strftime('%Y-%m-%d %H:%M:%f')
	// writes), so two syncs in one second still tell their rows apart
	began := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, page := 0, 1
	var seen []any
	for {
		resp, err := s.fc.ListCategories(ctx, page, pageLimit)
		if err != nil {
			return 0, fmt.Errorf("list categories page %d: %w", page, err)
		}
		for _, c := range resp.Data {
			id, err := strconv.ParseInt(c.ID, 10, 64)
			if err != nil || c.Attributes.Name == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO firefly_categories (firefly_id, name, last_synced_at) VALUES (?, ?, ?)
				ON CONFLICT(firefly_id) DO UPDATE SET name = excluded.name, last_synced_at = excluded.last_synced_at`,
				id, c.Attributes.Name, began); err != nil {
				return 0, fmt.Errorf("upsert category %s: %w", c.ID, err)
			}
			seen = append(seen, id)
			n++
		}
		if resp.Meta.Pagination.CurrentPage >= resp.Meta.Pagination.TotalPages || resp.Meta.Pagination.TotalPages == 0 {
			break
		}
		page++
	}
	drop, args := `DELETE FROM firefly_categories WHERE last_synced_at < ?`, []any{began}
	if len(seen) > 0 {
		drop += ` AND firefly_id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(seen)), ",") + `)`
		args = append(args, seen...)
	}
	if _, err := tx.ExecContext(ctx, drop, args...); err != nil {
		return 0, fmt.Errorf("drop categories firefly no longer has: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
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
