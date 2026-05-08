package firefly

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClient_AboutUser_HappyPath confirms the auth + accept headers are
// set and the response is decoded.
func TestClient_AboutUser_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/about/user" {
			t.Errorf("unexpected path: %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer testpat" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		if got := r.Header.Get("Accept"); !strings.Contains(got, "application/vnd.api+json") {
			t.Errorf("missing accept header: %q", got)
		}
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{"data":{"attributes":{"email":"alice@example.com","role":"owner","blocked":false}}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "testpat", srv.Client())
	got, err := c.AboutUser(context.Background())
	if err != nil {
		t.Fatalf("AboutUser: %v", err)
	}
	if got.Email != "alice@example.com" || got.Role != "owner" || got.Blocked {
		t.Errorf("unexpected user: %+v", got)
	}
}

// TestClient_ListTransactions_PaginationFields confirms the pagination
// meta block decodes correctly — the sync orchestrator depends on
// total_pages to know when to stop.
func TestClient_ListTransactions_PaginationFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{
			"data":[{"id":"7801","type":"transactions","attributes":{"transactions":[{
				"transaction_journal_id":"7801","type":"withdrawal","amount":"70.00",
				"currency_code":"INR","date":"2026-05-08T12:59:18+05:30",
				"source_id":"1","source_name":"HDFC Card","destination_id":"234",
				"destination_name":"Cake Palace","category_id":"5","category_name":"Eating outside",
				"budget_id":"","budget_name":"","description":"Snack","tags":[],"external_id":""
			}]}}],
			"meta":{"pagination":{"total":7014,"count":1,"per_page":50,"current_page":2,"total_pages":141}}
		}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "x", srv.Client())
	resp, err := c.ListTransactions(context.Background(), 2, 50)
	if err != nil {
		t.Fatalf("ListTransactions: %v", err)
	}
	if resp.Meta.Pagination.TotalPages != 141 {
		t.Errorf("expected 141 total pages, got %d", resp.Meta.Pagination.TotalPages)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Attributes.Transactions) != 1 {
		t.Fatalf("expected 1 group with 1 journal, got %#v", resp.Data)
	}
	j := resp.Data[0].Attributes.Transactions[0]
	if j.JournalID != "7801" || j.Amount != "70.00" || j.DestinationName != "Cake Palace" {
		t.Errorf("unexpected journal: %+v", j)
	}
}

// TestClient_NonOK_ReturnsTypedError confirms that a 401 (or anything
// non-2xx) surfaces as *Error so the sync orchestrator can branch
// (e.g., re-check the PAT on auth failure).
func TestClient_NonOK_ReturnsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Unauthenticated."}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, "wrong", srv.Client())
	_, err := c.AboutUser(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	ferr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if ferr.Status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", ferr.Status)
	}
	if !strings.Contains(ferr.Body, "Unauthenticated") {
		t.Errorf("body should include firefly's message: %q", ferr.Body)
	}
}

// TestClient_RejectsEmptyConfig guards against a misconfigured deploy
// reaching the firefly host with empty headers.
func TestClient_RejectsEmptyConfig(t *testing.T) {
	tests := []struct {
		name      string
		base, pat string
	}{
		{"no base", "", "pat"},
		{"no pat", "http://localhost", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient(tt.base, tt.pat, http.DefaultClient)
			if _, err := c.AboutUser(context.Background()); err == nil {
				t.Fatal("expected error for empty config")
			}
		})
	}
}
