<p align="center">
  <img src="./assets/logo.png" alt="texas-fold-em logo" width="280">
</p>

# texas-fold-em

A long-running broker that holds a [fold.money](https://fold.money) refresh
token and turns it into something useful.

## What this is

Two things, and you can use either or both.

**A token broker.** Fold's API gives you 15-minute access tokens that you
have to keep refreshing with a longer-lived refresh token. Doing that
correctly — handling races, persistence, retries, the moment when fold
revokes your chain — is fiddly. This service does it for you. Anything
on your network that needs to call fold's API can ask the broker for a
fresh access token and stop worrying about token lifecycle.

**An auto-classifier into firefly-iii.** If you also keep your books in
[firefly-iii](https://www.firefly-iii.org), there's a tedious manual
job: every few days, look at fold, type each transaction into firefly,
pick the right account, the right category, the right tags. Built on top
of the broker, this is a small pipeline that does that copy step for
you, intelligently, while keeping a human in the loop for anything
ambiguous.

## The problem this solves

Fold is great at *seeing* your money — every card, every account, every
UPI flow lands there. Firefly is great at *organising* it — the source
of truth for "what did I spend on, when, and why".

The gap between them is you, sat at a keyboard, retyping transactions.
And it's not even mechanical retyping: the same restaurant on different
days might map to different firefly categories; the same UPI receiver
might be a personal transfer in one context and an expense in another;
fold doesn't know which of your three credit cards or two bank accounts
paid for any given thing — well, it does, but only as an opaque
identifier that means nothing to firefly. So you're not just typing,
you're making small judgement calls all the way down.

This project automates the typing and the easy judgements, and surfaces
the genuinely hard ones for you to confirm in a small review UI.

## How the classifier thinks

A four-step ladder, fastest to most expensive. Each transaction tries
the steps in order and stops at the first one with enough confidence.

1. **Look it up.** "I've already seen this merchant 50 times in your
   firefly history; you always file it under category X with destination
   Y from card Z. Done." This is the cheap path; no AI needed, and it
   only gets cheaper over time as your firefly history grows.
2. **Vote on it.** If the merchant is unfamiliar, search firefly-side
   transactions whose narration shares words with this fold one and let
   the top-scoring matches vote on category and destination.
3. **Reason about it.** If the deterministic steps can't agree, hand the
   transaction to an LLM with a tightly-scoped prompt: the verbatim
   fold payload, your full firefly account / category / budget / tag
   inventory, the resolved fold-side card or bank that paid (so the LLM
   can tell that "Tata Neu Plus ****8943" matches your firefly asset
   "Tata Neu HDFC Bank Credit Card"), and the hints from the previous
   steps. Ask for a structured answer; reject any id the LLM didn't see
   on the menu.
4. **Punt to the human.** When even step 3 isn't confident, the row
   lands in the review UI for a one-click confirmation.

Three things that fall out of this design and are worth knowing:

- **It learns from you.** Every correction you make, every send, hold and
  skip is kept (`review_feedback`), and step 3 reads them back before
  anything else: how you corrected it on this merchant or UPI handle
  ("fold said *Coffee*, you wrote *Filter coffee and bun*"), who your
  ledger says a handle belongs to, what happened around the transaction
  (the ride there, a trip in progress and its tag), and whether it recurs.
  The merchant lookup behind step 1 is reinforced on every push too.
- **It keeps getting better while you're away.** Waiting cards don't keep
  the suggestion they arrived with. When you correct one card, its
  siblings from the same merchant are suggested again with that
  correction in hand, and cards an older engine suggested are revisited
  when a newer one ships (`TEXAS_FOLDEM_RESUGGEST_EVERY`). Nothing a person
  chose is ever touched: a card with any confirmed field, a hold or a
  manual row is left alone, and a failed attempt keeps the old suggestion.
- **It knows what it doesn't know.** A title's `___` blanks come with what
  they stand for ("items?", "who with?"), so filling one in is one tap.
- **It holds back what shouldn't be sent.** A card's "credited back" alert
  with no merchant is almost always a released authorisation — a charge
  that was never billed. fold books it as a refund of that charge (found
  by its foreign amount on the same card) and puts it on hold, saying
  which charge and what to check. Release the hold and fold never sets it
  on that transaction again.
- **It handles transfers.** Most categorisers only know
  spend-vs-income. This one explicitly picks between firefly's three
  transaction types — withdrawal, deposit, **and transfer** — so a
  savings → brokerage move gets recorded as a transfer rather than
  miscategorised as an expense.
- **It picks the right card.** Even if you've never used a particular
  merchant before, the LLM grounds source-account inference on fold's
  own knowledge of which card or account actually paid — so an
  unfamiliar restaurant on your travel card doesn't end up assigned to
  whichever card you happen to use most often. Money coming *in* lands on
  the account fold says received it, by the same rule.
- **It understands refunds.** A card refund ("Refund Received!") skips the
  ladder: it becomes a firefly **deposit** from the merchant's revenue account
  (the one named like its expense account) into the card that was charged,
  category *Refund*, titled "Refund for …" after the purchase it gives back
  for, with that purchase's tags. The purchase is found among fold's staged
  rows *and* firefly's own history — same card, same merchant, earlier, not
  already refunded in full; fold.money's own `refund_group_id` wins when it's
  set. An exact match within 30 days is ready to push; an older exact match,
  a partial refund (a larger purchase within a week) or no purchase found goes
  to review with the candidates listed. Rows classified before this — or
  edited by hand — get a purchase proposed only when the match is that
  confident.

## The review UI

`/` — the home screen — is where the transactions waiting for a human are decided,
one card at a time. It is built for a phone first, and works as well with a
keyboard:

- **Swipe right to send** a card to firefly, **left to look at it later** (it
  waits in a *Later* pile). On a desktop: → and ←. A send waits out a
  five-second undo window before anything reaches firefly — *Undo*, U or ⌘Z
  takes it back — and goes through the same push, with the same checks, as
  everywhere else. If firefly refuses it, the card comes back saying why.
- **A card shows what matters**: the amount, who it went to (or came from),
  when (in IST, the way you'd say it), from which account, and the title and
  category the push will send. Only what is exceptional is flagged: a `___`
  blank in the title, a missing payee, a refund with a purchase to pick, a
  possible duplicate, a hold.
- **The main button names the next step.** A card that can't go yet says what
  it needs — *Fill in the blank*, *Who was paid?* — and a right swipe opens
  exactly that: the blank already selected, your past titles and payees for
  the merchant one tap away, the name on the alert offered as the payee.
- **Hold** a row that must never be sent, with a reason; its next step
  becomes *Skip it*. Skips can be undone, and restored from the list.
- **Work one account at a time**, with a count of what waits on each, newest
  or oldest first (the order a statement runs in) — or surest first.
- **Surest first clears a backlog at the pace of a swipe.** The cards that go
  with one swipe and that fold is sure of come first, then the ones it is
  fairly sure of, then its guesses; then the cards waiting on a word from you,
  surest first again; then what is on hold. Within each band the deck keeps to
  time, newest first, so a day's payments stay together. Sure means the
  classifier would have accepted it unasked (confidence 0.85 or more, a card
  it settled, naming only accounts Firefly has), you chose who was paid, or
  you added the row from a statement. A card that would make an account in
  Firefly is for you to check (*New payee*), and a possible duplicate, a
  refund with a purchase to pick or fold's own case for a hold is never sure.
  Past the sure ones each card says how sure fold is — *Fairly sure*, *Not
  sure* — and the send that takes the last sure one says so.

`/transactions` lists every transaction by status, with the editor behind each
row: account, category, budget and tag fields are name autocompletes backed by
your real firefly history, and per-merchant tags appear as one-tap chips.

It dresses like fold.money, so moving between the two feels like one product:
navy ink on cool paper, white cards with hairline borders, fold blue for the
actions, small capitals for labels, and the rupee sign raised beside its
number. The cowboy is the favicon, the app bar's mark and the home-screen icon
(add it to your phone's home screen and it opens straight on the deck). The
type is [Outfit](https://github.com/Outfitio/Outfit-Fonts) (SIL OFL 1.1), the
nearest open face to fold's own, served from the binary with everything else.

Reconciling against a card statement is done in the same place:

- **Correct the money and the date.** A card alert fires at authorisation, so
  what settles can differ — a US restaurant tip is added later, fold.money
  converts foreign charges at its own rate, a statement posts a different day.
  Each row's review form takes the statement's INR amount, the foreign amount
  and an IST date/time; push (and *save & update firefly* for rows already
  pushed) sends the corrected values. Rows show a *corrected* badge.
- **Add a transaction fold never saw** (*+ add transaction*): a charge from
  before a card was linked, a missed alert, a refund, reversal, fee or waiver.
  It becomes a `MANUAL` row — pushed like any other, never touched by the
  classifier.
- **Clear a wrong suggestion.** Emptying the category or budget field now means
  *none*; the classifier's suggestion no longer comes back at push time.
- **Late changes on fold's side flow through.** When fold reports a new amount
  for a row that isn't pushed yet (say, one edited in the fold.money app), the
  next sync updates it and logs the change to the audit trail.
- **Duplicates are flagged.** fold.money's own `is_possible_duplicate` shows as
  a *possible duplicate* badge.
- **Filter by an account to reconcile it.** The account filter shows every row
  that moves money on that account — what it paid *and* what came into it (a
  card's bill payments, refunds, reversals) — with money in shown as `+`. The
  list's category column shows the category push will actually send.
- **Refunds are linked in firefly.** A refund's review form has a *refund of*
  picker (the classifier's pick, other candidates, or *none*). Firefly is the
  ledger, so the relationship is recorded there, as firefly's native **Refund**
  transaction link: the purchase shows "is (partially) refunded by" the refund.
  Push creates the link as soon as both sides are in firefly — pushing a
  purchase also links any refunds that were waiting for it — and a *link in
  firefly* button retries one. Rows carry *refund* / *refunded* badges, and the
  detail page shows fold.money's raw payload.

## Safety

Three properties worth calling out:

- **Read-only kill switch.** A single env flag halts every write to
  firefly without redeploying. Reads, classification, and the review UI
  keep working — you can always look without writing.
- **Idempotent everywhere.** Every fold sync, firefly mirror, push, and
  classify can be re-run. Pushes are deduplicated against firefly using
  fold's own UUID, so retries can't create doubles.
- **Audit log.** Every push to firefly — preview or confirmed — is
  appended to a local log, alongside the request body that was sent.

## Getting started

Published as a Helm chart on GHCR. Install:

```bash
helm install tfe oci://ghcr.io/rounakdatta/charts/texas-fold-em \
  --version 0.3.0 \
  -n fold --create-namespace \
  --set secret.brokerKey="$(openssl rand -base64 32)" \
  --set secret.adminKey="$(openssl rand -base64 32)"
```

Seed the refresh chain (one-time; do it again only if fold revokes the
chain — the broker will return `410 Gone` and stop trying when that
happens). Log in to fold.money in a browser, copy `refresh_token` and
`device_hash` from local storage, then:

```bash
kubectl -n fold port-forward svc/tfe 8080:8080 &
ADMIN=$(kubectl -n fold get secret tfe -o jsonpath='{.data.admin-key}' | base64 -d)

curl -X POST http://localhost:8080/init \
  -H "Authorization: Bearer $ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"refresh_token":"…","device_hash":"…"}'
```

You're now in broker mode. Anything in your network can call `/token`
with the broker key and use the result against fold's API.

To enable integration mode, point the broker at your firefly-iii
instance and (optionally) give it an LLM API key. Any OpenAI-compatible
chat-completions host works — DeepSeek (`deepseek-v4-flash`, the default),
OpenAI, or a gateway in front of Claude — by setting `LLM_BASE_URL` and
`LLM_MODEL`; no code changes. A slower, stronger model wants a longer
`LLM_TIMEOUT`; `LLM_REASONING_EFFORT` (`low`, `medium` …) asks a host that
supports it to think first. If the host starts failing or rate-limits,
the engine rests (the deck's side panel says until when) and cards keep
what they say.

```yaml
# values.yaml
extraEnv:
  - name: TEXAS_FOLDEM_INTEGRATION_ENABLED
    value: "true"
  - name: TEXAS_FOLDEM_FIREFLY_BASE
    value: "http://firefly.<namespace>.svc.cluster.local:8080"
  - name: TEXAS_FOLDEM_FIREFLY_PAT
    valueFrom: { secretKeyRef: { name: tfe, key: firefly-pat } }
  - name: TEXAS_FOLDEM_LLM_API_KEY           # optional but recommended
    valueFrom: { secretKeyRef: { name: tfe, key: llm-api-key } }
  # - name: TEXAS_FOLDEM_LLM_MODEL           # default deepseek-v4-flash; override for deepseek-v4-pro / gpt-4.1-mini / etc.
  #   value: "deepseek-v4-pro"
  # - name: TEXAS_FOLDEM_LLM_BASE_URL        # default https://api.deepseek.com/v1
  #   value: "https://api.openai.com/v1"
  # - name: TEXAS_FOLDEM_LLM_TIMEOUT         # per attempt; default 60s
  #   value: "110s"
  # - name: TEXAS_FOLDEM_LLM_REASONING_EFFORT # none | low | medium | high
  #   value: "low"
  - name: TEXAS_FOLDEM_PERIODIC_SYNC_EVERY   # set to 1h to run the pipeline hands-off
    value: "0"
  - name: TEXAS_FOLDEM_RESUGGEST_EVERY       # revisit waiting cards; 0 = off
    value: "0"
  # - name: TEXAS_FOLDEM_RESUGGEST_LIMIT     # cards per pass; default 8
  #   value: "6"
```

How good are the suggestions? `POST /api/engine/eval` (from the UI, with
`{"limit": 30}`, optionally another `model` or `reasoning`) re-suggests
transactions you already sent, hiding each one's answer from every lookup,
and grades type, payee, category, budget, tags and title against what
firefly holds — beside the suggestion fold made at the time. Poll
`GET /api/engine/eval` for the report; `GET /api/engine` is the engine's
status.

Then prime the pipeline (admin-key endpoints) — once, in this order:

```bash
curl -X POST -H "Authorization: Bearer $ADMIN" \
  http://localhost:8080/admin/firefly/sync         # mirror firefly history
curl -X POST -H "Authorization: Bearer $ADMIN" \
  http://localhost:8080/admin/fold/accounts/sync   # mirror your fold cards/accounts
curl -X POST -H "Authorization: Bearer $ADMIN" \
  "http://localhost:8080/admin/fold/sync?limit=50" # stage recent fold transactions
curl -X POST -H "Authorization: Bearer $ADMIN" \
  http://localhost:8080/admin/classify             # classify the staged rows
```

Open `/` to review and push. With `PERIODIC_SYNC_EVERY` set,
those four steps happen automatically on a tick.

Reverse-engineering notes on the underlying fold API live in
[`fold.md`](./fold.md). Reliability internals (atomic state writes,
refresh coalescing, validate-before-persist on `/init`, the rejection
tombstone, rotate-before-return) live in the code; the short version
is: a power cut, a concurrent `/token` storm, or a bad seed cannot
corrupt the refresh chain.

## Develop

```bash
make test         # go test -race ./...
make build        # native binary
```

## Local sandbox (fast classifier iteration)

The classifier reads only the local SQLite mirror (`firefly_txns`,
`merchant_lookup`, `fold_accounts`) — firefly, fold and the database are
touched only by the syncer and pusher. So you can iterate on
classification quality against *real* data without running any of them:
just a snapshot of `staging.db` plus an LLM key.

```bash
# point kubectl at the cluster first (see the access-homelab skill), then:
make sandbox            # pulls a staging.db snapshot + LLM key, runs tfe
REFRESH=1 make sandbox  # re-pull the live snapshot before running
```

Then open `http://127.0.0.1:8099/` and, to iterate on one
transaction end-to-end (reset → classify → inspect):

```bash
./scripts/reclassify-one.sh <fold_uuid>
```

The snapshot, LLM key and local state land in `./.local/` (gitignored —
it holds real financial data and a live key). No firefly/fold/MySQL run;
the broker is unseeded and the syncer/pusher never fire.

## License

Personal project, do whatever you want.
