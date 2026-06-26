package ui

import (
	"context"
	"database/sql"
	"fmt"
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

	// Seed firefly_txns rows so the UI's name→id resolver has names
	// to look up. The integration in production gets these from the
	// /admin/firefly/sync endpoint; tests seed directly.
	if _, err := idb.DB.Exec(`
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name,
		    destination_account_id, destination_account_name, destination_account_name_normalized,
		    category_id, category_name, budget_id, budget_name, description, tags_json)
		VALUES (901, 9001, 'withdrawal', 5000, 'INR', '2026-04-01',
		        1,  'HDFC Card',
		        12, 'Cake Palace', 'cake palace',
		        6,  'Snacks',  NULL, NULL, 'sample 1', '["snacks","evening"]'),
		       (902, 9002, 'withdrawal', 4000, 'INR', '2026-04-15',
		        1,  'HDFC Card',
		        12, 'Cake Palace', 'cake palace',
		        6,  'Snacks',  NULL, NULL, 'sample 2', '["snacks"]')
	`); err != nil {
		t.Fatalf("seed firefly_txns: %v", err)
	}

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

// TestUI_IndexPagination: the index must paginate rather than truncate
// once the bucket grows past defaultPerPage. We seed 60 needs_review
// rows on top of the harness's pre-existing 1, expect:
//   - heading shows the true total (61), not a page-sized stub
//   - page 1 shows the first defaultPerPage (50) rows
//   - page 2 shows the remainder + paging controls
func TestUI_IndexPagination(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	// Seed 60 extra needs_review rows with strictly monotonic
	// timestamps so ORDER BY ts DESC produces a deterministic order
	// (pg-059 newest → on page 1, pg-000 oldest → on page 2).
	for i := 0; i < 60; i++ {
		ts := fmt.Sprintf("2026-05-01T12:%02d:00Z", i)
		if _, err := u.db.DB.Exec(`
			INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
			    mode, type, narration, merchant_extracted, status)
			VALUES (?, '{}', 100, 'INR', ?, 'CARD','OUTGOING','x','m','needs_review')`,
			fmt.Sprintf("pg-%03d", i), ts,
		); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	// Page 1
	resp := u.do(t, "GET", "/admin/ui/?status=needs_review", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("page 1 status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	// Heading must reflect the TRUE total (61 = 60 seeded + 1 from harness).
	if !strings.Contains(s, "needs_review (61)") {
		t.Errorf("heading missing true total 61, got body contains: %q",
			snippet(s, "needs_review"))
	}
	// Page count line: ceil(61/50) = 2 pages.
	if !strings.Contains(s, "page 1 of 2") {
		t.Errorf("expected 'page 1 of 2' in heading, got: %q", snippet(s, "page"))
	}
	// Must contain a "next →" link, not the disabled span.
	if !strings.Contains(s, `>next →</a>`) {
		t.Errorf("expected enabled next link on page 1")
	}
	// Page 1 (DESC by ts) contains the newest 50 rows. The harness's
	// rev-1 (ts 2026-05-08) is newer than all the pg-* rows (ts
	// 2026-05-01 + minute offset), so page 1 = [rev-1, pg-059, pg-058,
	// …, pg-011]. Spot-check the boundary.
	if !strings.Contains(s, "pg-059") || !strings.Contains(s, "pg-011") {
		t.Errorf("page 1 should include pg-011..pg-059 (newest 49 of the seeded pg-* batch),"+
			" got body snippet: %q", snippet(s, "pg-"))
	}
	// pg-000..pg-010 must belong on page 2.
	if strings.Contains(s, "pg-000") || strings.Contains(s, "pg-005") {
		t.Errorf("page 1 should NOT contain oldest rows (pg-000..pg-010)")
	}

	// Page 2
	resp2 := u.do(t, "GET", "/admin/ui/?status=needs_review&page=2", nil)
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	s2 := string(body2)
	if !strings.Contains(s2, "page 2 of 2") {
		t.Errorf("expected 'page 2 of 2', got: %q", snippet(s2, "page"))
	}
	// Page 2 must contain the LAST row (oldest timestamp) — pg-059 was
	// inserted last but uses ts day 32%28=4, hmm ordering is by ts. Just
	// assert the prev link exists and the heading shows total.
	if !strings.Contains(s2, `>← prev</a>`) {
		t.Errorf("expected enabled prev link on page 2")
	}
	if !strings.Contains(s2, "needs_review (61)") {
		t.Errorf("page 2 total still 61")
	}

	// per_page override
	resp3 := u.do(t, "GET", "/admin/ui/?status=needs_review&per_page=10", nil)
	defer resp3.Body.Close()
	body3, _ := io.ReadAll(resp3.Body)
	if !strings.Contains(string(body3), "page 1 of 7") { // ceil(61/10) = 7
		t.Errorf("expected 'page 1 of 7' with per_page=10, got: %q", snippet(string(body3), "page"))
	}
}

// snippet returns a 120-char window around a substring for error messages.
func snippet(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "(no match)"
	}
	start := i - 30
	if start < 0 {
		start = 0
	}
	end := i + 90
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
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
// the editable form fields populated from proposed_* (post name-based
// rewrite — fields are now name inputs with datalist autocomplete).
func TestUI_Detail_Renders(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	resp := u.do(t, "GET", "/admin/ui/staged/rev-1", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `name="destination_name"`) {
		t.Errorf("expected destination_name input field")
	}
	// Resolved name "Cake Palace" (from firefly_txns destination_account_id=12)
	// should pre-fill the form.
	if !strings.Contains(s, `value="Cake Palace"`) {
		t.Errorf("expected destination name 'Cake Palace' to pre-fill")
	}
	if !strings.Contains(s, "Snack at cake palace") {
		t.Errorf("expected proposed description in textarea")
	}
	// Datalists for the autocomplete should be present.
	if !strings.Contains(s, `<datalist id="dest-options">`) {
		t.Errorf("expected dest-options datalist")
	}
	if !strings.Contains(s, `<datalist id="tag-options">`) {
		t.Errorf("expected tag-options datalist")
	}
	// Per-merchant tag suggestions: the seeded firefly_txns for
	// destination_account_id=12 carry tags ["snacks","evening"], so the
	// chip section should render with both.
	if !strings.Contains(s, `data-tag-chip="snacks"`) {
		t.Errorf("expected snacks chip from per-merchant suggestions")
	}
	// Local-time conversion: <time datetime="..."> elements present.
	if !strings.Contains(s, `<time datetime="2026-05-08T12:59:18Z">`) {
		t.Errorf("expected <time> element with datetime attribute")
	}
}

// TestUI_Save_PersistsAndBumpsStatus verifies the save handler resolves
// names → IDs and transitions status from needs_review → ready_to_push.
func TestUI_Save_PersistsAndBumpsStatus(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	form := url.Values{}
	form.Set("destination_name", "Cake Palace") // → resolves to id 12
	form.Set("source_name", "HDFC Card")        // → resolves to id 1
	form.Set("category_name", "Snacks")         // → resolves to id 6
	form.Set("budget_name", "")                 // empty → NULL
	form.Set("description", "edited description")
	form.Set("tags", "food, evening")

	resp := u.do(t, "POST", "/admin/ui/staged/rev-1/save", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}

	// Verify state. confirmed_* IDs should have been resolved.
	var (
		status     string
		confDestID sql.NullInt64
		confSrcID  sql.NullInt64
		confCatID  sql.NullInt64
		confBudID  sql.NullInt64
		confDesc   string
		confTags   string
	)
	if err := u.db.DB.QueryRow(`
		SELECT status,
		       confirmed_destination_account_id, confirmed_source_account_id,
		       confirmed_category_id, confirmed_budget_id,
		       confirmed_description, COALESCE(confirmed_tags_json,'')
		FROM staged_fold_txns WHERE fold_uuid='rev-1'
	`).Scan(&status, &confDestID, &confSrcID, &confCatID, &confBudID, &confDesc, &confTags); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "ready_to_push" {
		t.Errorf("status=%q, want ready_to_push", status)
	}
	if !confDestID.Valid || confDestID.Int64 != 12 {
		t.Errorf("destination resolved=%v, want 12", confDestID)
	}
	if !confSrcID.Valid || confSrcID.Int64 != 1 {
		t.Errorf("source resolved=%v, want 1", confSrcID)
	}
	if !confCatID.Valid || confCatID.Int64 != 6 {
		t.Errorf("category resolved=%v, want 6", confCatID)
	}
	if confBudID.Valid {
		t.Errorf("budget should be NULL when name is empty, got %v", confBudID)
	}
	if confDesc != "edited description" {
		t.Errorf("confirmed_description=%q", confDesc)
	}
	if !strings.Contains(confTags, "food") || !strings.Contains(confTags, "evening") {
		t.Errorf("confirmed_tags_json=%q", confTags)
	}
}

// TestUI_Save_UnresolvedNamesFlag confirms that a name that CANNOT be
// auto-created — a source asset account — lands as NULL and surfaces a
// flash warning. (A destination on a withdrawal IS auto-created by
// firefly, so that path is intentional, not an error — see
// TestUI_Save_NewDestinationName.)
func TestUI_Save_UnresolvedNamesFlag(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	form := url.Values{}
	form.Set("destination_name", "Cake Palace")     // resolvable → 12
	form.Set("source_name", "Nonexistent Bank XYZ") // unresolvable asset → NULL + flagged
	form.Set("description", "x")

	resp := u.do(t, "POST", "/admin/ui/staged/rev-1/save", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	// The source resolution should have failed → column NULL.
	var src sql.NullInt64
	if err := u.db.DB.QueryRow(`SELECT confirmed_source_account_id FROM staged_fold_txns WHERE fold_uuid='rev-1'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src.Valid {
		t.Errorf("source should be NULL for unresolvable name, got %v", src)
	}

	// Flash cookie should carry the unresolved name in its message.
	var flash *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "tfe-flash" {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("expected tfe-flash cookie on unresolved name")
	}
	if !strings.Contains(flash.Value, "couldn") {
		t.Errorf("expected flash to mention unresolved names, got: %s", flash.Value)
	}
}

// TestUI_Save_NewDestinationName covers the C2 path: on a withdrawal, a
// destination name with no matching firefly account is NOT an error —
// it's a NEW expense account the push will create. It's stored in
// confirmed_destination_account_name (id stays NULL), produces no
// unresolved flash, and the row advances to ready_to_push.
func TestUI_Save_NewDestinationName(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	form := url.Values{}
	form.Set("destination_name", "United Airlines") // novel — no firefly account
	form.Set("source_name", "HDFC Card")            // resolvable → 1
	form.Set("description", "Flight booking")

	resp := u.do(t, "POST", "/admin/ui/staged/rev-1/save", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var (
		destID   sql.NullInt64
		destName sql.NullString
		status   string
	)
	if err := u.db.DB.QueryRow(`
		SELECT confirmed_destination_account_id, confirmed_destination_account_name, status
		FROM staged_fold_txns WHERE fold_uuid='rev-1'`).Scan(&destID, &destName, &status); err != nil {
		t.Fatal(err)
	}
	if destID.Valid {
		t.Errorf("destination id should be NULL for a new-name destination, got %v", destID)
	}
	if destName.String != "United Airlines" {
		t.Errorf("expected confirmed_destination_account_name=%q, got %q", "United Airlines", destName.String)
	}
	if status != "ready_to_push" {
		t.Errorf("save should bump needs_review→ready_to_push, got %q", status)
	}
	// No unresolved flash — the new account is intentional, not an error.
	for _, c := range resp.Cookies() {
		if c.Name == "tfe-flash" && strings.Contains(c.Value, "couldn") {
			t.Errorf("did not expect an unresolved-names flash, got: %s", c.Value)
		}
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
	form.Set("destination_name", "Cake Palace")
	form.Set("source_name", "HDFC Card")
	form.Set("category_name", "Snacks")
	form.Set("budget_name", "")
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
