package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// tier3SystemPrompt is the standing instruction we give Gemini.
// Designed to:
//  1. Constrain the output shape (we parse JSON afterwards).
//  2. Force ID hallucination defence ("pick from the historical
//     examples ONLY"). Even with this, we re-validate IDs in code.
//  3. Encourage low-confidence self-reports rather than overconfident
//     guesses, since 'needs_review' is cheap and a wrong push isn't.
const tier3SystemPrompt = `You are a personal-finance classifier. Map a fold.money transaction onto a firefly-iii transaction by choosing the closest match from a small set of historical firefly transactions.

You MUST reply with a single JSON object with these fields (any field may be null if no good match):
{
  "destination_account_id": <int|null>,
  "source_account_id":      <int|null>,
  "category_id":            <int|null>,
  "budget_id":              <int|null>,
  "description_suggestion": <string|null>,
  "confidence":             <number 0..1>,
  "reasoning":              <string>
}

Rules:
- All IDs MUST be drawn from the historical examples shown to you. Never invent an ID.
- If you cannot find a clear match, return confidence < 0.5 and explain why in 'reasoning'.
- 'description_suggestion' is a short human-readable description (e.g. "Lunch at Zomato, May 8").
- Output the JSON object only. No prose, no markdown fences.`

// llmResponse is the structured shape we expect from Gemini. Pointer
// fields so we can distinguish "not provided" from "explicitly null".
type llmResponse struct {
	DestinationAccountID  *int64   `json:"destination_account_id"`
	SourceAccountID       *int64   `json:"source_account_id"`
	CategoryID            *int64   `json:"category_id"`
	BudgetID              *int64   `json:"budget_id"`
	DescriptionSuggestion *string  `json:"description_suggestion"`
	Confidence            float64  `json:"confidence"`
	Reasoning             string   `json:"reasoning"`
}

