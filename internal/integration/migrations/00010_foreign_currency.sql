-- +goose Up
-- Foreign-currency support. A fold transaction abroad carries BOTH amounts:
--   amount / currency               → the HOME amount billed to the card (INR)
--   source_amount / source_currency → the ORIGINAL charge (e.g. AED 5.99)
-- e.g. {"amount":143.27,"currency":"INR","source_amount":5.99,"source_currency":"AED"}
--
-- Earlier syncs stored the SOURCE side (AED 5.99) as the staged primary, so
-- the push sent currency_code=AED — which firefly 422s because AED isn't an
-- enabled currency. The correct firefly model is INR as the primary amount +
-- AED as foreign_amount/foreign_currency_code. These columns carry the
-- foreign side; the primary amount_paise/currency now holds the INR home side.
ALTER TABLE staged_fold_txns ADD COLUMN foreign_amount_paise INTEGER;
ALTER TABLE staged_fold_txns ADD COLUMN foreign_currency TEXT;

-- Backfill existing rows from the verbatim raw_payload. Only touch rows whose
-- payload actually carries the home-currency fields (newer wire shape); older
-- thin payloads are left as-is. For a domestic txn amount == source_amount so
-- the primary is unchanged; only genuinely-foreign rows flip AED→INR primary
-- and gain a foreign side.
UPDATE staged_fold_txns
SET amount_paise = CAST(ROUND(json_extract(raw_payload,'$.amount') * 100) AS INTEGER),
    currency     = json_extract(raw_payload,'$.currency')
WHERE json_extract(raw_payload,'$.amount')   IS NOT NULL
  AND json_extract(raw_payload,'$.currency') IS NOT NULL
  AND json_extract(raw_payload,'$.currency') <> '';

UPDATE staged_fold_txns
SET foreign_amount_paise = CAST(ROUND(json_extract(raw_payload,'$.source_amount') * 100) AS INTEGER),
    foreign_currency     = json_extract(raw_payload,'$.source_currency')
WHERE json_extract(raw_payload,'$.source_currency') IS NOT NULL
  AND json_extract(raw_payload,'$.source_currency') <> ''
  AND json_extract(raw_payload,'$.source_currency') <> json_extract(raw_payload,'$.currency');

-- +goose Down
ALTER TABLE staged_fold_txns DROP COLUMN foreign_amount_paise;
ALTER TABLE staged_fold_txns DROP COLUMN foreign_currency;
