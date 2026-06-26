#!/usr/bin/env bash
#
# reclassify-one.sh — the fast inner loop for iterating on how a single
# transaction classifies. Resets one staged row to 'pending' (clearing
# the classifier's output, never the confirmed_* human edits, exactly
# like the classifier's own resetToPending), then re-runs classification.
#
# Assumes `make sandbox` is already running locally. Since the snapshot
# normally has no other pending rows, scope=pending classifies just this
# one (one LLM call).
#
# Usage: ./scripts/reclassify-one.sh <fold_uuid> [port]
set -euo pipefail

UUID=${1:?usage: reclassify-one.sh <fold_uuid> [port]}
PORT=${2:-${TFE_PORT:-8099}}
DIR=${TFE_LOCAL_DIR:-.local}

sqlite3 "$DIR/staging.db" "
UPDATE staged_fold_txns SET
  status='pending', classifier_tier=NULL, classifier_confidence=NULL, classifier_evidence_json=NULL,
  proposed_source_account_id=NULL, proposed_destination_account_id=NULL, proposed_category_id=NULL,
  proposed_budget_id=NULL, proposed_description=NULL, proposed_tags_json=NULL, proposed_txn_type=NULL
WHERE fold_uuid='$UUID';"

echo "→ classifying $UUID …"
curl -fsS -X POST -H 'Authorization: Bearer localadmin' \
  "http://127.0.0.1:$PORT/admin/classify?scope=pending"; echo

echo "→ result:"
sqlite3 -box "$DIR/staging.db" "
SELECT classifier_tier AS tier, round(classifier_confidence,2) AS conf, status,
  proposed_source_account_id AS src, proposed_destination_account_id AS dst,
  proposed_category_id AS cat, proposed_txn_type AS ttype, proposed_description AS title
FROM staged_fold_txns WHERE fold_uuid='$UUID';"
