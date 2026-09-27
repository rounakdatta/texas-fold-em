package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// tier3SystemPrompt frames the LLM as a final-stage synthesiser, not
// a last-resort fallback. The prompt explicitly tells the model:
//   - It will receive deterministic-tier hints (Tier 1 lookup + Tier 2
//     FTS vote) AND the user's full asset/expense/category inventories
//     AND the verbatim fold-side raw_payload.
//   - It must pick IDs only from the inventories shown to it. Any
//     non-listed ID will be rejected by our hallucination guard.
//   - It is responsible for type-direction: if fold says INCOMING, the
//     firefly source is a revenue-side payer and the destination is one
//     of the user's asset accounts; OUTGOING is the inverse. The
//     deterministic tiers don't reason about this — Tier 3 must.
//   - For an OUTGOING txn the source card is NOT the LLM's call. fold's
//     account_id deterministically identifies the paying card, and the
//     classifier resolves it to a firefly asset after the LLM returns
//     (see matchFoldCardToFireflyAsset). Letting the LLM guess the source
//     is what produced hallucinations like an "AU Ixigo" charge landing
//     on "Axis Bank Ace". So the prompt tells it to leave source null for
//     withdrawals; it focuses on destination, category, tags, description.
const tier3SystemPrompt = `You are the review assistant in a personal-finance app. You turn one bank or card transaction, as the fold.money app recorded it, into the firefly-iii transaction its owner would book themselves: their payee, their category, their tags, their title. The owner reviews every suggestion before it is saved, so being right matters more than being complete — when you cannot know a detail, leave a placeholder instead of guessing.

You will be given (in the user message):
  - the raw fold transaction: narration, merchant, mode, type, amount, currency, source_amount/source_currency for a foreign charge, fold's summary, and the owner's own note in "notes"
  - THE NOTE ON THE PAYMENT: for a UPI payment, what the payer typed in the UPI app — for money out, the owner's own words
  - TIME CONTEXT: when it happened (IST, and the likely local time for a foreign charge)
  - the paying account
  - HOW YOU CORRECTED FOLD BEFORE: past suggestions for this merchant or handle the owner changed, and what they changed them to
  - YOUR RECENT CORRECTIONS ELSEWHERE: their latest corrections on other merchants — style, not facts about this one
  - WHAT YOU DID WITH SIMILAR TRANSACTIONS: what they sent, held back or skipped
  - THIS HANDLE / DESCRIPTOR IN YOUR LEDGER: past firefly rows paid to the same UPI handle or card descriptor
  - AROUND THIS TIME: their other transactions within ±36h; TRIP CONTEXT and RECURRING when they apply
  - STYLE SAMPLES: titles they wrote for this merchant
  - hints from deterministic tiers, the account/category/budget/tag inventories, and similar past firefly transactions

How to weigh the evidence, strongest first:
  1. The owner's corrections and decisions for the same merchant, handle or pattern. If they renamed a payee or rewrote a title for this merchant, do it their way — even when the historical examples or the hints disagree. A recent correction outweighs an older habit.
  2. The same UPI handle or card descriptor in their ledger: whoever they booked it to is almost certainly this counterparty.
  3. Their STYLE SAMPLES and titles for this payee.
  4. Similar historical transactions and the deterministic hints.
  5. The transaction's own text, the time, the neighbours, general knowledge.
  The owner's own words about THIS transaction — fold's "notes" and THE NOTE ON THE PAYMENT — outrank everything on its purpose and specifics: "CAB" on a payment to a friend is travel, whatever that friend is usually paid for.
  When signals still conflict, prefer the more specific and the more recent; if they still disagree, choose the safer answer and lower your confidence.

PAYEE (the destination for money out, the source for money in):
  - Reuse an existing account whenever one genuinely is this counterparty. Match the owner's naming shape: if their merchant accounts read "Merchant, Area, City", a new one should too.
  - A new name is the clean, canonical merchant — never the raw narration. Strip payment-processor prefixes and codes: "TST* ", "SQ *", "UEP*", "SMP**", "GRB*", "FH* ", "SNACK* ", "SP ", "PAYU*", "RAZORPAY*", "CASHFREE*", "BILLDESK*", "PAYPAL *", store numbers, state and country codes, phone numbers. "TST* NOOR INDIAN FUSIO BERKELEY CA" → "Noor Indian Fusion Kitchen, Berkeley".
  - A UPI payment to a person (a name, not a business) books to that person, named the way the owner named them before.

TITLE (description_suggestion) — the most visible thing you write:
  - Mirror the owner first. Their corrections and past titles for this merchant or handle set the format — length, vocabulary, structure. A structure they use repeatedly ("<items> from <shop>", "Ride from <A> to <B>", "<meal> at <place>") is followed with this transaction's specifics.
  - Write "___" (three underscores) for any specific the evidence does not give you: items bought, dishes, people, the two ends of a ride, an occasion. Never guess a specific — a wrong specific is worse than a blank the owner fills in one tap. "___ for dinner" beats "Pasta for dinner" when nothing says pasta.
  - The owner's own words are the best source of specifics: the payload's "notes", and THE NOTE ON THE PAYMENT, cut at about eighteen characters — read it whole ("ROLL DINNER" → "Roll for dinner", "BELATED HAPPY BIRT" → a belated birthday gift) and keep its people, dishes and details. "RAPIDO" means a Rapido ride paid to its driver.
  - Name the meal from the time (the LOCAL time for a foreign charge; see TIME CONTEXT) only when the owner's titles for similar places name meals. Never attach a meal to an online service or a subscription.
  - Neighbours may give context (the ride to the place, the meal before), never another transaction's specifics.
  - When the owner's history offers nothing better: a card bill payment is "Credit card repayment"; a refund is "Refund for <the purchase's title>"; a card fee says what it is ("Forex charges", "GST", "Annual fee"); interest and cashback say what they are.
  - Never put the raw narration, a reference number or the amount in the title.

TAGS group transactions the owner wants to find together: a trip, a reimbursable expense, an event. Add one only when this transaction is in that situation — within the trip's dates and currency (TRIP CONTEXT), a work meal on a working day for a reimbursement tag — or when the owner's RECENT transactions of exactly this kind all carry it. Never tag a meal, a category or a merchant's name. Use the existing vocabulary. When unsure, no tag: a missing tag costs one tap, a wrong one misleads.

BUDGET follows the payee first: the budget (or "none") this payee's own past rows carry — in THIS HANDLE / DESCRIPTOR, the historical examples, the corrections — decides it; one merchant's meals can go to a budget while a café's coffees go to none. For a payee with no history, the budgets on the owner's spends of the same kind around it come next (AROUND THIS TIME, and TRIP CONTEXT for a trip), then the category's budget when YOUR BUDGETS BY CATEGORY shows at least 80% of it there; otherwise null.

HOLDS: a card alert reporting an amount "credited back to your card" with no merchant is usually a released authorisation hold, not money received — and its original charge, the same amount a few days earlier, was usually never billed either. Say so in hold_suggestion; the owner decides.

Hard rules:
1. EVERY id MUST appear in the inventories you were shown. Inventing an id is a critical failure.
2. Firefly has THREE transaction types — pick the right one and report it as txn_type:
   - "withdrawal" — fold OUTGOING; source is a user ASSET account, destination is an EXPENSE account (merchant)
   - "deposit"    — fold INCOMING; source is a REVENUE account (payer), destination is a user ASSET account
   - "transfer"   — both source AND destination are the user's own ASSET accounts (e.g. savings → brokerage, a credit-card bill paid from savings to the credit-card account). It's a transfer if and only if BOTH endpoints are in the asset list.
3. SOURCE ACCOUNT: for a WITHDRAWAL, set source_account_id to null — do NOT try to pick it. The paying card is resolved deterministically from fold's account_id after you return, so any source you guess is discarded. For a DEPOSIT the source is the revenue-side payer — pick the best-matching revenue account (or null if none fits). For a TRANSFER, source is the user asset the money left from — pick it.
4. NEW destination accounts: for a WITHDRAWAL whose merchant has NO good match in the expense inventory, do NOT force-fit an unrelated id or a generic catch-all. Set destination_account_id to null and put the clean canonical name in destination_name_suggestion; firefly creates the account on push. For deposits and transfers, always use an existing id.
5. If the signals genuinely conflict, return confidence < 0.5 and explain in reasoning.
6. Output the JSON object only. No prose, no markdown fences.

Output exactly this JSON shape (any nullable field may be null). Write "reasoning" FIRST — think through the evidence before you decide:
{
  "reasoning":                   <string: what the evidence says about the payee, title, category, tags and type, citing it>,
  "txn_type":                    "withdrawal" | "deposit" | "transfer",
  "destination_account_id":      <int|null>,
  "destination_name_suggestion": <string|null>,
  "source_account_id":           <int|null>,
  "category_id":                 <int|null>,
  "budget_id":                   <int|null>,
  "tags":                        [<string>...],
  "description_suggestion":      <string|null>,
  "unknowns":                    [<string>...],
  "evidence":                    [<string>...],
  "hold_suggestion":             <string|null>,
  "confidence":                  <number 0..1>
}
"unknowns" names what the owner still has to tell you, one entry per ___ in the title ("items", "who with", "where from", "where to"). "evidence" is 1–4 short phrases naming what decided it ("your correction of 12 Sep", "handle paid 4 times before", "trip tag on the neighbouring charges").

The output JSON MUST include a "txn_type" field set to "withdrawal", "deposit", or "transfer".`

