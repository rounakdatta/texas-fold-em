package classifier

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
)

// seedRefundDB extends seedTestDB (whose merchant_lookup maps "zomato" to a
// WITHDRAWAL: destination Zomato 11, source HDFC Card 1) with two fold
// cards, their firefly assets, Zomato's revenue twin and the Refund category.
func seedRefundDB(t *testing.T) *sql.DB {
	t.Helper()
	db := seedTestDB(t)
	for _, q := range []string{
		`INSERT INTO fold_accounts (fold_account_id, kind, name, provider, network, last_four, raw_payload, is_closed) VALUES
		   ('scapia-acc','CREDIT_CARD','Scapia ****1743','Federal Bank','Visa','1743','{}',0),
		   ('au-acc','CREDIT_CARD','AU Ixigo ****9179','AU','Visa','9179','{}',0),
		   ('hdfc-acc','BANK','HDFC Bank ****5684','HDFC','','5684','{}',0)`,
		`INSERT INTO firefly_accounts (firefly_id, name, type, account_role, account_number, active, raw_payload) VALUES
		   (954,'Scapia Federal Bank Credit Card','asset','ccAsset','40298600001743',1,'{}'),
		   (1314,'Ixigo AU Bank Credit Card','asset','ccAsset','40697750359179',1,'{}'),
		   (12,'HDFC Bank','asset','defaultAsset','50100005684',1,'{}'),
		   (11,'Zomato','expense',NULL,NULL,1,'{}'),
		   (2011,'Zomato','revenue',NULL,NULL,1,'{}')`,
		// The user's Refund category, as firefly_txns knows it.
		`INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		   source_account_id, source_account_name, destination_account_id, destination_account_name,
		   destination_account_name_normalized, category_id, category_name, description, tags_json)
		 VALUES (801, 8001, 'deposit', 200, 'INR', '2025-11-12T19:56:00+00:00', 2040, 'Google Play',
		   954, 'Scapia Federal Bank Credit Card', 'scapia federal bank credit card', 48, 'Refund',
		   'Refund for Google One verification charges', '[]')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed refunds: %v", err)
		}
	}
	return db
}

// stagePurchase stages an OUTGOING card spend the way the classifier leaves
// one (source resolved to the card, destination to the merchant).
func stagePurchase(t *testing.T, db *sql.DB, uuid, account string, paise int64, ts, desc, tags string, srcAsset int64) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
		    narration, merchant_extracted, status, proposed_source_account_id, proposed_destination_account_id,
		    proposed_description, proposed_tags_json)
		VALUES (?, ?, ?, 'INR', ?, 'CARD', 'OUTGOING', 'CARD/x/Zomato/₹/x/OUTGOING', 'zomato', 'ready_to_push', ?, 11, ?, NULLIF(?,''))`,
		uuid, `{"account_id":"`+account+`"}`, paise, ts, srcAsset, desc, tags); err != nil {
		t.Fatalf("stage purchase: %v", err)
	}
}

func stageRefund(t *testing.T, db *sql.DB, uuid, raw string, paise int64, ts string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
		    narration, merchant_extracted, status)
		VALUES (?, ?, ?, 'INR', ?, 'CARD', 'INCOMING', 'CARD/y/Zomato/₹/x/INCOMING/Refund Received!', 'zomato', 'pending')`,
		uuid, raw, paise, ts); err != nil {
		t.Fatalf("stage refund: %v", err)
	}
}

type proposal struct {
	status, txnType, desc, refundOf, tags string
	tier                                  int
	conf                                  float64
	src, dst, cat                         sql.NullInt64
	srcName                               sql.NullString
}

func readProposal(t *testing.T, db *sql.DB, uuid string) proposal {
	t.Helper()
	var p proposal
	var txnType, desc, ref, tags sql.NullString
	var tier sql.NullInt64
	var conf sql.NullFloat64
	if err := db.QueryRow(`
		SELECT status, proposed_txn_type, proposed_description, proposed_refund_of, proposed_tags_json,
		       classifier_tier, classifier_confidence, proposed_source_account_id, proposed_source_account_name,
		       proposed_destination_account_id, proposed_category_id
		FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&p.status, &txnType, &desc, &ref, &tags,
		&tier, &conf, &p.src, &p.srcName, &p.dst, &p.cat); err != nil {
		t.Fatalf("read %s: %v", uuid, err)
	}
	p.txnType, p.desc, p.refundOf, p.tags = txnType.String, desc.String, ref.String, tags.String
	p.tier, p.conf = int(tier.Int64), conf.Float64
	return p
}

