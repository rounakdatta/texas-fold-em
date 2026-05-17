package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
)

// seedTestDB builds a fresh staging.db with a known firefly corpus and
// merchant_lookup populated for two merchants. The FTS5 + lookup tables
// are populated by hand here to avoid pulling in the full sync code —
// we want the classifier under test, not the syncer.
func seedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	idb, err := integration.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })

	db := idb.DB

	// Three firefly txns: two for "zomato" (same category), one for
	// "cake palace" (different category).
	for _, q := range []string{
		`INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		     source_account_id, source_account_name,
		     destination_account_id, destination_account_name, destination_account_name_normalized,
		     category_id, category_name, budget_id, budget_name, description, tags_json)
		 VALUES
		     (701, 7001, 'withdrawal',  97833, 'INR', '2026-05-01',
		      1, 'HDFC Card',
		      11, 'Zomato', 'zomato',
		      5, 'Eating outside', NULL, NULL, 'lunch order', '[]'),
		     (702, 7002, 'withdrawal',  41658, 'INR', '2026-05-07',
		      1, 'HDFC Card',
		      11, 'Zomato', 'zomato',
		      5, 'Eating outside', NULL, NULL, 'dinner order', '[]'),
		     (703, 7003, 'withdrawal',   5000, 'INR', '2026-05-04',
		      1, 'HDFC Card',
		      12, 'Cake Palace', 'cake palace',
		      6, 'Snacks',         NULL, NULL, 'birthday cake', '[]')
		`,
		// merchant_lookup: zomato has 2/2 = 1.0 confidence, cake palace 1/1 = 1.0.
		// zomato's modal_description is the user's most recent voice-matched
		// title for this merchant — so Tier-1 ships that on its Decision.
		`INSERT INTO merchant_lookup (
		     merchant_normalized,
		     modal_destination_account_id, modal_destination_account_name,
		     modal_source_account_id, modal_source_account_name,
		     modal_category_id, modal_category_name,
		     modal_budget_id, modal_budget_name,
		     modal_description,
		     sample_size, confidence, last_seen)
		 VALUES
		     ('zomato',      11, 'Zomato',      1, 'HDFC Card', 5, 'Eating outside', NULL, NULL, 'Lunch with Tushar', 2, 1.0, '2026-05-07'),
		     ('cake palace', 12, 'Cake Palace', 1, 'HDFC Card', 6, 'Snacks',         NULL, NULL, NULL,                1, 1.0, '2026-05-04')
		`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return db
}

// TestClassifyOne_Tier1Hit confirms that a known-merchant fold txn
// resolves via merchant_lookup with full confidence.
func TestClassifyOne_Tier1Hit(t *testing.T) {
	db := seedTestDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "u1",
		Narration:         "CARD/x/Zomato/Rs/100/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "zomato",
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if d.Tier != TierMerchantLookup {
		t.Errorf("expected Tier 1, got %d", d.Tier)
	}
	if d.Confidence < 0.99 {
		t.Errorf("expected confidence ~1.0, got %f", d.Confidence)
	}
	if d.DestinationAccountID == nil || *d.DestinationAccountID != 11 {
		t.Errorf("destination = %v, want 11", d.DestinationAccountID)
	}
	if d.CategoryID == nil || *d.CategoryID != 5 {
		t.Errorf("category = %v, want 5", d.CategoryID)
	}
	if d.Evidence.LookupHit == nil {
		t.Error("expected LookupHit in evidence")
	}
	// Tier-1 must surface the modal_description from the lookup table
	// so push.go's description fallback chain picks up a voice-matched
	// title without needing an LLM call.
	if d.Description != "Lunch with Tushar" {
		t.Errorf("Decision.Description = %q, want %q (seeded modal_description)",
			d.Description, "Lunch with Tushar")
	}
}

// TestClassifyOne_Tier2Hit covers the FTS fallback for a merchant
// extracted from the narration but absent from merchant_lookup.
func TestClassifyOne_Tier2Hit(t *testing.T) {
	db := seedTestDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)

	// "Zomato Pvt Ltd" is NOT in merchant_lookup (only "zomato" is),
	// so Tier 1 misses. FTS should still find the two zomato firefly
	// rows via the "zomato" token.
	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "u-novel",
		Narration:         "CARD/x/Zomato Pvt Ltd/Rs/200/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "zomato pvt ltd",
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if d.Tier != TierFTSVote {
		t.Errorf("expected Tier 2, got %d", d.Tier)
	}
	if d.CategoryID == nil || *d.CategoryID != 5 {
		t.Errorf("category = %v, want 5 (Eating outside, modal of FTS hits)", d.CategoryID)
	}
	if len(d.Evidence.FTSHits) == 0 {
		t.Error("expected FTSHits in evidence")
	}
}