// llmResponse is the structured shape we expect back from the LLM.
// Pointer fields distinguish "not provided" from "explicitly null".
type llmResponse struct {
	TxnType              string `json:"txn_type"` // "withdrawal" | "deposit" | "transfer"
	DestinationAccountID *int64 `json:"destination_account_id"`
	// DestinationNameSuggestion is a NEW expense-account name for a
	// withdrawal whose merchant has no matching account yet (id null).
	// firefly auto-creates it on push. See hard rule 6 in the prompt.
	DestinationNameSuggestion *string  `json:"destination_name_suggestion"`
	SourceAccountID           *int64   `json:"source_account_id"`
	CategoryID                *int64   `json:"category_id"`
	BudgetID                  *int64   `json:"budget_id"`
	Tags                      []string `json:"tags"`
	DescriptionSuggestion     *string  `json:"description_suggestion"`
	Confidence                float64  `json:"confidence"`
	Reasoning                 string   `json:"reasoning"`
	// Unknowns name what the owner still has to fill in (one per ___);
	// Evidence the signals that decided it; HoldSuggestion a released
	// authorisation or a charge never billed. All shown on the card.
	Unknowns       []string `json:"unknowns"`
	EvidenceNotes  []string `json:"evidence"`
	HoldSuggestion *string  `json:"hold_suggestion"`
}

// tier3Inputs bundles everything the synthesiser sees. Built by
// gatherTier3Inputs.
type tier3Inputs struct {
	hits            []tier3Hit
	assetAccounts   []AccountRef
	expenseAccounts []AccountRef
	revenueAccounts []AccountRef
	categories      []AccountRef
	budgets         []AccountRef
	tagLibrary      []string
	tier1           *Decision       // nil if Tier 1 missed
	tier2           *Decision       // nil if Tier 2 missed
	foldAccount     *FoldAccountRef // nil when raw_payload has no account_id, or it's not mirrored

	// mealCtx is a compact local-time-and-bucket string for the prompt's
	// TIME CONTEXT block. Empty when the staged row's timestamp can't be
	// parsed; the renderer omits the block in that case.
	mealCtx string

	// styleSamples are the user's past descriptions for this exact
	// merchant, used to teach the LLM the voice/format to mirror. Empty
	// for first-time merchants; the renderer omits the block then.
	styleSamples []styleSample

	// learn is what the owner decided before and what surrounds this
	// transaction (learning.go).
	learn learningContext

	// How often each payee was used in the last year, to rank the part of
	// the inventory the prompt shows (rankedAccounts).
	expenseUsage, revenueUsage map[int64]int

	// budgetHabits: which budget each category went to, last year.
	budgetHabits []string
}

