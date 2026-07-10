package firefly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateTransaction_HappyPath validates header set, body shape,
// and that the firefly journal id is parsed correctly.
func TestCreateTransaction_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/transactions" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer testpat" {
			t.Errorf("auth: %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var got CreateTransactionRequest
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if len(got.Transactions) != 1 {
			t.Errorf("expected 1 line, got %d", len(got.Transactions))
		}
		if got.Transactions[0].ExternalID != "fold-uuid-x" {
			t.Errorf("external_id: %q", got.Transactions[0].ExternalID)
		}
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{
			"data":{"id":"7950","attributes":{"transactions":[{"transaction_journal_id":"7951"}]}}
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "testpat", srv.Client())
	resp, err := c.CreateTransaction(context.Background(), CreateTransactionRequest{
		Transactions: []CreateTransactionLine{{
			Type:        "withdrawal",
			Date:        "2026-05-08T12:59:18+05:30",
			Amount:      "70.00",
			Description: "Snack at cake palace",
			SourceID:    "1",
			DestinationID: "10",
			ExternalID:  "fold-uuid-x",
		}},
	})
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if resp.GroupID != 7950 {
		t.Errorf("group id = %d, want 7950", resp.GroupID)
	}
	if len(resp.JournalIDs) != 1 || resp.JournalIDs[0] != 7951 {
		t.Errorf("journal ids = %v, want [7951]", resp.JournalIDs)
	}
}

// TestCreateTransaction_4xxReturnsTypedError surfaces firefly's
// validation errors as *Error so the caller can branch on Status.
func TestCreateTransaction_4xxReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"The selected source must exist."}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "p", srv.Client())
	_, err := c.CreateTransaction(context.Background(), CreateTransactionRequest{
		Transactions: []CreateTransactionLine{{
			Type: "withdrawal", Date: "2026-05-08", Amount: "1.00",
			Description: "x", SourceID: "9999",
		}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	ferr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if ferr.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", ferr.Status)
	}
	if !strings.Contains(ferr.Body, "must exist") {
		t.Errorf("expected firefly's message in body: %q", ferr.Body)
	}
}

// TestSearchByExternalID returns IDs for the dedup check.
func TestSearchByExternalID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search/transactions" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != "external_id_is:fold-uuid-x" {
			t.Errorf("query: %q", got)
		}
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{
			"data":[{"attributes":{"transactions":[{"transaction_journal_id":"42"},{"transaction_journal_id":"43"}]}}]
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "p", srv.Client())
	ids, err := c.SearchByExternalID(context.Background(), "fold-uuid-x")
	if err != nil {
		t.Fatalf("SearchByExternalID: %v", err)
	}
	if len(ids) != 2 || ids[0] != 42 || ids[1] != 43 {
		t.Errorf("ids = %v, want [42 43]", ids)
	}
}

// TestSearchByExternalID_Empty returns nil cleanly when firefly has
// no match, so the caller can branch.
func TestSearchByExternalID_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "p", srv.Client())
	ids, err := c.SearchByExternalID(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("SearchByExternalID: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

// TestEnsureCurrency covers the three states: create-when-404,
// enable-when-disabled, and no-op-when-enabled.
func TestEnsureCurrency(t *testing.T) {
	cases := []struct {
		name        string
		code        string
		getStatus   int
		getEnabled  bool
		wantCreate  bool // expect POST /currencies
		wantEnable  bool // expect POST /currencies/{code}/enable
	}{
		{"create when not defined", "AED", http.StatusNotFound, false, true, false},
		{"enable when disabled", "THB", http.StatusOK, false, false, true},
		{"noop when enabled", "USD", http.StatusOK, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var createHit, enableHit bool
			var createBody map[string]any
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/currencies/"+tc.code, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.api+json")
				if tc.getStatus != http.StatusOK {
					w.WriteHeader(tc.getStatus)
					_, _ = w.Write([]byte(`{"message":"Resource not found"}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{"attributes":{"enabled":` + boolStr(tc.getEnabled) + `}}}`))
			})
			mux.HandleFunc("/api/v1/currencies/"+tc.code+"/enable", func(w http.ResponseWriter, r *http.Request) {
				enableHit = true
				_, _ = w.Write([]byte(`{"data":{}}`))
			})
			mux.HandleFunc("/api/v1/currencies", func(w http.ResponseWriter, r *http.Request) {
				createHit = true
				_ = json.NewDecoder(r.Body).Decode(&createBody)
				_, _ = w.Write([]byte(`{"data":{}}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := NewClient(srv.URL, "p", srv.Client())
			if err := c.EnsureCurrency(context.Background(), tc.code); err != nil {
				t.Fatalf("EnsureCurrency: %v", err)
			}
			if createHit != tc.wantCreate {
				t.Errorf("createHit=%v want %v", createHit, tc.wantCreate)
			}
			if enableHit != tc.wantEnable {
				t.Errorf("enableHit=%v want %v", enableHit, tc.wantEnable)
			}
			if tc.wantCreate && createBody["code"] != tc.code {
				t.Errorf("create body code=%v want %s", createBody["code"], tc.code)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestUpdateTransaction verifies UpdateTransaction PUTs to the group path
// and parses the returned ids.
func TestUpdateTransaction(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{"data":{"id":"7950","attributes":{"transactions":[{"transaction_journal_id":"7951"}]}}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "p", srv.Client())
	resp, err := c.UpdateTransaction(context.Background(), 7950, CreateTransactionRequest{
		Transactions: []CreateTransactionLine{{Type: "transfer", Amount: "10.00", Description: "x", TransactionJournalID: "7951"}},
	})
	if err != nil {
		t.Fatalf("UpdateTransaction: %v", err)
	}
	if method != http.MethodPut || path != "/api/v1/transactions/7950" {
		t.Errorf("got %s %s, want PUT /api/v1/transactions/7950", method, path)
	}
	if resp.GroupID != 7950 {
		t.Errorf("group=%d, want 7950", resp.GroupID)
	}
}
