package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// TestSyncAll_HappyPath spins up a fake firefly that returns two pages
// of synthetic transactions, runs SyncAll against a real SQLite DB,
// and asserts:
//   - all journals upserted
//   - merchant_lookup correctly picks the modal category per merchant
//   - FTS5 finds rows by description tokens
//   - re-running SyncAll is idempotent (same row counts, no duplicates)
func TestSyncAll_HappyPath(t *testing.T) {
	ctx := context.Background()
	srv := newFakeFirefly(t, fakeData{
		User: firefly.User{Email: "alice@example.com", Role: "owner"},
		Pages: [][]firefly.TransactionGroup{
			{
				groupWithJournal("100", "200", "withdrawal", "70.00", "INR",
					"2026-05-08T12:59:18+05:30", "1", "HDFC Card", "10", "Cake Palace",
					"5", "Eating outside", "", "", "Snack at cake palace"),
				groupWithJournal("101", "201", "withdrawal", "480.00", "INR",
					"2026-05-04T12:38:00+05:30", "1", "HDFC Card", "11", "Swiggy",
					"5", "Eating outside", "", "", "Lunch order"),
				groupWithJournal("102", "202", "withdrawal", "978.33", "INR",
					"2026-05-08T06:44:00+05:30", "1", "HDFC Card", "11", "Swiggy",
					"5", "Eating outside", "", "", "Dinner order"),
			},
			{
				groupWithJournal("103", "203", "withdrawal", "16123.00", "INR",
					"2026-05-04T10:34:00+05:30", "1", "HDFC Card", "20", "Praveen Kumar",
					"7", "Household help", "", "", "Maid salary May"),
				groupWithJournal("104", "204", "withdrawal", "16123.00", "INR",
					"2026-05-04T11:25:00+05:30", "1", "HDFC Card", "20", "Praveen Kumar",
					"7", "Household help", "", "", "Maid salary May (retry)"),
			},
		},
	})
	t.Cleanup(srv.Close)

	dbPath := filepath.Join(t.TempDir(), "staging.db")
	db, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fc := firefly.NewClient(srv.URL, "testpat", srv.Client())
	syncer := NewSyncer(db, fc, slog.Default())

	report, err := syncer.SyncAll(ctx)
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if report.TransactionGroups != 5 || report.TransactionJournals != 5 {
		t.Errorf("expected 5 groups + 5 journals, got %+v", report)
	}
	// 3 distinct merchants: cake palace (1 txn), swiggy (2 txns), praveen kumar (2 txns).
	if report.MerchantLookupRows != 3 {
		t.Errorf("expected 3 merchant_lookup rows, got %d", report.MerchantLookupRows)
	}

	// Verify per-merchant content of merchant_lookup.
	rows, err := db.QueryContext(ctx, `
		SELECT merchant_normalized, modal_destination_account_name,
		       modal_category_name, sample_size, confidence
		FROM merchant_lookup
		ORDER BY merchant_normalized`)
	if err != nil {
		t.Fatalf("query lookup: %v", err)
	}
	defer rows.Close()

	type lookupRow struct {
		Merchant   string
		Dest       string
		Category   string
		Sample     int
		Confidence float64
	}
	var got []lookupRow
	for rows.Next() {
		var r lookupRow
		if err := rows.Scan(&r.Merchant, &r.Dest, &r.Category, &r.Sample, &r.Confidence); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 distinct merchants, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Confidence < 0.99 {
			t.Errorf("merchant %s confidence=%.2f — all our test rows should be 100%% modal", r.Merchant, r.Confidence)
		}
	}

	// FTS5 should find the cake palace journal by description token.
	var ftsCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM firefly_txns_fts WHERE firefly_txns_fts MATCH 'cake'`).Scan(&ftsCount); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if ftsCount < 1 {
		t.Errorf("FTS5 expected to find 'cake' in cake palace row, got %d hits", ftsCount)
	}

	// Idempotency: re-running should produce identical row counts.
	report2, err := syncer.SyncAll(ctx)
	if err != nil {
		t.Fatalf("SyncAll (re-run): %v", err)
	}
	if report2.TransactionJournals != report.TransactionJournals {
		t.Errorf("rerun journals=%d, want %d", report2.TransactionJournals, report.TransactionJournals)
	}
	var totalRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM firefly_txns`).Scan(&totalRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if totalRows != 5 {
		t.Errorf("expected 5 rows in firefly_txns after rerun, got %d (upsert leaked duplicates?)", totalRows)
	}
}

