// Package classifier turns a raw fold transaction into a proposed
// firefly transaction (source account, destination account, category,
// budget). Tier-1 and Tier-2 deterministic classifiers live here:
//
//	Tier 1 — merchant_lookup hit. The materialised lookup table built
//	          by the firefly syncer maps a normalised merchant string
//	          to its modal (category, source, budget) over the
//	          merchant's last 30 firefly transactions. If we find an
//	          entry above our confidence threshold, we use it. This
//	          handles the ~80% of repeat-merchant transactions
//	          (Zomato, Swiggy, your regular cafes).
//
//	Tier 2 — FTS5 vote. When the merchant lookup misses (new merchant,
//	          unusual narration), we BM25-rank firefly_txns by lexical
//	          similarity and vote on the top-K. Less precise than
//	          Tier 1 but still deterministic — handles "I've never
//	          shopped at THIS Cafe Coffee Day, but I've shopped at
//	          three other Cafe Coffee Days, all tagged 'Eating outside'."
//
// Tier 3 (LLM RAG) and Tier 4 (human review) live elsewhere.
//
// The classifier never writes to firefly. It writes to our SQLite
// (staged_fold_txns proposed_* + status). The push to firefly is a
// separate, explicitly-confirmed step in PR F.
package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// DefaultConfidenceThreshold is the cut-off above which a Tier-1 or
// Tier-2 decision is auto-accepted (status='ready_to_push'). Below it
// goes to 'needs_review' for human confirmation.
//
// 0.85 is empirical: with 30-row sample sizes per merchant, that's
// ~26/30 agreement — comfortably more than chance, low false-positive
// risk, while still auto-classifying the bulk.
const DefaultConfidenceThreshold = 0.85

// Tier indicates which classifier path produced the decision. Stored
// alongside the staged row so the UI can show "this was a Tier-1
// auto-classify" vs "Tier-3 LLM said so".
type Tier int

const (
	TierMerchantLookup Tier = 1
	TierFTSVote        Tier = 2
	TierLLM            Tier = 3
	TierHumanReview    Tier = 4
)

// Decision is what ClassifyOne produces. The pointer ID fields are
// nullable — firefly columns are optional (no category, no budget),
// and we mirror that.
type Decision struct {
	Tier       Tier
	Confidence float64
	// TxnType overrides the default fold-direction → firefly mapping.
	// Empty string defers to Pusher's foldTypeToFireflyType (which only
	// produces "withdrawal" or "deposit"); set to "transfer" by Tier 3
	// when both endpoints are user-owned asset accounts.
	TxnType                string
	DestinationAccountID   *int64
	DestinationAccountName string
	SourceAccountID        *int64
	SourceAccountName      string
	CategoryID             *int64
	CategoryName           string
	BudgetID               *int64
	BudgetName             string
	Description            string
	Tags                   []string
	Evidence               Evidence
}

// Evidence is the structured "why" — what the UI displays alongside
// the proposal so the human can see what the classifier saw. Stored as
// JSON in classifier_evidence_json.
type Evidence struct {
	Tier               Tier           `json:"tier"`
	MerchantNormalized string         `json:"merchant_normalized,omitempty"`
	LookupHit          *LookupHitView `json:"lookup_hit,omitempty"`
	FTSHits            []FTSHitView   `json:"fts_hits,omitempty"`
	Note               string         `json:"note,omitempty"`
	Tags               []string       `json:"tags,omitempty"`
}

// LookupHitView is a denormalised view of merchant_lookup for the UI.
type LookupHitView struct {
	DestinationAccountName string  `json:"destination_account_name"`
	CategoryName           string  `json:"category_name,omitempty"`
	SourceAccountName      string  `json:"source_account_name,omitempty"`
	BudgetName             string  `json:"budget_name,omitempty"`
	SampleSize             int     `json:"sample_size"`
	Confidence             float64 `json:"confidence"`
}

// FTSHitView captures one of the BM25-ranked firefly_txns returned by
// Tier 2. Stored so the UI can show the user "we matched these 5
// historical transactions".
type FTSHitView struct {
	FireflyID              int64     `json:"firefly_id"`
	Score                  float64   `json:"score"` // BM25 (more negative = better)
	DestinationAccountName string    `json:"destination_account_name"`
	CategoryName           string    `json:"category_name,omitempty"`
	Description            string    `json:"description"`
	Date                   time.Time `json:"date"`
}

// StagedRow is the slim view of staged_fold_txns the classifier needs.
// Defined here (rather than re-using a package-integration type) so
// the classifier subpackage compiles without dragging in the storage
// layer's full schema.
//
// RawPayload is the verbatim JSON fold returned for this transaction —
// it's passed through to Tier 3's LLM prompt so the model can use ALL
// the fields fold provides (account_id, merchant, kind, etc.), not
// just the subset our typed Go struct captures.
type StagedRow struct {
	FoldUUID          string
	Narration         string
	Mode              string
	Type              string
	MerchantExtracted string
	AmountPaise       int64
	Currency          string
	TxnTimestamp      string
	RawPayload        string
}

