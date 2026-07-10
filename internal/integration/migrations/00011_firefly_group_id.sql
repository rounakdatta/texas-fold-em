-- +goose Up
-- Store the firefly transaction GROUP id on the staged row at push time.
--
-- The review UI deep-links a pushed row to its firefly transaction at
-- /transactions/show/{group_id}. Until now that group id was only
-- discoverable by joining the firefly_txns mirror (firefly_id = the stored
-- journal id) — which fails whenever the journal hasn't been mirrored yet
-- (the eager-after-push mirror was fetching by journal id, which firefly
-- 404s, and the periodic full sync hadn't run). Result: freshly-pushed rows
-- showed no firefly link even though the transaction existed.
--
-- The group id is known at create time (POST /transactions response .data.id),
-- so we persist it directly and the link no longer depends on the mirror.
ALTER TABLE staged_fold_txns ADD COLUMN firefly_group_id INTEGER;

-- +goose Down
ALTER TABLE staged_fold_txns DROP COLUMN firefly_group_id;