// Inventory sizes shown in one prompt; the guard accepts every account.
const (
	shownExpenseAccounts = 250
	shownRevenueAccounts = 150
)

// tierThreeLLM gathers context, builds the prompt, calls the LLM,
// parses + validates the response. Returns (decision, ok, err) like
// the other tiers; err is non-nil only on transport / parse failure
// (the caller in ClassifyOne logs+drops it). ok=false means the LLM
// declined to commit (low confidence or missing destination).
func (c *Classifier) tierThreeLLM(ctx context.Context, staged StagedRow, tier1Hint, tier2Hint *Decision, ex exclusion) (Decision, bool, error) {
	inputs, err := c.gatherTier3Inputs(ctx, staged, tier1Hint, tier2Hint, ex)
	if err != nil {
		return Decision{}, false, fmt.Errorf("gather inputs: %w", err)
	}

	prompt := buildTier3Prompt(staged, inputs)
	jsonText, err := c.llm.GenerateJSON(ctx, tier3SystemPrompt, prompt)
	if err != nil {
		return Decision{}, false, fmt.Errorf("llm call: %w", err)
	}
	jsonText = extractJSONObject(jsonText)

	var llm llmResponse
	if err := json.Unmarshal([]byte(jsonText), &llm); err != nil {
		return Decision{}, false, fmt.Errorf("parse llm response: %w (raw: %s)", err, truncate(jsonText, 256))
	}

	// Hallucination guard: every non-null ID must appear in the
	// inventories we showed the LLM. Expanded from the old
	// "must be in FTS hits" to also accept asset/expense/revenue
	// account ids and full category/budget lists.
	if !idsAreFromInventories(llm, inputs) {
		return Decision{}, false, errors.New("llm produced ids not present in inventories (hallucination guard)")
	}

	// Validate txn_type first — we need it to decide whether a name-only
	// destination is allowed. The LLM is asked to set it; default to the
	// fold-direction mapping if it didn't (or returned garbage) so the
	// Pusher always has a usable value downstream.
	txnType := strings.ToLower(strings.TrimSpace(llm.TxnType))
	switch txnType {
	case "withdrawal", "deposit", "transfer":
		// ok
	default:
		txnType = fireflyTxnTypeFor(staged.Type)
	}

	// Resolve the destination. Two valid shapes:
	//   - an existing account by id (the common case), OR
	//   - a NEW expense account by name, for a withdrawal whose merchant
	//     has no matching account yet (e.g. a first-ever "United Airlines"
	//     flight). firefly auto-creates the expense account from the name
	//     on push. Honoured for withdrawals only; deposits/transfers must
	//     resolve to an existing asset id.
	newDestName := ""
	if llm.DestinationAccountID == nil && txnType == "withdrawal" && llm.DestinationNameSuggestion != nil {
		newDestName = strings.TrimSpace(*llm.DestinationNameSuggestion)
	}
	hasDestination := llm.DestinationAccountID != nil || newDestName != ""

	// Accept only when the LLM is reasonably confident and has SOME
	// destination (an existing id or a new name). The SOURCE requirement
	// is conditional: for an OUTGOING txn the paying card is resolved
	// deterministically from fold's account_id after we return (see
	// classifyAndApply), so we must NOT drop an otherwise-good withdrawal
	// just because the LLM (correctly) left source null. For INCOMING the
	// source is the revenue payer — the LLM's job — so keep requiring it.
	requiresLLMSource := staged.Type != "OUTGOING"
	if llm.Confidence < 0.5 || (requiresLLMSource && llm.SourceAccountID == nil) || !hasDestination {
		// Structural decision rejected, but the LLM's description is
		// independent of the structural confidence — it's based on
		// STYLE SAMPLES and TIME CONTEXT, which are valid signals
		// regardless of whether the model could pin the right ids.
		// Attach it to whichever Tier-1/Tier-2 hint we're about to
		// fall through to, so the title for the row isn't lost.
		if llm.DescriptionSuggestion != nil {
			if d := strings.TrimSpace(*llm.DescriptionSuggestion); d != "" {
				switch {
				case tier1Hint != nil:
					tier1Hint.Description = d
				case tier2Hint != nil:
					tier2Hint.Description = d
				}
			}
		}
		// With no deterministic hint to fall back to, the card goes to a
		// person — carrying what the model could say (its title, category,
		// budget and tags, and why it couldn't settle the rest) rather than
		// nothing. Never its payee: an unsure card makes the person pick
		// who was paid.
		return c.declinedDecision(staged, llm, inputs, txnType), false, nil
	}

	// Resolve human-readable names for the UI's display. An id-backed
	// destination is looked up across the inventories (already validated
	// by the hallucination guard); a name-only new destination IS its
	// own display name.
	destName := newDestName
	if llm.DestinationAccountID != nil {
		destName = lookupName(inputs, "destination", *llm.DestinationAccountID)
	}
	srcName := ""
	if llm.SourceAccountID != nil {
		srcName = lookupName(inputs, "source", *llm.SourceAccountID)
	}
	catName := ""
	if llm.CategoryID != nil {
		catName = lookupName(inputs, "category", *llm.CategoryID)
	}
	budName := ""
	if llm.BudgetID != nil {
		budName = lookupName(inputs, "budget", *llm.BudgetID)
	}

	desc := ""
	if llm.DescriptionSuggestion != nil {
		desc = *llm.DescriptionSuggestion
	}

	ftsView := make([]FTSHitView, 0, len(inputs.hits))
	for _, h := range inputs.hits {
		ftsView = append(ftsView, FTSHitView{
			FireflyID:              h.FireflyID,
			Score:                  h.Score,
			DestinationAccountName: h.DestinationAccountName,
			CategoryName:           h.CategoryName,
			Description:            h.Description,
			Date:                   h.Date,
		})
	}

	return Decision{
		Tier:                   TierLLM,
		Confidence:             llm.Confidence,
		TxnType:                txnType,
		DestinationAccountID:   llm.DestinationAccountID,
		DestinationAccountName: destName,
		SourceAccountID:        llm.SourceAccountID,
		SourceAccountName:      srcName,
		CategoryID:             llm.CategoryID,
		CategoryName:           catName,
		BudgetID:               llm.BudgetID,
		BudgetName:             budName,
		Description:            desc,
		Tags:                   llm.Tags,
		Engine:                 c.llm.Model(),
		Evidence: Evidence{
			Tier:               TierLLM,
			MerchantNormalized: staged.MerchantExtracted,
			FTSHits:            ftsView,
			Note:               llm.Reasoning,
			Tags:               llm.Tags,
			Signals:            capStrings(llm.EvidenceNotes, 4),
			Unknowns:           capStrings(llm.Unknowns, 6),
			HoldSuggestion:     strings.TrimSpace(derefString(llm.HoldSuggestion)),
			Learned:            inputs.learn.summary(),
		},
	}, true, nil
}

