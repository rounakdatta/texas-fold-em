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

// TestClassify_SourceNameFromFoldAccount: for an OUTGOING txn whose
// fold account_id maps to a known card but no firefly asset matches (and
// no tier pins a source), the classifier surfaces the fold card name as
// the proposed source — never blank — and routes the row to review.
func TestClassify_SourceNameFromFoldAccount(t *testing.T) {
	db := seedTestDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10) // no LLM

	// A card the user has on fold, with NO matching firefly asset.
	if _, err := db.Exec(`
		INSERT INTO fold_accounts (fold_account_id, kind, name, provider, network, last_four, raw_payload, is_closed)
		VALUES ('au-test','CREDIT_CARD','AU Ixigo ****9179','AU','Visa','9179','{}',0)`); err != nil {
		t.Fatalf("seed fold_accounts: %v", err)
	}
	// An OUTGOING txn paid on that card; merchant matches nothing in the
	// corpus → no Tier-1/2 hit, no LLM → Tier 4 with no source.
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status)
		VALUES ('src-1','{"account_id":"au-test"}',21200,'USD','2026-06-26T15:19:00Z',
		    'CARD','OUTGOING','CARD/x/DEEPSEEK/USD/2.12/OUTGOING','deepseek','pending')`); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	if _, err := c.ClassifyPending(context.Background()); err != nil {
		t.Fatalf("ClassifyPending: %v", err)
	}

	var status string
	var srcID sql.NullInt64
	var srcName sql.NullString
	if err := db.QueryRow(`
		SELECT status, proposed_source_account_id, proposed_source_account_name
		FROM staged_fold_txns WHERE fold_uuid='src-1'`).Scan(&status, &srcID, &srcName); err != nil {
		t.Fatal(err)
	}
	if srcID.Valid {
		t.Errorf("expected no source id (no firefly asset exists), got %d", srcID.Int64)
	}
	if srcName.String != "AU Ixigo ****9179" {
		t.Errorf("proposed_source_account_name=%q, want the fold card name", srcName.String)
	}
	if status != "needs_review" {
		t.Errorf("status=%q, want needs_review (new-name source)", status)
	}
}

// TestClassify_SourceResolvesFromFireflyMirror is the definitive
// regression test for the source-hallucination bug. The fold card
// (AU Ixigo) exists as a firefly asset in the mirror; the row's merchant
// (zomato) has a Tier-1 lookup whose modal source is a DIFFERENT card
// ("HDFC Card", id 1). The deterministic resolver must OVERRIDE that and
// set the source to the AU asset (1314) the card actually maps to — never
// the merchant's usual card, never the Axis decoy.
func TestClassify_SourceResolvesFromFireflyMirror(t *testing.T) {
	db := seedTestDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10) // no LLM

	// fold card the charge was actually made on.
	if _, err := db.Exec(`
		INSERT INTO fold_accounts (fold_account_id, kind, name, provider, network, last_four, raw_payload, is_closed)
		VALUES ('au-test','CREDIT_CARD','AU Ixigo ****9179','AU','Visa','9179','{}',0)`); err != nil {
		t.Fatalf("seed fold_accounts: %v", err)
	}
	// firefly's real asset list (mirror) — incl. the AU card plus decoys.
	if _, err := db.Exec(`
		INSERT INTO firefly_accounts (firefly_id, name, type, account_role, account_number, active, raw_payload)
		VALUES (1314,'Ixigo AU Bank Credit Card','asset','ccAsset','40697750350291',1,'{}'),
		       (163,'Axis Bank Ace Credit Card','asset','ccAsset','47001101015328',1,'{}'),
		       (954,'Scapia Federal Bank Credit Card','asset','ccAsset','40298600009717',1,'{}')`); err != nil {
		t.Fatalf("seed firefly_accounts: %v", err)
	}
	// A zomato charge (Tier-1 will claim it, modal source = HDFC Card id 1)
	// but paid on the AU card per fold's account_id.
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status)
		VALUES ('mirror-1','{"account_id":"au-test"}',5000,'INR','2026-06-26T15:19:00Z',
		    'CARD','OUTGOING','CARD/x/Zomato/Rs/50/OUTGOING','zomato','pending')`); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	if _, err := c.ClassifyPending(context.Background()); err != nil {
		t.Fatalf("ClassifyPending: %v", err)
	}

	var srcID sql.NullInt64
	var srcName sql.NullString
	var destID sql.NullInt64
	if err := db.QueryRow(`
		SELECT proposed_source_account_id, proposed_source_account_name, proposed_destination_account_id
		FROM staged_fold_txns WHERE fold_uuid='mirror-1'`).Scan(&srcID, &srcName, &destID); err != nil {
		t.Fatal(err)
	}
	if !srcID.Valid || srcID.Int64 != 1314 {
		t.Errorf("source id = %v, want 1314 (Ixigo AU) — deterministic resolver must override Tier-1's modal source", srcID)
	}
	if srcName.String != "Ixigo AU Bank Credit Card" {
		t.Errorf("source name = %q, want %q", srcName.String, "Ixigo AU Bank Credit Card")
	}
	// Tier-1's destination (Zomato, id 11) must be preserved — only the
	// source is overridden.
	if !destID.Valid || destID.Int64 != 11 {
		t.Errorf("destination id = %v, want 11 (Tier-1 Zomato preserved)", destID)
	}
}

