package classifier

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

func TestUPINoteIsTheOwnersWords(t *testing.T) {
	for narration, want := range map[string]string{
		"UPI-ASHA MENON-ASHA.M-1@OKICICI-ICIC0001234-612345678901-CAB":       "CAB",
		"UPI-RAVI KUMAR-9000000001@SLC-SBIN0000123-612345678902-ROLL DINNER": "ROLL DINNER",
		"UPI-RIDE DRIVER-9000000002@YBL-HDFC0000123-612345678903-RAPIDO":     "RAPIDO",
		"UPI/P2A/612345678904/ASHA MENON/june rent split/ICICI Bank":         "june rent split",
		"UPI-TEA TRAIL CAFE-TEATRAIL@OKAXIS-UTIB0000123-612345678905-UPI":    "",
		"UPI-TEA TRAIL CAFE-TEATRAIL@OKAXIS-UTIB0000123-612345678906-PAY":    "",
		"UPI-SHOP-SHOP@PAYTM-PYTM0123456-612345678907-SENT USING PAYTM U":    "",
		"UPI-SHOP-SHOP@RAZORPAY-UTIB0000123-612345678908-QX7KD2LPA9ZM41BTV8": "",
		"UPI-SHOP-SHOP@RAZORPAY-UTIB0000123-612345678909-9563787820527426 Z": "",
		"CARD/5d2c4b3a29181706/TEA TRAIL CAFE/Rs./64.00/OUTGOING/14-09-26":   "",
		"NEFT CR-XXXX0000000-BOND REGISTRY":                                  "",
	} {
		if got := upiNote(narration); got != want {
			t.Errorf("upiNote(%q) = %q, want %q", narration, got, want)
		}
	}
}

// The note reaches the model, ahead of everything it outranks.
func TestTheNoteOnThePaymentIsInThePrompt(t *testing.T) {
	db := learnDB(t)
	fake := newCapturingLLM(t, `{"reasoning":"r","txn_type":"withdrawal","destination_account_id":43,"category_id":7,"confidence":0.9}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", fake.URL, fake.Client()))
	if _, err := c.ClassifyOne(context.Background(), StagedRow{FoldUUID: "n", Type: "OUTGOING", Mode: "UPI", RawPayload: `{}`,
		Narration: "UPI-ASHA MENON-ASHA.M-1@OKICICI-ICIC0001234-612345678901-CAB"}); err != nil {
		t.Fatal(err)
	}
	p := fake.last()
	i := strings.Index(p, "== THE NOTE ON THE PAYMENT (what you typed in the UPI app — your own words")
	if i < 0 || !strings.Contains(p[i:], `"CAB"`) || i > strings.Index(p, "== DETERMINISTIC HINTS") {
		t.Errorf("the note isn't in the prompt, or comes after the hints:\n%s", p[:min(len(p), 1500)])
	}
}

// A model too unsure to name the payee still says what it can: the card
// goes to a person with its title, category and reasons — and no payee, so
// the person picks who was paid.
func TestAnUnsureModelStillLeavesItsBestWord(t *testing.T) {
	db := learnDB(t)
	fake := newCapturingLLM(t, `{"reasoning":"a UPI payment to a person named only by a phone handle; the note says CAB",
		"txn_type":"withdrawal","destination_account_id":null,"destination_name_suggestion":null,"category_id":7,
		"description_suggestion":"Cab ride from ___ to ___","unknowns":["where from","where to"],"evidence":["the note says CAB"],"confidence":0.35}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "claude-opus-5-5", fake.URL, fake.Client()))
	stagedAt(t, db, "u", "OUTGOING", "UPI-ASHA MENON-9000000001@SLC-SBIN0000123-612345678901-CAB", "", 25000, time.Date(2026, 9, 20, 7, 0, 0, 0, time.UTC), "pending")
	rows, _ := c.fetchStagedForClassify(context.Background(), `fold_uuid = 'u'`)
	d, err := c.ClassifyOne(context.Background(), rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != TierHumanReview || d.DestinationAccountID != nil || d.DestinationAccountName != "" {
		t.Fatalf("tier %d destination %v %q; want a card for a person, payee left to them", d.Tier, d.DestinationAccountID, d.DestinationAccountName)
	}
	if d.Description != "Cab ride from ___ to ___" || d.CategoryName != "Subscriptions" || !strings.Contains(d.Evidence.Note, "the note says CAB") {
		t.Errorf("decision = %q · %q · %q; want the model's title, category and reason", d.Description, d.CategoryName, d.Evidence.Note)
	}
	if err := c.ApplyDecision(context.Background(), "u", d); err != nil {
		t.Fatal(err)
	}
	var status string
	var version int
	_ = db.QueryRow(`SELECT status, classifier_version FROM staged_fold_txns WHERE fold_uuid = 'u'`).Scan(&status, &version)
	if status != "needs_review" || version != EngineVersion {
		t.Errorf("status %q version %d; want review, and the model's look recorded so it isn't asked again for nothing", status, version)
	}
}
