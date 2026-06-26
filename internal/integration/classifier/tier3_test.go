package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// TestTier3_HappyPath: even with NO FTS hits for an unseen merchant,
// the synthesiser-mode Tier 3 still has the user's full account/
// category/tag inventories to draw from, so the LLM can still pick
// a valid destination + source from the asset list and return a
// confident decision.
func TestTier3_HappyPath(t *testing.T) {
	db := seedTestDB(t)
	fakeLLM := newFakeLLM(t, `{
		"txn_type": "withdrawal",
		"destination_account_id": 11,
		"source_account_id": 1,
		"category_id": 5,
		"budget_id": null,
		"description_suggestion": "Lunch at Zomato",
		"confidence": 0.92,
		"reasoning": "Most similar historical txns are Zomato food orders, all categorised as Eating outside on the HDFC Card."
	}`)
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

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
	fakeLLM := newFakeLLM(t, `{
		"txn_type": "transfer",
		"destination_account_id": 1,
		"source_account_id": 1,
		"category_id": null,
		"budget_id": null,
		"description_suggestion": "savings to zerodha",
		"confidence": 0.90,
		"reasoning": "Both endpoints are user-owned asset accounts."
	}`)
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

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
	fakeLLM := newFakeLLM(t, `{
		"destination_account_id": 11,
		"source_account_id": 1,
		"category_id": 5,
		"budget_id": null,
		"description_suggestion": "Zomato delivery",
		"confidence": 0.85,
		"reasoning": "Narration mentions Zomato"
	}`)
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

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
// the retrieved candidates AND no high-confidence deterministic tier
// fired, the row is deferred (ErrLLMDeferred) — leaves the row pending
// for the next classify cycle to retry. Putting hallucinated rows
// into needs_review with no proposal would be the mediocre output we
// explicitly want to avoid.
func TestTier3_HallucinationGuard(t *testing.T) {
	db := seedTestDB(t)
	// id 9999 is NOT in seeded data — the LLM hallucinated it.
	fakeLLM := newFakeLLM(t, `{
		"destination_account_id": 9999,
		"category_id": 5,
		"confidence": 0.95,
		"reasoning": "I made up an id"
	}`)
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

	_, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-halluc",
		Narration:         "weird raw blob containing zomato wordmark",
		Mode:              "OTHERS",
		Type:              "OUTGOING",
		MerchantExtracted: "",
	})
	if !errors.Is(err, ErrLLMDeferred) {
		t.Fatalf("expected ErrLLMDeferred, got %v", err)
	}
}

// TestTier3_LowConfidence: if the LLM self-reports < 0.5 confidence,
// we don't take it. Falls through to Tier 4.
func TestTier3_LowConfidence(t *testing.T) {
	db := seedTestDB(t)
	fakeLLM := newFakeLLM(t, `{
		"destination_account_id": 11,
		"category_id": 5,
		"confidence": 0.3,
		"reasoning": "I'm guessing"
	}`)
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

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

// TestTier3_LLMError: when the LLM is unavailable AND no
// high-confidence deterministic tier fired, the row is deferred — we
// surface ErrLLMDeferred so the caller (classifyMatching) leaves the
// row's status untouched. The next cycle retries with a working LLM.
//
// We use 401 (terminal — not retried) rather than 503 so the test
// runs in milliseconds. Retry semantics for transient codes are
// exhaustively covered in the llm package's own client_test.go.
func TestTier3_LLMError(t *testing.T) {
	db := seedTestDB(t)
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad api key"}`))
	}))
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

	_, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-error",
		Narration:         "weird raw blob containing zomato wordmark",
		Mode:              "OTHERS",
		Type:              "OUTGOING",
		MerchantExtracted: "",
	})
	if !errors.Is(err, ErrLLMDeferred) {
		t.Fatalf("expected ErrLLMDeferred, got %v", err)
	}
}

