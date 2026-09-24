-- +goose Up
-- Human overrides for the money and time a fold alert got wrong.
--
-- fold sees the card ALERT, which fires at authorisation. The amount that
-- actually settles on the card statement can differ:
--   - a US restaurant tip is added after the alert (the settled charge is
--     7-17% higher than the alerted one);
--   - fold.money converts a foreign charge at its own rate (AED was ~8.5% low
--     against what the bank billed in July 2026);
--   - one alert can post as two statement lines (an airline ticket + fees).
-- Reconciling against the statement needs the operator to be able to say
-- "the real INR is X" and have push carry it to firefly.
--
-- NULL means "use fold's value" (amount_paise / foreign_amount_paise /
-- txn_timestamp), so every existing row behaves exactly as before.
ALTER TABLE staged_fold_txns ADD COLUMN confirmed_amount_paise INTEGER;
ALTER TABLE staged_fold_txns ADD COLUMN confirmed_foreign_amount_paise INTEGER;
ALTER TABLE staged_fold_txns ADD COLUMN confirmed_txn_timestamp DATETIME;

-- +goose Down
ALTER TABLE staged_fold_txns DROP COLUMN confirmed_txn_timestamp;
ALTER TABLE staged_fold_txns DROP COLUMN confirmed_foreign_amount_paise;
ALTER TABLE staged_fold_txns DROP COLUMN confirmed_amount_paise;
