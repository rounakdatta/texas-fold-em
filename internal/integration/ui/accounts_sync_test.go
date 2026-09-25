package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// syncHarness: a fold whose copy of Firefly's accounts can fall behind a
// Firefly whose account list a test changes (or breaks). Names are made up.
type syncHarness struct {
	db  *integration.DB
	srv *httptest.Server

	mu       sync.Mutex
	accounts []firefly.Account
	down     bool
}

func ffAccount(id int, name, typ string, active bool) firefly.Account {
	return firefly.Account{ID: fmt.Sprint(id), Type: "accounts", Attributes: firefly.AccountAttribs{Name: name, Type: typ, Active: active, CurrencyCode: "INR"}}
}

func newSyncHarness(t *testing.T, withSyncer bool) *syncHarness {
	t.Helper()
	idb, err := integration.Open(context.Background(), filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })
	sh := &syncHarness{db: idb}
	mustExec(t, idb, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload, last_synced_at) VALUES
		(10, 'Harbour Bank', 'asset', 1, '{}', '2026-01-02 03:04:05'),
		(30, 'Corner Bakery', 'expense', 1, '{}', '2026-01-02 03:04:05'),
		(50, 'Old Card', 'asset', 0, '{}', '2026-01-02 03:04:05')`)
	sh.accounts = []firefly.Account{ffAccount(10, "Harbour Bank", "asset", true), ffAccount(30, "Corner Bakery", "expense", true), ffAccount(50, "Old Card", "asset", false)}

	ff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		if r.URL.Path != "/api/v1/accounts" {
			http.NotFound(w, r)
			return
		}
		if sh.down {
			http.Error(w, `{"message":"down"}`, http.StatusInternalServerError)
			return
		}
		resp := firefly.AccountListResponse{Data: sh.accounts}
		resp.Meta.Pagination.CurrentPage, resp.Meta.Pagination.TotalPages = 1, 1
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ff.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := firefly.NewClient(ff.URL, "p", ff.Client())
	h, err := New(idb.DB, integration.NewPusher(idb, fc, log, false), log, "admin-key", AuthModeBypass)
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	if withSyncer {
		h.SetFireflyAccountsSyncer(integration.NewFireflyAccountsSyncer(idb, fc, log))
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	sh.srv = httptest.NewServer(mux)
	t.Cleanup(sh.srv.Close)
	return sh
}

type syncReply struct {
	OK       bool            `json:"ok"`
	SyncedAt string          `json:"syncedAt"`
	Added    []syncedAccount `json:"added"`
	Message  string          `json:"message"`
}

func (sh *syncHarness) sync(t *testing.T) (int, syncReply) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, sh.srv.URL+"/admin/ui/api/sync-accounts", nil)
	req.Header.Set("X-Fold-UI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out syncReply
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// An account made in Firefly since the last sync arrives, and the answer
// names it: your own accounts first (by what you call them), then payees,
// then payers. What is closed, or not offered anywhere, isn't news.
func TestSyncAccounts_SaysWhatArrived(t *testing.T) {
	sh := newSyncHarness(t, true)
	sh.mu.Lock()
	sh.accounts = append(sh.accounts,
		ffAccount(20, "Kestrel Bank Credit Card", "asset", true),
		ffAccount(31, "Night Market Stall", "expense", true),
		ffAccount(40, "Payroll Co", "revenue", true),
		ffAccount(60, "Closed Card", "asset", false),
		ffAccount(70, "Cash account", "cash", true))
	sh.mu.Unlock()

	code, r := sh.sync(t)
	if code != http.StatusOK || !r.OK {
		t.Fatalf("status %d: %+v", code, r)
	}
	var got []string
	for _, a := range r.Added {
		got = append(got, a.Kind+":"+a.Short)
	}
	if strings.Join(got, ", ") != "account:Kestrel card, payee:Night Market Stall, payer:Payroll Co" {
		t.Errorf("added = %v", got)
	}
	if r.Message != "Synced — 3 new: Kestrel card, Night Market Stall, Payroll Co." {
		t.Errorf("message = %q", r.Message)
	}
	if at, err := time.Parse(time.RFC3339, r.SyncedAt); err != nil || time.Since(at) > time.Minute {
		t.Errorf("syncedAt = %q, want just now", r.SyncedAt)
	}
	// asked again, nothing is new: the answer says so plainly
	if _, r2 := sh.sync(t); len(r2.Added) != 0 || r2.Message != "Synced — nothing new in Firefly." {
		t.Errorf("second sync = %+v", r2)
	}
}

// An account closed in fold's copy and open again in Firefly is back in the
// pickers, so it counts as arrived.
func TestSyncAccounts_AReopenedAccountIsNew(t *testing.T) {
	sh := newSyncHarness(t, true)
	sh.mu.Lock()
	sh.accounts[2] = ffAccount(50, "Old Card", "asset", true)
	sh.mu.Unlock()
	if _, r := sh.sync(t); len(r.Added) != 1 || r.Added[0].Name != "Old Card" {
		t.Errorf("added = %+v, want Old Card", r.Added)
	}
}

// Firefly unreachable: a plain sentence, fold's copy untouched.
func TestSyncAccounts_FireflyDown(t *testing.T) {
	sh := newSyncHarness(t, true)
	sh.mu.Lock()
	sh.down = true
	sh.mu.Unlock()
	code, r := sh.sync(t)
	if code != http.StatusBadGateway || r.OK || r.Message != "Couldn’t reach Firefly — try again in a moment." {
		t.Errorf("got %d %+v", code, r)
	}
	var n int
	_ = sh.db.DB.QueryRow(`SELECT COUNT(*) FROM firefly_accounts`).Scan(&n)
	if n != 3 {
		t.Errorf("mirror has %d rows, want the 3 it had", n)
	}
}

func TestSyncAccounts_NotSetUp(t *testing.T) {
	sh := newSyncHarness(t, false)
	if code, r := sh.sync(t); code != http.StatusServiceUnavailable || r.Message != "Account sync isn’t set up on this fold." {
		t.Errorf("got %d %+v", code, r)
	}
}

// Without JavaScript the button is a form post: it comes back to the page it
// was pressed on (never somewhere else), saying what arrived.
func TestSyncAccounts_FormPostComesBack(t *testing.T) {
	sh := newSyncHarness(t, true)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(referer string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, sh.srv.URL+"/admin/ui/sync-accounts", nil)
		req.Header.Set("Referer", referer)
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	resp := post(sh.srv.URL + "/admin/ui/staged/abc?back=%2Fadmin%2Fui%2Freview")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/ui/staged/abc?back=%2Fadmin%2Fui%2Freview" {
		t.Errorf("got %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var flash string
	for _, c := range resp.Cookies() {
		if c.Name == "tfe-flash" {
			flash, _ = url.QueryUnescape(c.Value)
		}
	}
	if !strings.Contains(flash, "Synced — nothing new in Firefly.") {
		t.Errorf("flash = %q", flash)
	}
	if resp := post("https://elsewhere.example/phish"); resp.Header.Get("Location") != "/admin/ui/" {
		t.Errorf("a foreign referer sent it to %q", resp.Header.Get("Location"))
	}
}

// Every page knows when the copy was last made; the deck and the editor carry
// the Accounts card, the Add form the one-line version — and none of them
// when there is nothing to sync with.
func TestSyncAccounts_OnEveryPage(t *testing.T) {
	sh := newSyncHarness(t, true)
	mustExec(t, sh.db, `INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration, merchant_extracted, status)
		VALUES ('row-1', '{}', 5000, 'INR', '2026-01-03T10:00:00Z', 'CARD', 'OUTGOING', 'CARD/x/Corner Bakery/Rs./50.00/OUTGOING', 'corner bakery', 'needs_review')`)
	for path, want := range map[string]string{
		"/admin/ui/review":       `class="side-sync"`,
		"/admin/ui/staged/row-1": `class="panel sync-card"`,
		"/admin/ui/new":          `class="sync-inline"`,
		"/admin/ui/?status=all":  `id="sync-accounts-form"`,
	} {
		body := getBody(t, sh.srv.URL+path)
		if !strings.Contains(body, want) || !strings.Contains(body, `data-synced-at="2026-01-02T03:04:05Z"`) || !strings.Contains(body, `data-can-sync="1"`) {
			t.Errorf("%s: missing %s or the sync state", path, want)
		}
		if path != "/admin/ui/?status=all" && !strings.Contains(body, "Last synced <time datetime=\"2026-01-02T03:04:05Z\" data-ago>") {
			t.Errorf("%s: doesn't say when it last synced", path)
		}
	}
	off := newSyncHarness(t, false)
	if body := getBody(t, off.srv.URL+"/admin/ui/review"); strings.Contains(body, "side-sync") || strings.Contains(body, "data-can-sync") {
		t.Error("with no syncer the deck still offers a sync")
	}
	var o struct {
		SyncedAt string `json:"syncedAt"`
	}
	_ = json.Unmarshal([]byte(getBody(t, sh.srv.URL+"/admin/ui/api/options")), &o)
	if o.SyncedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("options syncedAt = %q", o.SyncedAt)
	}
}

func getBody(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	return string(b)
}

func TestSyncMessage(t *testing.T) {
	a := func(names ...string) (out []syncedAccount) {
		for _, n := range names {
			out = append(out, syncedAccount{Name: n, Short: n})
		}
		return
	}
	for _, c := range []struct {
		in   []syncedAccount
		want string
	}{
		{nil, "Synced — nothing new in Firefly."},
		{a("Kestrel card"), "Synced — 1 new: Kestrel card."},
		{a("A", "B", "C"), "Synced — 3 new: A, B, C."},
		{a("A", "B", "C", "D", "E"), "Synced — 5 new: A, B, C and 2 more."},
	} {
		if got := syncMessage(c.in); got != c.want {
			t.Errorf("%d accounts: %q, want %q", len(c.in), got, c.want)
		}
	}
}

// Never rounded up: 59 minutes and a half is still "59 min ago".
func TestAgoText(t *testing.T) {
	at := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{14*time.Minute + 50*time.Second, "14 min ago"},
		{59*time.Minute + 30*time.Second, "59 min ago"},
		{90 * time.Minute, "1 hour ago"},
		{5*time.Hour + 59*time.Minute, "5 hours ago"},
	} {
		if got := agoText(at(c.d)); got != c.want {
			t.Errorf("%v ago: %q, want %q", c.d, got, c.want)
		}
	}
	if got := agoText(at(10 * 24 * time.Hour)); !strings.HasPrefix(got, "on ") {
		t.Errorf("ten days ago: %q", got)
	}
	if agoText("not a time") != "" {
		t.Error("garbage in should say nothing")
	}
}
