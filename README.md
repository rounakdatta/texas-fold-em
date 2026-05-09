# texas-fold-em

A long-running broker that holds a [fold.money](https://fold.money) refresh
token and turns it into something useful. Two modes:

- **Broker mode** (default) — hold the refresh token, hand out short-lived
  access tokens to anything in your network that asks. This is the original
  job, and it's all you need if you just want a stable way to call fold's
  API from your own scripts and services.
- **Integration mode** (opt-in) — a fold→[firefly-iii](https://www.firefly-iii.org)
  classification pipeline built on top of the broker. Pulls fresh fold
  transactions, figures out what each one *is* (which merchant, which
  category, which of your own cards/accounts paid for it), and either
  pushes the result to firefly automatically or flags it for a quick human
  review.

Reverse-engineering notes on the underlying fold API live in
[`fold.md`](./fold.md).

## Why this exists

If you keep your books in firefly-iii and you spend through fold (so fold
is the most authoritative ledger you have day-to-day), you have a tedious
copy-paste job: every few days, look at fold, type each transaction into
firefly, pick the right account, the right category, the right tags. This
project does that copy step for you, intelligently, while keeping a human
in the loop for anything ambiguous.

## At a glance

| You want to… | You need… |
|---|---|
| Just call fold's API from your own code | broker mode + a fold seed |
| Auto-mirror fold transactions into firefly | integration mode + a firefly token |
| Better merchant/category guesses than rules | integration mode + a Gemini API key |
| A nicer-than-firefly review UI for ambiguous rows | integration mode (the UI is bundled) |

## Install

The Helm chart is published as an OCI artifact to GHCR alongside the Docker
image.

```bash
helm install tfe oci://ghcr.io/rounakdatta/charts/texas-fold-em \
  --version 0.3.0 \
  -n fold --create-namespace \
  --set secret.brokerKey="$(openssl rand -base64 32)" \
  --set secret.adminKey="$(openssl rand -base64 32)"
```

That gets you broker mode. Bring your own Secret instead:
`--set secret.create=false --set secret.existingSecret=<name>`.

The chart wants two keys to exist in that secret:

- `broker-key` — bearer token your services use to ask for an access token
- `admin-key`  — bearer token you use for the privileged endpoints (`/init`, the `/admin/*` family)

## Bootstrap the refresh chain

This is the only manual step. You do it once, and again only if fold
revokes the chain (the broker will return `410 Gone` and stop trying when
that happens — see *Reliability* below).

Log in to fold.money in a browser. Open DevTools → Application →
Local Storage. Copy `refresh_token` and `device_hash`. Then:

```bash
kubectl -n fold port-forward svc/tfe 8080:8080 &
ADMIN=$(kubectl -n fold get secret tfe -o jsonpath='{.data.admin-key}' | base64 -d)

curl -sS -X POST http://localhost:8080/init \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"eyJ…","device_hash":"d051a97c-…"}'
```

The broker validates the seed by exchanging it with fold *before* writing
it to disk — a bad seed never overwrites good state.

## Using the broker (mode 1)

```bash
curl -sS -H "Authorization: Bearer $BROKER_KEY" \
  http://tfe.fold.svc.cluster.local:8080/token
# { "access_token": "...", "device_hash": "...", "user_uuid": "...", "expires_at": "..." }
```

The token is yours to use against fold's own API:

```bash
curl "https://api.fold.money/api/v3/users/$UID/transactions?limit=10" \
  -H "Authorization: Bearer $AT" \
  -H "X-Device-Hash: $DH" -H "X-Device-Type: Web" \
  -H "X-Device-Location: India" -H "X-Request-ID: $(uuidgen)"
```

The broker rolls the chain in the background a couple of minutes before
expiry, so consumers always see a fresh token when they ask.

## Turning on integration mode (mode 2)

Integration mode adds a small SQLite database, a periodic job, a handful
of admin endpoints, and a web UI for reviewing what the classifier
proposes. It's gated behind one feature flag, so the broker keeps working
exactly as before unless you opt in.

### Prerequisites

- A reachable firefly-iii instance.
- A firefly-iii Personal Access Token (Profile → OAuth → Personal Access
  Tokens). The classifier needs *read* access to mirror your transaction
  history; the pusher needs *write* access to create new ones.
- (Optional but recommended) A [Google AI Studio](https://aistudio.google.com)
  API key. Without it, the classifier still works for transactions you've
  seen before — but the smartest tier (the one that handles new merchants,
  rare cards, transfers between your own accounts) is off.

### Configure

Add the integration env vars to your values. Two secrets need plumbing
(`firefly-pat` and `gemini-api-key`); add them to the same Secret you
created above, or to a new one referenced via `extraEnv` `valueFrom`.

```yaml
# values.yaml
extraEnv:
  - name: TEXAS_FOLDEM_INTEGRATION_ENABLED
    value: "true"
  - name: TEXAS_FOLDEM_FIREFLY_BASE
    value: "http://firefly.<your-namespace>.svc.cluster.local:8080"
  - name: TEXAS_FOLDEM_FIREFLY_PAT
    valueFrom: { secretKeyRef: { name: tfe, key: firefly-pat } }
  - name: TEXAS_FOLDEM_GEMINI_API_KEY
    valueFrom: { secretKeyRef: { name: tfe, key: gemini-api-key } }

  # Optional: turn the periodic job on. Default 0 (off) so you can drive
  # the pipeline manually from the admin endpoints first.
  - name: TEXAS_FOLDEM_PERIODIC_SYNC_EVERY
    value: "0"
  - name: TEXAS_FOLDEM_PERIODIC_SYNC_LIMIT
    value: "50"

  # Optional: emergency kill-switch. Flip to "true" to halt all writes
  # to firefly without redeploying. Reads (mirror, classify, preview)
  # keep working.
  - name: TEXAS_FOLDEM_FIREFLY_READONLY
    value: "false"
```

`helm upgrade` and you're in.

### One-time setup, in order

These are admin-key endpoints (`Authorization: Bearer $ADMIN`). After
the initial run, the periodic job (or repeated invocations of these
endpoints) keeps everything fresh.

```bash
# 1. Mirror firefly's transaction history into the local SQLite. This is
#    the source of truth the classifier learns from.
curl -X POST -H "Authorization: Bearer $ADMIN" \
  http://localhost:8080/admin/firefly/sync

# 2. Mirror your fold-side asset registry (credit cards + bank accounts).
#    Cheap; fold has at most a handful of these per user. Lets the
#    classifier turn fold's opaque per-transaction account_id into a
#    real card name like "HDFC Tata Neu Plus ****8943".
curl -X POST -H "Authorization: Bearer $ADMIN" \
  http://localhost:8080/admin/fold/accounts/sync

# 3. Pull recent fold transactions into the staging table.
curl -X POST -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/admin/fold/sync?limit=50"

# 4. Classify the staged rows.
curl -X POST -H "Authorization: Bearer $ADMIN" \
  http://localhost:8080/admin/classify
```

After step 4, every staged row is in one of three terminal states:

- **ready_to_push** — the classifier was confident enough to commit on
  its own. You can either trust it and push automatically, or eyeball
  it in the UI first.
- **needs_review** — the classifier wasn't sure. Open the UI, edit if
  needed, click *save*. The row moves to ready_to_push.
- **skipped** — you told the UI you don't want this in firefly.

## How the classifier thinks

A four-tier ladder, fastest to most expensive. Each row tries the tiers
in order and stops at the first one with enough confidence.

1. **Look it up.** "I've already seen this merchant 50 times in your
   firefly history; you always file it under category X with destination
   Y from card Z. Done." — the cheap path; no AI needed.
2. **Vote on it.** If the merchant is unfamiliar, search firefly-side
   transactions whose narration shares words with this fold one and let
   the top-scoring matches vote on category/destination.
3. **Reason about it.** If the deterministic tiers can't agree, hand the
   row to the LLM with a tightly-scoped prompt: the verbatim fold payload,
   your full firefly account/category/budget/tag inventory, the
   resolved fold-side card/bank that paid, and the deterministic tiers'
   hints. Ask for a structured JSON answer back, then guard against
   hallucination by rejecting any id that wasn't on the menu.
4. **Punt to the human.** Mark the row `needs_review` and surface it in
   the UI.

Two LLM details worth knowing because they shape the experience:

- The LLM picks the firefly **transaction type** explicitly —
  `withdrawal`, `deposit`, or `transfer`. The "transfer" case (fold sees
  it as outgoing, but both endpoints are your own assets — e.g. savings
  → brokerage) was historically the hardest; the LLM gets it because
  it sees both your full asset list and the resolved fold account name.
- The LLM picks the **source account** by name-matching the resolved
  fold account against your firefly assets. Same provider + same last-four
  digits is treated as a near-certain link, so the LLM doesn't fall
  back to "the most popular card" when it sees an unfamiliar merchant.

## Pushing to firefly

Each transaction gets pushed individually so it's easy to audit:

```bash
# Preview — shows the firefly POST body that WOULD be sent.
curl -X POST -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/admin/push/<fold_uuid>"

# Confirm — checks for an existing firefly transaction with the same
# external_id (so retries are safe), then POSTs to firefly.
curl -X POST -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/admin/push/<fold_uuid>?confirm=true"
```

Every push is idempotent on `external_id=<fold_uuid>` (firefly's own
deduplication mechanism) and appended to a local audit log. A successful
push also reinforces the merchant lookup table — so the next time a
similar transaction shows up, tier 1 catches it on its own.

## The review UI

`/admin/ui/` is a small server-rendered web UI for working through
`needs_review` rows. The layout is deliberately keyboard-friendly:

- **Filters** — switch between pending / needs-review / ready-to-push /
  pushed.
- **Edit by name, not by id** — every account, category, and budget
  field is a name autocomplete backed by your real firefly history.
  Tags work the same way, plus a row of clickable suggested tags
  derived from your past transactions for that merchant.
- **Local timestamps** — fold and firefly both store UTC; the UI
  converts to your device's timezone client-side.
- **Push, skip, save edits** — all three are one click away.

When the cluster is fronted by a forward-auth proxy (e.g. tinyauth,
oauth2-proxy, Authelia), set `TEXAS_FOLDEM_UI_COOKIE_AUTH=false` and let
the proxy handle auth. For local dev (port-forward to your laptop), set
it to `true` and visit `/admin/ui/login?key=<admin-key>` first; the UI
sets a cookie and you can ignore auth from then on.

## Going hands-off (periodic sync)

Once you trust the pipeline, set `TEXAS_FOLDEM_PERIODIC_SYNC_EVERY=1h`
(or whatever cadence you like — the value is a Go duration string, so
`30m`, `6h`, `24h` all work). The broker will then, on every tick:

1. Mirror the latest firefly transactions (catches anything you typed
   in by hand or on mobile).
2. Stage the latest fold transactions.
3. Run the classifier on every new row.

It does **not** automatically push to firefly. That stays a deliberate
human action — either via the UI or `/admin/push/{fold_uuid}`.

## Re-classifying after a model upgrade

When you ship a classifier improvement, you usually want past auto-
classified rows re-evaluated. Three scopes:

```bash
curl -X POST -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/admin/classify?scope=pending"   # default — only new rows
curl -X POST -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/admin/classify?scope=review"    # also re-do needs_review
curl -X POST -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/admin/classify?scope=all"       # also re-do ready_to_push
```

Anything you've manually edited in the UI is treated as ground truth
and never overwritten — the re-classify operates only on rows where
you haven't touched the confirmed fields.

## API reference

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET`  | `/livez` | none | liveness probe; always 200 while running |
| `GET`  | `/health` | none | richer status JSON; 503 until seeded |
| `GET`  | `/token` | broker | hand out a usable fold access token |
| `POST` | `/init`  | admin  | seed / re-seed the refresh chain |
| `POST` | `/admin/firefly/sync` | admin | mirror firefly history + rebuild merchant lookup |
| `POST` | `/admin/fold/accounts/sync` | admin | mirror your fold cards + bank accounts |
| `POST` | `/admin/fold/sync?limit=N` | admin | stage the most recent N fold transactions |
| `POST` | `/admin/classify?scope=...` | admin | run the classifier (`pending`/`review`/`all`) |
| `POST` | `/admin/push/{fold_uuid}` | admin | preview (default) or confirm (`?confirm=true`) a push to firefly |
| `*`    | `/admin/ui/*` | per-deploy | review UI |

## Reliability summary

- **Atomic state writes** — `tmpfile + fsync + rename`. A power cut
  cannot corrupt the refresh chain.
- **Refresh coalescing** — N concurrent `/token` calls produce one
  upstream refresh, not N.
- **Validate-before-persist on `/init`** — a bad seed never overwrites
  good state.
- **Rejection tombstone** — once fold revokes the chain, the broker
  returns `410 Gone` and stops trying until `/init` provides a fresh
  seed. No bombardment.
- **Rotate-before-return** — a new refresh token is durably persisted
  before the new access token is handed out.
- **Read-only kill-switch on the firefly side** — flip
  `TEXAS_FOLDEM_FIREFLY_READONLY` to `true` to halt every push without
  redeploying. Reads keep working, so the classifier and UI remain
  functional.
- **Idempotent everywhere** — every fold sync, firefly mirror, push,
  and classify can be re-run safely.

## Configuration reference

All knobs are env vars prefixed `TEXAS_FOLDEM_`. The chart's `extraEnv`
is the place to add the integration-mode ones; the broker-mode ones
have sensible defaults.

| Env | Default | What it does |
|---|---|---|
| `BROKER_KEY` | (required) | bearer for `/token` |
| `ADMIN_KEY`  | (required) | bearer for `/init` and every `/admin/*` |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `STATE_PATH` | `~/.texas-fold-em/state.json` | refresh-chain on disk |
| `API_BASE` | `https://api.fold.money/api` | fold base URL |
| `REFRESH_LEAD` | `2m` | refresh access token this long before expiry |
| `KEEPWARM_EVERY` | `1m` | background keepwarm cadence (0 to disable) |
| `HTTP_TIMEOUT` | `15s` | upstream call timeout |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `INTEGRATION_ENABLED` | `false` | turn on integration mode |
| `STAGING_DB_PATH` | `~/.texas-fold-em/staging.db` | classification SQLite |
| `FIREFLY_BASE` | `http://firefly.apps.svc.cluster.local:8080` | firefly base URL |
| `FIREFLY_PAT` | (required when integration on) | firefly Personal Access Token |
| `GEMINI_API_KEY` | (optional) | Google AI Studio key for the LLM tier |
| `GEMINI_MODEL` | `gemini-3.1-flash-lite` | model name |
| `FIREFLY_READONLY` | `false` | kill-switch for all firefly writes |
| `UI_COOKIE_AUTH` | `false` | gate the UI behind its own cookie (otherwise relies on upstream proxy auth) |
| `PERIODIC_SYNC_EVERY` | `0` | run the cron pipeline this often (0 = off) |
| `PERIODIC_SYNC_LIMIT` | `50` | rows per fold sync tick |

## Develop

```bash
make test         # go test -race ./...
make build        # native binary
```

The integration's SQL migrations are embedded into the binary, so
`make build` + a fresh data directory is enough to spin up against a
real firefly + fold for local testing.

## License

Personal project, do whatever you want.
