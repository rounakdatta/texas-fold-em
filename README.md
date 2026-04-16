# texas-fold-em

A long-running Go broker that holds a [fold.money](https://fold.money) refresh
token and issues short-lived access tokens to consumers on demand. Runs as a
single-replica Deployment in Kubernetes.

Reverse-engineering notes on the underlying Fold API live in [`fold.md`](./fold.md) — the full endpoint catalog, response shapes, auth flow, known error codes, and operational guidance for running the broker.

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/init` | admin key | seed / re-seed the refresh chain |
| `GET` | `/token` | broker key | return a usable access token |
| `GET` | `/livez` | none | liveness probe (always `200` while running) |
| `GET` | `/health` | none | rich status JSON; `503` until seeded |

## Install

```bash
helm install tfe charts/texas-fold-em -n fold --create-namespace \
  --set image.repository=ghcr.io/you/texas-fold-em \
  --set image.tag=v1.0.0 \
  --set secret.brokerKey="$(openssl rand -base64 32)" \
  --set secret.adminKey="$(openssl rand -base64 32)"
```

Or bring your own Secret: `--set secret.create=false --set secret.existingSecret=<name>`.

## Bootstrap (one-time, and after a `410`)

Log in to [fold.money](https://fold.money), copy `refresh_token` and
`device_hash` from `localStorage`, then:

```bash
kubectl -n fold port-forward svc/tfe 8080:8080 &
ADMIN=$(kubectl -n fold get secret tfe -o jsonpath='{.data.admin-key}' | base64 -d)

curl -sS -X POST http://localhost:8080/init \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"eyJ…","device_hash":"d051a97c-…"}'
```

## Consume

```bash
curl -sS -H "Authorization: Bearer $BROKER_KEY" \
  http://tfe.fold.svc.cluster.local:8080/token
# { "access_token": "...", "device_hash": "...", "user_uuid": "...", "expires_at": "..." }
```

From there, hit Fold's API directly with the returned access token:

```bash
curl "https://api.fold.money/api/v3/users/$UID/transactions?limit=10" \
  -H "Authorization: Bearer $AT" \
  -H "X-Device-Hash: $DH" -H "X-Device-Type: Web" \
  -H "X-Device-Location: India" -H "X-Request-ID: $(uuidgen)"
```

## Reliability summary

- Atomic state writes (`tmpfile + fsync + rename`) — power cut cannot corrupt.
- Refresh coalescing — N concurrent `/token` calls → 1 upstream refresh.
- Validate-before-persist on `/init` — a bad seed never overwrites good state.
- Rejection tombstone — after Fold revokes the chain, the broker refuses to
  retry until `/init` provides a fresh seed. No bombardment.
- Rotate-before-return — a new refresh token is persisted before the new
  access token is handed out.

## Develop

```bash
make test         # go test -race
make build        # native binary
```

## License

Personal project, do whatever you want.
