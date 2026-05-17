package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// pageLimit is firefly's max per-page size we use. The API caps at 100;
// 50 is a good balance — fewer round-trips than 25, and 100 occasionally
// times out on large transaction pages.
const pageLimit = 50

// SyncReport is the structured outcome of a full sync. Returned by
// Syncer.SyncAll for logging and for the /admin/firefly/sync HTTP
// response so an operator can see what happened.
type SyncReport struct {
	TransactionPages    int           `json:"transaction_pages"`
	TransactionGroups   int           `json:"transaction_groups"`
	TransactionJournals int           `json:"transaction_journals"`
	MerchantLookupRows  int           `json:"merchant_lookup_rows"`
	Duration            time.Duration `json:"duration"`
}

// Syncer pulls firefly data into the local SQLite mirror and (re)builds
// the materialised merchant_lookup. Stateless beyond its dependencies;
// safe to call concurrently with a typical SQLite-bound rate (the DB
// has MaxOpenConns=1 so the underlying writes serialise).
type Syncer struct {
	db  *DB
	fc  *firefly.Client
	log *slog.Logger
}

// NewSyncer constructs a Syncer.
func NewSyncer(db *DB, fc *firefly.Client, log *slog.Logger) *Syncer {
	return &Syncer{db: db, fc: fc, log: log.With("component", "firefly_sync")}
}

// SyncAll runs the full pipeline:
//  1. Walk every page of firefly transactions, upsert each journal into
//     firefly_txns. The FTS5 virtual table stays consistent automatically
//     via the AFTER INSERT/UPDATE triggers from migration 002.
//  2. Rebuild merchant_lookup from the freshly-mirrored data.
//
// The whole thing runs under a single SQL transaction so a partial
// failure (e.g., network error mid-pagination) leaves the DB in its
// pre-sync state. This is safer than the alternative — partial state
// would mean the classifier sees an inconsistent corpus.
func (s *Syncer) SyncAll(ctx context.Context) (SyncReport, error) {
	start := time.Now()
	report := SyncReport{}

	// Sanity check — confirms PAT works and host is reachable before we
	// commit to fetching thousands of rows.
	user, err := s.fc.AboutUser(ctx)
	if err != nil {
		return report, fmt.Errorf("firefly: about/user (auth check): %w", err)
	}
	s.log.Info("firefly auth ok", "email", user.Email, "role", user.Role)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("begin tx: %w", err)
	}
	// Best-effort rollback if we don't commit. Safe to call after Commit
	// (which makes Rollback a no-op returning ErrTxDone — we ignore).
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	groups, journals, pages, err := s.syncTransactions(ctx, tx)
	if err != nil {
		return report, fmt.Errorf("sync transactions: %w", err)
	}
	report.TransactionPages = pages
	report.TransactionGroups = groups
	report.TransactionJournals = journals

	lookupRows, err := s.rebuildMerchantLookup(ctx, tx)
	if err != nil {
		return report, fmt.Errorf("rebuild merchant_lookup: %w", err)
	}
	report.MerchantLookupRows = lookupRows

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("commit: %w", err)
	}
	committed = true

	report.Duration = time.Since(start)
	s.log.Info("sync complete",
		"groups", report.TransactionGroups,
		"journals", report.TransactionJournals,
		"merchants", report.MerchantLookupRows,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// syncTransactions paginates through GET /api/v1/transactions and
// upserts each journal into firefly_txns. Returns (groups, journals,
// pagesFetched).
func (s *Syncer) syncTransactions(ctx context.Context, tx *sql.Tx) (int, int, int, error) {
	stmt, err := tx.PrepareContext(ctx, upsertFireflyTxnSQL)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("prepare upsert: %w", err)
	}
	defer stmt.Close()

	var groups, journals, pages int
	page := 1
	for {
		resp, err := s.fc.ListTransactions(ctx, page, pageLimit)
		if err != nil {
			return groups, journals, pages, fmt.Errorf("list transactions page %d: %w", page, err)
		}
		pages++
		for _, g := range resp.Data {
			groupID, err := strconv.ParseInt(g.ID, 10, 64)
			if err != nil {
				// Skip malformed group ids — log and continue rather than abort
				// the whole sync. Firefly should never produce these.
				s.log.Warn("skip malformed group id", "raw", g.ID, "err", err)
				continue
			}
			groups++
			for _, j := range g.Attributes.Transactions {
				if err := s.upsertJournal(ctx, stmt, groupID, j); err != nil {
					return groups, journals, pages, fmt.Errorf("upsert journal %s: %w", j.JournalID, err)
				}
				journals++
			}
		}
		s.log.Info("sync page",
			"page", resp.Meta.Pagination.CurrentPage,
			"total_pages", resp.Meta.Pagination.TotalPages,
			"running_journals", journals,
		)
		if resp.Meta.Pagination.CurrentPage >= resp.Meta.Pagination.TotalPages || resp.Meta.Pagination.TotalPages == 0 {
			break
		}
		page++
	}
	return groups, journals, pages, nil
}

