// Package integration is the fold→firefly transaction-classification
// subsystem of texas-fold-em. It is intentionally separate from the
// broker package so that schema migrations or classifier bugs cannot
// destabilise the refresh-token chain.
//
// State lives in a SQLite file (default /data/staging.db, distinct from
// the broker's /data/state.json). The whole subsystem is gated by the
// TEXAS_FOLDEM_INTEGRATION_ENABLED env var; when false (the default),
// none of this code runs.
//
// Non-destructive contract — load-bearing for the entire feature:
//   - Firefly is read via GET only in early PRs. The write client (POST
//     to /api/v1/transactions) lands in PR F; even then no PATCH/PUT/
//     DELETE codepath exists.
//   - Every write to firefly carries external_id=<fold_uuid> and is
//     idempotency-checked via GET /search/transactions before the POST.
//   - Every firefly mutation is appended to the audit_log table.
package integration

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // pure-Go driver, registers as "sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps the integration SQLite handle. It is safe to use from
// multiple goroutines because the underlying *sql.DB is.
type DB struct {
	*sql.DB
	path string
}

// Open creates the parent directory if needed, opens the SQLite file
// with WAL + foreign keys + busy_timeout, then runs all pending goose
// migrations. The returned DB is ready to use.
//
// Connection-string PRAGMAs:
//   - journal_mode=WAL: writers don't block readers; crash-safe.
//   - synchronous=NORMAL: WAL pairs with NORMAL safely; FULL is overkill.
//   - foreign_keys=ON: SQLite's per-connection default is OFF.
//   - busy_timeout=5000: yield up to 5s if another writer holds the lock.
//
// Note: even though we are single-writer in steady state, having
// busy_timeout on means goroutines like classifier + sync can never
// deadlock each other on transient locks.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("integration: db path is empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := ensureDir(dir); err != nil {
			return nil, fmt.Errorf("integration: ensure dir %s: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)",
		path,
	)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("integration: sql.Open: %w", err)
	}

	// SQLite is fundamentally single-writer; capping max open conns at 1
	// avoids "database is locked" thrash. Read concurrency comes from WAL
	// regardless of this cap on this driver.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("integration: ping: %w", err)
	}

	if err := runMigrations(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("integration: migrations: %w", err)
	}

	return &DB{DB: sqlDB, path: path}, nil
}

// Path returns the on-disk path the DB was opened from. Useful for
// /health surfaces.
func (d *DB) Path() string { return d.path }

// runMigrations applies all embedded goose migrations. Idempotent: a
// second call against an already-migrated DB is a no-op.
func runMigrations(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	// Quiet logger — goose prints to stdout by default which would clobber
	// our JSON-structured logs from main. We log start/finish ourselves
	// from the caller.
	goose.SetLogger(goose.NopLogger())
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}
