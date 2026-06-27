-- +goose Up
-- firefly_accounts mirrors firefly-iii's OWN account registry, pulled
-- from GET /api/v1/accounts. Until now the classifier learned the
-- user's asset list *indirectly* — by scraping distinct source/dest
-- accounts out of transaction history (see listAssetAccounts). That has
-- a fatal blind spot: an asset the user just *created* in firefly but
-- has no transactions on yet is invisible. That is exactly how an
-- "Ixigo AU Bank Credit Card" the user added could not be matched, and
-- the LLM force-fit the nearest plausible card ("AU" → "Axis Bank Ace").
--
-- With firefly's real account list mirrored here — including
-- account_number and account_role — the deterministic source resolver
-- can match a fold card (account_id → fold_accounts → last4 / name)
-- against the firefly asset the moment it exists, with no transaction
-- history required.
--
-- Denormalised cache like merchant_lookup / fold_accounts: rebuilt by
-- the firefly accounts sync (cron tick + POST /admin/firefly/accounts/sync).
-- Losing it forces a rebuild from firefly but never breaks the broker.
CREATE TABLE firefly_accounts (
    firefly_id           INTEGER  PRIMARY KEY,            -- firefly's numeric account id
    name                 TEXT     NOT NULL,               -- e.g. "Ixigo AU Bank Credit Card"
    type                 TEXT     NOT NULL,               -- asset | expense | revenue | liability | ...
    account_role         TEXT,                            -- asset-only: defaultAsset | ccAsset | savingAsset | ...
    account_number       TEXT,                            -- often holds the card/account number (last4 lives here)
    currency_code        TEXT,
    active               INTEGER  NOT NULL DEFAULT 1,      -- 1 if firefly reports the account active
    raw_payload          TEXT     NOT NULL,               -- full firefly account object
    last_synced_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    created_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- The source resolver and the asset-inventory query both filter by type,
-- so index it.
CREATE INDEX idx_firefly_accounts_type ON firefly_accounts(type);

-- +goose Down
DROP TABLE firefly_accounts;
