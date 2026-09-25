-- +goose Up
-- The review deck (ui/review.go) — one card per transaction, swiped right to
-- send it to firefly or left to look at later — needs two pieces of state
-- that a status column can't carry, because both are orthogonal to it (a
-- row can be needs_review OR ready_to_push and still be either):
--
--   later_at     the human swiped the card left: "needs a closer look".
--                It leaves the deck and waits in the Later pile until they
--                come back to it (saving it from the editor clears it).
--
--   hold_reason  someone established this row must NOT reach firefly, and
--                why — e.g. a statement reconciliation found "never billed
--                by AU". Sending is refused while it is set; the deck offers
--                Skip instead, with the reason on the card.
ALTER TABLE staged_fold_txns ADD COLUMN later_at DATETIME;
ALTER TABLE staged_fold_txns ADD COLUMN hold_reason TEXT;

-- +goose Down
ALTER TABLE staged_fold_txns DROP COLUMN hold_reason;
ALTER TABLE staged_fold_txns DROP COLUMN later_at;
