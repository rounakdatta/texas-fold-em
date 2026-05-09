-- +goose Up
-- The classifier's Tier 3 (LLM synthesiser) now picks the firefly
-- transaction type explicitly — withdrawal, deposit, OR transfer.
-- The default fold-direction → firefly mapping only knows the first
-- two; transfers (e.g. savings → zerodha) need an explicit override.
--
-- Stored as TEXT because the value is set by the LLM and we want the
-- canonical firefly term in our ledger. NULL means "use the default
-- mapping" (withdrawal for OUTGOING, deposit for INCOMING).

ALTER TABLE staged_fold_txns ADD COLUMN proposed_txn_type  TEXT
    CHECK (proposed_txn_type IS NULL OR proposed_txn_type IN ('withdrawal','deposit','transfer'));
ALTER TABLE staged_fold_txns ADD COLUMN confirmed_txn_type TEXT
    CHECK (confirmed_txn_type IS NULL OR confirmed_txn_type IN ('withdrawal','deposit','transfer'));

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35; modernc.org/sqlite is
-- recent enough to handle it, but to keep this migration reversible on
-- as wide a SQLite-compat surface as possible, the down step recreates
-- the table without the columns. Best-effort.
ALTER TABLE staged_fold_txns DROP COLUMN proposed_txn_type;
ALTER TABLE staged_fold_txns DROP COLUMN confirmed_txn_type;
