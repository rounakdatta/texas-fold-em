-- +goose Up
-- Lets the classifier propose a SOURCE account by NAME from fold's own
-- account_id → fold_accounts mapping, even when no matching firefly asset
-- exists yet.
--
-- fold always records which card/bank paid (raw_payload.account_id), so the
-- source should never be blank. When firefly has no asset for that card —
-- e.g. an "AU Ixigo ****9179" the user never set up in firefly — the
-- classifier now surfaces the fold-side card name here, and the Pusher
-- finds-or-creates the matching firefly asset on push. Mirrors 00007
-- (proposed/confirmed destination name) for the source side.
ALTER TABLE staged_fold_txns ADD COLUMN proposed_source_account_name  TEXT;
ALTER TABLE staged_fold_txns ADD COLUMN confirmed_source_account_name TEXT;

-- +goose Down
ALTER TABLE staged_fold_txns DROP COLUMN proposed_source_account_name;
ALTER TABLE staged_fold_txns DROP COLUMN confirmed_source_account_name;
