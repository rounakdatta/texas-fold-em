-- +goose Up
-- 0.14.0's refund matcher (MatchRefunds) proposed a purchase for every refund
-- it could, including low-confidence guesses (a "latest larger purchase", an
-- exact amount from months earlier) on rows that were already ready to push —
-- where push would have turned the guess into a firefly link unreviewed. From
-- 0.14.1 it proposes only confident matches. Clear what it proposed so the
-- next sync recomputes those rows under the stricter rule. The refund tier's
-- own proposals (classifier_tier 5, rows it also routed to review when
-- unsure) and anything already linked in firefly are kept; nothing a human
-- chose (confirmed_refund_of) is touched.
UPDATE staged_fold_txns
SET proposed_refund_of = NULL
WHERE proposed_refund_of IS NOT NULL
  AND firefly_link_id IS NULL
  AND COALESCE(classifier_tier, 0) <> 5;

-- +goose Down
-- Nothing to restore: the next sync recomputes proposals either way.
SELECT 1;