func classifyAll(t *testing.T, db *sql.DB) ClassifyReport {
	t.Helper()
	rep, err := New(db, slog.Default(), DefaultConfidenceThreshold, 10).ClassifyPending(context.Background())
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	return rep
}

// The canonical case, and the regression for the bug that booked six Zomato
// refunds as spends: a card refund becomes a DEPOSIT from Zomato's revenue
// twin into the card it landed on — never Tier 1's withdrawal mapping
// (destination Zomato, source HDFC) — titled after, tagged like, and pointing
// at the purchase it refunds.
func TestTierRefund_DepositIntoCardPointingAtPurchase(t *testing.T) {
	db := seedRefundDB(t)
	stagePurchase(t, db, "buy-1", "scapia-acc", 48620, "2026-03-13T07:06:00Z", "___ from Zomato for lunch", `["office"]`, 954)
	stageRefund(t, db, "ref-1", `{"account_id":"scapia-acc"}`, 48620, "2026-03-14T11:59:14Z")

	rep := classifyAll(t, db)
	if rep.RefundHits != 1 || rep.Tier1Hits != 0 {
		t.Errorf("report = %+v, want 1 refund hit and no Tier-1 hit", rep)
	}
	p := readProposal(t, db, "ref-1")
	if p.tier != int(TierRefund) || p.txnType != "deposit" {
		t.Errorf("tier/type = %d/%q, want %d/deposit", p.tier, p.txnType, TierRefund)
	}
	if !p.dst.Valid || p.dst.Int64 != 954 {
		t.Errorf("destination = %v, want the card 954 (never Zomato 11)", p.dst)
	}
	if !p.src.Valid || p.src.Int64 != 2011 || p.srcName.String != "Zomato" {
		t.Errorf("source = %v %q, want Zomato's revenue twin 2011", p.src, p.srcName.String)
	}
	if !p.cat.Valid || p.cat.Int64 != 48 {
		t.Errorf("category = %v, want Refund (48)", p.cat)
	}
	if p.desc != "Refund for ___ from Zomato for lunch" {
		t.Errorf("description = %q", p.desc)
	}
	if p.tags != `["office"]` {
		t.Errorf("tags = %q, want the purchase's tags", p.tags)
	}
	if p.refundOf != "fold:buy-1" {
		t.Errorf("refund_of = %q, want fold:buy-1", p.refundOf)
	}
	if p.status != "ready_to_push" || p.conf != 1.0 {
		t.Errorf("status/conf = %q/%.2f, want ready_to_push/1.00", p.status, p.conf)
	}
}

// Two identical orders, two refunds: each refund claims a DIFFERENT
// purchase (a fully refunded purchase isn't offered again).
func TestTierRefund_IdenticalPurchasesEachClaimedOnce(t *testing.T) {
	db := seedRefundDB(t)
	stagePurchase(t, db, "buy-a", "scapia-acc", 154100, "2026-04-05T07:20:00Z", "Lunch A", "", 954)
	stagePurchase(t, db, "buy-b", "scapia-acc", 154100, "2026-04-05T07:24:00Z", "Lunch B", "", 954)
	stageRefund(t, db, "ref-a", `{"account_id":"scapia-acc"}`, 154100, "2026-04-07T16:19:12Z")
	stageRefund(t, db, "ref-b", `{"account_id":"scapia-acc"}`, 154100, "2026-04-08T10:00:00Z")

	classifyAll(t, db)
	a, b := readProposal(t, db, "ref-a"), readProposal(t, db, "ref-b")
	if a.refundOf == "" || b.refundOf == "" || a.refundOf == b.refundOf {
		t.Fatalf("refund_of = %q / %q, want two different purchases", a.refundOf, b.refundOf)
	}
	for _, r := range []string{a.refundOf, b.refundOf} {
		if r != "fold:buy-a" && r != "fold:buy-b" {
			t.Errorf("unexpected refund_of %q", r)
		}
	}
}

