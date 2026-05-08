-- +goose Up
-- merchant_lookup is the materialised result of "for each merchant in
-- firefly's history, what is the modal category, source account, and
-- budget?". Tier 1 of the classifier.
--
-- This table is rebuilt periodically (a /admin/classifier/rebuild
-- endpoint will land in PR D). It's a denormalised cache — losing it is
-- not a data event, it just forces a rebuild from firefly_txns.
CREATE TABLE merchant_lookup (
    merchant_normalized               TEXT     PRIMARY KEY,
    modal_destination_account_id      INTEGER  NOT NULL,
    modal_destination_account_name    TEXT     NOT NULL,
    modal_source_account_id           INTEGER,
    modal_source_account_name         TEXT,
    modal_category_id                 INTEGER,
    modal_category_name               TEXT,
    modal_budget_id                   INTEGER,
    modal_budget_name                 TEXT,
    sample_size                       INTEGER  NOT NULL,                -- how many firefly txns this merchant has
    confidence                        REAL     NOT NULL,                -- modal_count / sample_size
    last_seen                         DATETIME NOT NULL,
    rebuilt_at                        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_lookup_dest_account ON merchant_lookup(modal_destination_account_id);
CREATE INDEX idx_lookup_last_seen    ON merchant_lookup(last_seen DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_lookup_last_seen;
DROP INDEX IF EXISTS idx_lookup_dest_account;
DROP TABLE IF EXISTS merchant_lookup;
