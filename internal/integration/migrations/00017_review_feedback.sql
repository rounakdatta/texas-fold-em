-- +goose Up
-- What a person did with fold's suggestions: the feedback the classifier
-- learns from. One row per decision — an edit that changed a suggested field
-- (a correction), a send (the values that went to firefly), a hold, a skip,
-- or the release of a hold fold set (so fold never sets it again).
-- The Tier-3 prompt shows the relevant ones back to the model, as the
-- strongest evidence of how this person books things, and the re-suggest
-- loop uses them to revisit waiting cards that could now be suggested
-- better. suggested_json / chosen_json hold names, not ids, because the
-- model reads names:
--   {"type","title","payee","category","budget","tags"}
CREATE TABLE review_feedback (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    fold_uuid      TEXT NOT NULL,
    at             DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f', 'now')), -- to the ms
    action         TEXT NOT NULL,             -- edit | send | hold | skip | release
    merchant_key   TEXT NOT NULL DEFAULT '',  -- staged_fold_txns.merchant_extracted
    narration      TEXT NOT NULL DEFAULT '',
    direction      TEXT NOT NULL DEFAULT '',  -- OUTGOING | INCOMING
    amount_paise   INTEGER NOT NULL DEFAULT 0,
    foreign_label  TEXT NOT NULL DEFAULT '',  -- "SGD 2.87" for a foreign charge
    suggested_json TEXT NOT NULL DEFAULT '{}',
    chosen_json    TEXT NOT NULL DEFAULT '{}',
    note           TEXT NOT NULL DEFAULT ''   -- a hold's reason
);
CREATE INDEX idx_review_feedback_merchant ON review_feedback (merchant_key, at);
CREATE INDEX idx_review_feedback_at ON review_feedback (at);
CREATE INDEX idx_review_feedback_uuid ON review_feedback (fold_uuid);

-- Which engine made a row's current suggestion (so a better one can revisit
-- what an older one suggested); when and why it was last revisited, and
-- whether that changed what the card says.
ALTER TABLE staged_fold_txns ADD COLUMN classifier_model TEXT;
ALTER TABLE staged_fold_txns ADD COLUMN classifier_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE staged_fold_txns ADD COLUMN resuggested_at DATETIME;
ALTER TABLE staged_fold_txns ADD COLUMN resuggest_reason TEXT;
ALTER TABLE staged_fold_txns ADD COLUMN resuggest_changed INTEGER NOT NULL DEFAULT 0;

-- Who put a row on hold: 'fold' when the classifier did (a card's
-- credit-back of a charge never billed), NULL when a person did.
ALTER TABLE staged_fold_txns ADD COLUMN hold_by TEXT;

-- +goose Down
ALTER TABLE staged_fold_txns DROP COLUMN hold_by;
ALTER TABLE staged_fold_txns DROP COLUMN resuggest_changed;
ALTER TABLE staged_fold_txns DROP COLUMN resuggest_reason;
ALTER TABLE staged_fold_txns DROP COLUMN resuggested_at;
ALTER TABLE staged_fold_txns DROP COLUMN classifier_version;
ALTER TABLE staged_fold_txns DROP COLUMN classifier_model;
DROP INDEX idx_review_feedback_uuid;
DROP INDEX idx_review_feedback_at;
DROP INDEX idx_review_feedback_merchant;
DROP TABLE review_feedback;