// upsertFireflyTxnSQL is the upsert used per journal. ON CONFLICT keeps
// firefly_id as the dedup key. We deliberately update every column on
// conflict because firefly transactions can be edited (description,
// category, etc.) — we want the latest state.
const upsertFireflyTxnSQL = `
INSERT INTO firefly_txns (
    firefly_id, group_id, txn_type, amount_paise, currency, date,
    source_account_id, source_account_name,
    destination_account_id, destination_account_name, destination_account_name_normalized,
    category_id, category_name, budget_id, budget_name,
    description, tags_json, external_id, notes, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(firefly_id) DO UPDATE SET
    group_id                            = excluded.group_id,
    txn_type                            = excluded.txn_type,
    amount_paise                        = excluded.amount_paise,
    currency                            = excluded.currency,
    date                                = excluded.date,
    source_account_id                   = excluded.source_account_id,
    source_account_name                 = excluded.source_account_name,
    destination_account_id              = excluded.destination_account_id,
    destination_account_name            = excluded.destination_account_name,
    destination_account_name_normalized = excluded.destination_account_name_normalized,
    category_id                         = excluded.category_id,
    category_name                       = excluded.category_name,
    budget_id                           = excluded.budget_id,
    budget_name                         = excluded.budget_name,
    description                         = excluded.description,
    tags_json                           = excluded.tags_json,
    external_id                         = excluded.external_id,
    notes                               = excluded.notes,
    updated_at                          = CURRENT_TIMESTAMP
`

// MirrorJournal upserts a single firefly transaction journal into the
// local mirror. Used by the eager-after-push code path so a freshly
// created firefly transaction lands in firefly_txns (and firefly_txns_fts
// via the triggers) within seconds of the push completing — without
// waiting for the next periodic /admin/firefly/sync to walk the whole
// corpus.
//
// Wraps the existing upsertJournal in its own small transaction so
// callers don't need to manage one. The FTS triggers fire as part of
// the same transaction, keeping the index consistent.
func (s *Syncer) MirrorJournal(ctx context.Context, j firefly.TransactionJournal, groupID int64) error {
	tx, err := s.db.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mirror: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	stmt, err := tx.PrepareContext(ctx, upsertFireflyTxnSQL)
	if err != nil {
		return fmt.Errorf("mirror: prepare upsert: %w", err)
	}
	defer stmt.Close()
	if err := s.upsertJournal(ctx, stmt, groupID, j); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mirror: commit: %w", err)
	}
	committed = true
	return nil
}

func (s *Syncer) upsertJournal(ctx context.Context, stmt *sql.Stmt, groupID int64, j firefly.TransactionJournal) error {
	journalID, err := strconv.ParseInt(j.JournalID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse journal id %q: %w", j.JournalID, err)
	}
	amountPaise, err := decimalToPaise(j.Amount)
	if err != nil {
		return fmt.Errorf("parse amount %q: %w", j.Amount, err)
	}
	date, err := parseFireflyDate(j.Date)
	if err != nil {
		return fmt.Errorf("parse date %q: %w", j.Date, err)
	}
	tagsJSON, err := json.Marshal(j.Tags)
	if err != nil {
		return fmt.Errorf("marshal tags: %w", err)
	}

	_, err = stmt.ExecContext(ctx,
		journalID,
		groupID,
		j.Type,
		amountPaise,
		j.CurrencyCode,
		date,
		nullableID(j.SourceID),
		nullIfEmpty(j.SourceName),
		nullableID(j.DestinationID),
		nullIfEmpty(j.DestinationName),
		nullIfEmpty(NormalizeMerchant(j.DestinationName)),
		nullableID(j.CategoryID),
		nullIfEmpty(j.CategoryName),
		nullableID(j.BudgetID),
		nullIfEmpty(j.BudgetName),
		j.Description,
		string(tagsJSON),
		nullIfEmpty(j.ExternalID),
		nullIfEmpty(j.Notes),
	)
	if err != nil {
		return fmt.Errorf("exec upsert: %w", err)
	}
	return nil
}

