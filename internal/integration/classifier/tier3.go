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
//   - Source-account inference is its primary value-add. The hints from
//     Tiers 1+2 should be treated as "known-good destination + category"
//     but the source pick is the LLM's responsibility — we trust the
//     LLM to read mode/account_id/narration and pick the right card.
const tier3SystemPrompt = `You are a personal-finance synthesiser mapping a fold.money transaction onto a firefly-iii transaction.

You will be given:
  - the raw fold transaction (full JSON, including fold's own account_id, mode, type, merchant, narration)
  - lists of the user's available firefly accounts, categories, budgets, and tags
  - examples of similar past firefly transactions (RAG retrieval)
  - the user's own past description strings for this exact merchant (STYLE SAMPLES) — these teach you their voice
  - a TIME CONTEXT block: when the transaction happened in IST and which meal/occasion bucket it falls in
  - hints from deterministic tiers, when available (these are GUIDANCE, not commands)

Your job: produce the cleanest possible firefly proposal.

Output exactly this JSON shape (any nullable field may be null):
{
  "txn_type":               "withdrawal" | "deposit" | "transfer",
  "destination_account_id": <int|null>,
  "destination_name_suggestion": <string|null>,
  "source_account_id":      <int|null>,
  "category_id":            <int|null>,
  "budget_id":              <int|null>,
  "tags":                   [<string>...],
  "description_suggestion": <string|null>,
  "confidence":             <number 0..1>,
  "reasoning":              <string>
}

Hard rules:
1. EVERY id MUST appear in the inventories you were shown. Inventing an id is a critical failure.
2. Firefly has THREE transaction types — pick the right one and report it as txn_type:
   - "withdrawal" — fold OUTGOING; source is a user ASSET account, destination is an EXPENSE account (merchant)
   - "deposit"    — fold INCOMING; source is a REVENUE account (payer), destination is a user ASSET account
   - "transfer"   — both source AND destination are the user's own ASSET accounts (e.g. savings → zerodha, credit-card-bill payment from savings to credit-card-account). Fold sees this as OUTGOING/INCOMING but it's a transfer if and only if BOTH endpoints are in the asset list.
3. Pick the source account using fold's mode + account_id + narration signals — don't blindly copy a "modal source" hint if fold's signals point elsewhere (different card, different bank).
4. Use the historical examples to pick category, budget, and tags. If the user has previously tagged this merchant, mirror those tags.
5. If the signals genuinely conflict, return confidence < 0.5 and explain in 'reasoning'.
6. NEW destination accounts: for a WITHDRAWAL whose merchant has NO good match in the expense inventory, do NOT force-fit an unrelated id and do NOT dump it in a generic catch-all account. Instead set destination_account_id to null and put a clean, canonical merchant name in destination_name_suggestion (e.g. "United Airlines" — never the raw bank narration, never a guessed id). firefly creates the expense account on push. Always prefer an existing id when one genuinely fits; only suggest a new name when none does. For deposits and transfers, always use an existing id (do not invent names).
7. Output the JSON object only. No prose, no markdown fences.

description_suggestion guidance (this is what becomes the transaction TITLE in firefly — treat it as a first-class output, not an afterthought):
  A. MIRROR the user's voice from STYLE SAMPLES. Their past descriptions for this merchant
     show the format they prefer (length, vocabulary, structure). Match it.
       e.g. samples are "Dinner with X", "Lunch with Y", "Coffee solo"
            → produce "Dinner with ___" or "Lunch with ___", not "Restaurant meal"
  B. USE the TIME CONTEXT to guess the meal/occasion. A 21:42 txn at a restaurant is
     dinner; 13:15 is lunch; 09:30 is breakfast. Fold this into the description
     when STYLE SAMPLES show the user tends to mark the meal explicitly.
  C. USE "___" (three underscores) as a placeholder when you don't know a specific detail
     the user typically includes — companion name, dish name, occasion. The user will
     fill these in during review. PREFER partial-with-placeholder over
     generic-and-complete:
       BETTER: "Dinner with ___"        WORSE: "Mezzaluna dinner"
       BETTER: "___ for lunch at Zomato" WORSE: "Lunch at Zomato"
  D. WHEN there are no STYLE SAMPLES for this merchant, fall back to a short
     deterministic title: "[meal_bucket] at [merchant]" or just "[merchant]". Keep it
     under ~6 words; the user will edit if they want richer.
  E. NEVER write the raw bank narration as the description — that goes in notes
     separately; description is the human-friendly title.

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
}

// tierThreeLLM gathers context, builds the prompt, calls the LLM,
// parses + validates the response. Returns (decision, ok, err) like
// the other tiers; err is non-nil only on transport / parse failure
// (the caller in ClassifyOne logs+drops it). ok=false means the LLM
// declined to commit (low confidence or missing destination).
func (c *Classifier) tierThreeLLM(ctx context.Context, staged StagedRow, tier1Hint, tier2Hint *Decision) (Decision, bool, error) {
	inputs, err := c.gatherTier3Inputs(ctx, staged, tier1Hint, tier2Hint)
	if err != nil {
		return Decision{}, false, fmt.Errorf("gather inputs: %w", err)
	}

	prompt := buildTier3Prompt(staged, inputs)
	jsonText, err := c.llm.GenerateJSON(ctx, tier3SystemPrompt, prompt)
	if err != nil {
		return Decision{}, false, fmt.Errorf("llm call: %w", err)
	}
	jsonText = stripJSONFences(jsonText)

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

	// Accept only when the LLM is reasonably confident, pinned a source,
	// and has SOME destination (an existing id or a new name). Without a
	// source + destination the firefly POST would 422.
	if llm.Confidence < 0.5 || llm.SourceAccountID == nil || !hasDestination {
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
		return Decision{}, false, nil
	}

	// Resolve human-readable names for the UI's display. An id-backed
	// destination is looked up across the inventories (already validated
	// by the hallucination guard); a name-only new destination IS its
	// own display name.
	destName := newDestName
	if llm.DestinationAccountID != nil {
		destName = lookupName(inputs, "destination", *llm.DestinationAccountID)
	}
	srcName := lookupName(inputs, "source", *llm.SourceAccountID)
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
		Evidence: Evidence{
			Tier:               TierLLM,
			MerchantNormalized: staged.MerchantExtracted,
			FTSHits:            ftsView,
			Note:               llm.Reasoning,
			Tags:               llm.Tags,
		},
	}, true, nil
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
func (c *Classifier) gatherTier3Inputs(ctx context.Context, staged StagedRow, tier1, tier2 *Decision) (tier3Inputs, error) {
	hits, err := c.retrieveTier3Candidates(ctx, staged)
	if err != nil {
		return tier3Inputs{}, err
	}
	asset, _ := listAssetAccounts(ctx, c.db)
	expense, _ := listExpenseAccounts(ctx, c.db)
	revenue, _ := listRevenueAccounts(ctx, c.db)
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
	samples, _ := recentSameMerchantDescriptions(ctx, c.db, destForStyle, staged.MerchantExtracted, 10)

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
		mealCtx:         mealContext(staged.TxnTimestamp),
		styleSamples:    samples,
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
func (c *Classifier) retrieveTier3Candidates(ctx context.Context, staged StagedRow) ([]tier3Hit, error) {
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
		ORDER BY score
		LIMIT ?
	`, query, fireflyTxnTypeFor(staged.Type), k)
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
	if staged.Type == "OUTGOING" {
		b.WriteString("\nasset accounts (pick SOURCE from these):\n")
		writeAccountList(&b, in.assetAccounts)
		b.WriteString("\nexpense accounts (pick DESTINATION from these — or any from the FTS examples below):\n")
		writeAccountList(&b, capList(in.expenseAccounts, 200))
	} else {
		b.WriteString("\nasset accounts (pick DESTINATION from these):\n")
		writeAccountList(&b, in.assetAccounts)
		b.WriteString("\nrevenue accounts (pick SOURCE from these):\n")
		writeAccountList(&b, capList(in.revenueAccounts, 200))
	}
	b.WriteString("\ncategories:\n")
	writeAccountList(&b, in.categories)
	if len(in.budgets) > 0 {
		b.WriteString("\nbudgets:\n")
		writeAccountList(&b, in.budgets)
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

func capList(list []AccountRef, n int) []AccountRef {
	if len(list) > n {
		return list[:n]
	}
	return list
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