// Only a larger purchase: a partial refund — proposed, but for review.
func TestTierRefund_PartialRefundGoesToReview(t *testing.T) {
	db := seedRefundDB(t)
	stagePurchase(t, db, "buy-1", "scapia-acc", 100000, "2026-09-18T15:18:00Z", "Groceries", "", 954)
	stageRefund(t, db, "ref-1", `{"account_id":"scapia-acc"}`, 17900, "2026-09-23T16:53:52Z")

	classifyAll(t, db)
	p := readProposal(t, db, "ref-1")
	if p.refundOf != "fold:buy-1" || p.status != "needs_review" {
		t.Errorf("refund_of/status = %q/%q, want fold:buy-1/needs_review", p.refundOf, p.status)
	}
	if p.desc != "Refund for Groceries" {
		t.Errorf("description = %q", p.desc)
	}
}

// No purchase anywhere: the deposit is still right; review to find one.
func TestTierRefund_NoPurchaseGoesToReview(t *testing.T) {
	db := seedRefundDB(t)
	stageRefund(t, db, "ref-1", `{"account_id":"scapia-acc"}`, 99900, "2026-04-27T17:09:28Z")

	classifyAll(t, db)
	p := readProposal(t, db, "ref-1")
	if p.refundOf != "" || p.status != "needs_review" {
		t.Errorf("refund_of/status = %q/%q, want none/needs_review", p.refundOf, p.status)
	}
	if p.desc != "Refund for ___ from Zomato" {
		t.Errorf("description = %q", p.desc)
	}
	if !p.dst.Valid || p.dst.Int64 != 954 || p.txnType != "deposit" {
		t.Errorf("still a deposit into the card: dst=%v type=%q", p.dst, p.txnType)
	}
}

// A purchase that lives only in firefly (entered by hand, before fold)
// is found there: firefly is the ledger.
func TestTierRefund_FindsPurchaseOnlyInFirefly(t *testing.T) {
	db := seedRefundDB(t)
	if _, err := db.Exec(`
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		   source_account_id, source_account_name, destination_account_id, destination_account_name,
		   destination_account_name_normalized, category_id, category_name, description, tags_json, notes)
		VALUES (7931, 7700, 'withdrawal', 60275, 'INR', '2025-12-30T13:00:00+00:00', 954, 'Scapia Federal Bank Credit Card',
		   11, 'Zomato', 'zomato', 3, 'Food', 'Maharaja chicken from Meghana', '["dinner"]', 'CARD/z/Zomato/₹/602.75/OUTGOING')`); err != nil {
		t.Fatal(err)
	}
	stageRefund(t, db, "ref-1", `{"account_id":"scapia-acc"}`, 60275, "2025-12-31T12:26:00Z")

	classifyAll(t, db)
	p := readProposal(t, db, "ref-1")
	if p.refundOf != "journal:7931" {
		t.Errorf("refund_of = %q, want journal:7931", p.refundOf)
	}
	if p.desc != "Refund for Maharaja chicken from Meghana" || p.tags != `["dinner"]` {
		t.Errorf("desc/tags = %q/%q", p.desc, p.tags)
	}
}

