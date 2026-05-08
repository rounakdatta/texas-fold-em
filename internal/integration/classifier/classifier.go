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
	"time"
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
	Tier                   Tier
	Confidence             float64
	DestinationAccountID   *int64
	DestinationAccountName string
	SourceAccountID        *int64
	SourceAccountName      string
	CategoryID             *int64
	CategoryName           string
	BudgetID               *int64
	BudgetName             string
	Description            string
	Evidence               Evidence
}

// Evidence is the structured "why" — what the UI displays alongside
// the proposal so the human can see what the classifier saw. Stored as
// JSON in classifier_evidence_json.
type Evidence struct {
	Tier               Tier              `json:"tier"`
	MerchantNormalized string            `json:"merchant_normalized,omitempty"`
	LookupHit          *LookupHitView    `json:"lookup_hit,omitempty"`
	FTSHits            []FTSHitView      `json:"fts_hits,omitempty"`
	Note               string            `json:"note,omitempty"`
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
type StagedRow struct {
	FoldUUID          string
	Narration         string
	Mode              string
	Type              string
	MerchantExtracted string
}

// Classifier runs the deterministic tiers. Stateless beyond its
// dependencies; safe for concurrent use under a typical SQLite-bound
// rate (the DB has MaxOpenConns=1 so writes serialise).
type Classifier struct {
	db        *sql.DB
	log       *slog.Logger
	threshold float64
	ftsTopK   int
}

// New constructs a Classifier. Pass DefaultConfidenceThreshold for
// production; tests can pass a lower threshold to assert below-bar
// behaviour without crafting borderline fixtures.
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

// ClassifyOne runs the tiered pipeline against one staged row. Pure
// function — does not touch staged_fold_txns. Caller (Apply or the
// orchestrator endpoint) is responsible for persisting.
//
// Order: Tier 1 (lookup) → Tier 2 (FTS) → Tier 4 (needs_review).
// Tier 3 (LLM RAG) lands in PR E and slots in between.
func (c *Classifier) ClassifyOne(ctx context.Context, staged StagedRow) (Decision, error) {
	if d, ok, err := c.tierOneMerchantLookup(ctx, staged); err != nil {
		return Decision{}, fmt.Errorf("tier 1: %w", err)
	} else if ok {
		return d, nil
	}
	if d, ok, err := c.tierTwoFTSVote(ctx, staged); err != nil {
		return Decision{}, fmt.Errorf("tier 2: %w", err)
	} else if ok {
		return d, nil
	}
	// Nothing matched; queue for human review.
	return Decision{
		Tier:       TierHumanReview,
		Confidence: 0,
		Evidence: Evidence{
			Tier:               TierHumanReview,
			MerchantNormalized: staged.MerchantExtracted,
			Note:               "no Tier-1 lookup match, no Tier-2 FTS hits",
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
	DestinationAccountID      int64
	DestinationAccountName    string
	SourceAccountID           int64
	SourceAccountIDValid      bool
	SourceAccountName         string
	CategoryID                int64
	CategoryIDValid           bool
	CategoryName              string
	BudgetID                  int64
	BudgetIDValid             bool
	BudgetName                string
	SampleSize                int
	Confidence                float64
}

func (c *Classifier) queryMerchantLookup(ctx context.Context, merchantNorm string) (merchantLookupRow, error) {
	var (
		row              merchantLookupRow
		srcID, catID, bID sql.NullInt64
		srcName, catName, bName sql.NullString
	)
	err := c.db.QueryRowContext(ctx, `
		SELECT modal_destination_account_id, modal_destination_account_name,
		       modal_source_account_id, modal_source_account_name,
		       modal_category_id, modal_category_name,
		       modal_budget_id, modal_budget_name,
		       sample_size, confidence
		FROM merchant_lookup
		WHERE merchant_normalized = ?
	`, merchantNorm).Scan(
		&row.DestinationAccountID, &row.DestinationAccountName,
		&srcID, &srcName,
		&catID, &catName,
		&bID, &bName,
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
		  AND t.txn_type = 'withdrawal'
		ORDER BY score
		LIMIT ?
	`, query, c.ftsTopK)
	if err != nil {
		return Decision{}, false, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	type hit struct {
		FireflyID                int64
		DestinationAccountID     sql.NullInt64
		DestinationAccountName   sql.NullString
		SourceAccountID          sql.NullInt64
		SourceAccountName        sql.NullString
		CategoryID               sql.NullInt64
		CategoryName             sql.NullString
		BudgetID                 sql.NullInt64
		BudgetName               sql.NullString
		Description              string
		Date                     time.Time
		Score                    float64
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

// ApplyDecision writes the decision back to staged_fold_txns. It also
// records classified_at, the chosen tier, and JSON-serialises the
// evidence for UI display. Status is set to 'ready_to_push' for
// confident classifications, 'needs_review' otherwise.
func (c *Classifier) ApplyDecision(ctx context.Context, foldUUID string, d Decision) error {
	status := "ready_to_push"
	if d.Tier == TierHumanReview || d.Confidence < c.threshold {
		status = "needs_review"
	}
	evidenceJSON, err := json.Marshal(d.Evidence)
	if err != nil {
		return fmt.Errorf("marshal evidence: %w", err)
	}
	_, err = c.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET status                            = ?,
		    classifier_tier                   = ?,
		    classifier_confidence             = ?,
		    classifier_evidence_json          = ?,
		    proposed_source_account_id        = ?,
		    proposed_destination_account_id   = ?,
		    proposed_category_id              = ?,
		    proposed_budget_id                = ?,
		    proposed_description              = ?,
		    classified_at                     = CURRENT_TIMESTAMP,
		    updated_at                        = CURRENT_TIMESTAMP
		WHERE fold_uuid = ?
	`,
		status,
		int(d.Tier),
		d.Confidence,
		string(evidenceJSON),
		nullableInt64(d.SourceAccountID),
		nullableInt64(d.DestinationAccountID),
		nullableInt64(d.CategoryID),
		nullableInt64(d.BudgetID),
		nullableString(d.Description),
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
	Examined     int           `json:"examined"`     // total pending rows seen
	AutoClassified int         `json:"auto_classified"` // status moved to ready_to_push
	NeedsReview  int           `json:"needs_review"`
	Tier1Hits    int           `json:"tier1_hits"`
	Tier2Hits    int           `json:"tier2_hits"`
	Duration     time.Duration `json:"duration"`
}

// ClassifyPending iterates every status='pending' row in
// staged_fold_txns and applies a Decision. Idempotent in the sense
// that a row's terminal status (ready_to_push / needs_review) never
// reverts to pending — re-running classify only acts on rows that are
// still 'pending'.
func (c *Classifier) ClassifyPending(ctx context.Context) (ClassifyReport, error) {
	start := time.Now()
	report := ClassifyReport{}

	rows, err := c.db.QueryContext(ctx, `
		SELECT fold_uuid, narration, mode, type, COALESCE(merchant_extracted,'')
		FROM staged_fold_txns
		WHERE status = 'pending'
		ORDER BY txn_timestamp DESC
	`)
	if err != nil {
		return report, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()

	var staged []StagedRow
	for rows.Next() {
		var r StagedRow
		if err := rows.Scan(&r.FoldUUID, &r.Narration, &r.Mode, &r.Type, &r.MerchantExtracted); err != nil {
			return report, fmt.Errorf("scan: %w", err)
		}
		staged = append(staged, r)
	}
	if err := rows.Err(); err != nil {
		return report, err
	}

	for _, s := range staged {
		report.Examined++
		d, err := c.ClassifyOne(ctx, s)
		if err != nil {
			c.log.Warn("classify one failed; skipping", "fold_uuid", s.FoldUUID, "err", err)
			continue
		}
		if err := c.ApplyDecision(ctx, s.FoldUUID, d); err != nil {
			return report, fmt.Errorf("apply %s: %w", s.FoldUUID, err)
		}
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
	}
	report.Duration = time.Since(start)
	c.log.Info("classify pending complete",
		"examined", report.Examined,
		"auto", report.AutoClassified,
		"review", report.NeedsReview,
		"tier1", report.Tier1Hits,
		"tier2", report.Tier2Hits,
		"duration_ms", report.Duration.Milliseconds(),
	)
	return report, nil
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
