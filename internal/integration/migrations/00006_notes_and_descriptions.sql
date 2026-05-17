-- +goose Up
-- Two related improvements to "make the system remember more":
--
-- 1. firefly_txns.notes — for months the user has been pasting raw fold
--    narration into firefly's per-transaction Notes field by hand (e.g.
--    `CARD/19b4989b2e3afba8/SHREE VINAYAKA ENTE/Rs./60.00/OUTGOING/...`
--    on a transaction filed under "Sri Udupi Park, Indiranagar"). The
--    /admin/firefly/sync code currently pulls `description` but throws
--    away `notes`, so the classifier never sees these human-curated
--    narration↔merchant mappings. Tier-2 (FTS) and Tier-3 (LLM) both
--    suffer: a fold txn with `merchant_extracted="shree vinayaka ente"`
--    has zero token overlap with the destination's stored fields
--    ("Sri Udupi Park"), so BM25 returns nothing, the LLM has no
--    evidence, and the row falls to needs_review despite the answer
--    being one column away.
--
-- 2. merchant_lookup.modal_description — until now the lookup stored
--    only structural ids (destination, source, category, budget). On a
--    Tier-1 hit, the classifier returned an empty Description and the
--    push fell back to merchant_extracted as the title. With this
--    column, every successful push reinforces both the structural and
--    the descriptive sides: next time the same merchant comes up,
--    Tier-1 ships a Decision with a voice-matched title attached and
--    no LLM call is needed.

-- +goose StatementBegin
ALTER TABLE firefly_txns ADD COLUMN notes TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE merchant_lookup ADD COLUMN modal_description TEXT;
-- +goose StatementEnd

-- SQLite FTS5 doesn't support ALTER ADD COLUMN, so we drop and recreate
-- firefly_txns_fts with the new `notes` column. Triggers are recreated
-- to project notes into the index on every insert / delete / update.
-- After the migration runs, /admin/firefly/sync re-pulls each transaction
-- and the UPDATE trigger refreshes the FTS row with notes populated.

-- +goose StatementBegin
DROP TRIGGER IF EXISTS firefly_txns_au;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS firefly_txns_ad;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS firefly_txns_ai;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS firefly_txns_fts;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE VIRTUAL TABLE firefly_txns_fts USING fts5(
    destination_account_name,
    description,
    category_name,
    budget_name,
    notes,
    content='firefly_txns',
    content_rowid='firefly_id'
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER firefly_txns_ai AFTER INSERT ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(rowid, destination_account_name, description, category_name, budget_name, notes)
    VALUES (new.firefly_id, new.destination_account_name, new.description, new.category_name, new.budget_name, COALESCE(new.notes,''));
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER firefly_txns_ad AFTER DELETE ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(firefly_txns_fts, rowid, destination_account_name, description, category_name, budget_name, notes)
    VALUES ('delete', old.firefly_id, old.destination_account_name, old.description, old.category_name, old.budget_name, COALESCE(old.notes,''));
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER firefly_txns_au AFTER UPDATE ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(firefly_txns_fts, rowid, destination_account_name, description, category_name, budget_name, notes)
    VALUES ('delete', old.firefly_id, old.destination_account_name, old.description, old.category_name, old.budget_name, COALESCE(old.notes,''));
    INSERT INTO firefly_txns_fts(rowid, destination_account_name, description, category_name, budget_name, notes)
    VALUES (new.firefly_id, new.destination_account_name, new.description, new.category_name, new.budget_name, COALESCE(new.notes,''));
END;
-- +goose StatementEnd

-- Re-populate the FTS index from current firefly_txns rows. notes is NULL
-- for everything until the next /admin/firefly/sync; this just seeds the
-- index so existing destination_account_name / description hits keep
-- working between the migration apply and the next sync.

-- +goose StatementBegin
INSERT INTO firefly_txns_fts(rowid, destination_account_name, description, category_name, budget_name, notes)
SELECT firefly_id, destination_account_name, description, category_name, budget_name, COALESCE(notes,'')
FROM firefly_txns;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS firefly_txns_au;
DROP TRIGGER IF EXISTS firefly_txns_ad;
DROP TRIGGER IF EXISTS firefly_txns_ai;
DROP TABLE IF EXISTS firefly_txns_fts;

CREATE VIRTUAL TABLE firefly_txns_fts USING fts5(
    destination_account_name,
    description,
    category_name,
    budget_name,
    content='firefly_txns',
    content_rowid='firefly_id'
);
CREATE TRIGGER firefly_txns_ai AFTER INSERT ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(rowid, destination_account_name, description, category_name, budget_name)
    VALUES (new.firefly_id, new.destination_account_name, new.description, new.category_name, new.budget_name);
END;
CREATE TRIGGER firefly_txns_ad AFTER DELETE ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(firefly_txns_fts, rowid, destination_account_name, description, category_name, budget_name)
    VALUES ('delete', old.firefly_id, old.destination_account_name, old.description, old.category_name, old.budget_name);
END;
CREATE TRIGGER firefly_txns_au AFTER UPDATE ON firefly_txns BEGIN
    INSERT INTO firefly_txns_fts(firefly_txns_fts, rowid, destination_account_name, description, category_name, budget_name)
    VALUES ('delete', old.firefly_id, old.destination_account_name, old.description, old.category_name, old.budget_name);
    INSERT INTO firefly_txns_fts(rowid, destination_account_name, description, category_name, budget_name)
    VALUES (new.firefly_id, new.destination_account_name, new.description, new.category_name, new.budget_name);
END;

ALTER TABLE firefly_txns DROP COLUMN notes;
ALTER TABLE merchant_lookup DROP COLUMN modal_description;