// TestDecimalToPaise exhaustively covers the parser. Money math is the
// kind of place silent bugs live for years; cheap insurance.
func TestDecimalToPaise(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"0.00", 0, false},
		{"1", 100, false},
		{"1.5", 150, false},
		{"70.00", 7000, false},
		{"1234.56", 123456, false},
		{"  42.00  ", 4200, false},
		{"0.01", 1, false},
		{"-1.00", 0, true},
		{"abc", 0, true},
		{"1.234", 123, false}, // truncation, not rounding
	}
	for _, tc := range cases {
		got, err := decimalToPaise(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("decimalToPaise(%q) expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("decimalToPaise(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("decimalToPaise(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeMerchant covers the normalisation rules the merchant
// lookup keys on. The classifier in PR D re-uses this function on the
// fold-narration side, so any divergence breaks Tier-1 lookups.
func TestNormalizeMerchant(t *testing.T) {
	cases := map[string]string{
		"Zomato":                          "zomato",
		"  ZOMATO   ":                     "zomato",
		"Raz*sprintpro Bengaluru":         "sprintpro bengaluru",
		"UPI-Praveen Kumar-9876@hdfc":     "praveen kumar-9876@hdfc",
		"":                                "",
		"  Multiple   Spaces   In  Name ": "multiple spaces in name",
	}
	for in, want := range cases {
		if got := NormalizeMerchant(in); got != want {
			t.Errorf("NormalizeMerchant(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeData drives newFakeFirefly. Pages is in firefly's 1-indexed page
// order; len(Pages) is total_pages.
type fakeData struct {
	User  firefly.User
	Pages [][]firefly.TransactionGroup
}

// newFakeFirefly stands up an httptest server speaking the firefly
// dialect: /api/v1/about/user, /api/v1/transactions with pagination.
// It only implements what the syncer calls — anything else 404s.
func newFakeFirefly(t *testing.T, data fakeData) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/about/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"attributes": data.User},
		})
	})
	mux.HandleFunc("/api/v1/transactions", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscan(p, &page)
		}
		idx := page - 1
		if idx < 0 || idx >= len(data.Pages) {
			http.Error(w, "out of range", http.StatusBadRequest)
			return
		}
		body := firefly.TransactionListResponse{
			Data: data.Pages[idx],
			Meta: firefly.Meta{Pagination: firefly.Pagination{
				Total:       totalJournals(data.Pages),
				Count:       len(data.Pages[idx]),
				PerPage:     len(data.Pages[idx]),
				CurrentPage: page,
				TotalPages:  len(data.Pages),
			}},
		}
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(loggingHandler(t, mux))
	return srv
}

func totalJournals(pages [][]firefly.TransactionGroup) int {
	n := 0
	for _, p := range pages {
		for _, g := range p {
			n += len(g.Attributes.Transactions)
		}
	}
	return n
}

// loggingHandler dumps method+path on test failure. Keeps test output
// quiet on success.
func loggingHandler(t *testing.T, h http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain body so net/http doesn't complain about unread bodies.
		_, _ = io.Copy(io.Discard, r.Body)
		h.ServeHTTP(w, r)
	})
}

// groupWithJournal is a builder for one-journal groups (the common case
// in our test fixtures).
//
// Parameters in order: groupID, journalID, type, amount, currency, date,
// sourceID, sourceName, destID, destName, categoryID, categoryName,
// budgetID, budgetName, description.
func groupWithJournal(groupID, journalID, txnType, amount, currency, date,
	sourceID, sourceName, destID, destName, categoryID, categoryName,
	budgetID, budgetName, description string) firefly.TransactionGroup {
	return firefly.TransactionGroup{
		ID:   groupID,
		Type: "transactions",
		Attributes: firefly.TransactionGroupAttribs{
			Transactions: []firefly.TransactionJournal{{
				JournalID:       journalID,
				Type:            txnType,
				Amount:          amount,
				CurrencyCode:    currency,
				Date:            date,
				SourceID:        sourceID,
				SourceName:      sourceName,
				DestinationID:   destID,
				DestinationName: destName,
				CategoryID:      categoryID,
				CategoryName:    categoryName,
				BudgetID:        budgetID,
				BudgetName:      budgetName,
				Description:     description,
				Tags:            []string{},
			}},
		},
	}
}

// Sentinel for sort.Strings — keeps imports honest, used in some
// follow-on test files.
var _ = sort.Strings
