-- +goose Up
-- Refunds: which purchase a refund gives money back for, and the firefly
-- link that records it.
--
-- A card refund arrives from fold as an INCOMING row ("Refund Received!").
-- In firefly it is a deposit: the merchant's REVENUE twin (same name as its
-- expense account) pays the card that was charged, category Refund. Firefly
-- also has a native way to tie the two together: a transaction link of the
-- built-in type "Refund" ("(partially) refunds" / "is (partially) refunded
-- by"). Firefly is the ledger, so that link is where the relationship lives;
-- these columns only stage it until both sides exist in firefly.
--
-- A refund reference is one of:
--   fold:<fold_uuid>      the original purchase is a staged row (pending or pushed)
--   journal:<journal_id>  the original is a firefly journal fold never staged
--   none                  (confirmed only) the human says there is no original
-- proposed_refund_of is the classifier's pick; confirmed_refund_of is the
-- human's and wins when set. firefly_link_id is the firefly transaction-link
-- id once push has created it.
ALTER TABLE staged_fold_txns ADD COLUMN proposed_refund_of TEXT;
ALTER TABLE staged_fold_txns ADD COLUMN confirmed_refund_of TEXT;
ALTER TABLE staged_fold_txns ADD COLUMN firefly_link_id INTEGER;

-- The review list asks "does any refund point at this row?" for every row it
-- shows; index the effective reference so that stays a lookup.
CREATE INDEX idx_staged_refund_of ON staged_fold_txns(COALESCE(NULLIF(confirmed_refund_of,''), proposed_refund_of));

-- +goose Down
DROP INDEX IF EXISTS idx_staged_refund_of;
ALTER TABLE staged_fold_txns DROP COLUMN firefly_link_id;
ALTER TABLE staged_fold_txns DROP COLUMN confirmed_refund_of;
ALTER TABLE staged_fold_txns DROP COLUMN proposed_refund_of;
