-- +goose Up
-- The integration's three core tables. Lives in /data/staging.db on a
-- separate file from the broker's state.json so that schema migration
-- bugs in the integration code can never destabilise the refresh chain.

-- staged_fold_txns is our queue. One row per fold transaction we've seen.
-- The fold UUID is the primary key — re-syncing the same window of fold
-- transactions is naturally idempotent (INSERT OR IGNORE).
CREATE TABLE staged_fold_txns (
    fold_uuid                       TEXT     PRIMARY KEY,
    raw_payload                     TEXT     NOT NULL,                  -- full JSON object from /api/v3/.../transactions
    amount_paise                    INTEGER  NOT NULL,                  -- INR × 100, integer math everywhere
    currency                        TEXT     NOT NULL,
    txn_timestamp                   DATETIME NOT NULL,                  -- UTC, RFC3339
    mode                            TEXT     NOT NULL,                  -- CARD | UPI | OTHERS | …
    type                            TEXT     NOT NULL,                  -- INCOMING | OUTGOING
    narration                       TEXT     NOT NULL,
    merchant_extracted              TEXT,                               -- normalised merchant string (lowercase, trimmed)

    -- Classifier output. NULL until classifier runs. Status drives UI surfaces.
    -- pending      : just inserted, classifier hasn't seen it yet
    -- needs_review : classifier ran but confidence below threshold
    -- ready_to_push: classifier high-confidence OR human-confirmed
    -- pushed       : successfully created in firefly
    -- skipped      : human chose to exclude this transaction from firefly
    status                          TEXT     NOT NULL DEFAULT 'pending'
                                             CHECK (status IN ('pending','needs_review','ready_to_push','pushed','skipped')),
    classifier_tier                 INTEGER,                            -- 1 (lookup) | 2 (FTS5 vote) | 3 (LLM RAG) | 4 (human)
    classifier_confidence           REAL,                               -- 0.0 .. 1.0
    classifier_evidence_json        TEXT,                               -- top-K matches, scores, LLM reasoning

    -- Proposed labels (classifier's suggestion). NULL until classifier runs.
    proposed_source_account_id      INTEGER,                            -- firefly account.id (asset)
    proposed_destination_account_id INTEGER,                            -- firefly account.id (expense, the merchant)
    proposed_category_id            INTEGER,
    proposed_budget_id              INTEGER,
    proposed_description            TEXT,
    proposed_tags_json              TEXT,                               -- JSON array of strings

    -- Confirmed labels (after human review or auto-confirm). What we actually push.
    confirmed_source_account_id     INTEGER,
    confirmed_destination_account_id INTEGER,
    confirmed_category_id           INTEGER,
    confirmed_budget_id             INTEGER,
    confirmed_description           TEXT,
    confirmed_tags_json             TEXT,

    -- Outcome of the firefly write.
    firefly_txn_id                  INTEGER,                            -- POST response.data.id
    pushed_at                       DATETIME,

    classified_at                   DATETIME,
    reviewed_at                     DATETIME,
    created_at                      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_staged_status         ON staged_fold_txns(status);
CREATE INDEX idx_staged_merchant       ON staged_fold_txns(merchant_extracted);
CREATE INDEX idx_staged_txn_timestamp  ON staged_fold_txns(txn_timestamp DESC);

-- firefly_txns is our local mirror of firefly's transaction journal. We
-- maintain it via periodic sync calls to firefly's GET endpoints. It is
-- the corpus the classifier reads from. We never write back into firefly
-- through this table; the canonical store remains firefly itself.
CREATE TABLE firefly_txns (
    firefly_id                          INTEGER  PRIMARY KEY,           -- transaction journal id
    group_id                            INTEGER  NOT NULL,              -- transaction group id (split parent)
    txn_type                            TEXT     NOT NULL,              -- withdrawal | deposit | transfer
    amount_paise                        INTEGER  NOT NULL,              -- absolute value, type carries sign
    currency                            TEXT     NOT NULL,
    date                                DATETIME NOT NULL,
    source_account_id                   INTEGER,
    source_account_name                 TEXT,
    destination_account_id              INTEGER,
    destination_account_name            TEXT,                           -- the merchant, for expense rows
    destination_account_name_normalized TEXT,                           -- lowercased + stripped, for fast joins
    category_id                         INTEGER,
    category_name                       TEXT,
    budget_id                           INTEGER,
    budget_name                         TEXT,
    description                         TEXT     NOT NULL,
    tags_json                           TEXT,                           -- JSON array
    external_id                         TEXT,                           -- firefly's external_id (we set it to fold_uuid on push)
    created_at                          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at                          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_firefly_external_id ON firefly_txns(external_id);
CREATE INDEX idx_firefly_dest_norm   ON firefly_txns(destination_account_name_normalized);
CREATE INDEX idx_firefly_date        ON firefly_txns(date DESC);
CREATE INDEX idx_firefly_category    ON firefly_txns(category_id);

-- audit_log is append-only and tracks every interaction with firefly's
-- mutation surface (reads aren't logged — only the create-side calls).
-- This is the trail we'd grep through if we ever asked "what did the
-- integration do at 3am last night?"
CREATE TABLE audit_log (
    id              INTEGER  PRIMARY KEY AUTOINCREMENT,
    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    actor           TEXT     NOT NULL,                                  -- 'system' | 'admin'
    action          TEXT     NOT NULL,                                  -- e.g. 'firefly_create', 'firefly_dedup_hit', 'fold_sync'
    fold_uuid       TEXT,                                               -- staging row reference
    firefly_txn_id  INTEGER,                                            -- firefly transaction id when applicable
    payload_hash    TEXT,                                               -- sha256 hex of the request body
    response_status INTEGER,                                            -- HTTP status of any upstream call
    notes           TEXT
);

CREATE INDEX idx_audit_ts     ON audit_log(ts DESC);
CREATE INDEX idx_audit_action ON audit_log(action);

-- +goose Down
DROP INDEX IF EXISTS idx_audit_action;
DROP INDEX IF EXISTS idx_audit_ts;
DROP TABLE IF EXISTS audit_log;
DROP INDEX IF EXISTS idx_firefly_category;
DROP INDEX IF EXISTS idx_firefly_date;
DROP INDEX IF EXISTS idx_firefly_dest_norm;
DROP INDEX IF EXISTS idx_firefly_external_id;
DROP TABLE IF EXISTS firefly_txns;
DROP INDEX IF EXISTS idx_staged_txn_timestamp;
DROP INDEX IF EXISTS idx_staged_merchant;
DROP INDEX IF EXISTS idx_staged_status;
DROP TABLE IF EXISTS staged_fold_txns;