// TestReclassifyUUIDs re-runs the classifier on a specific row set:
// non-pushed rows are reprocessed, pushed rows are skipped (terminal,
// already in firefly), and unknown ids are ignored.
func TestReclassifyUUIDs(t *testing.T) {
	db := seedTestDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10) // no LLM → deterministic

	// A needs_review zomato row (Tier-1 lookup will claim it) + a pushed
	// zomato row that must be left untouched.
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status)
		VALUES ('rc-1','{}',5000,'INR','2026-05-08T10:00:00Z','CARD','OUTGOING','x','zomato','needs_review'),
		       ('rc-pushed','{}',6000,'INR','2026-05-08T11:00:00Z','CARD','OUTGOING','x','zomato','pushed')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rep, err := c.ReclassifyUUIDs(context.Background(), []string{"rc-1", "rc-pushed", "does-not-exist"})
	if err != nil {
		t.Fatalf("ReclassifyUUIDs: %v", err)
	}
	if rep.Examined != 1 {
		t.Errorf("examined=%d, want 1 (pushed + missing skipped)", rep.Examined)
	}

	// rc-1 → Tier-1 lookup hit (zomato is in merchant_lookup), ready_to_push.
	var tier int
	var status string
	if err := db.QueryRow(`SELECT classifier_tier, status FROM staged_fold_txns WHERE fold_uuid='rc-1'`).Scan(&tier, &status); err != nil {
		t.Fatal(err)
	}
	if tier != int(TierMerchantLookup) {
		t.Errorf("rc-1 tier=%d, want %d", tier, TierMerchantLookup)
	}
	if status != "ready_to_push" {
		t.Errorf("rc-1 status=%q, want ready_to_push", status)
	}

	// rc-pushed is untouched.
	var pStatus string
	if err := db.QueryRow(`SELECT status FROM staged_fold_txns WHERE fold_uuid='rc-pushed'`).Scan(&pStatus); err != nil {
		t.Fatal(err)
	}
	if pStatus != "pushed" {
		t.Errorf("rc-pushed status=%q, want pushed (untouched)", pStatus)
	}

	// Empty set → no-op.
	if rep2, err := c.ReclassifyUUIDs(context.Background(), nil); err != nil || rep2.Examined != 0 {
		t.Errorf("empty reclassify: rep=%+v err=%v", rep2, err)
	}
}

// TestBuildFTSQuery covers the FTS5 escaping. Borderline merchant names
// (apostrophes, ampersands, Unicode) should not produce malformed FTS5
// expressions.
func TestBuildFTSQuery(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"zomato":        `"zomato"`,
		"cake palace":   `"cake" OR "palace"`,
		"at&t":          `"at&t"`,
		"m&s food":      `"m&s" OR "food"`,
		`some "quoted"`: `"some" OR """quoted"""`,
	}
	for in, want := range cases {
		if got := buildFTSQuery(in); got != want {
			t.Errorf("buildFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