// Classifier runs the deterministic tiers (and optionally Tier-3 LLM
// when an LLM client is attached). Stateless beyond its dependencies;
// safe for concurrent use under a typical SQLite-bound rate (the DB
// has MaxOpenConns=1 so writes serialise).
type Classifier struct {
	db        *sql.DB
	log       *slog.Logger
	threshold float64
	ftsTopK   int
	llm       *llm.Client // nil → Tier-3 skipped
	// concurrency bounds how many rows classifyMatching processes in
	// parallel. <=1 (the default) is strictly sequential. The Tier-3 LLM
	// call dominates per-row latency and holds no DB connection, so a
	// bound of N overlaps N LLM calls while SQLite's single writer
	// serialises the cheap reads/writes — turning a multi-hour backfill
	// into minutes. Set via SetConcurrency (main wires it from config).
	concurrency int
}

// New constructs a Classifier with Tier-3 disabled. Use SetLLM to
// enable it. Pass DefaultConfidenceThreshold for production; tests
// can pass a lower threshold to assert below-bar behaviour without
// crafting borderline fixtures.
func New(db *sql.DB, log *slog.Logger, threshold float64, ftsTopK int) *Classifier {
	if threshold <= 0 {
		threshold = DefaultConfidenceThreshold
	}
	if ftsTopK <= 0 {
		ftsTopK = 10
	}
	return &Classifier{
		db:        db,
		log:       log.With("component", "classifier"),
		threshold: threshold,
		ftsTopK:   ftsTopK,
	}
}

// SetLLM attaches an OpenAI-compatible LLM client (DeepSeek by
// default). When set, ClassifyOne invokes Tier-3 synthesis after
// gathering deterministic hints from Tiers 1+2. Pass nil to disable.
func (c *Classifier) SetLLM(client *llm.Client) { c.llm = client }

// SetConcurrency sets how many rows classifyMatching processes in
// parallel (see the concurrency field). Values <=1 force the original
// strictly-sequential path. Safe to leave unset for tests; main wires
// it from TEXAS_FOLDEM_CLASSIFY_CONCURRENCY.
func (c *Classifier) SetConcurrency(n int) { c.concurrency = n }

// ClassifyOne runs the tiered pipeline against one staged row. Pure
// function — does not touch staged_fold_txns. Caller (Apply or the
// orchestrator endpoint) is responsible for persisting.
//
// Order, post LLM-synthesiser refactor:
//
//	Tier 1 (merchant_lookup) and Tier 2 (FTS5 vote) run as candidate
//	gatherers. Their results become HINTS for Tier 3 — they no
//	longer terminate the pipeline early.
//	Tier 3 (LLM synthesiser) is then ALWAYS invoked when an LLM
//	client is configured. It sees the raw fold payload, the user's
//	full account / category / budget / tag inventories, the FTS hits,
//	and the deterministic hints. It produces the final decision —
//	including type direction (withdrawal / deposit / transfer) and
//	source-account inference, both of which are too nuanced for the
//	deterministic tiers.
//
// Fallbacks:
//   - LLM not configured → use Tier 1, else Tier 2, else Tier 4.
//   - LLM configured but errored / declined → use Tier 1, else Tier 2,
//     else Tier 4. (Same priority — Tier 3 enriches, never breaks.)
func (c *Classifier) ClassifyOne(ctx context.Context, staged StagedRow) (Decision, error) {
	var tier1Hint, tier2Hint *Decision

	if d, ok, err := c.tierOneMerchantLookup(ctx, staged); err != nil {
		return Decision{}, fmt.Errorf("tier 1: %w", err)
	} else if ok {
		dCopy := d
		tier1Hint = &dCopy
	}

	if d, ok, err := c.tierTwoFTSVote(ctx, staged); err != nil {
		return Decision{}, fmt.Errorf("tier 2: %w", err)
	} else if ok {
		dCopy := d
		tier2Hint = &dCopy
	}

	if c.llm != nil {
		if d, ok, err := c.tierThreeLLM(ctx, staged, tier1Hint, tier2Hint); err != nil {
			// Tier-3 transport / parse / hallucination failure. If a
			// deterministic tier already produced an above-threshold
			// hint, that's not a mediocre fallback — it's deterministic
			// ground truth — so use it. Otherwise defer the row: stay
			// pending, next classify cycle retries when the LLM is
			// back. We deliberately do NOT route the row to
			// needs_review with a half-baked proposal: a real human
			// review should be reserved for cases where the model
			// genuinely couldn't decide, not for cases where it never
			// got a chance to try.
			if tier1Hint == nil && tier2Hint == nil {
				return Decision{}, fmt.Errorf("%w: %v", ErrLLMDeferred, err)
			}
			c.log.Warn("tier 3 LLM error; using deterministic hint",
				"fold_uuid", staged.FoldUUID, "err", err)
		} else if ok {
			return d, nil
		}
	}

	// Fallback ladder: prefer the more confident deterministic tier.
	if tier1Hint != nil {
		return *tier1Hint, nil
	}
	if tier2Hint != nil {
		return *tier2Hint, nil
	}
	return Decision{
		Tier:       TierHumanReview,
		Confidence: 0,
		Evidence: Evidence{
			Tier:               TierHumanReview,
			MerchantNormalized: staged.MerchantExtracted,
			Note:               "no Tier-1 lookup match, no Tier-2 FTS hits, no usable Tier-3 LLM response",
		},
	}, nil
}

