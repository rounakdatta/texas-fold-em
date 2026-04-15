# fold.money — reverse-engineering notes

Captured on 2026-04-15 by inspecting the live web app at `https://fold.money/`
via Chrome DevTools Protocol, intercepting XHR/fetch from within the authed
session, and reading the Next.js JS bundle (`pages/_app-7f8dfad252a5a595.js`).

Goal of this doc: everything needed to write a non-browser client that can
pull a user's own Fold data on a schedule.

---

## 1. Infrastructure & domains

| Role | Host | Notes |
|---|---|---|
| Marketing site | `https://fold.money/` | Next.js, static, behind Cloudflare (as CDN, not WAF) |
| App SPA | `https://fold.money/` (same origin, authed routes) | Next.js pages: `/`, `/transactions`, `/credit-cards`, `/banks`, `/login`, `/ca`, `/auth/callback` |
| JSON API | `https://api.fold.money/api/` | Go server, also behind Cloudflare |
| WebSocket | `wss://api.fold.money/api/` | Only used for the web-login QR flow |
| Static icons | `https://cdn.fold.money/icons/*.svg`, `https://cdn.fold.money/banks/*.png`, `https://cdn.fold.money/cc/*.png` | CDN, no auth |

Cloudflare response headers are present (`server: cloudflare`, `cf-ray`, `cf-cache-status`, `report-to`, `nel`) but **no `cf-mitigated` / Turnstile / JS challenge was ever issued** against plain `curl`. The site is using Cloudflare as a CDN, not as a bot gate. No fingerprinting beacons (Akamai Bot Manager, DataDome, PerimeterX) observed.

Backend product name appears to be **"marble"** (see JWT `iss: marble-server`, `aud: marble-client`).

---

## 2. Request envelope & headers

### Required request headers (server enforces these **before** auth is checked)

```
X-Request-ID:      <fresh UUIDv4 per request>
X-Device-Hash:     <stable UUIDv4; one per install>
X-Device-Type:     Web          # also seen: iOS, Android on mobile builds
X-Device-Location: India        # seems to be the user's selected country
Accept:            application/json, text/plain, */*
Content-Type:      application/json   # only on POST/PUT/PATCH
Authorization:     Bearer <access_token>   # after login
```

If you omit the device headers you get `422` with body:

```json
{"error":{"code":2007,"message":"missing device headers","how_to_fix":"Add device headers to request"}}
```

