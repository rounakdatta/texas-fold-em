-- +goose Up
-- Lets the classifier propose a destination by NAME for a novel merchant
-- that has no matching firefly expense account yet.
--
-- Until now the classifier could only point destination_account_id at an
-- EXISTING firefly account. For a first-ever merchant (e.g. a "United
-- Airlines" flight when no airline expense account exists), the LLM was
-- forced to either pick a poor existing id or fall back to the user's
-- generic catch-all account. Firefly, however, auto-creates an expense
-- account from `destination_name` on a withdrawal POST — so if we carry a
-- proposed name through, the push can mint the right account on the spot.
--
-- proposed_destination_account_name  — set by Tier 3 when it returns a
--   destination_name_suggestion and no existing id fits. id stays NULL.
-- confirmed_destination_account_name — the human's equivalent: a name
--   typed in review that doesn't resolve to an existing account (on a
--   withdrawal) is kept here rather than dropped, so the push creates it.
--
-- The Pusher prefers an id when present; it only sends destination_name
-- (id omitted) when the id is NULL, which is when firefly auto-creates.
ALTER TABLE staged_fold_txns ADD COLUMN proposed_destination_account_name  TEXT;
ALTER TABLE staged_fold_txns ADD COLUMN confirmed_destination_account_name TEXT;

-- +goose Down
ALTER TABLE staged_fold_txns DROP COLUMN proposed_destination_account_name;
ALTER TABLE staged_fold_txns DROP COLUMN confirmed_destination_account_name;