// tierOneMerchantLookup looks up the staged row's normalised merchant
// in merchant_lookup. Returns (decision, true, nil) when above
// threshold; (zero, false, nil) when miss or below threshold; error
// only on unexpected DB failures.
func (c *Classifier) tierOneMerchantLookup(ctx context.Context, staged StagedRow) (Decision, bool, error) {
	if staged.MerchantExtracted == "" {
		return Decision{}, false, nil
	}
	row, err := c.queryMerchantLookup(ctx, staged.MerchantExtracted)
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, false, nil
	}
	if err != nil {
		return Decision{}, false, err
	}
	// Below threshold — let Tier 2 take a shot. We deliberately don't
	// return a low-confidence Tier-1 result here because Tier 2 might
	// produce a stronger signal for an unusual narration of a known
	// merchant.
	if row.Confidence < c.threshold {
		return Decision{}, false, nil
	}
	return Decision{
		Tier:                   TierMerchantLookup,
		Confidence:             row.Confidence,
		DestinationAccountID:   ptr(row.DestinationAccountID),
		DestinationAccountName: row.DestinationAccountName,
		SourceAccountID:        ptrIfSet(row.SourceAccountID, row.SourceAccountIDValid),
		SourceAccountName:      row.SourceAccountName,
		CategoryID:             ptrIfSet(row.CategoryID, row.CategoryIDValid),
		CategoryName:           row.CategoryName,
		BudgetID:               ptrIfSet(row.BudgetID, row.BudgetIDValid),
		BudgetName:             row.BudgetName,
		// Description from merchant_lookup is the most-recent description
		// the operator confirmed for this merchant. Surfacing it here
		// lets Tier-1 hits ship with a voice-matched title — same
		// signal the Tier-3 LLM would produce, but without an LLM call.
		// Empty is fine; the Pusher's fallback chain takes over.
		Description: row.ModalDescription,
		Evidence: Evidence{
			Tier:               TierMerchantLookup,
			MerchantNormalized: staged.MerchantExtracted,
			LookupHit: &LookupHitView{
				DestinationAccountName: row.DestinationAccountName,
				CategoryName:           row.CategoryName,
				SourceAccountName:      row.SourceAccountName,
				BudgetName:             row.BudgetName,
				SampleSize:             row.SampleSize,
				Confidence:             row.Confidence,
			},
		},
	}, true, nil
}

// merchantLookupRow is a private mirror of merchant_lookup, with
// nullability flags so we can distinguish "no source account" (column
// is NULL) from "id 0" (impossible but safe to model).
type merchantLookupRow struct {
	DestinationAccountID   int64
	DestinationAccountName string
	SourceAccountID        int64
	SourceAccountIDValid   bool
	SourceAccountName      string
	CategoryID             int64
	CategoryIDValid        bool
	CategoryName           string
	BudgetID               int64
	BudgetIDValid          bool
	BudgetName             string
	ModalDescription       string
	SampleSize             int
	Confidence             float64
}

