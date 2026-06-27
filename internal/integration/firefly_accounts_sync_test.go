package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// fakeFireflyAccounts serves GET /api/v1/accounts with the given pages,
// honouring the ?page= query param and reporting TotalPages so the
// syncer's pagination loop terminates.
func fakeFireflyAccounts(t *testing.T, pages [][]firefly.Account) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		var data []firefly.Account
		if page-1 < len(pages) {
			data = pages[page-1]
		}
		resp := firefly.AccountListResponse{Data: data}
		resp.Meta.Pagination.CurrentPage = page
		resp.Meta.Pagination.TotalPages = len(pages)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

func acct(id int64, name, typ, role, number string) firefly.Account {
	return firefly.Account{
		ID:   strconv.FormatInt(id, 10),
		Type: "accounts",
		Attributes: firefly.AccountAttribs{
			Name: name, Type: typ, Active: true, AccountRole: role, AccountNumber: number,
		},
	}
}

// TestFireflyAccountsSync_MirrorsAssets verifies the syncer paginates,
// upserts every account, counts assets, and stores the account_number
// (the field the source resolver matches a fold card's last4 against).
func TestFireflyAccountsSync_MirrorsAssets(t *testing.T) {
	ctx := context.Background()
	srv := fakeFireflyAccounts(t, [][]firefly.Account{
		{
			acct(1314, "Ixigo AU Bank Credit Card", "asset", "ccAsset", "40697750350291"),
			acct(12, "HDFC Bank", "asset", "savingAsset", "50100304725684"),
			acct(900, "Zomato", "expense", "", ""), // expense — mirrored but not an asset
		},
		{
			acct(954, "Scapia Federal Bank Credit Card", "asset", "ccAsset", "40298600009717"),
		},
	})
	t.Cleanup(srv.Close)

	db, err := Open(ctx, filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	syncer := NewFireflyAccountsSyncer(db, firefly.NewClient(srv.URL, "testpat", srv.Client()), slog.Default())
	report, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if report.Pages != 2 || report.Fetched != 4 || report.Upserted != 4 {
		t.Errorf("report = %+v, want pages=2 fetched=4 upserted=4", report)
	}
	if report.Assets != 3 {
		t.Errorf("assets = %d, want 3", report.Assets)
	}

	// The AU card must be in the mirror with its account_number intact.
	var name, number, typ, role string
	if err := db.QueryRow(`
		SELECT name, account_number, type, account_role
		FROM firefly_accounts WHERE firefly_id = 1314`).Scan(&name, &number, &typ, &role); err != nil {
		t.Fatalf("query AU asset: %v", err)
	}
	if name != "Ixigo AU Bank Credit Card" || number != "40697750350291" || typ != "asset" || role != "ccAsset" {
		t.Errorf("AU row = (%q,%q,%q,%q)", name, number, typ, role)
	}

	// Re-running is idempotent: still 4 rows, no duplicates.
	if _, err := syncer.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM firefly_accounts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("row count after re-sync = %d, want 4 (idempotent)", n)
	}
}
