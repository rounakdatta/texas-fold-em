package integration

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpen_FreshFileMigratesAllExpectedTables boots a brand-new SQLite
// file and confirms every table the integration depends on is present,
// the FTS5 virtual table is queryable, and the goose tracking table
// recorded all three migrations.
func TestOpen_FreshFileMigratesAllExpectedTables(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "staging.db")

	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	wantTables := []string{
		"staged_fold_txns",
		"firefly_txns",
		"audit_log",
		"merchant_lookup",
	}
	for _, tbl := range wantTables {
		var name string
		err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("expected table %q to exist: %v", tbl, err)
		}
	}

	// FTS5 virtual table is a different `type` in sqlite_master.
	var ftsName string
	err = db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE name='firefly_txns_fts'`).Scan(&ftsName)
	if err != nil {
		t.Errorf("expected FTS5 virtual table firefly_txns_fts: %v", err)
	}

	// Sanity-query the FTS5 table — empty corpus, but the syntax should parse.
	rows, err := db.QueryContext(ctx, `SELECT rowid FROM firefly_txns_fts WHERE firefly_txns_fts MATCH ?`, "anything")
	if err != nil {
		t.Errorf("FTS5 query failed (FTS5 may not be enabled in build): %v", err)
	} else {
		_ = rows.Close()
	}

	// goose migration ledger should have all three of our migrations.
	var applied int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0`).Scan(&applied); err != nil {
		t.Fatalf("goose ledger: %v", err)
	}
	if applied != 3 {
		t.Errorf("expected 3 migrations applied, got %d", applied)
	}
}

// TestOpen_IsIdempotent guards against future regressions where Open
// would re-run migrations or otherwise mutate state on a healthy DB.
// Two consecutive Opens of the same file must succeed.
func TestOpen_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "staging.db")

	first, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	var applied int
	if err := second.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0`).Scan(&applied); err != nil {
		t.Fatalf("goose ledger: %v", err)
	}
	if applied != 3 {
		t.Errorf("expected 3 migrations after re-open, got %d", applied)
	}
}

// TestOpen_RejectsEmptyPath catches a misconfigured deploy at startup
// rather than letting it silently write to ":memory:" or similar.
func TestOpen_RejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty path")
	}
}