func (c *Classifier) queryMerchantLookup(ctx context.Context, merchantNorm string) (merchantLookupRow, error) {
	var (
		row                     merchantLookupRow
		srcID, catID, bID       sql.NullInt64
		srcName, catName, bName sql.NullString
		modalDesc               sql.NullString
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT modal_destination_account_id, modal_destination_account_name,
		       modal_source_account_id, modal_source_account_name,
		       modal_category_id, modal_category_name,
		       modal_budget_id, modal_budget_name,
		       modal_description,
		       sample_size, confidence
		FROM merchant_lookup
		WHERE merchant_normalized = ?
	`, merchantNorm).Scan(
		&row.DestinationAccountID, &row.DestinationAccountName,
		&srcID, &srcName,
		&catID, &catName,
		&bID, &bName,
		&modalDesc,
		&row.SampleSize, &row.Confidence,
	)
	if err != nil {
		return row, err
	}
	row.SourceAccountID, row.SourceAccountIDValid = srcID.Int64, srcID.Valid
	if srcName.Valid {
		row.SourceAccountName = srcName.String
	}
	row.CategoryID, row.CategoryIDValid = catID.Int64, catID.Valid
	if catName.Valid {
		row.CategoryName = catName.String
	}
	row.BudgetID, row.BudgetIDValid = bID.Int64, bID.Valid
	if bName.Valid {
		row.BudgetName = bName.String
	}
	if modalDesc.Valid {
		row.ModalDescription = modalDesc.String
	}
	return row, nil
}

// tierTwoFTSVote runs an FTS5 query against firefly_txns_fts and
// votes the modal category/destination/source/budget across the top-K
// withdrawals. Confidence is the proportion of agreeing hits.
//
// We require the merchant to have been extracted; without that, FTS5
// over the full narration is too noisy (UPI handles, transaction refs
// etc. drown out the signal). Better to send to human review.
func (c *Classifier) tierTwoFTSVote(ctx context.Context, staged StagedRow) (Decision, bool, error) {
	merch := staged.MerchantExtracted
	if merch == "" {
		return Decision{}, false, nil
	}
	query := buildFTSQuery(merch)
	if query == "" {
		return Decision{}, false, nil
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
		  AND t.txn_type = ?
		ORDER BY score
		LIMIT ?
	`, query, fireflyTxnTypeFor(staged.Type), c.ftsTopK)
	if err != nil {
		return Decision{}, false, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	type hit struct {
		FireflyID              int64
		DestinationAccountID   sql.NullInt64
		DestinationAccountName sql.NullString
		SourceAccountID        sql.NullInt64
		SourceAccountName      sql.NullString
		CategoryID             sql.NullInt64
		CategoryName           sql.NullString
		BudgetID               sql.NullInt64
		BudgetName             sql.NullString
		Description            string
		Date                   time.Time
		Score                  float64
	}
	var hits []hit
	for rows.Next() {
		var h hit
		var dateStr string
		if err := rows.Scan(
			&h.FireflyID,
			&h.DestinationAccountID, &h.DestinationAccountName,
			&h.SourceAccountID, &h.SourceAccountName,
			&h.CategoryID, &h.CategoryName,
			&h.BudgetID, &h.BudgetName,
			&h.Description, &dateStr, &h.Score,
		); err != nil {
			return Decision{}, false, fmt.Errorf("scan: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
			h.Date = t
		} else if t, err := time.Parse("2006-01-02 15:04:05+00:00", dateStr); err == nil {
			h.Date = t
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return Decision{}, false, err
	}
	if len(hits) == 0 {
		return Decision{}, false, nil
	}

	// Vote on each field independently. Per-field confidence = mode count /
	// total hits. Overall decision confidence = min across non-null fields,
	// because we want all fields to be reasonably consistent before we
	// claim auto-classify.
	destID, destName, destConf := modeAccount(hits, func(h hit) (sql.NullInt64, sql.NullString) { return h.DestinationAccountID, h.DestinationAccountName })
	srcID, srcName, _ := modeAccount(hits, func(h hit) (sql.NullInt64, sql.NullString) { return h.SourceAccountID, h.SourceAccountName })
	catID, catName, catConf := modeAccount(hits, func(h hit) (sql.NullInt64, sql.NullString) { return h.CategoryID, h.CategoryName })
	bID, bName, _ := modeAccount(hits, func(h hit) (sql.NullInt64, sql.NullString) { return h.BudgetID, h.BudgetName })

	// destAcc is the most important field — without it, we have no
	// firefly target. Bail to review if even that's ambiguous.
	if !destID.Valid {
		return Decision{}, false, nil
	}

	// Aggregate confidence: weighted average of dest + category. Source
	// and budget can vary (different cards, no budget) without making
	// the classification wrong, so they don't bring the score down.
	overall := destConf
	if catID.Valid {
		// Soften by category agreement. If they're both 0.9 we get 0.9;
		// if dest is 0.9 but category is 0.5, we get 0.7.
		overall = (destConf + catConf) / 2
	}

	ftsHits := make([]FTSHitView, 0, len(hits))
	for _, h := range hits {
		ftsHits = append(ftsHits, FTSHitView{
			FireflyID:              h.FireflyID,
			Score:                  h.Score,
			DestinationAccountName: nullableToString(h.DestinationAccountName),
			CategoryName:           nullableToString(h.CategoryName),
			Description:            h.Description,
			Date:                   h.Date,
		})
	}

	d := Decision{
		Tier:                   TierFTSVote,
		Confidence:             overall,
		DestinationAccountID:   ptrIfSet(destID.Int64, destID.Valid),
		DestinationAccountName: nullableToString(destName),
		SourceAccountID:        ptrIfSet(srcID.Int64, srcID.Valid),
		SourceAccountName:      nullableToString(srcName),
		CategoryID:             ptrIfSet(catID.Int64, catID.Valid),
		CategoryName:           nullableToString(catName),
		BudgetID:               ptrIfSet(bID.Int64, bID.Valid),
		BudgetName:             nullableToString(bName),
		Evidence: Evidence{
			Tier:               TierFTSVote,
			MerchantNormalized: staged.MerchantExtracted,
			FTSHits:            ftsHits,
		},
	}
	return d, true, nil
}

// modeAccount returns the modal (id, name) pair across hits and the
// proportion of hits it represents.
func modeAccount[T any](hits []T, pick func(T) (sql.NullInt64, sql.NullString)) (sql.NullInt64, sql.NullString, float64) {
	type key struct {
		ID    int64
		Valid bool
	}
	counts := map[key]int{}
	names := map[key]string{}
	for _, h := range hits {
		id, name := pick(h)
		k := key{ID: id.Int64, Valid: id.Valid}
		counts[k]++
		if name.Valid {
			names[k] = name.String
		}
	}
	var bestKey key
	bestCount := -1
	for k, n := range counts {
		if n > bestCount || (n == bestCount && k.Valid && !bestKey.Valid) {
			// Tiebreak: prefer the non-NULL choice over NULL when counts
			// match, so a single null-row doesn't outvote a single id-row.
			bestKey = k
			bestCount = n
		}
	}
	conf := 0.0
	if len(hits) > 0 {
		conf = float64(bestCount) / float64(len(hits))
	}
	return sql.NullInt64{Int64: bestKey.ID, Valid: bestKey.Valid},
		sql.NullString{String: names[bestKey], Valid: bestKey.Valid},
		conf
}

// buildFTSQuery turns a normalised merchant string into an FTS5
// expression. We use phrase matching by default ("zomato pvt ltd" →
// docs containing all three tokens in order, then any order, then
// partial). FTS5 syntax docs: https://www.sqlite.org/fts5.html#full_text_query_syntax
//
// For multi-word merchants we use OR over individual tokens because
// firefly's destination_account_name often abbreviates ("Zomato" vs
// fold's "Zomato Limited"). False-positives go through the modal vote
// which surfaces real signals; OR doesn't hurt precision much.
func buildFTSQuery(merchantNorm string) string {
	tokens := strings.Fields(merchantNorm)
	if len(tokens) == 0 {
		return ""
	}
	// Quote each token individually to neutralise any FTS5 syntax chars
	// (e.g., a merchant name like "AT&T" or "M&S"). FTS5 phrase quoting
	// uses double quotes; double them up to escape an actual " in the
	// token (rare but possible).
	quoted := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.ReplaceAll(t, `"`, `""`)
		quoted = append(quoted, `"`+t+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// resetToPending wipes the classifier-owned fields on a staged row
// and sets status back to 'pending'. Called when the LLM is
// unavailable and the row is currently sitting in a non-pending state
// from a previous (potentially low-quality) classify run — the next
// cycle will re-evaluate it cleanly. Never touches confirmed_*
// columns; those are the operator's source of truth.
func (c *Classifier) resetToPending(ctx context.Context, foldUUID string) error {
	_, err := c.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET status                          = 'pending',
		    classifier_tier                 = NULL,
		    classifier_confidence           = NULL,
		    classifier_evidence_json        = NULL,
		    proposed_source_account_id      = NULL,
		    proposed_destination_account_id = NULL,
		    proposed_category_id            = NULL,
		    proposed_budget_id              = NULL,
		    proposed_description            = NULL,
		    proposed_tags_json              = NULL,
		    proposed_txn_type               = NULL,
		    classified_at                   = NULL,
		    updated_at                      = CURRENT_TIMESTAMP
		WHERE fold_uuid = ?
	`, foldUUID)
	return err
}

// ApplyDecision writes the decision back to staged_fold_txns. It also
// records classified_at, the chosen tier, and JSON-serialises the
// evidence for UI display. Status is set to 'ready_to_push' for
// confident classifications, 'needs_review' otherwise.
func (c *Classifier) ApplyDecision(ctx context.Context, foldUUID string, d Decision) error {
	status := "ready_to_push"
	// A new-name destination (no existing firefly id, just a proposed
	// name) always goes to review: pushing it will CREATE a firefly
	// expense account, so a human should eyeball the name first.
	newDestination := d.DestinationAccountID == nil && strings.TrimSpace(d.DestinationAccountName) != ""
	// A new-name source (no firefly asset, just the fold card name) also
	// goes to review: pushing it find-or-creates a firefly asset account,
	// so a human should eyeball it first.
	newSource := d.SourceAccountID == nil && strings.TrimSpace(d.SourceAccountName) != ""
	if d.Tier == TierHumanReview || d.Confidence < c.threshold || newDestination || newSource {
		status = "needs_review"
	}
	var proposedDestName any
	if newDestination {
		proposedDestName = strings.TrimSpace(d.DestinationAccountName)
	}
	// Always persist the source name when we have one — not just for a
	// new-name source. A source that RESOLVED to a freshly-created firefly
	// asset has an id but NO transaction history, so the display-time join
	// (firefly_txns → name) finds nothing; storing the name here is what
	// makes "Ixigo AU Bank Credit Card" render the moment it's matched.
	var proposedSrcName any
	if s := strings.TrimSpace(d.SourceAccountName); s != "" {
		proposedSrcName = s
	}
	evidenceJSON, err := json.Marshal(d.Evidence)
	if err != nil {
		return fmt.Errorf("marshal evidence: %w", err)
	}
	tagsJSON := ""
	if len(d.Tags) > 0 {
		if b, err := json.Marshal(d.Tags); err == nil {
			tagsJSON = string(b)
		}
	}

	_, err = c.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET status                            = ?,
		    classifier_tier                   = ?,
		    classifier_confidence             = ?,
		    classifier_evidence_json          = ?,
		    proposed_source_account_id        = ?,
		    proposed_source_account_name      = ?,
		    proposed_destination_account_id   = ?,
		    proposed_destination_account_name = ?,
		    proposed_category_id              = ?,
		    proposed_budget_id                = ?,
		    proposed_description              = ?,
		    proposed_tags_json                = COALESCE(NULLIF(?, ''), proposed_tags_json),
		    proposed_txn_type                 = ?,
		    classified_at                     = CURRENT_TIMESTAMP,
		    updated_at                        = CURRENT_TIMESTAMP
		WHERE fold_uuid = ?
	`,
		status,
		int(d.Tier),
		d.Confidence,
		string(evidenceJSON),
		nullableInt64(d.SourceAccountID),
		proposedSrcName,
		nullableInt64(d.DestinationAccountID),
		proposedDestName,
		nullableInt64(d.CategoryID),
		nullableInt64(d.BudgetID),
		nullableString(d.Description),
		tagsJSON,                  // proposed_tags_json (COALESCE preserves existing on empty)
		nullableString(d.TxnType), // proposed_txn_type
		foldUUID,
	)
	if err != nil {
		return fmt.Errorf("update staged_fold_txns: %w", err)
	}
	return nil
}

// ClassifyReport is what ClassifyPending returns. Counts surface in the
// /admin/classify HTTP response so an operator can see what changed.
type ClassifyReport struct {
	Examined       int           `json:"examined"`        // total pending rows seen
	AutoClassified int           `json:"auto_classified"` // status moved to ready_to_push
	NeedsReview    int           `json:"needs_review"`
	Deferred       int           `json:"deferred"` // LLM unavailable; row left pending for retry next cycle
	Tier1Hits      int           `json:"tier1_hits"`
	Tier2Hits      int           `json:"tier2_hits"`
	Duration       time.Duration `json:"duration"`
}

// ErrLLMDeferred is returned by ClassifyOne when the Tier-3 LLM
// errored AND no high-confidence deterministic tier (1 or 2) had a
// hit. The caller should leave the row's status untouched (stays
// `pending`) so the next classify cycle retries with a working LLM.
//
// Why defer instead of falling through to TierHumanReview: the user's
// review queue should be filled with rows where the model genuinely
// couldn't decide on quality grounds, not rows where the model never
// got a fair shot. Putting LLM-outage rows into needs_review without
// any proposal is mediocre output we'd rather avoid.
var ErrLLMDeferred = errors.New("classifier: llm unavailable; row deferred for retry")

// ClassifyPending iterates every status='pending' row in
// staged_fold_txns and applies a Decision. Idempotent — terminal
// statuses (pushed, skipped) are never touched.
func (c *Classifier) ClassifyPending(ctx context.Context) (ClassifyReport, error) {
	return c.classifyMatching(ctx, scopePendingOnly)
}

// ReclassifyPendingAndReview also re-runs classification on existing
// 'needs_review' rows where the human hasn't yet edited any fields
// (confirmed_* still NULL). This is the intended escape hatch for
// "I improved the classifier; re-run it on rows it previously punted
// to human review". Rows the human has touched are left alone.
func (c *Classifier) ReclassifyPendingAndReview(ctx context.Context) (ClassifyReport, error) {
	return c.classifyMatching(ctx, scopeIncludeReview)
}

// ReclassifyAllUnconfirmed extends ReclassifyPendingAndReview to also
// re-run classification on `ready_to_push` rows where the human
// hasn't yet edited any fields. The intended use is "I shipped a
// significant classifier upgrade and need every row that hasn't been
// human-touched to be re-evaluated against the new prompt" — for
// example, after wiring up the fold-accounts mirror so the LLM can
// finally ground source-account inference. Pushed/skipped rows
// (terminal) and any row where confirmed_* is non-NULL are skipped.
func (c *Classifier) ReclassifyAllUnconfirmed(ctx context.Context) (ClassifyReport, error) {
	return c.classifyMatching(ctx, scopeAllUnconfirmed)
}

type classifyScope int

const (
	scopePendingOnly classifyScope = iota
	scopeIncludeReview
	scopeAllUnconfirmed
)

func (c *Classifier) classifyMatching(ctx context.Context, scope classifyScope) (ClassifyReport, error) {
	// "no human edits" guard, applied to any non-pending row so we
	// never overwrite something the user has manually adjusted.
	const noHumanEdits = `confirmed_destination_account_id IS NULL
		                  AND confirmed_source_account_id      IS NULL
		                  AND confirmed_category_id            IS NULL
		                  AND confirmed_budget_id              IS NULL
		                  AND confirmed_description            IS NULL`
	var statusFilter string
	switch scope {
	case scopeIncludeReview:
		statusFilter = `(status = 'pending'
		                 OR (status = 'needs_review' AND ` + noHumanEdits + `))`
	case scopeAllUnconfirmed:
		statusFilter = `(status = 'pending'
		                 OR (status IN ('needs_review','ready_to_push') AND ` + noHumanEdits + `))`
	default:
		statusFilter = `status = 'pending'`
	}

	staged, err := c.fetchStagedForClassify(ctx, statusFilter)
	if err != nil {
		return ClassifyReport{}, err
	}
	return c.runClassify(ctx, staged)
}

// ReclassifyUUIDs re-runs the classifier on a specific set of staged rows
// — the "backfill these particular transactions" action behind the review
// UI's reclassify-selected button. Pushed rows are skipped (they already
// live in firefly; re-classifying would un-set their terminal status).
// confirmed_* human edits are preserved — only proposed_* and the row's
// status get recomputed.
func (c *Classifier) ReclassifyUUIDs(ctx context.Context, uuids []string) (ClassifyReport, error) {
	if len(uuids) == 0 {
		return ClassifyReport{}, nil
	}
	ph := make([]string, len(uuids))
	args := make([]any, len(uuids))
	for i, u := range uuids {
		ph[i] = "?"
		args[i] = u
	}
	where := "fold_uuid IN (" + strings.Join(ph, ",") + ") AND status <> 'pushed'"
	staged, err := c.fetchStagedForClassify(ctx, where, args...)
	if err != nil {
		return ClassifyReport{}, err
	}
	return c.runClassify(ctx, staged)
}

// fetchStagedForClassify loads the slim StagedRow set matching a WHERE
// clause against staged_fold_txns, newest first.
func (c *Classifier) fetchStagedForClassify(ctx context.Context, where string, args ...any) ([]StagedRow, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT fold_uuid, narration, mode, type, COALESCE(merchant_extracted,''),
		       amount_paise, currency, txn_timestamp, raw_payload
		FROM staged_fold_txns
		WHERE `+where+`
		ORDER BY txn_timestamp DESC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list staged: %w", err)
	}
	defer rows.Close()
	var staged []StagedRow
	for rows.Next() {
		var r StagedRow
		if err := rows.Scan(&r.FoldUUID, &r.Narration, &r.Mode, &r.Type, &r.MerchantExtracted,
			&r.AmountPaise, &r.Currency, &r.TxnTimestamp, &r.RawPayload); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		staged = append(staged, r)
	}
	return staged, rows.Err()
}

// runClassify processes a pre-fetched staged set through the tiered
// pipeline, persisting each decision and accumulating the report.
//
// Tier 3's LLM call dominates per-row latency (seconds); run serially, a
// backfill (hundreds of rows) takes hours. The LLM calls are independent
// network I/O and ClassifyOne holds no DB connection across them, so we
// fan out across a bounded worker pool: the slow LLM calls overlap while
// SQLite's single writer serialises the cheap reads/writes underneath.
// errgroup gives us the bound (SetLimit), first-error abort, and
// ctx-cancellation. concurrency <= 1 keeps a strictly-sequential path.
func (c *Classifier) runClassify(ctx context.Context, staged []StagedRow) (ClassifyReport, error) {
	start := time.Now()
	report := ClassifyReport{}
	var mu sync.Mutex // guards the shared report counters
	if c.concurrency <= 1 {
		for _, s := range staged {
			if err := c.classifyAndApply(ctx, s, &report, &mu); err != nil {
				return report, err
			}
		}
	} else {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(c.concurrency)
		for _, s := range staged {
			if gctx.Err() != nil {
				break // ctx cancelled or a sibling returned a fatal error
			}
			g.Go(func() error { return c.classifyAndApply(gctx, s, &report, &mu) })
		}
		if err := g.Wait(); err != nil {
			return report, err
		}
	}
	report.Duration = time.Since(start)
	c.log.Info("classify pending complete",
		"examined", report.Examined,
		"auto", report.AutoClassified,
		"review", report.NeedsReview,
		"deferred", report.Deferred,
		"tier1", report.Tier1Hits,
		"tier2", report.Tier2Hits,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
}

// classifyAndApply classifies one staged row and persists the decision,
// updating the shared report under mu. It is the per-row body shared by
// the sequential and parallel paths in classifyMatching, so both behave
// identically. Returns a non-nil error only for a fatal persistence
// failure (which aborts the whole batch); per-row classification issues
// (LLM deferral, a single bad row) are handled in place and return nil.
//
// Concurrency notes: ClassifyOne reads the DB and calls the LLM but holds
// no connection across the HTTP call, so parallel callers overlap their
// LLM latency while SQLite's single connection serialises every DB op.
// mu guards only the in-memory counters.
func (c *Classifier) classifyAndApply(ctx context.Context, s StagedRow, report *ClassifyReport, mu *sync.Mutex) error {
	mu.Lock()
	report.Examined++
	mu.Unlock()

	d, err := c.ClassifyOne(ctx, s)
	if err != nil {
		if errors.Is(err, ErrLLMDeferred) {
			// LLM unavailable. A previous (worse) run may have already
			// saved a low-quality decision to this row; reset to pending
			// so the next classify cycle treats it as fresh. The "no
			// human edits" guard upstream protects manual adjustments.
			if resetErr := c.resetToPending(ctx, s.FoldUUID); resetErr != nil {
				c.log.Warn("classify deferred; reset to pending failed",
					"fold_uuid", s.FoldUUID, "err", resetErr)
			}
			mu.Lock()
			report.Deferred++
			mu.Unlock()
			c.log.Info("classify deferred; LLM unavailable",
				"fold_uuid", s.FoldUUID, "reason", err)
			return nil
		}
		c.log.Warn("classify one failed; skipping", "fold_uuid", s.FoldUUID, "err", err)
		return nil
	}
	// Deterministic source resolution: for an OUTGOING txn fold's
	// account_id IS the paying (source) card — that's ground truth, not a
	// judgment call. We resolve it deterministically and OVERRIDE whatever
	// any tier (LLM included, and Tier-1's modal source) guessed. This is
	// what makes a hallucinated source — an "AU Ixigo" charge shown on
	// "Axis Bank Ace" — structurally impossible: no tier gets a vote on
	// the card.
	//   - fold card → unique firefly asset ⇒ resolved id (pushable)
	//   - fold card → no confident match   ⇒ propose the fold card name,
	//       which ApplyDecision routes to review as a new/unmatched card
	//       (never a wrong existing asset)
	// For INCOMING the account_id is the *receiving* account — a
	// destination concern — so we leave source to the LLM's revenue pick.
	if s.Type == "OUTGOING" {
		if fa, _ := lookupFoldAccountForStaged(ctx, c.db, s.RawPayload); fa != nil && fa.Name != "" {
			assets, _ := listFireflyAssetsFromMirror(ctx, c.db)
			if id, name, ok := matchFoldCardToFireflyAsset(fa, assets); ok {
				d.SourceAccountID = &id
				d.SourceAccountName = name
			} else {
				d.SourceAccountID = nil
				d.SourceAccountName = fa.Name
			}
		}
	}
	if err := c.ApplyDecision(ctx, s.FoldUUID, d); err != nil {
		return fmt.Errorf("apply %s: %w", s.FoldUUID, err)
	}
	mu.Lock()
	switch d.Tier {
	case TierMerchantLookup:
		report.Tier1Hits++
	case TierFTSVote:
		report.Tier2Hits++
	}
	if d.Tier == TierHumanReview || d.Confidence < c.threshold {
		report.NeedsReview++
	} else {
		report.AutoClassified++
	}
	mu.Unlock()
	return nil
}

// helpers ////////////////////////////////////////////////////////////////

func ptr[T any](v T) *T { return &v }

func ptrIfSet(v int64, ok bool) *int64 {
	if !ok {
		return nil
	}
	return ptr(v)
}

func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableToString(n sql.NullString) string {
	if n.Valid {
		return n.String
	}
	return ""
}