// fold.money's own refund_group_id wins over amount matching.
func TestTierRefund_FoldRefundGroupWins(t *testing.T) {
	db := seedRefundDB(t)
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
		    narration, merchant_extracted, status, proposed_source_account_id, proposed_destination_account_id, proposed_description)
		VALUES ('buy-g', '{"account_id":"scapia-acc","refund_group_id":"g-1"}', 50000, 'INR', '2026-09-10T10:00:00Z',
		        'CARD', 'OUTGOING', 'CARD/x/Zomato/x', 'zomato', 'pushed', 954, 11, 'Party order'),
		       ('buy-x', '{"account_id":"scapia-acc"}', 17900, 'INR', '2026-09-12T10:00:00Z',
		        'CARD', 'OUTGOING', 'CARD/x/Zomato/x', 'zomato', 'ready_to_push', 954, 11, 'Other order')`); err != nil {
		t.Fatal(err)
	}
	stageRefund(t, db, "ref-g", `{"account_id":"scapia-acc","refund_group_id":"g-1"}`, 17900, "2026-09-13T10:00:00Z")

	classifyAll(t, db)
	if p := readProposal(t, db, "ref-g"); p.refundOf != "fold:buy-g" || p.conf != 1.0 {
		t.Errorf("refund_of/conf = %q/%.2f, want fold:buy-g/1.00 (fold.money grouped them)", p.refundOf, p.conf)
	}
}

// A purchase on a different card is never the one refunded.
func TestTierRefund_IgnoresOtherCards(t *testing.T) {
	db := seedRefundDB(t)
	stagePurchase(t, db, "buy-au", "au-acc", 48620, "2026-03-13T07:06:00Z", "AU lunch", "", 1314)
	stageRefund(t, db, "ref-1", `{"account_id":"scapia-acc"}`, 48620, "2026-03-14T11:59:14Z")

	classifyAll(t, db)
	if p := readProposal(t, db, "ref-1"); p.refundOf != "" {
		t.Errorf("refund_of = %q, want none (the only purchase was on the AU card)", p.refundOf)
	}
}

// The first refund from a merchant with no revenue twin yet: the name is
// proposed (firefly creates the revenue account on push) and a human looks.
func TestTierRefund_NewRevenueTwinGoesToReview(t *testing.T) {
	db := seedRefundDB(t)
	if _, err := db.Exec(`DELETE FROM firefly_accounts WHERE firefly_id = 2011`); err != nil {
		t.Fatal(err)
	}
	stagePurchase(t, db, "buy-1", "scapia-acc", 48620, "2026-03-13T07:06:00Z", "Lunch", "", 954)
	stageRefund(t, db, "ref-1", `{"account_id":"scapia-acc"}`, 48620, "2026-03-14T11:59:14Z")

	classifyAll(t, db)
	p := readProposal(t, db, "ref-1")
	if p.src.Valid || p.srcName.String != "Zomato" || p.status != "needs_review" {
		t.Errorf("source=%v %q status=%q, want name-only Zomato + needs_review", p.src, p.srcName.String, p.status)
	}
}

func TestRefundSignal(t *testing.T) {
	for _, tc := range []struct {
		mode, typ, narration string
		want                 bool
	}{
		{"CARD", "INCOMING", "CARD/1a/Zomato/₹/486.20/INCOMING/Refund Received!", true},
		{"UPI", "INCOMING", "UPI-SWIGGY-REFUND-123", true},
		{"OTHERS", "INCOMING", "IMPS reversal of failed txn", true},
		{"CARD", "INCOMING", "CARD/1a/Scapia/₹/100/INCOMING/Cashback credited", false},
		{"CARD", "INCOMING", "CARD/1a/Payment Received/₹/30067.33/INCOMING", false},
		{"UPI", "INCOMING", "UPI-ACME CORP-acme@hdfc-SALARY", false},
		{"UPI", "INCOMING", "UPI-RAHUL-rahul@okaxis-dinner split", false},
		{"CARD", "OUTGOING", "CARD/1a/Zomato/₹/486.20/OUTGOING", false},
	} {
		got, _ := refundSignal(StagedRow{Mode: tc.mode, Type: tc.typ, Narration: tc.narration, RawPayload: "{}"})
		if got != tc.want {
			t.Errorf("refundSignal(%s %s %q) = %v, want %v", tc.mode, tc.typ, tc.narration, got, tc.want)
		}
	}
	if got, _ := refundSignal(StagedRow{Mode: "UPI", Type: "INCOMING", Narration: "UPI-X", RawPayload: `{"refund_group_id":"g"}`}); !got {
		t.Error("a fold.money refund group should signal a refund")
	}
}

// Money coming IN never gets Tier 1's withdrawal mapping, and lands on the
// account fold says received it.
func TestClassify_IncomingSkipsLookupAndUsesFoldAccount(t *testing.T) {
	db := seedRefundDB(t)
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
		    narration, merchant_extracted, status)
		VALUES ('in-1', '{"account_id":"hdfc-acc"}', 150000, 'INR', '2026-06-01T10:00:00Z', 'UPI', 'INCOMING',
		        'UPI-ZOMATO-zomato@hdfc-SETTLEMENT', 'zomato', 'pending')`); err != nil {
		t.Fatal(err)
	}
	classifyAll(t, db)
	p := readProposal(t, db, "in-1")
	if p.tier == int(TierMerchantLookup) {
		t.Errorf("tier = 1: the withdrawal lookup was applied to money coming in")
	}
	if !p.dst.Valid || p.dst.Int64 != 12 {
		t.Errorf("destination = %v, want HDFC Bank (12), the account fold says received it", p.dst)
	}
}