// declinedDecision is a card for a person, from a model answer too unsure to
// stand: what it could say, and why it stopped.
func (c *Classifier) declinedDecision(staged StagedRow, r llmResponse, in tier3Inputs, txnType string) Decision {
	d := Decision{
		Tier:       TierHumanReview,
		Confidence: r.Confidence,
		TxnType:    txnType,
		Tags:       r.Tags,
		Engine:     c.llm.Model(),
		Evidence: Evidence{
			Tier:               TierHumanReview,
			MerchantNormalized: staged.MerchantExtracted,
			Note:               "fold's model couldn't settle this one: " + strings.TrimSpace(r.Reasoning),
			Signals:            capStrings(r.EvidenceNotes, 4),
			Unknowns:           capStrings(r.Unknowns, 6),
			HoldSuggestion:     strings.TrimSpace(derefString(r.HoldSuggestion)),
			Learned:            in.learn.summary(),
		},
	}
	if r.DescriptionSuggestion != nil {
		d.Description = strings.TrimSpace(*r.DescriptionSuggestion)
	}
	if r.CategoryID != nil {
		d.CategoryID, d.CategoryName = r.CategoryID, lookupName(in, "category", *r.CategoryID)
	}
	if r.BudgetID != nil {
		d.BudgetID, d.BudgetName = r.BudgetID, lookupName(in, "budget", *r.BudgetID)
	}
	return d
}

// tier3Hit is the flat candidate row we feed both the prompt and the
// hallucination guard. Built from FTS5 hits.
type tier3Hit struct {
	FireflyID              int64
	DestinationAccountID   sql.NullInt64
	DestinationAccountName string
	SourceAccountID        sql.NullInt64
	SourceAccountName      string
	CategoryID             sql.NullInt64
	CategoryName           string
	BudgetID               sql.NullInt64
	BudgetName             string
	Description            string
	Date                   time.Time
	Score                  float64
	TagsJSON               string
	// Notes is the raw narration the operator (or our v0.6.0+ push)
	// stored on the firefly transaction. Often contains fold's truncated
	// merchant string verbatim — e.g. "CARD/.../SHREE VINAYAKA ENTE/..."
	// which links to firefly's friendlier "Sri Udupi Park, Indiranagar".
	// Surfacing this in the prompt is what teaches the LLM the
	// narration↔merchant mapping that's otherwise invisible.
	Notes string
}

// gatherTier3Inputs assembles every slice of context the prompt needs.
// Cheap: 5 small lookups against firefly_txns. Heavy lifting (the FTS
// retrieval) is the only part bounded by query work.
func (c *Classifier) gatherTier3Inputs(ctx context.Context, staged StagedRow, tier1, tier2 *Decision, ex exclusion) (tier3Inputs, error) {
	hits, err := c.retrieveTier3Candidates(ctx, staged, ex)
	if err != nil {
		return tier3Inputs{}, err
	}
	asset, _ := listAssetAccounts(ctx, c.db)
	expense, _ := listExpenseAccounts(ctx, c.db)
	revenue, _ := listRevenueAccounts(ctx, c.db)
	if ex.accountID != 0 {
		expense, revenue = withoutAccount(expense, ex.accountID), withoutAccount(revenue, ex.accountID)
	}
	cats, _ := listCategories(ctx, c.db)
	buds, _ := listBudgets(ctx, c.db)
	tags, _ := allTagsFromMirror(ctx, c.db)
	foldAcc, _ := lookupFoldAccountForStaged(ctx, c.db, staged.RawPayload)

	// Style samples for description generation: prefer the Tier-1 hit's
	// destination_account_id (an exact match against firefly), fall back
	// to a name-based lookup on the staged row's normalised merchant.
	// Either way, we want the user's last N descriptions for THIS
	// merchant so the LLM can mirror their voice.
	var destForStyle int64
	if tier1 != nil && tier1.DestinationAccountID != nil {
		destForStyle = *tier1.DestinationAccountID
	}
	samples, _ := recentSameMerchantDescriptions(ctx, c.db, destForStyle, staged.MerchantExtracted, 10, ex.fireflyID)
	// The payee the deterministic tiers point at, for the recurring check.
	var payeeHint *int64
	switch {
	case tier1 != nil && tier1.DestinationAccountID != nil:
		payeeHint = tier1.DestinationAccountID
	case tier2 != nil && tier2.DestinationAccountID != nil:
		payeeHint = tier2.DestinationAccountID
	}
	if staged.Type != "OUTGOING" {
		payeeHint = nil
	}

	return tier3Inputs{
		hits:            hits,
		assetAccounts:   asset,
		expenseAccounts: expense,
		revenueAccounts: revenue,
		categories:      cats,
		budgets:         buds,
		tagLibrary:      tags,
		tier1:           tier1,
		tier2:           tier2,
		foldAccount:     foldAcc,
		mealCtx:         mealContext(staged.TxnTimestamp, foreignCurrencyOf(staged.RawPayload)),
		styleSamples:    samples,
		learn:           c.gatherLearning(ctx, staged, payeeHint, ex),
		expenseUsage:    accountUsage(ctx, c.db, "destination", time.Now().AddDate(-1, 0, 0)),
		revenueUsage:    accountUsage(ctx, c.db, "source", time.Now().AddDate(-1, 0, 0)),
		budgetHabits:    budgetHabits(ctx, c.db, fireflyTxnTypeFor(staged.Type), time.Now()),
	}, nil
}

