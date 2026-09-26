-- +goose Up
-- fold's copy of Firefly's categories. Until now fold knew only the
-- categories its mirrored transactions carry, so one made in Firefly but not
-- used yet was invisible: it could not be picked, and a card holding it showed
-- no category at all. The accounts mirror fills this table on every sync
-- cycle; creating a category from the deck's picker writes its row at once.
CREATE TABLE firefly_categories (
    firefly_id     INTEGER PRIMARY KEY,
    name           TEXT NOT NULL,
    last_synced_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_firefly_categories_name ON firefly_categories (name COLLATE NOCASE);

-- +goose Down
DROP INDEX idx_firefly_categories_name;
DROP TABLE firefly_categories;
