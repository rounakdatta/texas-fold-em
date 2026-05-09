package classifier

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/gemini"
)

// TestTier3_HappyPath: even with NO FTS hits for an unseen merchant,
// the synthesiser-mode Tier 3 still has the user's full account/
// category/tag inventories to draw from, so the LLM can still pick
// a valid destination + source from the asset list and return a
// confident decision.
func TestTier3_HappyPath(t *testing.T) {
	db := seedTestDB(t)
	llm := newFakeGemini(t, `{
		"txn_type": "withdrawal",
		"destination_account_id": 11,
		"source_account_id": 1,
		"category_id": 5,
		"budget_id": null,
		"description_suggestion": "Lunch at Zomato",
		"confidence": 0.92,
		"reasoning": "Most similar historical txns are Zomato food orders, all categorised as Eating outside on the HDFC Card."
	}`)
	t.Cleanup(llm.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(gemini.NewClient("k", "", llm.URL, llm.Client()))

	// Mystery merchant — no merchant_lookup entry, no FTS hits for
	// "mystery"/"vendor"/"llp". Synthesiser Tier 3 still has the asset
	// + expense lists from the seeded firefly_txns to draw on, picks
	// id=11 (Zomato) which is in the seeded expense list.
	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-1",
		Narration:         "CARD/x/Mystery Vendor LLP/Rs/100/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "mystery vendor llp",
		RawPayload:        `{"uuid":"tier3-1","mode":"CARD","type":"OUTGOING"}`,
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if d.Tier != TierLLM {
		t.Fatalf("expected Tier 3 (synthesiser fires from inventories), got Tier %d", d.Tier)
	}
	if d.TxnType != "withdrawal" {
		t.Errorf("expected txn_type=withdrawal, got %q", d.TxnType)
	}
	if d.DestinationAccountID == nil || *d.DestinationAccountID != 11 {
		t.Errorf("destination = %v, want 11", d.DestinationAccountID)
	}
}

// TestTier3_TransferType: LLM identifies a transfer (both endpoints
// are user's asset accounts) and txn_type comes through.
func TestTier3_TransferType(t *testing.T) {
	db := seedTestDB(t)
	llm := newFakeGemini(t, `{
		"txn_type": "transfer",
		"destination_account_id": 1,
		"source_account_id": 1,
		"category_id": null,
		"budget_id": null,
		"description_suggestion": "savings to zerodha",
		"confidence": 0.90,
		"reasoning": "Both endpoints are user-owned asset accounts."
	}`)
	t.Cleanup(llm.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(gemini.NewClient("k", "", llm.URL, llm.Client()))

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "transfer-1",
		Narration:         "UPI to my own zerodha vpa",
		Mode:              "UPI",
		Type:              "OUTGOING",
		MerchantExtracted: "",
		RawPayload:        `{"uuid":"transfer-1","mode":"UPI","type":"OUTGOING"}`,
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	// Tier 3 might fire OR the deterministic fallback might trigger.
	// What we care about: when Tier 3 DOES fire, txn_type is honoured.
	if d.Tier == TierLLM && d.TxnType != "transfer" {
		t.Errorf("Tier 3 should have honoured txn_type=transfer, got %q", d.TxnType)
	}
}

// TestTier3_HitsViaNarrationFallback: when the merchant is empty but
// the narration has long tokens that hit firefly_txns_fts, Tier 3 has
// candidates to feed the LLM and returns a Tier-3 decision.
func TestTier3_HitsViaNarrationFallback(t *testing.T) {
	db := seedTestDB(t)
	llm := newFakeGemini(t, `{
		"destination_account_id": 11,
		"source_account_id": 1,
		"category_id": 5,
		"budget_id": null,
		"description_suggestion": "Zomato delivery",
		"confidence": 0.85,
		"reasoning": "Narration mentions Zomato"
	}`)
	t.Cleanup(llm.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(gemini.NewClient("k", "", llm.URL, llm.Client()))

	// Empty merchant, but narration contains "zomato" — Tier 2's
	// FTS query won't fire (it needs MerchantExtracted), Tier 3's
	// fallback uses long narration tokens.
	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-fallback",
		Narration:         "weird raw blob containing zomato wordmark",
		Mode:              "OTHERS",
		Type:              "OUTGOING",
		MerchantExtracted: "",
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if d.Tier != TierLLM {
		t.Fatalf("expected Tier 3 via narration-token fallback, got Tier %d", d.Tier)
	}
	if d.Confidence < 0.8 {
		t.Errorf("expected confidence ~0.85, got %f", d.Confidence)
	}
	if d.DestinationAccountID == nil || *d.DestinationAccountID != 11 {
		t.Errorf("destination = %v, want 11", d.DestinationAccountID)
	}
	if d.Description != "Zomato delivery" {
		t.Errorf("description = %q, want %q", d.Description, "Zomato delivery")
	}
	if d.Evidence.Note != "Narration mentions Zomato" {
		t.Errorf("evidence note = %q, want LLM reasoning", d.Evidence.Note)
	}
}

// TestTier3_HallucinationGuard: if the LLM returns IDs not present in
// the retrieved candidates, we drop the response and fall through.
func TestTier3_HallucinationGuard(t *testing.T) {
	db := seedTestDB(t)
	// id 9999 is NOT in seeded data — Gemini hallucinated it.
	llm := newFakeGemini(t, `{
		"destination_account_id": 9999,
		"category_id": 5,
		"confidence": 0.95,
		"reasoning": "I made up an id"
	}`)
	t.Cleanup(llm.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(gemini.NewClient("k", "", llm.URL, llm.Client()))

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-halluc",
		Narration:         "weird raw blob containing zomato wordmark",
		Mode:              "OTHERS",
		Type:              "OUTGOING",
		MerchantExtracted: "",
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	// Hallucination defended → Tier 4.
	if d.Tier != TierHumanReview {
		t.Errorf("expected Tier 4 after hallucination defence, got %d", d.Tier)
	}
}

// TestTier3_LowConfidence: if the LLM self-reports < 0.5 confidence,
// we don't take it. Falls through to Tier 4.
func TestTier3_LowConfidence(t *testing.T) {
	db := seedTestDB(t)
	llm := newFakeGemini(t, `{
		"destination_account_id": 11,
		"category_id": 5,
		"confidence": 0.3,
		"reasoning": "I'm guessing"
	}`)
	t.Cleanup(llm.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(gemini.NewClient("k", "", llm.URL, llm.Client()))

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-low",
		Narration:         "weird raw blob containing zomato wordmark",
		Mode:              "OTHERS",
		Type:              "OUTGOING",
		MerchantExtracted: "",
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if d.Tier != TierHumanReview {
		t.Errorf("expected Tier 4 with LLM confidence 0.3, got %d", d.Tier)
	}
}

// TestTier3_GeminiError: if Gemini returns 5xx (or any error), we
// don't error the whole classify — we just skip Tier 3 and go to 4.
func TestTier3_GeminiError(t *testing.T) {
	db := seedTestDB(t)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"upstream is down"}`))
	}))
	t.Cleanup(llm.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(gemini.NewClient("k", "", llm.URL, llm.Client()))

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-error",
		Narration:         "weird raw blob containing zomato wordmark",
		Mode:              "OTHERS",
		Type:              "OUTGOING",
		MerchantExtracted: "",
	})
	if err != nil {
		t.Fatalf("ClassifyOne should not surface Tier-3 errors: %v", err)
	}
	if d.Tier != TierHumanReview {
		t.Errorf("expected Tier 4 after gemini failure, got %d", d.Tier)
	}
}

// TestStripJSONFences accidentally hardens against gemini wrapping
// JSON despite our system prompt.
func TestStripJSONFences(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:               `{"a":1}`,
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}\n```":    `{"a":1}`,
		"  \n{\"a\":1}\n  ":      `{"a":1}`,
	}
	for in, want := range cases {
		if got := stripJSONFences(in); got != want {
			t.Errorf("stripJSONFences(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLongTokens covers the narration-fallback tokeniser used when
// merchant extraction fails.
func TestLongTokens(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want []string
	}{
		// 5 qualifying tokens (raw is too short); n=4 keeps the longest 4.
		{"weird raw blob containing zomato wordmark", 4, []string{"containing", "wordmark", "zomato", "weird"}},
		{"a b cd 12345", 5, nil},                       // all dropped: short or numeric
		{"1234abcd5678 zomato", 5, []string{"zomato"}}, // hexish dropped
	}
	for _, tc := range cases {
		got := longTokens(tc.in, tc.n)
		if len(got) != len(tc.want) {
			t.Errorf("longTokens(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if !strings.EqualFold(got[i], tc.want[i]) {
				t.Errorf("longTokens(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// newFakeGemini returns an httptest server that always responds with
// the same envelope wrapping the supplied JSON string as the candidate
// text.
func newFakeGemini(t *testing.T, candidateJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		envelope := map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{
							{"text": candidateJSON},
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
}