// TestTier3_LLMErrorWithTier1Hint: when the LLM fails BUT a
// high-confidence Tier-1 hint (merchant_lookup) is available, we use
// the Tier-1 result instead of deferring. Tier-1 above threshold is
// deterministic ground truth — falling back to it is not "mediocre",
// it's the right answer at high quality.
func TestTier3_LLMErrorWithTier1Hint(t *testing.T) {
	db := seedTestDB(t)
	// Terminal 401 to skip the retry budget — see TestTier3_LLMError.
	fakeLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad api key"}`))
	}))
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-err-with-hint",
		Narration:         "CARD/x/Zomato/Rs/100/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "zomato",
	})
	if err != nil {
		t.Fatalf("expected Tier-1 fallback, got error: %v", err)
	}
	if d.Tier != TierMerchantLookup {
		t.Errorf("expected TierMerchantLookup, got %d", d.Tier)
	}
}

// TestTier3_FoldAccountHintAppearsInPrompt: when staging row's
// raw_payload carries an account_id that matches a fold_accounts row,
// the prompt receives a "FOLD-SIDE PAYING ACCOUNT" block. This is the
// keystone fix for the c3c79fef KARAN BAHADUR misclassification —
// the LLM previously saw only an opaque UUID, which it ignored.
func TestTier3_FoldAccountHintAppearsInPrompt(t *testing.T) {
	db := seedTestDB(t)

	// Seed a fold_accounts row for the account_id that will appear in
	// raw_payload below.
	if _, err := db.Exec(`
		INSERT INTO fold_accounts (fold_account_id, kind, name, provider, network, last_four, raw_payload, is_closed)
		VALUES ('8582f77b-fdcc-449c-9d1a-86d9ad349325', 'CREDIT_CARD',
		        'HDFC Tata Neu Plus ****8943', 'HDFC', 'RuPay', '8943', '{}', 0)
	`); err != nil {
		t.Fatalf("seed fold_accounts: %v", err)
	}

	// Capture the prompt by spying on the fake LLM server.
	var capturedReqBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedReqBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		envelope := map[string]any{
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"content": `{
						"txn_type":"withdrawal",
						"destination_account_id":11,"source_account_id":1,
						"category_id":5,"budget_id":null,"tags":[],
						"description_suggestion":"x","confidence":0.9,
						"reasoning":"matched fold-side card to firefly asset"
					}`,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(srv.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", srv.URL, srv.Client()))

	rawPayload := `{"uuid":"c3c79fef","account_id":"8582f77b-fdcc-449c-9d1a-86d9ad349325","mode":"CARD","type":"OUTGOING","narration":"CARD/x/KARAN BAHADUR SAUD/Rs/70/OUTGOING"}`
	_, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "c3c79fef",
		Narration:         "CARD/x/KARAN BAHADUR SAUD/Rs/70/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "karan bahadur saud",
		RawPayload:        rawPayload,
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if !strings.Contains(capturedReqBody, "FOLD-SIDE PAYING ACCOUNT") {
		t.Errorf("prompt missing fold-account block:\n%s", capturedReqBody)
	}
	for _, want := range []string{"HDFC Tata Neu Plus", "8943", "HDFC", "RuPay"} {
		if !strings.Contains(capturedReqBody, want) {
			t.Errorf("prompt missing %q in fold-account block", want)
		}
	}
}

// TestTier3_NoFoldAccountHintWhenUnmirrored: when raw_payload carries
// an account_id that is not yet in fold_accounts, the prompt OMITS
// the fold-account block (we don't fabricate). LLM still classifies
// using whatever else it has.
func TestTier3_NoFoldAccountHintWhenUnmirrored(t *testing.T) {
	db := seedTestDB(t)

	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = string(body)
		w.Header().Set("Content-Type", "application/json")
		envelope := map[string]any{
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"content": `{
						"txn_type":"withdrawal",
						"destination_account_id":11,"source_account_id":1,
						"confidence":0.85,"reasoning":"x"}`,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(srv.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", srv.URL, srv.Client()))

	rawPayload := `{"uuid":"unseen-1","account_id":"never-mirrored-uuid","mode":"CARD","type":"OUTGOING"}`
	_, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "unseen-1",
		Narration:         "CARD/x/zomato/Rs/100/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "zomato",
		RawPayload:        rawPayload,
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if strings.Contains(captured, "FOLD-SIDE PAYING ACCOUNT") {
		t.Errorf("prompt should NOT include fold-account block when id is unmirrored:\n%s", captured)
	}
}

// TestStripJSONFences hardens against an LLM wrapping its JSON in
// ``` fences despite the system prompt asking for plain JSON.
func TestStripJSONFences(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                 `{"a":1}`,
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}\n```":     `{"a":1}`,
		"  \n{\"a\":1}\n  ":       `{"a":1}`,
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

// TestTier3_PromptHasTimeContextAndStyleSamples: for a known-merchant
// staged row, the Tier-3 user prompt must include:
//   - a TIME CONTEXT block computed from staged.TxnTimestamp (in IST,
//     with a meal/occasion bucket)
//   - a STYLE SAMPLES block listing the user's past descriptions for
//     that merchant from firefly_txns
//
// These two blocks are what let the LLM mirror the user's voice and
// infer the meal/occasion — without them, descriptions degrade to
// generic strings like "Lunch at Zomato".
func TestTier3_PromptHasTimeContextAndStyleSamples(t *testing.T) {
	db := seedTestDB(t)

	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedPrompt = string(body)
		w.Header().Set("Content-Type", "application/json")
		// Reply with a low-confidence response so we exercise the path
		// without affecting downstream Decision.
		envelope := map[string]any{
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": `{"txn_type":"withdrawal","destination_account_id":null,"source_account_id":null,"confidence":0.2,"reasoning":"low"}`,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(srv.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", srv.URL, srv.Client()))

	// A zomato txn at 16:00 UTC on 2026-05-12 — 21:30 IST Tue → "dinner".
	// seedTestDB already populates two firefly_txns rows for destination
	// "zomato" with descriptions "lunch order" and "dinner order" —
	// those should surface in the STYLE SAMPLES block.
	_, _ = c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-tc",
		Narration:         "CARD/x/Zomato/Rs/300/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "zomato",
		TxnTimestamp:      "2026-05-12T16:00:00Z",
		RawPayload:        `{"uuid":"tier3-tc","mode":"CARD","type":"OUTGOING"}`,
	})

	if !strings.Contains(capturedPrompt, "TIME CONTEXT") {
		t.Errorf("prompt missing TIME CONTEXT block:\n%s", snippet(capturedPrompt, "FOLD"))
	}
	if !strings.Contains(capturedPrompt, "dinner") {
		t.Errorf("prompt should contain meal-bucket 'dinner' for a 21:30 IST txn, body:\n%s", snippet(capturedPrompt, "TIME"))
	}
	if !strings.Contains(capturedPrompt, "STYLE SAMPLES") {
		t.Errorf("prompt missing STYLE SAMPLES block:\n%s", capturedPrompt)
	}
	// Both seeded historical descriptions should appear so the LLM has
	// the user's voice to mirror.
	for _, want := range []string{"lunch order", "dinner order"} {
		if !strings.Contains(capturedPrompt, want) {
			t.Errorf("STYLE SAMPLES missing %q:\n%s", want, snippet(capturedPrompt, "STYLE"))
		}
	}
	// Reminder line about placeholders MUST be present — that's how the
	// LLM is licensed to emit ___ rather than invent text.
	if !strings.Contains(capturedPrompt, "___") {
		t.Errorf("prompt missing placeholder-reminder line, body:\n%s", snippet(capturedPrompt, "STYLE"))
	}
}

// TestTier3_PromptIncludesHistoricalNotes: when a firefly_txn carries
// a `notes` field with the raw fold narration (e.g.
// `CARD/.../SHREE VINAYAKA ENTE/...` filed under "Sri Udupi Park,
// Indiranagar"), the Tier-3 prompt's HISTORICAL EXAMPLES block must
// surface that notes string so the LLM can map a future fold txn
// with the same truncated merchant name to the right firefly
// destination. This is the data path that closes the
// "in the age of AI, why doesn't it know?" gap.
func TestTier3_PromptIncludesHistoricalNotes(t *testing.T) {
	db := seedTestDB(t)

	// Seed a historical firefly_txn whose `notes` field contains the
	// raw fold narration. The destination is the firefly-canonical
	// merchant; the narration is the bank-side string. FTS5 must
	// find this row when the staged row's merchant tokens match
	// against `notes`.
	if _, err := db.Exec(`
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name,
		    destination_account_id, destination_account_name, destination_account_name_normalized,
		    category_id, category_name, description, tags_json, notes)
		VALUES (8001, 8001, 'withdrawal', 6000, 'INR', '2025-12-23',
		    1, 'Tata Neu HDFC Bank Credit Card',
		    501, 'Sri Udupi Park, Indiranagar', 'sri udupi park, indiranagar',
		    5, 'Food', 'Morning Filter Coffee and Kesari Bath', '[]',
		    'CARD/19b4989b2e3afba8/SHREE VINAYAKA ENTE/Rs./60.00/OUTGOING/23-12-25')
	`); err != nil {
		t.Fatalf("seed historical notes row: %v", err)
	}

	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedPrompt = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"{\"confidence\":0.1}"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", srv.URL, srv.Client()))

	// Staged row whose narration matches the historical notes. FTS
	// query uses MerchantExtracted ("shree vinayaka ente") — tokens
	// "shree", "vinayaka", "ente" all appear in the historical row's
	// `notes`. BM25 must find the row → it appears in HISTORICAL
	// EXAMPLES → the prompt carries the notes string.
	_, _ = c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-notes",
		Narration:         "CARD/19b4989b2e3afba8/SHREE VINAYAKA ENTE/Rs./60.00/OUTGOING/23-12-25",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "shree vinayaka ente",
		TxnTimestamp:      "2026-05-17T10:00:00Z",
		RawPayload:        `{"uuid":"tier3-notes"}`,
	})

	if !strings.Contains(capturedPrompt, "HISTORICAL EXAMPLES") {
		t.Fatalf("prompt missing HISTORICAL EXAMPLES block")
	}
	if !strings.Contains(capturedPrompt, "SHREE VINAYAKA ENTE") {
		t.Errorf("prompt missing the raw narration from notes — Tier-2/3 can't bridge bank↔merchant without it. snippet:\n%s",
			snippet(capturedPrompt, "HISTORICAL"))
	}
	if !strings.Contains(capturedPrompt, "Sri Udupi Park, Indiranagar") {
		t.Errorf("prompt missing canonical destination, snippet:\n%s",
			snippet(capturedPrompt, "HISTORICAL"))
	}
}

// TestTier3_KeepsDescriptionOnLowConfidence: when the LLM returns
// ok=false (low structural confidence on ids), we still want its
// description_suggestion to flow through onto the Tier-1/2 fallback
// Decision — the description is independent of structural confidence
// and is based on STYLE SAMPLES + TIME CONTEXT, both of which are
// valid signals regardless.
func TestTier3_KeepsDescriptionOnLowConfidence(t *testing.T) {
	db := seedTestDB(t)

	// LLM returns a sensible description but null structural ids and
	// confidence < 0.5 → ok=false. Tier-1 hint exists because "zomato"
	// is in merchant_lookup.
	fakeLLM := newFakeLLM(t, `{
		"txn_type": "withdrawal",
		"destination_account_id": null,
		"source_account_id": null,
		"confidence": 0.3,
		"description_suggestion": "Dinner with ___",
		"reasoning": "low structural confidence but description is independent"
	}`)
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "tier3-keep-desc",
		Narration:         "CARD/x/Zomato/Rs/300/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "zomato",
		TxnTimestamp:      "2026-05-12T16:00:00Z",
		RawPayload:        `{"uuid":"tier3-keep-desc","mode":"CARD","type":"OUTGOING"}`,
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	// Tier 1 should have won the structural decision (LLM punted)…
	if d.Tier != TierMerchantLookup {
		t.Errorf("expected TierMerchantLookup, got %d", d.Tier)
	}
	// …but the LLM's description gets preserved onto the Tier-1
	// Decision so the title isn't lost.
	if d.Description != "Dinner with ___" {
		t.Errorf("expected LLM's description to be preserved, got %q", d.Description)
	}
}

// TestTier3_ProposesNewDestinationName covers C2: for a WITHDRAWAL whose
// merchant has no matching firefly expense account, the LLM may return a
// null destination_account_id plus a destination_name_suggestion. We
// accept that as a Tier-3 decision carrying the NEW name (id nil), persist
// it to proposed_destination_account_name, and force needs_review even at
// high confidence — because pushing it will CREATE a firefly account, so a
// human should eyeball the name first.
func TestTier3_ProposesNewDestinationName(t *testing.T) {
	db := seedTestDB(t)
	// High confidence (0.9 ≥ threshold) so that landing in needs_review
	// proves the new-destination forcing, not just a low score.
	fakeLLM := newFakeLLM(t, `{
		"txn_type": "withdrawal",
		"destination_account_id": null,
		"destination_name_suggestion": "United Airlines",
		"source_account_id": 1,
		"category_id": 5,
		"confidence": 0.9,
		"description_suggestion": "DEL-___ flight booking",
		"reasoning": "airline ticket; no existing expense account, propose a new one"
	}`)
	t.Cleanup(fakeLLM.Close)

	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "", fakeLLM.URL, fakeLLM.Client()))

	// Seed the staged row so ApplyDecision's UPDATE lands.
	if _, err := db.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status)
		VALUES ('ua-newdest','{}',3516100,'INR','2026-06-23T01:27:00Z',
		    'CARD','OUTGOING','x','united airlines new delhi in','pending')`); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	d, err := c.ClassifyOne(context.Background(), StagedRow{
		FoldUUID:          "ua-newdest",
		Narration:         "CARD/x/United Airlines New Delhi In/INR/35161/OUTGOING",
		Mode:              "CARD",
		Type:              "OUTGOING",
		MerchantExtracted: "united airlines new delhi in",
		RawPayload:        `{"uuid":"ua-newdest","mode":"CARD","type":"OUTGOING"}`,
	})
	if err != nil {
		t.Fatalf("ClassifyOne: %v", err)
	}
	if d.Tier != TierLLM {
		t.Fatalf("expected Tier 3, got %d", d.Tier)
	}
	if d.DestinationAccountID != nil {
		t.Errorf("destination id should be nil for a new-name destination, got %v", *d.DestinationAccountID)
	}
	if d.DestinationAccountName != "United Airlines" {
		t.Errorf("destination name = %q, want %q", d.DestinationAccountName, "United Airlines")
	}
	if d.SourceAccountID == nil || *d.SourceAccountID != 1 {
		t.Errorf("source = %v, want 1", d.SourceAccountID)
	}

	if err := c.ApplyDecision(context.Background(), "ua-newdest", d); err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	var (
		status   string
		destID   sql.NullInt64
		destName sql.NullString
	)
	if err := db.QueryRow(`
		SELECT status, proposed_destination_account_id, proposed_destination_account_name
		FROM staged_fold_txns WHERE fold_uuid='ua-newdest'`).Scan(&status, &destID, &destName); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "needs_review" {
		t.Errorf("a new-name destination must force needs_review (even at conf 0.9), got %q", status)
	}
	if destID.Valid {
		t.Errorf("proposed_destination_account_id should be NULL, got %d", destID.Int64)
	}
	if destName.String != "United Airlines" {
		t.Errorf("proposed_destination_account_name = %q, want %q", destName.String, "United Airlines")
	}
}

// snippet returns a 200-char window around a substring for error
// messages — full bodies are several KB and unhelpful to dump.
func snippet(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "(no match for " + needle + ")"
	}
	start := i - 40
	if start < 0 {
		start = 0
	}
	end := i + 160
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// newFakeLLM returns an httptest server that always responds with the
// OpenAI chat-completions envelope wrapping the supplied JSON string
// as the assistant message content. Used by the Tier-3 test suite to
// stand in for DeepSeek (or any OpenAI-compatible host).
func newFakeLLM(t *testing.T, contentJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		envelope := map[string]any{
			"id": "chatcmpl-test",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": contentJSON,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     1,
				"completion_tokens": 1,
				"total_tokens":      2,
			},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
}