// rebuildMerchantLookup replaces merchant_lookup with the modal
// category/budget/source_account per (destination_account_id, normalised
// name) pair, computed from the freshly-synced firefly_txns.
//
// "Modal" here means: of the most recent N transactions for this
// merchant, the most-frequent (category, source_account) combination.
// We cap N because tastes change; the last 30 transactions tell us more
// about the current pattern than the lifetime mode.
func (s *Syncer) rebuildMerchantLookup(ctx context.Context, tx *sql.Tx) (int, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM merchant_lookup`); err != nil {
		return 0, fmt.Errorf("clear lookup: %w", err)
	}

	// One CTE per merchant: the latest 30 expense rows. Then GROUP BY
	// merchant + label combo, keep the most-frequent. This is one query
	// instead of N+1; SQLite handles it fine for our 7k corpus.
	//
	// modal_description is treated separately from the structural fields:
	// we take the MOST-RECENT non-empty description per merchant rather
	// than a modal vote, because descriptions vary in specifics
	// ("Dinner with Tushar", "Dinner with Jojo") and the latest is more
	// useful than the most-frequent. The operator edits during review
	// before push if the specifics differ.
	const buildSQL = `
WITH ranked AS (
    SELECT
        firefly_id,
        destination_account_id,
        destination_account_name,
        destination_account_name_normalized AS norm,
        source_account_id,
        source_account_name,
        category_id,
        category_name,
        budget_id,
        budget_name,
        description,
        date,
        ROW_NUMBER() OVER (PARTITION BY destination_account_name_normalized ORDER BY date DESC) AS rn
    FROM firefly_txns
    WHERE txn_type = 'withdrawal'
      AND destination_account_name_normalized IS NOT NULL
      AND destination_account_name_normalized <> ''
),
recent AS (
    SELECT * FROM ranked WHERE rn <= 30
),
counts AS (
    SELECT
        norm,
        destination_account_id,
        destination_account_name,
        source_account_id,
        source_account_name,
        category_id,
        category_name,
        budget_id,
        budget_name,
        COUNT(*) AS combo_count,
        MAX(date) AS last_seen
    FROM recent
    GROUP BY norm, destination_account_id, source_account_id, category_id, budget_id
),
sample_sizes AS (
    SELECT norm, COUNT(*) AS sample_size FROM recent GROUP BY norm
),
latest_descriptions AS (
    SELECT norm,
           description AS modal_description
    FROM (
        SELECT norm, description,
               ROW_NUMBER() OVER (PARTITION BY norm ORDER BY date DESC) AS rn_desc
        FROM recent
        WHERE description IS NOT NULL AND TRIM(description) <> ''
    )
    WHERE rn_desc = 1
),
ranked_combos AS (
    SELECT
        c.*,
        s.sample_size,
        ld.modal_description,
        ROW_NUMBER() OVER (PARTITION BY c.norm ORDER BY c.combo_count DESC, c.last_seen DESC) AS combo_rank
    FROM counts c
    JOIN sample_sizes s USING (norm)
    LEFT JOIN latest_descriptions ld USING (norm)
)
INSERT INTO merchant_lookup (
    merchant_normalized,
    modal_destination_account_id, modal_destination_account_name,
    modal_source_account_id,      modal_source_account_name,
    modal_category_id,            modal_category_name,
    modal_budget_id,              modal_budget_name,
    modal_description,
    sample_size, confidence, last_seen
)
SELECT
    norm,
    destination_account_id, destination_account_name,
    source_account_id,      source_account_name,
    category_id,            category_name,
    budget_id,              budget_name,
    modal_description,
    sample_size,
    CAST(combo_count AS REAL) / CAST(sample_size AS REAL),
    last_seen
FROM ranked_combos
WHERE combo_rank = 1
`
	res, err := tx.ExecContext(ctx, buildSQL)
	if err != nil {
		return 0, fmt.Errorf("rebuild lookup: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// decimalToPaise converts firefly's wire string (e.g. "70.00", "1234.56",
// "0") to integer paise (e.g. 7000, 123456, 0). Errors on negative
// values — firefly amounts are always non-negative in the wire format
// (the txn_type carries the sign).
func decimalToPaise(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasPrefix(s, "-") {
		return 0, errors.New("amount must be non-negative")
	}
	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("whole part: %w", err)
	}
	if len(parts) == 1 {
		return whole * 100, nil
	}
	frac := parts[1]
	switch {
	case len(frac) == 0:
		return whole * 100, nil
	case len(frac) == 1:
		frac += "0"
	case len(frac) > 2:
		// Truncate (don't round) to two decimals — firefly's wire format is
		// always 2 decimals for the currencies we care about (INR), so this
		// branch is defensive against a future schema change.
		frac = frac[:2]
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("frac part: %w", err)
	}
	return whole*100 + cents, nil
}

// parseFireflyDate parses firefly's date wire format. Firefly emits
// RFC3339 with offset (e.g. "2026-05-08T12:59:18+05:30"). We tolerate
// a couple of variants because field tests are field tests.
func parseFireflyDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty date")
	}
	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised date format: %q", s)
}

// NormalizeMerchant canonicalises a merchant name for fuzzy matching.
// Lowercases, collapses whitespace, strips a few common payment-rail
// noise prefixes/suffixes. Exposed because the classifier in PR D
// re-uses the same normalisation when extracting merchants from fold
// narrations — both sides must agree on normalisation or merchant
// lookup misses.
func NormalizeMerchant(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	// Strip common razorpay/upi prefixes that are noise for matching.
	for _, prefix := range []string{"raz*", "razorpay*", "upi-"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
		}
	}
	// Collapse internal whitespace runs into single spaces.
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// nullIfEmpty returns NULL for empty strings so the DB column reflects
// "no value" rather than "empty string". Helps queries (IS NOT NULL)
// stay correct.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableID parses an id-like string into int64 OR returns NULL if
// the string is empty. Firefly returns "" for unset IDs (no category,
// no budget, internal accounts, etc.).
func nullableID(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return n
}
