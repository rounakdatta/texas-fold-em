package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// uiTestHarness wraps an httptest.Server hosting the UI mounted on a
// fresh /admin/ui/* mux, plus the underlying SQLite DB so tests can
// poke and verify state.
type uiTestHarness struct {
	server *httptest.Server
	db     *integration.DB
}

func newUITestHarness(t *testing.T, auth AuthMode) *uiTestHarness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "staging.db")
	idb, err := integration.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })

	// Seed one needs_review row.
	if _, err := idb.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    classifier_tier, classifier_confidence, classifier_evidence_json,
		    proposed_destination_account_id, proposed_category_id, proposed_description)
		VALUES ('rev-1','{}',7000,'INR','2026-05-08T12:59:18Z',
		        'CARD','OUTGOING','CARD/x/Cake Palace/Rs/70.00/OUTGOING','cake palace','needs_review',
		        4, 0, '{"tier":4,"note":"no match"}',
		        12, 6, 'Snack at cake palace')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Pusher needs a firefly client; make a fake that just 200s the create
	// (the UI's push tests will exercise that codepath).
	fakeFF := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/search/transactions"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/transactions"):
			_, _ = w.Write([]byte(`{"data":{"id":"7950","attributes":{"transactions":[{"transaction_journal_id":"7951"}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fakeFF.Close)

	pusher := integration.NewPusher(idb, firefly.NewClient(fakeFF.URL, "p", fakeFF.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)

	h, err := New(idb.DB, pusher, slog.New(slog.NewTextHandler(io.Discard, nil)), "admin-key", auth)
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}

	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &uiTestHarness{server: srv, db: idb}
}

// fetch helper that doesn't follow redirects so tests can assert 303.
func (u *uiTestHarness) do(t *testing.T, method, path string, form url.Values, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	var body io.Reader
	var contentType string
	if form != nil {
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequest(method, u.server.URL+path, body)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestUI_IndexBypassMode renders the index page when auth is bypass.
func TestUI_IndexBypassMode(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	resp := u.do(t, "GET", "/admin/ui/?status=needs_review", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "rev-1") {
		t.Errorf("expected rev-1 in index body, got: %s", body)
	}
	if !strings.Contains(string(body), "cake palace") {
		t.Errorf("expected merchant in body, got: %s", body)
	}
}

// TestUI_CookieAuth_Blocks blocks requests without the cookie.
func TestUI_CookieAuth_Blocks(t *testing.T) {
	u := newUITestHarness(t, AuthModeCookie)
	resp := u.do(t, "GET", "/admin/ui/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

// TestUI_CookieAuth_AllowsAfterLogin: hitting /admin/ui/login sets the
// cookie that subsequent requests can use.
func TestUI_CookieAuth_AllowsAfterLogin(t *testing.T) {
	u := newUITestHarness(t, AuthModeCookie)
	resp := u.do(t, "GET", "/admin/ui/login?key=admin-key", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "tfe-admin" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("expected tfe-admin cookie set")
	}
	resp2 := u.do(t, "GET", "/admin/ui/", nil, cookie)
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("expected 200 with cookie, got %d", resp2.StatusCode)
	}
}

// TestUI_Detail_Renders verifies the detail page loads and contains
// the editable form fields populated from proposed_*.
func TestUI_Detail_Renders(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	resp := u.do(t, "GET", "/admin/ui/staged/rev-1", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `name="destination_account_id"`) {
		t.Errorf("expected destination_account_id input field")
	}
	// Proposed value 12 should pre-fill.
	if !strings.Contains(s, `value="12"`) {
		t.Errorf("expected proposed destination 12 to pre-fill")
	}
	if !strings.Contains(s, "Snack at cake palace") {
		t.Errorf("expected proposed description in textarea")
	}
}

// TestUI_Save_PersistsAndBumpsStatus verifies the save handler updates
// confirmed_* and transitions status from needs_review → ready_to_push.
func TestUI_Save_PersistsAndBumpsStatus(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	form := url.Values{}
	form.Set("destination_account_id", "12")
	form.Set("source_account_id", "1")
	form.Set("category_id", "6")
	form.Set("budget_id", "")
	form.Set("description", "edited description")
	form.Set("tags", "food, evening")

	resp := u.do(t, "POST", "/admin/ui/staged/rev-1/save", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}

	// Verify state.
	var (
		status     string
		confDestID int64
		confDesc   string
		confTags   string
	)
	if err := u.db.DB.QueryRow(`
		SELECT status, confirmed_destination_account_id, confirmed_description, COALESCE(confirmed_tags_json,'')
		FROM staged_fold_txns WHERE fold_uuid='rev-1'
	`).Scan(&status, &confDestID, &confDesc, &confTags); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "ready_to_push" {
		t.Errorf("status=%q, want ready_to_push", status)
	}
	if confDestID != 12 {
		t.Errorf("confirmed_destination_account_id=%d, want 12", confDestID)
	}
	if confDesc != "edited description" {
		t.Errorf("confirmed_description=%q", confDesc)
	}
	if !strings.Contains(confTags, "food") || !strings.Contains(confTags, "evening") {
		t.Errorf("confirmed_tags_json=%q", confTags)
	}
}

// TestUI_Skip_Persists transitions to skipped without touching firefly.
func TestUI_Skip_Persists(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	resp := u.do(t, "POST", "/admin/ui/staged/rev-1/skip", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var status string
	if err := u.db.DB.QueryRow(`SELECT status FROM staged_fold_txns WHERE fold_uuid='rev-1'`).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "skipped" {
		t.Errorf("status=%q, want skipped", status)
	}
}

// TestUI_Push_HappyPath: form posts to /push, save runs, then Pusher
// fires (fake firefly returns 200 with a journal id), status=pushed.
func TestUI_Push_HappyPath(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)

	form := url.Values{}
	form.Set("destination_account_id", "12")
	form.Set("source_account_id", "1")
	form.Set("category_id", "6")
	form.Set("budget_id", "")
	form.Set("description", "snack")
	form.Set("tags", "")

	resp := u.do(t, "POST", "/admin/ui/staged/rev-1/push", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 303, got %d: %s", resp.StatusCode, body)
	}
	var (
		status string
		fid    int64
	)
	if err := u.db.DB.QueryRow(`SELECT status, firefly_txn_id FROM staged_fold_txns WHERE fold_uuid='rev-1'`).Scan(&status, &fid); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "pushed" {
		t.Errorf("status=%q, want pushed", status)
	}
	if fid != 7951 {
		t.Errorf("firefly_txn_id=%d, want 7951", fid)
	}
}