// Rows classified before refunds were understood — including rows the human
// already edited — get their purchase proposed, and nothing else changes.
func TestMatchRefunds_BackfillsOnlyTheReference(t *testing.T) {
	db := seedRefundDB(t)
	stagePurchase(t, db, "buy-1", "scapia-acc", 48620, "2026-03-13T07:06:00Z", "Lunch", "", 954)
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
		    narration, merchant_extracted, status, classifier_tier,
		    confirmed_source_account_name, confirmed_destination_account_id, confirmed_category_id, confirmed_description)
		VALUES ('ref-edited', '{"account_id":"scapia-acc"}', 48620, 'INR', '2026-03-14T11:59:14Z', 'CARD', 'INCOMING',
		        'CARD/y/Zomato/₹/486.20/INCOMING/Refund Received!', 'zomato', 'ready_to_push', 1,
		        'Zomato', 954, 48, 'Refund for my lunch'),
		       ('ref-none', '{"account_id":"scapia-acc"}', 48620, 'INR', '2026-03-15T11:59:14Z', 'CARD', 'INCOMING',
		        'CARD/y/Zomato/₹/486.20/INCOMING/Refund Received!', 'zomato', 'ready_to_push', 1,
		        'Zomato', 954, 48, 'Refund, no purchase')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE staged_fold_txns SET confirmed_refund_of = 'none' WHERE fold_uuid = 'ref-none'`); err != nil {
		t.Fatal(err)
	}
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	n, err := c.MatchRefunds(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("MatchRefunds = %d, %v; want 1", n, err)
	}
	var ref, status, desc string
	if err := db.QueryRow(`SELECT COALESCE(proposed_refund_of,''), status, confirmed_description FROM staged_fold_txns WHERE fold_uuid='ref-edited'`).
		Scan(&ref, &status, &desc); err != nil {
		t.Fatal(err)
	}
	if ref != "fold:buy-1" || status != "ready_to_push" || desc != "Refund for my lunch" {
		t.Errorf("got ref=%q status=%q desc=%q; want only the reference added", ref, status, desc)
	}
	var none sql.NullString
	_ = db.QueryRow(`SELECT proposed_refund_of FROM staged_fold_txns WHERE fold_uuid='ref-none'`).Scan(&none)
	if none.Valid {
		t.Errorf("a row the human marked 'none' got %q", none.String)
	}
	if n, _ := c.MatchRefunds(context.Background()); n != 0 {
		t.Errorf("second run matched %d, want 0 (idempotent)", n)
	}
}

// A MANUAL deposit added from a statement line: its merchant is its source
// and its card its destination.
func TestMatchRefunds_ManualDeposit(t *testing.T) {
	db := seedRefundDB(t)
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
		    narration, merchant_extracted, status, proposed_source_account_id, proposed_destination_account_id, proposed_description)
		VALUES ('buy-sp', '{"account_id":"scapia-acc"}', 200, 'INR', '2026-06-30T15:00:00Z', 'CARD', 'OUTGOING',
		        'CARD/x/Spotify/₹/2.00/OUTGOING', 'spotify', 'ready_to_push', 954, 25, 'Spotify card verification charge')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
		    narration, status, confirmed_txn_type, confirmed_source_account_name, confirmed_destination_account_id,
		    confirmed_category_id, confirmed_description)
		VALUES ('manual-0001', '{"manual":true}', 200, 'INR', '2026-06-30T20:00:00Z', 'MANUAL', 'INCOMING',
		        'statement: Spotify reversal 2.00', 'ready_to_push', 'deposit', 'Spotify', 954, 48,
		        'Spotify card verification charge reversal')`); err != nil {
		t.Fatal(err)
	}
	n, err := New(db, slog.Default(), DefaultConfidenceThreshold, 10).MatchRefunds(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("MatchRefunds = %d, %v; want 1", n, err)
	}
	var ref string
	_ = db.QueryRow(`SELECT proposed_refund_of FROM staged_fold_txns WHERE fold_uuid='manual-0001'`).Scan(&ref)
	if ref != "fold:buy-sp" {
		t.Errorf("manual deposit refund_of = %q, want fold:buy-sp", ref)
	}
}