// tierThreeLLM retrieves broader RAG context (FTS5 top-K with looser
// query, plus narration tokens), asks Gemini to classify, parses the
// JSON response, and validates that the LLM's IDs actually exist in
// the retrieved candidates. Anything off → return false so the caller
// falls through to Tier 4.
func (c *Classifier) tierThreeLLM(ctx context.Context, staged StagedRow) (Decision, bool, error) {
	hits, err := c.retrieveTier3Candidates(ctx, staged)
	if err != nil {
		return Decision{}, false, fmt.Errorf("retrieve candidates: %w", err)
	}
	if len(hits) == 0 {
		// No examples to anchor the LLM. Without a corpus, asking it
		// would be pure guess; better to escalate to human review.
		return Decision{}, false, nil
	}

	prompt := buildTier3Prompt(staged, hits)
	jsonText, err := c.llm.GenerateJSON(ctx, tier3SystemPrompt, prompt)
	if err != nil {
		return Decision{}, false, fmt.Errorf("gemini call: %w", err)
	}
	jsonText = stripJSONFences(jsonText)

	var llm llmResponse
	if err := json.Unmarshal([]byte(jsonText), &llm); err != nil {
		return Decision{}, false, fmt.Errorf("parse llm response: %w (raw: %s)", err, truncate(jsonText, 256))
	}

	// Validate: every non-null ID must appear in the retrieved hits.
	// This is the hallucination guard. If the LLM invented an ID, we
	// drop it and fall through to Tier 4.
	if !idsAreFromCandidates(llm, hits) {
		return Decision{}, false, errors.New("llm produced ids not present in candidates (hallucination guard)")
	}

	// Build a Decision. We accept Tier-3 only if the LLM expressed
	// reasonable confidence AND we have a destination_account_id —
	// without a destination, there's no firefly target.
	if llm.Confidence < 0.5 || llm.DestinationAccountID == nil {
		return Decision{}, false, nil
	}

	// Resolve the human-readable names for display from the hit set.
	destName := lookupHitName(hits, "destination", *llm.DestinationAccountID)
	srcName := ""
	if llm.SourceAccountID != nil {
		srcName = lookupHitName(hits, "source", *llm.SourceAccountID)
	}
	catName := ""
	if llm.CategoryID != nil {
		catName = lookupHitName(hits, "category", *llm.CategoryID)
	}
	budName := ""
	if llm.BudgetID != nil {
		budName = lookupHitName(hits, "budget", *llm.BudgetID)
	}

	desc := ""
	if llm.DescriptionSuggestion != nil {
		desc = *llm.DescriptionSuggestion
	}

	ftsView := make([]FTSHitView, 0, len(hits))
	for _, h := range hits {
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
		DestinationAccountID:   llm.DestinationAccountID,
		DestinationAccountName: destName,
		SourceAccountID:        llm.SourceAccountID,
		SourceAccountName:      srcName,
		CategoryID:             llm.CategoryID,
		CategoryName:           catName,
		BudgetID:               llm.BudgetID,
		BudgetName:             budName,
		Description:            desc,
		Evidence: Evidence{
			Tier:               TierLLM,
			MerchantNormalized: staged.MerchantExtracted,
			FTSHits:            ftsView,
			Note:               llm.Reasoning,
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
}

// retrieveTier3Candidates pulls top-K firefly transactions for the LLM
// to reason about. Strategy:
//  1. If the staged row has an extracted merchant, use it as the FTS
//     query (same as Tier 2).
//  2. If not, fall back to the 5 longest tokens from the narration —
//     fold's narrations contain useful tokens (currency, descriptors,
//     occasionally fragments of a merchant name).
//  3. If even that yields nothing, return empty.
//
// We pull more rows than Tier 2 (15 vs 10) because the LLM benefits
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
		       t.description,            t.date,
		       bm25(firefly_txns_fts)    AS score
		FROM firefly_txns_fts
		JOIN firefly_txns t ON t.firefly_id = firefly_txns_fts.rowid
		WHERE firefly_txns_fts MATCH ?
		  AND t.txn_type = 'withdrawal'
		ORDER BY score
		LIMIT ?
	`, query, k)
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
			dateStr string
		)
		if err := rows.Scan(
			&h.FireflyID,
			&h.DestinationAccountID, &dn,
			&h.SourceAccountID, &sn,
			&h.CategoryID, &cn,
			&h.BudgetID, &bn,
			&h.Description, &dateStr, &h.Score,
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

// buildTier3Prompt formats the staged transaction + retrieved hits
// into the user-prompt text. We keep it terse — Gemini is good at
// extracting structure from short, structured prose.
func buildTier3Prompt(staged StagedRow, hits []tier3Hit) string {
	var b strings.Builder
	b.WriteString("Historical firefly transactions (recent matches):\n")
	for i, h := range hits {
		b.WriteString(fmt.Sprintf("[%d] firefly_id=%d  amount-currency-historical\n", i+1, h.FireflyID))
		b.WriteString(fmt.Sprintf("    destination=%q (id=%s)\n", h.DestinationAccountName, formatNullID(h.DestinationAccountID)))
		b.WriteString(fmt.Sprintf("    source=%q (id=%s)\n", h.SourceAccountName, formatNullID(h.SourceAccountID)))
		b.WriteString(fmt.Sprintf("    category=%q (id=%s)\n", h.CategoryName, formatNullID(h.CategoryID)))
		if h.BudgetName != "" {
			b.WriteString(fmt.Sprintf("    budget=%q (id=%s)\n", h.BudgetName, formatNullID(h.BudgetID)))
		}
		b.WriteString(fmt.Sprintf("    description=%q\n", h.Description))
		b.WriteString(fmt.Sprintf("    date=%s\n", h.Date.Format("2006-01-02")))
	}
	b.WriteString("\nNew fold transaction to classify:\n")
	b.WriteString(fmt.Sprintf("  fold_uuid: %s\n", staged.FoldUUID))
	b.WriteString(fmt.Sprintf("  mode:      %s\n", staged.Mode))
	b.WriteString(fmt.Sprintf("  type:      %s\n", staged.Type))
	if staged.MerchantExtracted != "" {
		b.WriteString(fmt.Sprintf("  merchant_extracted: %q\n", staged.MerchantExtracted))
	}
	b.WriteString(fmt.Sprintf("  narration: %q\n", staged.Narration))
	b.WriteString("\nReturn the JSON object now.")
	return b.String()
}

// idsAreFromCandidates checks every non-null ID in the LLM response
// against the retrieved hit set. If the LLM hallucinated an ID, we
// fail this check and bail.
func idsAreFromCandidates(r llmResponse, hits []tier3Hit) bool {
	dest := map[int64]bool{}
	src := map[int64]bool{}
	cat := map[int64]bool{}
	bud := map[int64]bool{}
	for _, h := range hits {
		if h.DestinationAccountID.Valid {
			dest[h.DestinationAccountID.Int64] = true
		}
		if h.SourceAccountID.Valid {
			src[h.SourceAccountID.Int64] = true
		}
		if h.CategoryID.Valid {
			cat[h.CategoryID.Int64] = true
		}
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

// lookupHitName scans hits for the first record matching id and
// returns the corresponding name field. Used for UI display.
func lookupHitName(hits []tier3Hit, kind string, id int64) string {
	for _, h := range hits {
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

// stripJSONFences removes ``` fences if Gemini wraps the JSON despite
// our system prompt asking it not to. Defensive — current behaviour
// is clean, but model updates can regress.
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
	// Drop tokens that look like UPI handles / hex blobs / pure numbers.
	type kept struct {
		tok string
	}
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
	// Sort by length desc, stable so the same input produces the same
	// FTS query across runs (cache friendly, debugging friendly).
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
