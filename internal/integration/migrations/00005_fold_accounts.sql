-- +goose Up
-- fold_accounts mirrors the user's fold-side asset registry: every
-- credit card from /api/v3/users/{uid}/credit-cards and every bank
-- account from /api/v1/users/{uid}/bank_accounts. Keyed by fold's
-- per-account UUID, which is what shows up as `account_id` on each
-- transaction in /api/v3/users/{uid}/transactions.
--
-- Why this exists: the transactions endpoint gives `account_id` as an
-- opaque UUID (e.g. "8582f77b-fdcc-…") with no human-readable name,
-- provider, or last-four-digits inline. The classifier's Tier-3 LLM
-- needs the *name* — "Tata Neu Plus", "HDFC Bank ****5684" — to ground
-- source-account inference against the user's firefly asset list. With
-- only the UUID, an unseen merchant defaults to whatever credit card
-- shows up most in FTS history; that produced the c3c79fef KARAN
-- BAHADUR misclassification (wire said HDFC RuPay, LLM said Axis).
--
-- Treated as a denormalised cache (like merchant_lookup): rebuilt by
-- /admin/fold/accounts/sync, losing it forces a rebuild from fold but
-- never breaks the broker.
CREATE TABLE fold_accounts (
    fold_account_id      TEXT     PRIMARY KEY,        -- fold's stable per-account UUID
    kind                 TEXT     NOT NULL            -- BANK | CREDIT_CARD
                                  CHECK (kind IN ('BANK','CREDIT_CARD')),
    name                 TEXT     NOT NULL,           -- e.g. "Tata Neu Plus", "HDFC Bank ****5684"
    provider             TEXT,                        -- e.g. "HDFC", "Axis Bank" (FIP-side institution)
    network              TEXT,                        -- credit-card-only: "Visa", "RuPay", "Mastercard"
    last_four            TEXT,                        -- last 4 of card or last 4 of masked account number
    nickname             TEXT,                        -- user-set nickname if any
    holder_name          TEXT,
    raw_payload          TEXT     NOT NULL,           -- full fold response object for this account
    is_closed            INTEGER  NOT NULL DEFAULT 0, -- 1 if fold reports the account as closed
    last_synced_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE fold_accounts;