// TestClassifyOne_NoMerchantNoFTS returns Tier 4 / human review.
func TestClassifyOne_NoMerchantNoFTS(t *testing.T) {
	db := seedTestDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "u-mystery",
		Narration:         "weird narration with no recognised pattern",
		Mode:              "OTHERS",
		Type:              "OUTGOING",
		MerchantExtracted: "",
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if d.Tier != TierHumanReview {
		t.Errorf("expected Tier 4 (human review), got %d", d.Tier)
	}
	if d.Confidence != 0 {
		t.Errorf("expected confidence 0 for human review, got %f", d.Confidence)
	}
}

// TestClassifyPending_OrchestratesAndPersists asserts the end-to-end
// orchestrator: staged rows pending → classifier runs → status updates
// + proposed_* fields written + classifier_evidence_json populated.
func TestClassifyPending_OrchestratesAndPersists(t *testing.T) {
	db := seedTestDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)

	// Insert two staged fold txns: one matches Tier 1, one mystery.
	for _, q := range []string{
		`INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		     mode, type, narration, merchant_extracted, status)
		 VALUES
		     ('zoma-1', '{}', 50000, 'INR', '2026-05-08T10:00:00Z',
		      'CARD', 'OUTGOING', 'CARD/x/Zomato/Rs/500/OUTGOING', 'zomato', 'pending')
		`,
		`INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		     mode, type, narration, merchant_extracted, status)
		 VALUES
		     ('myst-1', '{}', 12345, 'INR', '2026-05-08T11:00:00Z',
		      'OTHERS', 'OUTGOING', 'unknown thing', '', 'pending')
		`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("insert staged: %v", err)
		}
	}

	report, err := c.ClassifyPending(context.Background())
	if err != nil {
		t.Fatalf("ClassifyPending: %v", err)
	}
	if report.Examined != 2 {
		t.Errorf("examined=%d, want 2", report.Examined)
	}
	if report.AutoClassified != 1 {
		t.Errorf("auto=%d, want 1", report.AutoClassified)
	}
	if report.NeedsReview != 1 {
		t.Errorf("review=%d, want 1", report.NeedsReview)
	}
	if report.Tier1Hits != 1 {
		t.Errorf("tier1=%d, want 1", report.Tier1Hits)
	}

	// Re-running ClassifyPending should examine ZERO rows — both have
	// transitioned away from 'pending'.
	report2, err := c.ClassifyPending(context.Background())
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if report2.Examined != 0 {
		t.Errorf("re-run examined=%d, want 0", report2.Examined)
	}

	// Spot-check the persisted row for zoma-1.
	var (
		status     string
		tier       int
		confidence float64
		catID      sql.NullInt64
		evidence   string
	)
	err = db.QueryRow(`
		SELECT status, classifier_tier, classifier_confidence,
		       proposed_category_id, classifier_evidence_json
		FROM staged_fold_txns WHERE fold_uuid='zoma-1'`).
		Scan(&status, &tier, &confidence, &catID, &evidence)
	if err != nil {
		t.Fatalf("read zoma-1: %v", err)
	}
	if status != "ready_to_push" {
		t.Errorf("status=%q, want ready_to_push", status)
	}
	if tier != int(TierMerchantLookup) {
		t.Errorf("tier=%d, want %d", tier, TierMerchantLookup)
	}
	if !catID.Valid || catID.Int64 != 5 {
		t.Errorf("proposed_category_id=%v, want 5", catID)
	}
	// Evidence JSON should round-trip back to a structured Evidence.
	var ev Evidence
	if err := json.Unmarshal([]byte(evidence), &ev); err != nil {
		t.Errorf("evidence json: %v", err)
	}
	if ev.Tier != TierMerchantLookup {
		t.Errorf("evidence tier=%d, want %d", ev.Tier, TierMerchantLookup)
	}

	// Mystery row should be needs_review with tier 4.
	var mystStatus string
	var mystTier int
	if err := db.QueryRow(`SELECT status, classifier_tier FROM staged_fold_txns WHERE fold_uuid='myst-1'`).
		Scan(&mystStatus, &mystTier); err != nil {
		t.Fatalf("read myst-1: %v", err)
	}
	if mystStatus != "needs_review" {
		t.Errorf("myst-1 status=%q, want needs_review", mystStatus)
	}
	if mystTier != int(TierHumanReview) {
		t.Errorf("myst-1 tier=%d, want %d", mystTier, TierHumanReview)
	}
}

// TestBuildFTSQuery covers the FTS5 escaping. Borderline merchant names
// (apostrophes, ampersands, Unicode) should not produce malformed FTS5
// expressions.
func TestBuildFTSQuery(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"zomato":          `"zomato"`,
		"cake palace":     `"cake" OR "palace"`,
		"at&t":            `"at&t"`,
		"m&s food":        `"m&s" OR "food"`,
		`some "quoted"`:   `"some" OR """quoted"""`,
	}
	for in, want := range cases {
		if got := buildFTSQuery(in); got != want {
			t.Errorf("buildFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