// retrieveTier3Candidates pulls top-K firefly transactions for the LLM
// to reason about. Filtered by txn_type matching the fold direction —
// for OUTGOING fold, withdrawals; for INCOMING, deposits.
//
// Strategy:
//  1. If the staged row has an extracted merchant, use it as the FTS
//     query (same as Tier 2).
//  2. If not, fall back to long tokens from the narration.
//  3. If still nothing, return empty — the LLM gets prompted with no
//     RAG examples but still has the full inventories, mode, account_id
//     etc. to reason from.
//
// Pull more rows than Tier 2 (15 vs 10) because the LLM benefits
// from broader context.
func (c *Classifier) retrieveTier3Candidates(ctx context.Context, staged StagedRow, ex exclusion) ([]tier3Hit, error) {
	const k = 15
	query := buildFTSQuery(staged.MerchantExtracted)
	if query == "" {
		query = buildFTSQuery(strings.Join(longTokens(staged.Narration, 5), " "))
	}
	if query == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT t.firefly_id,
		       t.destination_account_id, t.destination_account_name,
		       t.source_account_id,      t.source_account_name,
		       t.category_id,            t.category_name,
		       t.budget_id,              t.budget_name,
		       t.description,            t.date, t.tags_json,
		       COALESCE(t.notes, ''),
		       bm25(firefly_txns_fts)    AS score
		FROM firefly_txns_fts
		JOIN firefly_txns t ON t.firefly_id = firefly_txns_fts.rowid
		WHERE firefly_txns_fts MATCH ?
		  AND t.txn_type = ?
		  AND t.firefly_id <> ?
		ORDER BY score
		LIMIT ?
	`, query, fireflyTxnTypeFor(staged.Type), ex.fireflyID, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []tier3Hit
	for rows.Next() {
		var (
			h       tier3Hit
			dn, sn  sql.NullString
			cn, bn  sql.NullString
			tagsJS  sql.NullString
			dateStr string
		)
		if err := rows.Scan(
			&h.FireflyID,
			&h.DestinationAccountID, &dn,
			&h.SourceAccountID, &sn,
			&h.CategoryID, &cn,
			&h.BudgetID, &bn,
			&h.Description, &dateStr, &tagsJS, &h.Notes, &h.Score,
		); err != nil {
			return nil, err
		}
		if dn.Valid {
			h.DestinationAccountName = dn.String
		}
		if sn.Valid {
			h.SourceAccountName = sn.String
		}
		if cn.Valid {
			h.CategoryName = cn.String
		}
		if bn.Valid {
			h.BudgetName = bn.String
		}
		if tagsJS.Valid {
			h.TagsJSON = tagsJS.String
		}
		if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
			h.Date = t
		} else if t, err := time.Parse("2006-01-02 15:04:05+00:00", dateStr); err == nil {
			h.Date = t
		} else if t, err := time.Parse("2006-01-02", dateStr); err == nil {
			h.Date = t
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// budgetHabits says, per category, which budget the owner's spends in it go
// to: "Food → "Eating outside" (44 of 51, last 4 months)", or "Grocery → no
// budget (40 of 45, last year)". The owner's recent practice wins — the
// habit of the last four months when a category has three rows there, else
// the year's — because a budget adopted in spring is the habit now, however
// many older rows went without one. Categories used at least three times
// recently or five in the year; most used first, at most 30.
func budgetHabits(ctx context.Context, db *sql.DB, txnType string, now time.Time) []string {
	type habit struct {
		top      string
		topN, n  int
		inWindow string
	}
	read := func(since time.Time) map[string]*habit {
		out := map[string]*habit{}
		rows, err := db.QueryContext(ctx, `
			SELECT category_name, COALESCE(budget_name, ''), COUNT(*) FROM firefly_txns
			WHERE txn_type = ? AND category_name IS NOT NULL AND TRIM(category_name) <> '' AND substr(date, 1, 10) >= ?
			GROUP BY 1, 2`, txnType, since.Format("2006-01-02"))
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var cat, bud string
			var n int
			if rows.Scan(&cat, &bud, &n) != nil {
				continue
			}
			h := out[cat]
			if h == nil {
				h = &habit{}
				out[cat] = h
			}
			h.n += n
			if n > h.topN || (n == h.topN && bud < h.top) {
				h.top, h.topN = bud, n
			}
		}
		return out
	}
	recent, year := read(now.AddDate(0, -4, 0)), read(now.AddDate(-1, 0, 0))
	chosen := map[string]*habit{}
	for cat, h := range year {
		if r := recent[cat]; r != nil && r.n >= 3 {
			r.inWindow = "last 4 months"
			chosen[cat] = r
		} else if h.n >= 5 {
			h.inWindow = "last year"
			chosen[cat] = h
		}
	}
	type kv struct {
		cat string
		h   *habit
	}
	var list []kv
	for cat, h := range chosen {
		list = append(list, kv{cat, h})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].h.n > list[j].h.n || (list[i].h.n == list[j].h.n && list[i].cat < list[j].cat)
	})
	var out []string
	for i, e := range list {
		if i >= 30 {
			break
		}
		to := "no budget"
		if e.h.top != "" {
			to = fmt.Sprintf("%q", e.h.top)
		}
		out = append(out, fmt.Sprintf("%s → %s in %d of %d (%d%%), %s", e.cat, to, e.h.topN, e.h.n, 100*e.h.topN/e.h.n, e.h.inWindow))
	}
	return out
}

func withoutAccount(list []AccountRef, id int64) []AccountRef {
	out := make([]AccountRef, 0, len(list))
	for _, a := range list {
		if a.ID != id {
			out = append(out, a)
		}
	}
	return out
}

// allTagsFromMirror flattens firefly_txns.tags_json into a deduped
// list. Cheap (~7k rows on this corpus) and lets the prompt include
// the user's tag vocabulary so the LLM uses existing tags rather than
// inventing new ones.
func allTagsFromMirror(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT tags_json FROM firefly_txns
		WHERE tags_json IS NOT NULL AND tags_json <> '' AND tags_json <> '[]'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var arr []string
		if json.Unmarshal([]byte(raw), &arr) != nil {
			continue
		}
		for _, t := range arr {
			t = strings.TrimSpace(t)
			if t != "" {
				seen[t] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// buildTier3Prompt formats inputs into the user-prompt text. Compact
// tabular form — modern chat models handle structured prose well.
func buildTier3Prompt(staged StagedRow, in tier3Inputs) string {
	var b strings.Builder

	// Block 1: the fold transaction itself, full JSON. The LLM has
	// access to ALL the fields fold returns (account_id, merchant,
	// kind, current_balance, etc.), not just our typed subset.
	b.WriteString("== FOLD TRANSACTION (raw payload from fold's API) ==\n")
	b.WriteString(staged.RawPayload)
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("== FOLD TYPE: %s   MODE: %s ==\n", staged.Type, staged.Mode))
	b.WriteString(fmt.Sprintf("(Firefly side will be: %s)\n\n", fireflyTxnTypeFor(staged.Type)))
	if note := upiNote(staged.Narration); note != "" {
		who := "what the payer typed in the UPI app"
		if staged.Type == "OUTGOING" {
			who = "what you typed in the UPI app — your own words"
		}
		b.WriteString("== THE NOTE ON THE PAYMENT (" + who + ", cut at ~18 characters) ==\n")
		b.WriteString(fmt.Sprintf("  %q\n\n", note))
	}

	// Block 1c: TIME CONTEXT — when this happened in IST, and which
	// meal/occasion bucket it falls in. Used by the LLM to infer
	// whether to call this "Lunch", "Dinner", etc. when STYLE SAMPLES
	// show the user marks the meal explicitly.
	if in.mealCtx != "" {
		b.WriteString("== TIME CONTEXT ==\n")
		b.WriteString("  " + in.mealCtx + "\n\n")
	}

	// Block 1d: STYLE SAMPLES — the user's past description strings
	// for THIS merchant. Distinct from the HISTORICAL EXAMPLES (BM25)
	// block below, which is for ID evidence; this is purely for voice.
	// Capping at 10 keeps prompt size bounded.
	if len(in.styleSamples) > 0 {
		b.WriteString("== STYLE SAMPLES — past description strings YOU wrote for this merchant ==\n")
		b.WriteString("(most recent first. MIRROR this voice — length, vocabulary, structure)\n")
		for i, s := range in.styleSamples {
			if i >= 10 {
				break
			}
			dateStr := ""
			if !s.Date.IsZero() {
				dateStr = " — " + s.Date.Format("2006-01-02")
			}
			b.WriteString(fmt.Sprintf("  %d. %q%s\n", i+1, s.Description, dateStr))
		}
		b.WriteString("\nReminder: use ___ as a placeholder for missing specifics (companion, dish, occasion).\n")
		b.WriteString("Prefer partial-with-placeholder over generic-and-complete.\n\n")
	}

	// Block 1b: the resolved fold-side account. This is the single
	// strongest source-account signal we have — fold's `account_id`
	// is an opaque UUID, but with the mirror joined we know the
	// human-readable name (and provider/network/last-four) of the
	// card or bank that paid. Match it against the firefly asset list
	// by name; same provider + same last-four is a near-certain link.
	if in.foldAccount != nil {
		fa := in.foldAccount
		b.WriteString("== FOLD-SIDE PAYING ACCOUNT (resolved from raw_payload.account_id) ==\n")
		b.WriteString(fmt.Sprintf("  name:     %q\n", fa.Name))
		b.WriteString(fmt.Sprintf("  kind:     %s\n", fa.Kind))
		if fa.Provider != "" {
			b.WriteString(fmt.Sprintf("  provider: %q\n", fa.Provider))
		}
		if fa.Network != "" {
			b.WriteString(fmt.Sprintf("  network:  %q\n", fa.Network))
		}
		if fa.LastFour != "" {
			b.WriteString(fmt.Sprintf("  last4:    %s\n", fa.LastFour))
		}
		b.WriteString("STRONG HINT: this is the user's own asset that paid. ")
		switch staged.Type {
		case "OUTGOING":
			b.WriteString("Pick the firefly asset whose name best matches the above (provider + product + last4) as source_account_id. ")
			b.WriteString("Do NOT default to a popular card from FTS hits if a different asset is named here.\n\n")
		case "INCOMING":
			b.WriteString("Pick the firefly asset whose name best matches the above (provider + product + last4) as destination_account_id.\n\n")
		default:
			b.WriteString("Pick the firefly asset whose name best matches the above (provider + product + last4).\n\n")
		}
	}

	// Block 1e: what the owner decided before, and what surrounds this
	// transaction — ahead of the hints and inventories because it
	// outranks them (learning.go).
	in.learn.render(&b)

	// Block 2: hints from the deterministic tiers.
	b.WriteString("== DETERMINISTIC HINTS ==\n")
	if in.tier1 != nil {
		b.WriteString(fmt.Sprintf("Tier 1 (merchant_lookup, conf=%.2f):\n", in.tier1.Confidence))
		writeDecisionHint(&b, in.tier1)
	} else {
		b.WriteString("Tier 1: no merchant_lookup match.\n")
	}
	if in.tier2 != nil {
		b.WriteString(fmt.Sprintf("Tier 2 (FTS5 vote, conf=%.2f):\n", in.tier2.Confidence))
		writeDecisionHint(&b, in.tier2)
	} else {
		b.WriteString("Tier 2: no FTS5 modal vote.\n")
	}
	b.WriteString("\n")

	// Block 3: user's account inventories, scoped by what the LLM
	// will need given the fold direction.
	b.WriteString("== USER'S ACCOUNT INVENTORIES ==\n")
	words := promptWords(staged)
	partial := func(shown, all int, what string) string {
		if shown >= all {
			return ""
		}
		return fmt.Sprintf(" — %d of your %d %s: every one whose name shares a word with this transaction, then the ones you use most", shown, all, what)
	}
	if staged.Type == "OUTGOING" {
		b.WriteString("\nasset accounts (pick SOURCE from these):\n")
		writeAccountList(&b, in.assetAccounts)
		shown := rankedAccounts(in.expenseAccounts, in.expenseUsage, words, shownExpenseAccounts)
		b.WriteString("\nexpense accounts (pick DESTINATION from these, or any from the historical examples below" + partial(len(shown), len(in.expenseAccounts), "payees") + "):\n")
		writeAccountList(&b, shown)
	} else {
		b.WriteString("\nasset accounts (pick DESTINATION from these):\n")
		writeAccountList(&b, in.assetAccounts)
		shown := rankedAccounts(in.revenueAccounts, in.revenueUsage, words, shownRevenueAccounts)
		b.WriteString("\nrevenue accounts (pick SOURCE from these" + partial(len(shown), len(in.revenueAccounts), "payers") + "):\n")
		writeAccountList(&b, shown)
	}
	b.WriteString("\ncategories:\n")
	writeAccountList(&b, in.categories)
	if len(in.budgets) > 0 {
		b.WriteString("\nbudgets:\n")
		writeAccountList(&b, in.budgets)
	}
	if len(in.budgetHabits) > 0 {
		b.WriteString("\nYOUR BUDGETS BY CATEGORY (the budget you use most for each, of how many — your recent practice first):\n")
		for _, h := range in.budgetHabits {
			b.WriteString("  " + h + "\n")
		}
	}
	if len(in.tagLibrary) > 0 {
		b.WriteString("\nexisting tag vocabulary (use these when applicable):\n")
		b.WriteString("  ")
		b.WriteString(strings.Join(in.tagLibrary, ", "))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Block 4: the FTS retrieval examples.
	if len(in.hits) > 0 {
		b.WriteString("== HISTORICAL EXAMPLES (BM25 nearest) ==\n")
		for i, h := range in.hits {
			b.WriteString(fmt.Sprintf("[%d] firefly_id=%d  date=%s\n", i+1, h.FireflyID, h.Date.Format("2006-01-02")))
			b.WriteString(fmt.Sprintf("    destination=%q (id=%s)\n", h.DestinationAccountName, formatNullID(h.DestinationAccountID)))
			b.WriteString(fmt.Sprintf("    source=%q (id=%s)\n", h.SourceAccountName, formatNullID(h.SourceAccountID)))
			b.WriteString(fmt.Sprintf("    category=%q (id=%s)\n", h.CategoryName, formatNullID(h.CategoryID)))
			if h.BudgetName != "" {
				b.WriteString(fmt.Sprintf("    budget=%q (id=%s)\n", h.BudgetName, formatNullID(h.BudgetID)))
			} else {
				b.WriteString("    budget=none\n")
			}
			b.WriteString(fmt.Sprintf("    description=%q\n", h.Description))
			if h.TagsJSON != "" && h.TagsJSON != "[]" {
				b.WriteString(fmt.Sprintf("    tags=%s\n", h.TagsJSON))
			}
			// Notes carries the raw fold narration the operator (or a
			// recent v0.6.0+ push) stored on this firefly transaction.
			// Often contains the bank's truncated merchant string
			// verbatim — the LLM uses it to map "SHREE VINAYAKA ENTE"
			// in the current staged narration to this row's clean
			// destination ("Sri Udupi Park, Indiranagar").
			if h.Notes != "" {
				b.WriteString(fmt.Sprintf("    notes=%q\n", h.Notes))
			}
		}
		b.WriteString("\nWhen the current fold transaction's narration shares tokens with any HISTORICAL EXAMPLE's `notes` field above, that row's destination is a high-confidence match — the user has effectively already mapped this bank string to a firefly merchant. Mirror that mapping unless other signals strongly disagree.\n")
	} else {
		b.WriteString("== HISTORICAL EXAMPLES ==\n(none — first time seeing this kind of transaction; rely on inventories.)\n")
	}

	b.WriteString("\nReturn the JSON object now.")
	return b.String()
}

// writeDecisionHint dumps a tier 1/2 hint compactly. Intentionally
// terse — the LLM doesn't need a manifesto.
func writeDecisionHint(b *strings.Builder, d *Decision) {
	if d.DestinationAccountID != nil {
		b.WriteString(fmt.Sprintf("  destination_account_id = %d (%q)\n", *d.DestinationAccountID, d.DestinationAccountName))
	}
	if d.SourceAccountID != nil {
		b.WriteString(fmt.Sprintf("  source_account_id      = %d (%q)\n", *d.SourceAccountID, d.SourceAccountName))
	}
	if d.CategoryID != nil {
		b.WriteString(fmt.Sprintf("  category_id            = %d (%q)\n", *d.CategoryID, d.CategoryName))
	}
	if d.BudgetID != nil {
		b.WriteString(fmt.Sprintf("  budget_id              = %d (%q)\n", *d.BudgetID, d.BudgetName))
	}
}

func writeAccountList(b *strings.Builder, list []AccountRef) {
	for _, a := range list {
		b.WriteString(fmt.Sprintf("  - id=%d  %q\n", a.ID, a.Name))
	}
	if len(list) == 0 {
		b.WriteString("  (none)\n")
	}
}

// idsAreFromInventories generalises the old idsAreFromCandidates to
// validate against the union of every ID universe in the prompt. The
// LLM might pick a category id from `inputs.categories` that doesn't
// happen to appear in the FTS hits — that's still legal.
func idsAreFromInventories(r llmResponse, in tier3Inputs) bool {
	dest := idSet(in.expenseAccounts)
	addAssets(dest, in.assetAccounts)
	for _, h := range in.hits {
		if h.DestinationAccountID.Valid {
			dest[h.DestinationAccountID.Int64] = true
		}
	}
	src := idSet(in.assetAccounts)
	addAssets(src, in.revenueAccounts)
	for _, h := range in.hits {
		if h.SourceAccountID.Valid {
			src[h.SourceAccountID.Int64] = true
		}
	}
	cat := idSet(in.categories)
	for _, h := range in.hits {
		if h.CategoryID.Valid {
			cat[h.CategoryID.Int64] = true
		}
	}
	bud := idSet(in.budgets)
	for _, h := range in.hits {
		if h.BudgetID.Valid {
			bud[h.BudgetID.Int64] = true
		}
	}
	if r.DestinationAccountID != nil && !dest[*r.DestinationAccountID] {
		return false
	}
	if r.SourceAccountID != nil && !src[*r.SourceAccountID] {
		return false
	}
	if r.CategoryID != nil && !cat[*r.CategoryID] {
		return false
	}
	if r.BudgetID != nil && !bud[*r.BudgetID] {
		return false
	}
	return true
}

func idSet(refs []AccountRef) map[int64]bool {
	m := make(map[int64]bool, len(refs))
	for _, r := range refs {
		m[r.ID] = true
	}
	return m
}

func addAssets(m map[int64]bool, refs []AccountRef) {
	for _, r := range refs {
		m[r.ID] = true
	}
}

// lookupName resolves an ID to its display name across every inventory
// the LLM saw. Used purely for UI display; the ID itself is the
// authoritative reference once it's passed the hallucination guard.
func lookupName(in tier3Inputs, kind string, id int64) string {
	switch kind {
	case "destination":
		if n := nameFromRefs(in.expenseAccounts, id); n != "" {
			return n
		}
		if n := nameFromRefs(in.assetAccounts, id); n != "" {
			return n
		}
	case "source":
		if n := nameFromRefs(in.assetAccounts, id); n != "" {
			return n
		}
		if n := nameFromRefs(in.revenueAccounts, id); n != "" {
			return n
		}
	case "category":
		if n := nameFromRefs(in.categories, id); n != "" {
			return n
		}
	case "budget":
		if n := nameFromRefs(in.budgets, id); n != "" {
			return n
		}
	}
	for _, h := range in.hits {
		switch kind {
		case "destination":
			if h.DestinationAccountID.Valid && h.DestinationAccountID.Int64 == id {
				return h.DestinationAccountName
			}
		case "source":
			if h.SourceAccountID.Valid && h.SourceAccountID.Int64 == id {
				return h.SourceAccountName
			}
		case "category":
			if h.CategoryID.Valid && h.CategoryID.Int64 == id {
				return h.CategoryName
			}
		case "budget":
			if h.BudgetID.Valid && h.BudgetID.Int64 == id {
				return h.BudgetName
			}
		}
	}
	return ""
}

func nameFromRefs(refs []AccountRef, id int64) string {
	for _, r := range refs {
		if r.ID == id {
			return r.Name
		}
	}
	return ""
}

// extractJSONObject returns the JSON object in a model's reply: the text
// itself when it is one, else the outermost {...} — a model behind an
// OpenAI-shaped gateway without a native JSON mode may add a fence or a line
// of prose around it.
func extractJSONObject(s string) string {
	s = stripJSONFences(s)
	if json.Valid([]byte(s)) {
		return s
	}
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start >= 0 && end > start && json.Valid([]byte(s[start:end+1])) {
		return s[start : end+1]
	}
	return s
}

func capStrings(list []string, n int) []string {
	var out []string
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" && len(out) < n {
			out = append(out, s)
		}
	}
	return out
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// stripJSONFences removes ``` fences if the LLM wraps the JSON
// despite our system prompt asking it not to (and despite the
// response_format=json_object flag, which most providers respect).
// Defensive — cheap to keep, cuts off a class of preventable errors.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func formatNullID(n sql.NullInt64) string {
	if !n.Valid {
		return "null"
	}
	return fmt.Sprintf("%d", n.Int64)
}

// longTokens picks up to n tokens from a string, prefer-longest.
// Used as the fallback FTS query when no merchant is extractable.
func longTokens(s string, n int) []string {
	tokens := strings.Fields(s)
	type kept struct{ tok string }
	scored := make([]kept, 0, len(tokens))
	for _, t := range tokens {
		t = strings.Trim(t, "/.,:;-_+()")
		if len(t) < 4 {
			continue
		}
		if isHexish(t) || isNumeric(t) {
			continue
		}
		scored = append(scored, kept{tok: t})
	}
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && len(scored[j].tok) > len(scored[j-1].tok); j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	if len(scored) > n {
		scored = scored[:n]
	}
	out := make([]string, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.tok)
	}
	return out
}

func isHexish(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			if r != '.' {
				return false
			}
		}
	}
	return true
}
