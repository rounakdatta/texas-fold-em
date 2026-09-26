#!/usr/bin/env bash
#
# local-sandbox.sh — run texas-fold-em locally against a snapshot of the
# live homelab staging.db, for fast iteration on the classifier.
#
# Why this works without firefly/fold/MySQL: the classifier reads ONLY the
# local SQLite mirror (firefly_txns, merchant_lookup, fold_accounts).
# firefly/fold are touched only by the syncer and pusher, neither of which
# runs during /admin/classify. So a copy of staging.db + an LLM key is a
# complete, faithful sandbox for classification work.
#
# Both the DB snapshot and the LLM key are pulled from the cluster, so set
# KUBECONFIG first (see the access-homelab-setup-k3s skill, which writes
# /tmp/k3s-kubeconfig.yaml — the default below).
#
# Usage:
#   make sandbox                 # first run pulls the snapshot, then runs
#   REFRESH=1 make sandbox       # re-pull the live snapshot, then run
#   TFE_PORT=9001 make sandbox   # override the listen port (default 8099)
#
# Once running:
#   open http://127.0.0.1:8099/
#   ./scripts/reclassify-one.sh <fold_uuid>     # reset+classify one row
#   curl -XPOST -H 'Authorization: Bearer localadmin' \
#        'http://127.0.0.1:8099/admin/classify?scope=pending'
set -euo pipefail

NS=${TFE_NS:-apps}
SECRET=${TFE_SECRET:-texas-fold-em-credentials}
SELECTOR=${TFE_SELECTOR:-app.kubernetes.io/name=texas-fold-em}
DIR=${TFE_LOCAL_DIR:-.local}
PORT=${TFE_PORT:-8099}
: "${KUBECONFIG:=/tmp/k3s-kubeconfig.yaml}"
export KUBECONFIG

need() { command -v "$1" >/dev/null || { echo "missing dependency: $1" >&2; exit 1; }; }
need kubectl; need sqlite3; need go

mkdir -p "$DIR"

if [[ "${REFRESH:-0}" == "1" || ! -f "$DIR/staging.db" ]]; then
  echo "→ locating texas-fold-em pod in ns/$NS …"
  POD=$(kubectl -n "$NS" get pods -l "$SELECTOR" -o name | head -1)
  [[ -n "$POD" ]] || { echo "no pod matched '$SELECTOR' in ns/$NS (set TFE_SELECTOR/TFE_NS)" >&2; exit 1; }
  echo "→ pulling /data/staging.db from ${POD#pod/} …"
  kubectl -n "$NS" cp "${POD#pod/}:/data/staging.db" "$DIR/staging.db"
  # The live copy may carry an un-checkpointed WAL; fold it into the base
  # file so the sandbox sees the current state as a single clean file.
  sqlite3 "$DIR/staging.db" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null 2>&1 || true
  echo "→ fetching LLM key from secret/$SECRET …"
  kubectl -n "$NS" get secret "$SECRET" -o jsonpath='{.data.llm-api-key}' | base64 -d > "$DIR/.llmkey"
  chmod 600 "$DIR/.llmkey"
fi

[[ -s "$DIR/.llmkey" ]] || { echo "no LLM key at $DIR/.llmkey — run 'REFRESH=1 make sandbox'" >&2; exit 1; }

echo "→ firefly_txns: $(sqlite3 "$DIR/staging.db" 'SELECT count(*) FROM firefly_txns;')   staged: $(sqlite3 "$DIR/staging.db" 'SELECT count(*) FROM staged_fold_txns;')   fold_accounts: $(sqlite3 "$DIR/staging.db" 'SELECT count(*) FROM fold_accounts;')"
echo "→ tfe on http://127.0.0.1:$PORT/   (admin key: localadmin · Ctrl-C to stop)"

# go run . so edits rebuild on restart. Broker is unseeded (fine — classify
# never needs a fold token); firefly base/PAT are dummies (never called by
# classify); periodic sync + keep-warm off so nothing reaches out.
exec env \
  TEXAS_FOLDEM_BROKER_KEY=localbroker \
  TEXAS_FOLDEM_ADMIN_KEY=localadmin \
  TEXAS_FOLDEM_INTEGRATION_ENABLED=true \
  TEXAS_FOLDEM_STATE_PATH="$DIR/state.local.json" \
  TEXAS_FOLDEM_STAGING_DB_PATH="$DIR/staging.db" \
  TEXAS_FOLDEM_FIREFLY_BASE="http://127.0.0.1:9" \
  TEXAS_FOLDEM_FIREFLY_PAT=dummy \
  TEXAS_FOLDEM_LLM_API_KEY="$(cat "$DIR/.llmkey")" \
  TEXAS_FOLDEM_PERIODIC_SYNC_EVERY=0 \
  TEXAS_FOLDEM_KEEPWARM_EVERY=0 \
  TEXAS_FOLDEM_UI_COOKIE_AUTH=false \
  TEXAS_FOLDEM_LISTEN_ADDR="127.0.0.1:$PORT" \
  TEXAS_FOLDEM_LOG_LEVEL=debug \
  go run .
