package classifier

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LearnFromPushed is the active-learning loop: every successful push
// immediately reinforces merchant_lookup so the next fold transaction
// from the same merchant auto-classifies via Tier 1, no human in the
// loop.
//
// Behaviour:
//   - Read the staged row's merchant_extracted + confirmed_* labels
//     (or proposed_* as fallback).
//   - UPSERT into merchant_lookup with sample_size += 1 and
//     confidence recomputed conservatively (treat THIS push as one
//     supporting datapoint; cap confidence at 1.0).
//   - If the merchant has no row yet, create one with sample_size=1,
//     confidence=1.0.
//
// Why not just /admin/firefly/sync after every push? That's a
// full-corpus walk — expensive (~15s) and pulls down 7k transactions
// just to learn one new pattern. UPSERT-on-push is O(1) and gets us
// the same Tier-1 hit on the next attempt. The next periodic sync
// will reconcile any drift.
//
// Idempotent: re-calling for the same fold_uuid after a push doesn't
// double-count (status='pushed' guards the second call from
// reinforcing again).
func (c *Classifier) LearnFromPushed(ctx context.Context, foldUUID string) error {
	if foldUUID == "" {
		return errors.New("learn: empty fold_uuid")
	}

	// Read confirmed labels (fall back to proposed) + the merchant
	// string. We require status='pushed' to prevent reinforcement off
	// rows that were rolled back somehow.
	var (
		merchant sql.NullString
		status   string
		dstID    sql.NullInt64
		dstName  sql.NullString
		srcID    sql.NullInt64
		srcName  sql.NullString
		catID    sql.NullInt64
		catName  sql.NullString
		budID    sql.NullInt64
		budName  sql.NullString
		desc     sql.NullString
	)
	// We resolve human-readable names by joining against firefly_txns.
	// In the moment immediately after a push, the just-created firefly
	// transaction isn't yet mirrored locally (next periodic sync gets
	// it), so destination/category names may be NULL. We fall back to
	// the merchant string (for destination) and a "category-<id>" form
	// (when even that fails) so the NOT-NULL invariants on
	// merchant_lookup.modal_destination_account_name are preserved.
	err := c.db.QueryRowContext(ctx, `
		SELECT merchant_extracted, status,
		       COALESCE(confirmed_destination_account_id, proposed_destination_account_id),
		       (SELECT destination_account_name FROM firefly_txns
		         WHERE destination_account_id = COALESCE(confirmed_destination_account_id, proposed_destination_account_id)
		         LIMIT 1),
		       COALESCE(confirmed_source_account_id, proposed_source_account_id),
		       (SELECT source_account_name FROM firefly_txns
		         WHERE source_account_id = COALESCE(confirmed_source_account_id, proposed_source_account_id)
		         LIMIT 1),
		       COALESCE(confirmed_category_id, proposed_category_id),
		       (SELECT category_name FROM firefly_txns
		         WHERE category_id = COALESCE(confirmed_category_id, proposed_category_id)
		         LIMIT 1),
		       COALESCE(confirmed_budget_id, proposed_budget_id),
		       (SELECT budget_name FROM firefly_txns
		         WHERE budget_id = COALESCE(confirmed_budget_id, proposed_budget_id)
		         LIMIT 1),
		       COALESCE(NULLIF(TRIM(confirmed_description),''), NULLIF(TRIM(proposed_description),''))
		FROM staged_fold_txns
		WHERE fold_uuid = ?
	`, foldUUID).Scan(&merchant, &status, &dstID, &dstName, &srcID, &srcName, &catID, &catName, &budID, &budName, &desc)
	if err != nil {
		return fmt.Errorf("learn: read staged: %w", err)
	}
	if status != "pushed" {
		// Defensive: only learn from confirmed-and-pushed rows.
		return nil
	}
	// A confirmed id of 0 is the review form's explicit "none" (a wrong
	// suggestion the human cleared). Learn it as "no category / no budget",
	// never as an account id 0.
	if catID.Valid && catID.Int64 == 0 {
		catID = sql.NullInt64{}
	}
	if budID.Valid && budID.Int64 == 0 {
		budID = sql.NullInt64{}
	}
	merchantNorm := ""
	if merchant.Valid {
		merchantNorm = strings.TrimSpace(strings.ToLower(merchant.String))
	}
	if merchantNorm == "" {
		// No merchant string to key on — nothing to learn. (UPI to
		// individuals where extraction returned empty fall here. The
		// Tier-3 LLM path should have proposed labels but those don't
		// help future Tier-1 hits without a key.)
		return nil
	}
	if !dstID.Valid {
		// No destination account → not a useful training datapoint.
		return nil
	}

	// Fallback for the NOT-NULL modal_destination_account_name when
	// the local firefly mirror doesn't yet have the freshly-created
	// transaction (firefly sync is periodic, not eager).
	dstNameStr := merchantNorm
	if dstName.Valid && dstName.String != "" {
		dstNameStr = dstName.String
	}

	// UPSERT. ON CONFLICT bumps sample_size and recomputes confidence
	// modestly (we don't have category-distribution data here without
	// a re-scan, so we keep confidence stable when the same labels
	// reinforce, and lower it conservatively when labels CHANGED).
	// modal_description is updated to the row's own confirmed/proposed
	// description so that Tier-1 hits the next time around can ship
	// with a voice-matched title — no LLM call needed. "Latest wins"
	// matches the periodic-rebuild semantics in sync.go.
	now := time.Now().UTC()
	_, err = c.db.ExecContext(ctx, `
		INSERT INTO merchant_lookup (
		    merchant_normalized,
		    modal_destination_account_id, modal_destination_account_name,
		    modal_source_account_id, modal_source_account_name,
		    modal_category_id, modal_category_name,
		    modal_budget_id, modal_budget_name,
		    modal_description,
		    sample_size, confidence, last_seen
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1.0, ?)
		ON CONFLICT(merchant_normalized) DO UPDATE SET
		    modal_destination_account_id   = excluded.modal_destination_account_id,
		    modal_destination_account_name = excluded.modal_destination_account_name,
		    modal_source_account_id        = COALESCE(excluded.modal_source_account_id, modal_source_account_id),
		    modal_source_account_name      = COALESCE(excluded.modal_source_account_name, modal_source_account_name),
		    modal_category_id              = COALESCE(excluded.modal_category_id, modal_category_id),
		    modal_category_name            = COALESCE(excluded.modal_category_name, modal_category_name),
		    modal_budget_id                = COALESCE(excluded.modal_budget_id, modal_budget_id),
		    modal_budget_name              = COALESCE(excluded.modal_budget_name, modal_budget_name),
		    modal_description              = COALESCE(excluded.modal_description, modal_description),
		    sample_size                    = sample_size + 1,
		    -- Stable confidence rule: when the labels match what's
		    -- already there, keep at 1.0. When they differ (the human
		    -- corrected the modal), drop one notch — Tier 1 keeps
		    -- firing but a future periodic firefly sync will recompute
		    -- the true modal precisely.
		    confidence = CASE
		        WHEN modal_destination_account_id = excluded.modal_destination_account_id
		         AND modal_category_id IS NOT DISTINCT FROM excluded.modal_category_id
		        THEN MIN(confidence, 1.0)
		        ELSE 0.85
		    END,
		    last_seen = excluded.last_seen
	`,
		merchantNorm,
		dstID.Int64, dstNameStr,
		nullToInt64Any(srcID), nullToString(srcName),
		nullToInt64Any(catID), nullToString(catName),
		nullToInt64Any(budID), nullToString(budName),
		nullToString(desc),
		now,
	)
	if err != nil {
		return fmt.Errorf("learn: upsert lookup: %w", err)
	}
	c.log.Info("learned from push",
		"merchant", merchantNorm,
		"category_id", nullableLogValue(catID),
		"description_seeded", desc.Valid,
	)
	return nil
}

func nullToString(n sql.NullString) any {
	if !n.Valid {
		return nil
	}
	return n.String
}

func nullToInt64Any(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

func nullableLogValue(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}
