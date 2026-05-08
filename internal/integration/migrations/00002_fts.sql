-- +goose Up
-- FTS5 full-text index over firefly_txns. This is Tier 2 of the
-- classifier: when merchant lookup misses (Tier 1), we BM25-rank firefly
-- transactions by narrative similarity and vote the modal category from
-- the top-K hits.
--
-- Backed by triggers so the index stays consistent with firefly_txns
-- without us having to remember to write to both. content='firefly_txns'
-- means FTS5 stores only the inverted index, not duplicate text.

-- +goose StatementBegin
CREATE VIRTUAL TABLE firefly_txns_fts USING fts5(
    destination_account_name,
    description,
    category_name,
    budget_name,
    content='firefly_txns',
    content_rowid='firefly_id'
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER firefly_txns_ai AFTER INSERT ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(rowid, destination_account_name, description, category_name, budget_name)
    VALUES (new.firefly_id, new.destination_account_name, new.description, new.category_name, new.budget_name);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER firefly_txns_ad AFTER DELETE ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(firefly_txns_fts, rowid, destination_account_name, description, category_name, budget_name)
    VALUES ('delete', old.firefly_id, old.destination_account_name, old.description, old.category_name, old.budget_name);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER firefly_txns_au AFTER UPDATE ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(firefly_txns_fts, rowid, destination_account_name, description, category_name, budget_name)
    VALUES ('delete', old.firefly_id, old.destination_account_name, old.description, old.category_name, old.budget_name);
    INSERT INTO firefly_txns_fts(rowid, destination_account_name, description, category_name, budget_name)
    VALUES (new.firefly_id, new.destination_account_name, new.description, new.category_name, new.budget_name);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS firefly_txns_au;
DROP TRIGGER IF EXISTS firefly_txns_ad;
DROP TRIGGER IF EXISTS firefly_txns_ai;
DROP TABLE IF EXISTS firefly_txns_fts;