If you omit `Authorization` (on an auth'd endpoint) you get `401`:

```json
{"error":{"code":2000,"message":"Authorization token is missing","how_to_fix":"Add Authorization header of the form 'Bearer <token>' …"}}
```

`X-Device-Hash` is generated client-side and written to localStorage; the server keeps no list of valid hashes (I verified by calling endpoints with an all-zero UUID and refresh worked). For automation: generate one UUID, reuse it forever per "virtual device". **Do not re-roll it every run** — the server tracks sessions by it, and refresh-token rotation is device-scoped.

### Response envelope

Every JSON response (success or error) is:

```json
{
  "meta":  { "request_id": "<echoed>", "timestamp": "<ISO8601>", "uri": "<path>" },
  "data":  <payload> | null,
  "error": { "code": <int>, "message": "...", "how_to_fix": "...", "data": null } | null
}
```

The `how_to_fix` field is genuinely populated — very helpful to drive a client from.

### CORS

```
Access-Control-Allow-Origin:      https://fold.money
Access-Control-Allow-Credentials: true
Access-Control-Allow-Headers:     authorization,x-device-hash,…
Access-Control-Allow-Methods:     GET (echoed per preflight)
```

Not relevant for a non-browser client (CORS is browser-enforced only); just noting that `Origin` can be omitted.

---

## 3. Authentication

### Token shape

Both access and refresh tokens are **JWT (HS256)**. Example (redacted) payload of the refresh token:

```json
{
  "iss":  "marble-server",
  "sub":  "<user uuid>",
  "aud":  ["marble-client"],
  "exp":  1784037336,
  "nbf":  1776261336,
  "iat":  1776261336,
  "jti":  "<rotation id>",
  "type": "refresh-token"        // access token has type: "access-token"
}
```

- Access token TTL: **~15 min** (observed `iat→exp` of 900 seconds)
- Refresh token TTL: **~90 days**
- Tokens are HMAC-signed with a server secret — you cannot forge them; you must obtain them through a login flow.

### Token storage (web)

All in `localStorage`, no cookies:

| Key | Value |
|---|---|
| `refresh_token` | the JWT refresh token |
| `expires_at` | ISO timestamp of access-token expiry (e.g. `2026-04-15T14:10:36.324616119Z`) |
| `device_hash` | stable UUID for this browser |
| (access token) | held in-memory only (axios default header), **not persisted** |
| `onboarding`, `userPref`, `txn-onboarding` | UI state, not security-relevant |

Note: the app also stores a `USER_ID` key per the logout code (`localStorage.removeItem(o.Bp.USER_ID)`), though it wasn't present in this session. Probably set during onboarding.

### Refresh flow — `POST /api/v1/auth/tokens/refresh`

Request:

```http
POST /api/v1/auth/tokens/refresh HTTP/2
Host: api.fold.money
Content-Type: application/json
X-Request-ID: <uuid>
X-Device-Hash: <stable uuid>
X-Device-Type: Web
X-Device-Location: India

{"refresh_token":"<current refresh JWT>"}
```

Response `data`:

```json
{
  "token_type":    "Bearer",
  "access_token":  "eyJ…",         // ~380 chars
  "refresh_token": "eyJ…",         // ~363 chars — NEW one, rotate
  "expires_at":    "2026-04-15T14:12:54.989237207Z"
}
```

> ⚠️ **Refresh-token rotation is on.** Each successful refresh invalidates the
> previous refresh token and issues a new one. Your client **must** persist the
> new `refresh_token` (and the new `expires_at`) after every refresh, or the
> next run will be logged out.
>
> I accidentally triggered this during recon by refreshing the token outside of
> the app's lifecycle, which de-synced the web session.

### Login flows (3 options)

The app's auth endpoint constants (extracted from the JS bundle):

```js
sendOTP:         "/v1/auth/otp"
verifyOTP:       "/v1/auth/otp/verify"
refreshToken:    "/v1/auth/tokens/refresh"
logout:          "/v1/auth/logout"
logoutAll:       "/v1/auth/logout/all"
socialLogin:     "/v1/auth/social/web"
generateQrCode:  "/v1/auth/web/generate_qr_code"   // WebSocket
qrCodeLogin:     "/v1/auth/web/qr_code_login"
```

#### (a) Phone OTP — simplest for scripting

```
POST /api/v1/auth/otp          { "phone": "+91XXXXXXXXXX", "channel": "SMS" }
POST /api/v1/auth/otp/verify   { "phone": "+91XXXXXXXXXX", "otp": "123456" }
```

- `channel` is one of `SMS`, `CALL`, `WHATSAPP` (from `OTP_CHANNEL` constant).
- `verify` returns the same payload shape as the refresh endpoint (`token_type`, `access_token`, `refresh_token`, `expires_at`).
- Phone format observed: E.164 with `+91` prefix for India.

This is probably what you want for bootstrapping: run once interactively to get a refresh token, then the GitHub Action only ever uses `/tokens/refresh` thereafter. (If the refresh token expires after 90 days, you'll need to re-OTP.)

#### (b) Social — Google / Apple OAuth token exchange

```
POST /api/v1/auth/social/web   { "account": "<google or apple OAuth id token>" }
```

Not useful for automation (needs a browser OAuth round trip).

#### (c) Web QR — mobile-app-to-web handoff (how you probably logged into fold.money)

- Web client opens a **WebSocket** to `wss://api.fold.money/api/v1/auth/web/generate_qr_code`.
- Server pushes a short `login_code` down the socket; web renders it as a QR.
- User approves in the authed mobile app.
- Web then `POST /api/v1/auth/web/qr_code_login` with `{"login_code": "…"}` and receives the token bundle.

The JS call is:

```js
b = async e => {
  const { data } = await axios.post("/v1/auth/web/qr_code_login", { login_code: e });
  storeTokens(data.data);            // s.vD in the bundle
  return data;
}
```

Not useful for headless automation (still needs a human with the mobile app), but good to understand so you don't confuse this endpoint with OTP login.

### Logout

```
POST /api/v1/auth/logout        # current session
POST /api/v1/auth/logout/all    # all sessions — invalidates every refresh token
```

If you call `logout/all` you'll kill the automation too. Avoid calling either from a script.

---

## 4. Full endpoint catalog

Extracted verbatim from the bundle's endpoint-constants block. `{{userId}}` is the authed user's UUID (`sub` claim in the JWT; also returned at `/users/me`).

### Auth
| Method | Path | Body |
|---|---|---|
| POST | `/api/v1/auth/otp` | `{phone, channel}` |
| POST | `/api/v1/auth/otp/verify` | `{phone, otp}` |
| POST | `/api/v1/auth/tokens/refresh` | `{refresh_token}` |
| POST | `/api/v1/auth/logout` | — |
| POST | `/api/v1/auth/logout/all` | — |
| POST | `/api/v1/auth/social/web` | `{account}` |
| WS   | `/api/v1/auth/web/generate_qr_code` | — |
| POST | `/api/v1/auth/web/qr_code_login` | `{login_code}` |

### User & app state
| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/app_state/web` | Thin: `{total_balance: [<hash>]}` |
| GET | `/api/v2/users/me` | Rich user+settings+plan+onboarding blob |
| GET | `/api/v2/users/{{userId}}` | Same as `/me` if you pass your own id |
| GET | `/api/v1/users/{{userId}}` | v1, probably leaner |
| GET | `/api/v1/users/{{userId}}/bank_accounts` | |
| POST | `/api/v1/users/{{userId}}/bank_statement` | Trigger statement fetch |
| GET | `/api/v1/users/{{userId}}/graphs/total_balance` | |
| POST | `/api/v1/users/{{userId}}/link/{{account}}/web` | Link social account |
| POST | `/api/v1/users/{{userId}}/populate/random` | Dev / demo only |
| PUT  | `/api/v1/users/{{userId}}/transactions/{{transactionId}}/merchant` | Detach merchant |
| GET | `/api/v2/users/{{userId}}/recent_merchants` | |

### Account Aggregator (India's AA framework — Finvu/Setu under the hood)
| Method | Path |
|---|---|
| GET    | `/api/v1/aa` |
| POST   | `/api/v1/aa/?service=<setu|finvu>` (create AA consent) |
| DELETE | `/api/v1/aa` |
| GET    | `/api/v1/aa/data` — all linked accounts + their pulled transactions |
| POST   | `/api/v1/aa/data/refresh` — force a refresh |
| GET    | `/api/v1/aa/fips` — 45 supported financial info providers |
| GET    | `/api/v1/aa/health/fip/{{uuid}}` — FIP liveness |

### Transactions (core data)
| Method | Path | Notes |
|---|---|---|
| GET   | `/api/v3/users/{{userId}}/transactions` | List with filters + `after` cursor |
| GET   | `/api/v3/users/{{userId}}/transactions/counts` | Transaction counts (group-by etc.) |
| GET   | `/api/v3/users/{{userId}}/transactions/{{transactionId}}` | One txn |
| GET   | `/api/v3/users/{{userId}}/transactions/export` | Returns a **blob** (CSV/XLSX) |
| POST  | `/api/v3/users/{{userId}}/transactions/export/list` | Export a specific list of ids, blob |
| PATCH | `/api/v3/users/{{userId}}/transactions/{{transactionId}}` | Update a txn |
| PATCH | `/api/v3/users/{{userId}}/transactions` | Batch update: body `{transaction_ids: [...], ...fields}` |
| GET   | `/api/v3/users/{{userId}}/transactions/{{transactionId}}/receipts/download-urls` | Pre-signed receipt URLs |

Query params I observed on `/transactions` (partial — only confirmed via live traffic):

- `limit=30` (int)
- `count_by=month`
- `include[]=count_by_totals`
- `after=<opaque cursor>` — pagination; cursor is base64-looking, starts with `REVTQzo6OnRpbWU6Ojoy…` which decodes to `DESC:::time:::2…` (sort-direction + field + value).
- `account_id` (array; also accepted on cashflow/spending endpoints)
- `category_id` (UUID; used on detailed spending endpoint)

### Credit cards, graphs, summaries
| Method | Path | Notes |
|---|---|---|
| GET | `/api/v3/users/{{userId}}/credit-cards` | Cards + cycle info + usage-by-bank groups |
| GET | `/api/v3/users/{{userId}}/graphs/cashflow` | Params: `account_id`, `interval` |
| GET | `/api/v3/users/{{userId}}/spending-summary` | Per-category spend |
| GET | `/api/v3/users/{{userId}}/spending-summary/detailed` | Drill-down by `category_id` |
| POST | `/api/v3/users/{{userId}}/f1pro-chat` | Fold's AI assistant ("f1 pro") |

### Merchants & categories
| Method | Path |
|---|---|
| GET  | `/api/v1/merchants` |
| POST | `/api/v1/merchants/custom` (body `{name}`) |
| GET  | `/api/v1/merchants/generic` |
| GET  | `/api/v2/categories/search?type=INCOMING` (14 categories) |
| GET  | `/api/v2/categories/search?type=OUTGOING` (33 categories) |

### CA (Chartered Accountant) portal
| Method | Path |
|---|---|
| GET  | `/api/v3/ca/me` |
| GET  | `/api/v3/ca/{{caId}}/clients` |
| GET  | `/api/v3/ca/{{caId}}/clients/{{consentId}}/download` |
| POST | `/api/v3/ca/{{caId}}/invite-client` |

### Admin (gated by `role`/`is_internal_user`)
`/api/v1/admin/whitelist`, `/internal`, `/delete_user`, `/merchants`, `/fip`, `/fips`, `/fip/total_connected`, `/fip/{{uuid}}/test_users`, `/cc/recreate_emails_processor`, `/system-configs/aa-data-pulls`, `/system-configs/server-maintenance`. Will 403 for regular users.

### Email / misc
- `/api/v1/emails/unsubscribe` (public, one-click unsub)
- `/api/health` (served off `fold.money`, not `api.fold.money` — Next.js route)

---

## 5. Response shapes (field-level)

Captured live, with personal values redacted. Field names preserved so you can map into your own schema.

### `GET /api/v2/users/me` → `data.user`
```
uuid, first_name, middle_name, last_name, email, email_verified,
phone, phone_verified, google_linked, apple_linked,
role, is_internal_user, beta_access, web_beta_access, cc_enabled,
timezone, created_at, updated_at, username, profile_pic
```

### `GET /api/v2/users/me` → `data.settings.widgets`
Dashboard widget config, including per-widget `bank_accounts[]`, `credit_card_accounts[]`, `manual_accounts[]` filter lists:
```
bank_balance, cash_flow, bank_account, other_bank_account,
spending_summary, stocks, credit_cards, bills, net_worth
```

### `GET /api/v2/users/me` → `data.plan`
```
tier, is_in_trial_period, tier_start, tier_end, believer_rank,
show_upgrade,
feature_access: {
  radar, secondary_account_linking_bank, change_auto_refresh_time,
  export_transactions, connect_amex, web_app, show_ads
},
feature_limits: { allowed_app_themes[], disallowed_tags[], max_secondary_account_bank, max_space_members },
subscription: { display_name, is_refundable, platform, is_in_grace_period }
```

### `GET /api/v1/aa/data` → `data.accounts[]`
```
uuid, holder_name, masked_account_number ("****7037"), account_number,
account_number_verified, type ("deposit" | …), last_fetched_at,
currency, current_balance, ifsc_code, nickname, track ("ACTIVELY" | …),
swift_code, first_pull_completed, is_closed,
financial_information_provider: {
  uuid, name ("Axis Bank"), fip_id ("AXIS001"),
  is_valid_time, invalid_txn_id, logo_url
},
transactions: [ { uuid, amount, current_balance, txn_timestamp, txn_date,
                  is_valid_time, mode, type, narration,
                  category, category_icon, merchant, kind, account_id,
                  created_at, updated_at } ]
```

### `GET /api/v1/aa/fips` → `data[]` (45 entries)
```
uuid, fip_id ("SLICE_FIP_PROD"), name, logo, status ("ACTIVE"),
is_valid_time, time_valid_after, invalid_txn_id,
rating, review, otp_length, twitter_handle,
fi_types: ["DEPOSIT" | "MUTUAL_FUNDS" | …],
success_rate: { consent, discovery, account_linking, account_delink,
                fip_otp_request, data_notification, data_pull_request,
                data_pull_response, consent_notification
                /* each is {current, threshold} */ },
supported_providers: ["finvu" | "setu"]
```

### `GET /api/v3/users/{uid}/credit-cards` → `data.accounts[]`
```
uuid, holder_name, holder_email, holder_address, last_four_digits ("1743"),
email_last_synced_at, indicator_icons: { light, dark, black },
should_save_pdf, fip_id, statement_last_synced_at,
payment_network: { logo, name ("Visa"), uuid },
credit_card: { name ("Scapia"), uuid, provider: { icon, name, uuid } },
is_closed, card_last_four_digits (int), nickname, oauth_token,
cycle: {
  uuid, account_id, statement_date, credit_limit, cash_limit,
  available_credit_limit, available_cash_limit,
  minimum_amount_due, total_amount_due,
  amount_spent_this_cycle, amount_with_previous_outstanding,
  payment_due_date, percentage_used, percentage_used_this_cycle,
  calculated_payment_due, paid,
  reconciled_at, statement_arrived, statement_arrived_at,
  is_onboarding_cycle, bbps_reconciled_at, is_manually_reconciled,
  total_rewards_earned, created_at, updated_at
},
previous_cycle: { … same shape },
account_closed_at
```

Also `data.usage[]` with per-FIP grouping `{fip_id, groups:[{amount_spent_this_cycle, credit_limit, account_ids[]}]}`.

### `GET /api/v3/users/{uid}/transactions` → `data`
```
transactions[]:
  uuid, amount, source_amount, currency, source_currency,
  txn_timestamp, mode ("CARD"|"UPI"|"OTHERS"|…),
  type ("INCOMING"|"OUTGOING"),
  narration, summary,
  category: { id, subcategory_id },
  merchant: { id, name, type, logo, address },
  account_id, kind ("NORMAL"|…),
  financial_information_provider_id, notes,
  excluded_from_cash_flow, is_bookmarked, is_hidden,
  transaction_id, reference, extracted_time, via, account_in,
  refund: { status, notify, received_on },
  receipts[], group_ids,
  source ("CREDIT_CARD"|"BANK"|…),
  is_cc_manual_or_bank_linked, linked_cc_account_id_for_bill,
  linked_cc_transaction_id, user_manual_added,
  split_type, remaining_amount, parent_transaction_id,
  is_possible_duplicate, refund_group_id,
  excluded_from_wallet, remaining_refund_amount,
  global_intelligent_tags,
  allowed_actions: { update_amount, update_time, update_type, update_cash_flow }

counts[]: monthly / periodic counts (when count_by=month)
total:    int
after:    opaque cursor string (~108 chars, base64 of "DESC:::time:::…")
parent_transactions: null | [...]
search_summary: null | {…}
```

### `GET /api/v2/categories/search` → `data.categories[]`
```
uuid, name ("Food & Drinks"), subtext, type: ["INCOMING"|"OUTGOING"],
icons: [{uuid, display_name, name ("knife-fork"), icon (URL), no_icon}],
single_icon, exclude_from_spending_summary
```

---

## 6. Known error codes

Collected from live responses:

| code | HTTP | message | how_to_fix |
|---|---|---|---|
| 1000 | 400 | Request body must not be empty | Please make sure that the request body is not empty. |
| 2000 | 401 | Authorization token is missing | Add Authorization header of the form 'Bearer <token>' … |
| 2007 | 422 | missing device headers | Add device headers to request |

(Bundle also references `STATUS.REFRESH_TOKEN_ERROR` and `STATUS.PAID_TIER_FEATURE_NOT_ALLOWED_ERROR` — worth handling.)

---

## 7. Minimum viable client recipe

Pseudocode for a daily scrape job (what a GitHub Action would do), **not** implemented here per your instruction:

```
device_hash     = <persisted once, UUIDv4>
refresh_token   = <persisted from last run or from OTP bootstrap>

H = {
  "X-Device-Hash":     device_hash,
  "X-Device-Type":     "Web",
  "X-Device-Location": "India",
  "X-Request-ID":      new_uuid(),
  "Accept":            "application/json, text/plain, */*",
}

# 1. Refresh
r = POST https://api.fold.money/api/v1/auth/tokens/refresh
      headers = H | {"Content-Type":"application/json"}
      json    = {"refresh_token": refresh_token}
access_token   = r.data.access_token
refresh_token  = r.data.refresh_token      # ROTATE — persist this
expires_at     = r.data.expires_at

A = H | {"Authorization": "Bearer " + access_token, "X-Request-ID": new_uuid()}

# 2. Whoever you are
me     = GET /api/v2/users/me             headers=A ; uid = me.data.user.uuid

# 3. Bank accounts + their latest txns
aa     = GET /api/v1/aa/data              headers=A

# 4. Credit cards
cc     = GET /api/v3/users/{uid}/credit-cards headers=A

# 5. Transactions — walk the cursor
params = {"limit": 200}
while True:
    t = GET /api/v3/users/{uid}/transactions?<params>  headers=A
    yield t.data.transactions
    if not t.data.after: break
    params["after"] = t.data.after

# 6. Cash flow + spending summary
cf     = GET /api/v3/users/{uid}/graphs/cashflow        headers=A
ss     = GET /api/v3/users/{uid}/spending-summary       headers=A

# 7. Or just: server-side export blob
exp    = GET /api/v3/users/{uid}/transactions/export?from=…&to=… headers=A
```

Bootstrap: one-time interactive OTP run to obtain the first `refresh_token`,
then store it in an encrypted secret. The refresh token JWT has a 90-day TTL,
so the automation will need re-bootstrap roughly quarterly (or earlier if the
server revokes — watch for 401 with `REFRESH_TOKEN_ERROR`).

---

## 8. Observations on protections / antibot

- **Cloudflare** is in front of both `fold.money` and `api.fold.money`, but as a CDN/proxy only. No JS challenge, no Turnstile on API calls from `curl`.
- **No request signing / HMAC / nonce beyond `X-Request-ID`.** The request id is echoed in the response and looks informational (server-side tracing); it isn't validated against a nonce store.
- **No client secret / API key**; everything is Bearer-token.
- **`X-Device-Hash` is unvalidated** in the sense that any well-formed UUID is accepted. It's used to scope refresh-token rotation per device; keep it stable.
- **Refresh-token rotation is enforced** — see §3.
- **No captcha** on OTP send/verify endpoints (verified from JS; not probed to avoid nuisance SMS to the user).
- **Rate-limits** not probed. Fold very likely has per-phone and per-IP limits on `/auth/otp`; be polite.

---

## 9. Things still unknown / worth testing before writing a client

1. **Full filter param list on `/v3/users/{uid}/transactions`** — the JS passes an arbitrary `params` object; I only confirmed `limit`, `after`, `count_by`, `include[]`, `account_id`. The app UI has filters for date range, merchant, category, mode (card/UPI), amount range — those param names would come out of a live filter interaction that I didn't trigger.
2. **Export endpoint query params** — `GET /transactions/export` takes `params` and returns a blob. Expected keys similar to list endpoint + `format=csv|xlsx`, but not confirmed.
3. **Shape of OTP verify response** when the phone isn't yet registered (sign-up vs sign-in branching).
4. **WebSocket message schema** for `generate_qr_code` — what JSON the server pushes.
5. **Whether `/aa/data/refresh` is synchronous** or returns a job id to poll.
6. **Whether Fold returns a 429** before Cloudflare does; where the rate-limit boundary is.

All of these can be mapped in 10 minutes by clicking through the relevant UI with the fetch/XHR interceptor from §2 installed. Recipe:

```js
// paste in devtools or inject via CDP — capture into window.__cap
const origFetch = fetch; window.__cap = [];
fetch = async function (u, i) {
  const out = { url: typeof u === "string" ? u : u.url, method: (i||{}).method||"GET", headers: {...(i||{}).headers}, body: (i||{}).body };
  const r = await origFetch.apply(this, arguments);
  out.status = r.status;
  r.clone().text().then(t => out.response_preview = t.slice(0, 1000));
  window.__cap.push(out); return r;
};
// then use the app, then: copy(JSON.stringify(window.__cap, null, 2))
```

---

## 10. Concrete recipe: "fetch my last 10 transactions from an ephemeral machine"

This is the smallest end-to-end integration — a GitHub Action, a cron job on a laptop, a Lambda, whatever. It deliberately ignores everything not strictly required.

### Secrets to store (2)

| Name | Value | Rotates? |
|---|---|---|
| `FOLD_REFRESH_TOKEN` | the JWT at `localStorage.refresh_token` right after you log in | **yes — after every successful refresh**, the ephemeral machine must persist the new token back to the secret store, or the next run will 401 |
| `FOLD_DEVICE_HASH` | the UUID at `localStorage.device_hash`, or any UUIDv4 you make up and keep forever | no — stable identity for this "device" |

No other secrets. Specifically: no API key, no client secret, no user UUID (derived at runtime), no access token (minted at runtime), no cookies.

### Per-run flow

```
# 1. Derive user uuid from the refresh token (no network call)
jwt_payload = base64url_decode( refresh_token.split('.')[1] )
user_uuid   = json.loads(jwt_payload)["sub"]

# 2. Refresh → fresh access token + new refresh token
POST https://api.fold.money/api/v1/auth/tokens/refresh
  Headers:
    Content-Type:     application/json
    Accept:           application/json, text/plain, */*
    X-Device-Hash:    <FOLD_DEVICE_HASH>
    X-Device-Type:    Web
    X-Device-Location: India
    X-Request-ID:     <uuidv4()>
  Body:
    {"refresh_token": "<FOLD_REFRESH_TOKEN>"}

response.data = {
  "token_type":    "Bearer",
  "access_token":  "<A>",
  "refresh_token": "<R_new>",   ← CRITICAL: write back to FOLD_REFRESH_TOKEN
  "expires_at":    "…"
}

# 3. Persist the rotated refresh token BEFORE making data calls,
#    so if a data call fails you still have the latest good token.
secret_store.put("FOLD_REFRESH_TOKEN", response.data.refresh_token)

# 4. Fetch the last 10 transactions
GET https://api.fold.money/api/v3/users/{user_uuid}/transactions?limit=10
  Headers:
    Authorization:    Bearer <A>
    Accept:           application/json, text/plain, */*
    X-Device-Hash:    <FOLD_DEVICE_HASH>
    X-Device-Type:    Web
    X-Device-Location: India
    X-Request-ID:     <uuidv4()>
```

### Exactly which query params

| Param | Value | Needed? |
|---|---|---|
| `limit` | `10` | **yes** — that's the only thing required to bound the list |
| `count_by` | `month` | no — only affects `data.counts[]`, unused here |
| `include[]` | `count_by_totals` | no — extra summary data |
| `after` | cursor | no — only for pages 2+ |
| `account_id` | UUID | no — omit to get all accounts |

So the full request URL is literally just:

```
GET /api/v3/users/<user_uuid>/transactions?limit=10
```

### What comes back

```json
{
  "meta": { "request_id": "...", "timestamp": "...", "uri": "..." },
  "data": {
    "transactions": [ /* 10 items, newest first; shape documented in §5 */ ],
    "counts": [],
    "total": <int>,
    "after": "<cursor — ignore for a top-10 run>",
    "parent_transactions": null,
    "search_summary": null
  },
  "error": null
}
```

Pull the fields you care about (`txn_timestamp`, `amount`, `type`, `narration`, `merchant.name`, `category`, `account_id`, `mode`) into whatever sink you want (CSV, SQLite, Google Sheet, Slack message).

### GitHub Actions specifics

- Write the rotated refresh token back with `gh secret set FOLD_REFRESH_TOKEN --body "$NEW"` from within the job. Requires a PAT with `repo` scope (since `GITHUB_TOKEN` can't update secrets).
- Alternative: keep the refresh token in a commit encrypted with [`sops`](https://github.com/getsops/sops) / age, and the workflow decrypts, refreshes, re-encrypts, commits the new blob. Slightly more infra but survives without a long-lived PAT.
- Schedule: once a day is safe. The refresh token itself lasts ~90 days, so even if the daily job misses a run, you won't be logged out.

### Failure modes to guard against

| Condition | HTTP | What to do |
|---|---|---|
| `expires_at` on `FOLD_REFRESH_TOKEN` already past | 401, `code` in `REFRESH_TOKEN_ERROR` set | refresh flow has expired; re-bootstrap via OTP/QR manually |
| Missing or malformed device headers | 422, `code: 2007` | fix request (shouldn't happen in a tested client) |
| Access token drifted (clock skew) | 401, `code: 2000` | retry after re-refresh once; don't loop |
| Cloudflare 5xx | 5xx | exponential backoff, max 3 retries |

### Re-bootstrap procedure (when `FOLD_REFRESH_TOKEN` dies)

Roughly quarterly, or any time the action logs 401 with `REFRESH_TOKEN_ERROR`:

1. Log in to `https://fold.money/` in a real browser.
2. Open DevTools → Application → Local Storage → `https://fold.money`.
3. Copy `refresh_token` and (optionally) `device_hash`.
4. Update the two GitHub Actions secrets.
5. Leave the browser session alone — as long as you don't call `/tokens/refresh` from outside the app, it stays logged in.

---

## 11. Managing the refresh token across runs ("but the refresh token rotates!")

Fold has no fixed-forever API key — no PAT endpoint exists in the route map
(only OTP, social, QR, and refresh). Access tokens are 15 min. Refresh tokens
rotate on use. So any automation has to solve the same problem every OAuth2
integration solves: **the refresh token is the persistent credential; you
must read-modify-write it each run**.

That is not a broken design — it's the same pattern Google Drive / Gmail /
Dropbox / Strava / Spotify integrations all use. It also gives a nice property:
if the token leaks, the next scheduled run rotates it out of the attacker's
hands.

Four patterns, cheapest-first:

### Pattern A — Self-rotating GitHub Actions secret

One workflow, one PAT with `secrets:write` scope, rotate the secret from
inside the job. Smallest possible infra.

```yaml
name: fold-daily
on:
  schedule: [{ cron: "15 2 * * *" }]   # 07:45 IST
  workflow_dispatch:
jobs:
  pull:
    runs-on: ubuntu-latest
    env:
      GH_TOKEN:           ${{ secrets.GH_PAT_FOR_SECRET_WRITES }}
      FOLD_REFRESH_TOKEN: ${{ secrets.FOLD_REFRESH_TOKEN }}
      FOLD_DEVICE_HASH:   ${{ secrets.FOLD_DEVICE_HASH }}
    steps:
      - uses: actions/checkout@v4
      - run: |
          # refresh → capture new refresh_token → WRITE IT BACK FIRST, then do data calls
          resp=$(curl -fsS -X POST https://api.fold.money/api/v1/auth/tokens/refresh \
              -H "Content-Type: application/json" \
              -H "X-Device-Hash: $FOLD_DEVICE_HASH" \
              -H "X-Device-Type: Web" -H "X-Device-Location: India" \
              -H "X-Request-ID: $(uuidgen)" \
              -d "{\"refresh_token\":\"$FOLD_REFRESH_TOKEN\"}")
          new_rt=$(echo "$resp" | jq -er .data.refresh_token)
          gh secret set FOLD_REFRESH_TOKEN --body "$new_rt" --repo "$GITHUB_REPOSITORY"
          # then use $resp's access_token for data calls …
```

Caveats:
- Crash between refresh and `gh secret set` → refresh consumed, new one lost → must re-bootstrap. Rotate *before* data calls, not after.
- Requires a PAT (classic with `repo`, or fine-grained with `secrets:write`). Store it as `GH_PAT_FOR_SECRET_WRITES` — never the `GITHUB_TOKEN` (it can't write secrets).

### Pattern B — SOPS-encrypted blob in the repo

Refresh token lives encrypted in the repo (`fold_state.enc`). Workflow
decrypts → refreshes → re-encrypts → commits. No PAT needed.

```yaml
- uses: mozilla/sops-action@v1
- run: |
    sops -d fold_state.enc > /tmp/state.json
    ./scripts/fold-sync.sh /tmp/state.json   # writes back to /tmp/state.json
    sops -e /tmp/state.json > fold_state.enc
- run: |
    git config user.email bot@local
    git config user.name  fold-bot
    git add fold_state.enc
    git diff --cached --quiet || git commit -m "rotate fold token [skip ci]"
    git push
```

Downsides: one rotation-commit per run (365/year; noisy but versioned).
Upside: `git log` is an audit trail; you can roll back state if a run
corrupts the token.

### Pattern C — External secret store

Doppler / 1Password Connect / HashiCorp Vault / AWS SSM Parameter Store /
GCP Secret Manager — any of these with a read+write API key. Same
read-modify-write logic, just the secret lives outside GitHub.

Worth it if you already run one of these for other reasons. Otherwise it's
the same amount of state as Pattern A with more moving parts.

### Pattern D — Token broker (the "fixed credential" illusion)

The only way to make the **ephemeral worker itself** stateless: put the
rotation dance behind a tiny always-on service.

```
 [ ephemeral cron job ]
     │ fixed bearer: BROKER_KEY
     ▼
 [ broker: Cloudflare Worker + KV ]  ── owns FOLD_REFRESH_TOKEN
     │ refreshes when access < 5 min left                           returns { access_token, device_hash }
     ▼
 [ api.fold.money ]
```

Worker pseudocode (Cloudflare Workers + KV, ~30 lines):

```js
export default {
  async fetch(req, env) {
    if (req.headers.get("authorization") !== `Bearer ${env.BROKER_KEY}`)
      return new Response("nope", { status: 401 });

    let state = JSON.parse(await env.KV.get("fold_state"));  // {refresh_token, access_token, expires_at, device_hash}
    if (Date.parse(state.expires_at) - Date.now() < 5 * 60 * 1000) {
      const r = await fetch("https://api.fold.money/api/v1/auth/tokens/refresh", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Device-Hash": state.device_hash,
          "X-Device-Type": "Web",
          "X-Device-Location": "India",
          "X-Request-ID": crypto.randomUUID(),
        },
        body: JSON.stringify({ refresh_token: state.refresh_token }),
      }).then(r => r.json());
      state = { ...state, ...r.data };
      await env.KV.put("fold_state", JSON.stringify(state));
    }
    return Response.json({ access_token: state.access_token, device_hash: state.device_hash });
  }
};
```

The ephemeral machine just does:

```bash
resp=$(curl -fsS https://fold-broker.<you>.workers.dev \
       -H "Authorization: Bearer $BROKER_KEY")
access=$(jq -r .access_token <<<"$resp")
dev=$(jq -r .device_hash <<<"$resp")
curl "https://api.fold.money/api/v3/users/$UID/transactions?limit=10" \
   -H "Authorization: Bearer $access" \
   -H "X-Device-Hash: $dev" \
   -H "X-Device-Type: Web" -H "X-Device-Location: India" \
   -H "X-Request-ID: $(uuidgen)"
```

The ephemeral side now holds only `BROKER_KEY`, which you picked yourself and
never rotates. The state still exists (in the Worker's KV), you just stopped
pretending it doesn't.

Free tier of Cloudflare Workers + KV is way under the limits for personal
use. Fly.io `fly machines`, a $4/mo VPS, or a Lambda + DynamoDB work just as
well.

### Recommendation

Personal daily-sync → **Pattern A**. Least infra, and the "self-rotating
secret" pattern is a well-trodden path. Every Strava/Spotify/Notion GH Action
in the wild uses it.

If you want to spin up multiple ephemeral consumers (cron + Slack bot + an
LLM that asks your finances questions on demand) → **Pattern D**. Each
consumer holds a cheap broker key; only the broker knows the Fold refresh
chain. This is also the shape you'd build if you were productizing the
integration for more users later.

### Broker lifecycle: bootstrap, restart, disaster recovery

If you pick Pattern D, here's the operational manual.

#### First-ever boot — seeding the broker

The broker needs the same two values an ephemeral machine would:

```bash
# 1. Log in to https://fold.money in a normal browser.
# 2. DevTools → Application → Local Storage → https://fold.money
#      copy the current values of `refresh_token` and `device_hash`.
# 3. Seed KV out-of-band (NOT via an HTTP endpoint — see below):
wrangler kv:key put --binding=KV 'fold_state' '{
  "refresh_token": "eyJhbGciOiJIUzI1NiIs…",
  "device_hash":   "d051a97c-29e6-4da7-8d1d-1d9ca281395d"
}'

# 4. Set the broker's own fixed credential (any random string you pick):
wrangler secret put BROKER_KEY
```

Note that the seed JSON has no `access_token` / `expires_at`. On the first
real request, the broker sees those missing, runs the refresh flow, and
persists the rotated refresh_token + fresh access_token + expires_at back
to KV. No special "init" code path — same logic as any stale-token run.

#### Respawn scenarios (in order of likelihood)

| Scenario | Duration down | KV state | Action required |
|---|---|---|---|
| **(a)** Worker restart / code redeploy | seconds | intact | none — next call refreshes if needed |
| **(b)** Host outage longer than access-token TTL | minutes–hours (<15 min TTL on access, 90d on refresh) | intact | none — broker refreshes on next call |
| **(c)** Outage longer than refresh-token TTL | >90 days | intact but refresh token is server-side-expired | re-bootstrap: re-run the seeding steps above |
| **(d)** KV wiped (accidental delete, account lockout) | any | gone | restore from backup (see below) or re-bootstrap |

`(a)` and `(b)` cover >99% of real-world events. Durable KV + HTTPS API +
stateless consumer means your automation genuinely survives restarts with
zero intervention.

#### Backup strategy (so (d) is a minor hassle, not a crisis)

Cost ~2 lines of code. On every successful refresh, mirror the new state
to a second location:

```js
// inside the refresh-and-persist block
await env.KV.put("fold_state", JSON.stringify(state));
await env.KV_BACKUP.put("fold_state", JSON.stringify(state));
```

`KV_BACKUP` can be:
- **another KV namespace** in the same account — survives accidental `wrangler kv:key delete` on the primary, doesn't survive account loss;
- **a private GitHub gist** (via gist API) — survives Cloudflare account loss, different blast radius;
- **R2 / S3 / GCS** with server-side encryption — most robust.

Restore is one command:

```bash
wrangler kv:key get --binding=KV_BACKUP 'fold_state' \
  | wrangler kv:key put --binding=KV 'fold_state' --path=-
```

MTTR: under a minute, no SMS OTP needed.

#### Deliberate non-feature: the broker has NO write endpoints

It's tempting to add a `POST /seed` or `POST /rotate-now` so you can bootstrap
without touching wrangler. Don't.

- A leaked `BROKER_KEY` becomes "attacker can read access tokens for ≤15 min
  until next rotation". Acceptable, self-healing.
- A leaked `BROKER_KEY` with a `/seed` endpoint becomes "attacker can
  overwrite the refresh chain with their own token, permanently redirecting
  your consumers into an attacker-controlled auth session, and locking you
  out until you re-bootstrap". Not acceptable.
- Seeding is a once-per-90-days operation. Optimizing the convenience of a
  rare event is not worth the permanent attack surface.

The broker should expose **exactly one route**: "give me a currently valid
access token". Everything else (seed, rotate, delete, inspect) happens via
wrangler / the dashboard with your Cloudflare credentials.

#### Other operational hygiene

- **Don't log the refresh token.** `console.log(state)` in the Worker is one
  typo away from leaking the refresh chain to Cloudflare logs (which are
  retained by default). Log only non-secret fields: `request_id`,
  `expires_at`, `status`.
- **Keep the Worker tiny.** No npm deps, no analytics sidecars. Every
  dependency is a supply-chain foothold into your refresh token. The broker
  is ~30 lines; it should stay that way.
- **Rate-limit or origin-gate the broker.** A leaked `BROKER_KEY` is annoying
  but not catastrophic if requests from unexpected IPs get 403'd. Easy with
  Cloudflare Access or a Worker IP allowlist.
- **Monitor for refresh failures.** If `/tokens/refresh` returns 401 twice
  in a row, that's probably (c) or an unrelated server issue — alert yourself
  (Pushbullet / ntfy.sh / email) so you know to re-bootstrap.

---

## 12. Legal / terms note (you already mentioned this is blessed by Fold)

Fold's ToS at the time of writing do not publish an official API; a custom client is a tolerated grey area. Since you've spoken with them and they're fine with it:

- Send a real-looking `X-Device-Hash` and keep it stable — helps their analytics.
- Maybe set `X-Device-Type: Web` (what I saw) rather than `Script`/`Automation` to blend in, OR use a distinctive value they can whitelist/identify as your integration — ask them.
- Don't hammer `/auth/otp`. Only hit refresh on schedule, never force a new OTP from automation.
- `User-Agent` of your HTTP client will be visible to them; consider setting it to something like `fold-personal-integration/0.1 (+contact email)`.