// Learning from a pushed refund would teach Tier 1 to send the next Zomato
// ORDER to the card; learning from a transfer (a card repayment) would teach
// it a merchant that is really an asset. Neither may touch merchant_lookup.
func TestLearnFromPushed_IgnoresRefundsAndTransfers(t *testing.T) {
	for _, tc := range []struct {
		name, typ string
		dst       int64
	}{
		// 999 is in no mirror, so only the direction guard can stop this one.
		{"refund", "INCOMING", 999},
		// An OUTGOING row whose destination is the user's own card.
		{"transfer", "OUTGOING", 954},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := seedRefundDB(t)
			if _, err := db.Exec(`
				INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type,
				    narration, merchant_extracted, status, proposed_source_account_id, proposed_destination_account_id, proposed_category_id)
				VALUES ('row-p', '{}', 48620, 'INR', '2026-03-14T11:59:14Z', 'CARD', ?,
				        'CARD/y/Zomato/x', 'zomato', 'pushed', 2011, ?, 48)`, tc.typ, tc.dst); err != nil {
				t.Fatal(err)
			}
			if err := New(db, slog.Default(), DefaultConfidenceThreshold, 10).LearnFromPushed(context.Background(), "row-p"); err != nil {
				t.Fatal(err)
			}
			var dst int64
			var n int
			_ = db.QueryRow(`SELECT modal_destination_account_id, sample_size FROM merchant_lookup WHERE merchant_normalized='zomato'`).Scan(&dst, &n)
			if dst != 11 || n != 2 {
				t.Errorf("merchant_lookup zomato = dest %d, samples %d; want untouched (11, 2)", dst, n)
			}
		})
	}
}

func TestPickRefundCandidate_Reasons(t *testing.T) {
	if b, c, n := pickRefundCandidate(nil); b != nil || c >= DefaultConfidenceThreshold || !strings.Contains(n, "no matching") {
		t.Errorf("empty: %v %.2f %q", b, c, n)
	}
}
