package classifier

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
)

// A fictional card and a fictional ticket site.
func creditBackDB(t *testing.T) *sql.DB {
	t.Helper()
	db := learnDB(t)
	mustExec(t, db, `INSERT INTO fold_accounts (fold_account_id, kind, name, provider, network, last_four, raw_payload, is_closed)
		VALUES ('card-1', 'CREDIT_CARD', 'Orchid Bank Card ****0000', 'Orchid', 'Visa', '0000', '{}', 0)`)
	return db
}

// stageCharge stages a foreign card charge as fold sync writes one.
func stageCharge(t *testing.T, db *sql.DB, uuid, typ, narration string, inr, foreign int64, cur string, at time.Time, status string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, foreign_amount_paise, foreign_currency,
		txn_timestamp, mode, type, narration, merchant_extracted, status)
		VALUES (?, '{"account_id":"card-1"}', ?, 'INR', NULLIF(?, 0), NULLIF(?, ''), ?, 'CARD', ?, ?, '', ?)`,
		uuid, inr, foreign, cur, at, typ, narration, status)
}

func TestIsCreditBack(t *testing.T) {
	for narration, want := range map[string]bool{
		"CARD/7c1e0a5b9d3f2e11//USD/42.50/INCOMING/is credited back to your Card":  true,
		"CARD/7c1e0a5b9d3f2e11//Rs./740.00/INCOMING/is credited back to your Card": true,
		"CARD/7c1e0a5b9d3f2e11/TICKETSITE/USD/42.50/INCOMING/is credited back":     false, // names a merchant: a refund
		"CARD/7c1e0a5b9d3f2e11//USD/42.50/INCOMING/Refund credited back":           false,
		"CARD/7c1e0a5b9d3f2e11//USD/42.50/INCOMING/Refund Received!":               false,
		"UPI/x@ok//credited back": false,
	} {
		if got := isCreditBack(StagedRow{Type: "INCOMING", Narration: narration, RawPayload: `{}`}); got != want {
			t.Errorf("isCreditBack(%q) = %v, want %v", narration, got, want)
		}
	}
	if isCreditBack(StagedRow{Type: "OUTGOING", Narration: "CARD/7c1e0a5b9d3f2e11//USD/2.00/OUTGOING/is credited back", RawPayload: `{}`}) {
		t.Error("money out read as a credit-back")
	}
	if isCreditBack(StagedRow{Type: "INCOMING", Narration: "CARD/7c1e0a5b9d3f2e11//USD/2.00/INCOMING/is credited back", RawPayload: `{"merchant":{"name":"Ticket Site"}}`}) {
		t.Error("a credit fold.money names a merchant for read as a credit-back")
	}
}

// The released hold of a never-billed charge: booked like a refund of that
// charge, found by its foreign amount (the rupee side differs with the day's
// rate), and held with a reason that names it.
func TestACreditBackIsHeldWithTheChargeItGivesBack(t *testing.T) {
	db := creditBackDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	day := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	stageCharge(t, db, "charge", "OUTGOING", "CARD/7c1e0a5b9d3f2e11/TICKETSITE/USD/42.50/OUTGOING/14-07-26", 368900, 4250, "USD", day, "needs_review")
	mustExec(t, db, `UPDATE staged_fold_txns SET proposed_description = 'Concert tickets', proposed_destination_account_name = 'Ticket Site' WHERE fold_uuid = 'charge'`)
	stageCharge(t, db, "other-card", "OUTGOING", "CARD/x/ELSEWHERE/USD/42.50/OUTGOING", 368900, 4250, "USD", day, "needs_review")
	mustExec(t, db, `UPDATE staged_fold_txns SET raw_payload = '{"account_id":"card-2"}' WHERE fold_uuid = 'other-card'`)
	stageCharge(t, db, "back", "INCOMING", "CARD/7c1e0a5b9d3f2e22//USD/42.50/INCOMING/is credited back to your Card", 361275, 4250, "USD", day.Add(3*time.Hour), "pending")

	rows, _ := c.fetchStagedForClassify(context.Background(), `fold_uuid = 'back'`)
	d, err := c.ClassifyOne(context.Background(), rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != TierRefund || d.RefundOf != "fold:charge" {
		t.Fatalf("tier %d refund of %q; want the refund tier, linked to the charge on the same card", d.Tier, d.RefundOf)
	}
	for _, want := range []string{"released card hold, not money in", "USD 42.50 charge at Ticket Site on Tue 14 Jul", "If neither is on the statement, skip both; if both are, release the hold"} {
		if !strings.Contains(d.Hold, want) {
			t.Errorf("hold reason %q lacks %q", d.Hold, want)
		}
	}
	if d.Description != "Refund for Concert tickets" {
		t.Errorf("title = %q; want it named after the charge", d.Description)
	}

	if err := c.ApplyDecision(context.Background(), "back", d); err != nil {
		t.Fatal(err)
	}
	var reason, by string
	_ = db.QueryRow(`SELECT COALESCE(hold_reason, ''), COALESCE(hold_by, '') FROM staged_fold_txns WHERE fold_uuid = 'back'`).Scan(&reason, &by)
	if reason != d.Hold || by != "fold" {
		t.Errorf("stored hold %q by %q; want fold's", reason, by)
	}
}

// A charge the owner already skipped as never billed: the credit-back is
// its other half — skip it too — and is not booked as a refund of it.
func TestACreditBackOfASkippedChargeSaysSkipItToo(t *testing.T) {
	db := creditBackDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	day := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	stageCharge(t, db, "charge", "OUTGOING", "CARD/7c1e0a5b9d3f2e33/AIRPORT KIOSK/USD/18.00/OUTGOING", 152940, 1800, "USD", day.Add(-24*time.Hour), "skipped")
	stageCharge(t, db, "back", "INCOMING", "CARD/7c1e0a5b9d3f2e33//USD/18.00/INCOMING/is credited back to your Card", 152940, 1800, "USD", day, "pending")
	rows, _ := c.fetchStagedForClassify(context.Background(), `fold_uuid = 'back'`)
	d, _ := c.ClassifyOne(context.Background(), rows[0])
	if !strings.Contains(d.Hold, "which you skipped") || !strings.Contains(d.Hold, "AIRPORT KIOSK") {
		t.Errorf("hold = %q; want it to say the charge was skipped, naming it", d.Hold)
	}
	if d.RefundOf == "fold:charge" {
		t.Error("the credit-back was linked as a refund of a charge that was skipped")
	}
}

// Nothing to pair it with: still held, still explained.
func TestACreditBackWithNoChargeIsStillHeld(t *testing.T) {
	db := creditBackDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	stageCharge(t, db, "back", "INCOMING", "CARD/7c1e0a5b9d3f2e44//USD/2.00/INCOMING/is credited back to your Card", 17130, 200, "USD", time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC), "pending")
	rows, _ := c.fetchStagedForClassify(context.Background(), `fold_uuid = 'back'`)
	d, _ := c.ClassifyOne(context.Background(), rows[0])
	if !strings.Contains(d.Hold, "no charge it gives back") {
		t.Errorf("hold = %q", d.Hold)
	}
}

// fold never takes a decision from a person: it doesn't replace their
// hold, and once they release fold's hold, fold doesn't set it again.
func TestFoldNeverOverridesAPersonsHoldOrRelease(t *testing.T) {
	db := creditBackDB(t)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	stageCharge(t, db, "mine", "INCOMING", "CARD/7c1e0a5b9d3f2e44//USD/2.00/INCOMING/is credited back to your Card", 17130, 200, "USD", time.Now(), "needs_review")
	mustExec(t, db, `UPDATE staged_fold_txns SET hold_reason = 'my own words' WHERE fold_uuid = 'mine'`)
	stageCharge(t, db, "released", "INCOMING", "CARD/7c1e0a5b9d3f2e44//USD/2.00/INCOMING/is credited back to your Card", 17130, 200, "USD", time.Now(), "needs_review")
	if err := feedback.RecordDecision(context.Background(), db, "released", feedback.FeedbackRelease, ""); err != nil {
		t.Fatal(err)
	}
	d := Decision{Tier: TierRefund, Confidence: 0.7, TxnType: "deposit", Hold: "fold's reason"}
	for _, u := range []string{"mine", "released"} {
		if err := c.ApplyDecision(context.Background(), u, d); err != nil {
			t.Fatal(err)
		}
	}
	var mine, mineBy, rel string
	_ = db.QueryRow(`SELECT hold_reason, COALESCE(hold_by, '') FROM staged_fold_txns WHERE fold_uuid = 'mine'`).Scan(&mine, &mineBy)
	_ = db.QueryRow(`SELECT COALESCE(hold_reason, '') FROM staged_fold_txns WHERE fold_uuid = 'released'`).Scan(&rel)
	if mine != "my own words" || mineBy != "" {
		t.Errorf("a person's hold became %q by %q", mine, mineBy)
	}
	if rel != "" {
		t.Errorf("fold held a row a person had released: %q", rel)
	}
	// A decision with no hold leaves every hold alone.
	if err := c.ApplyDecision(context.Background(), "mine", Decision{Tier: TierRefund, Confidence: 0.7}); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(`SELECT hold_reason FROM staged_fold_txns WHERE fold_uuid = 'mine'`).Scan(&mine)
	if mine != "my own words" {
		t.Errorf("a decision without a hold cleared the person's: %q", mine)
	}
}

func TestRupeesUseIndianGrouping(t *testing.T) {
	for p, want := range map[int64]string{0: "₹0.00", 5: "₹0.05", 152940: "₹1,529.40", 12345678: "₹1,23,456.78", 999999999: "₹99,99,999.99", -150: "−₹1.50"} {
		if got := rupees(p); got != want {
			t.Errorf("rupees(%d) = %q, want %q", p, got, want)
		}
	}
}

func TestSayDayGivesTheYearOnlyWhenItDiffers(t *testing.T) {
	ref := time.Date(2026, 9, 27, 6, 0, 0, 0, time.UTC)
	if got := sayDay(time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC), ref); got != "Wed 12 Aug" { // 01:30 IST the next day
		t.Errorf("sayDay = %q, want the IST day, Wed 12 Aug", got)
	}
	if got := sayDay(time.Date(2025, 12, 30, 6, 0, 0, 0, time.UTC), ref); got != "Tue 30 Dec 2025" {
		t.Errorf("sayDay = %q, want the year for last year", got)
	}
	if got := sayDay(time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC), ref); got != "Tue 22 Sept" {
		t.Errorf("sayDay = %q, want September the way the deck writes it", got)
	}
}
