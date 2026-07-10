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
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
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
	// Deterministic classifier (no LLM) so reclassify-selected works in tests.
	h.SetClassifier(classifier.New(idb.DB, slog.New(slog.NewTextHandler(io.Discard, nil)), 0, 10))

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
	// Destination now resolves to the firefly account name (proposed
	// destination 12 → "Cake Palace"), not the raw lowercase merchant.
	if !strings.Contains(string(body), "Cake Palace") {
		t.Errorf("expected resolved destination 'Cake Palace' in body, got: %s", body)
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

// TestUI_IndexAccountFilter verifies the source-account filter narrows
// the list to rows whose effective source account matches, that the
// dropdown offers every account present on staged rows, and that the
// unfiltered view still shows everything.
func TestUI_IndexAccountFilter(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)

	// A second asset account so id 2 resolves to a name in the dropdown.
	if _, err := u.db.DB.Exec(`
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name,
		    destination_account_id, destination_account_name, destination_account_name_normalized,
		    description, tags_json)
		VALUES (910, 9100, 'withdrawal', 1000, 'INR', '2026-04-02',
		        2, 'Amex Card', 13, 'Some Shop', 'some shop', 'seed amex', '[]')
	`); err != nil {
		t.Fatalf("seed firefly_txns: %v", err)
	}

	// Two ready_to_push rows with different effective source accounts:
	// one on HDFC (id 1), one on Amex (id 2).
	if _, err := u.db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status, proposed_source_account_id)
		VALUES ('acct-hdfc','{}',2500,'INR','2026-05-09T10:00:00Z','CARD','OUTGOING','x','zomato','ready_to_push',1),
		       ('acct-amex','{}',3500,'INR','2026-05-09T11:00:00Z','CARD','OUTGOING','x','district','ready_to_push',2)
	`); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	// Filter to HDFC (id 1): the zomato row only.
	resp := u.do(t, "GET", "/admin/ui/?status=ready_to_push&account=1", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	s, _ := io.ReadAll(resp.Body)
	body := string(s)
	if !strings.Contains(body, "zomato") {
		t.Errorf("expected HDFC (zomato) row in filtered list")
	}
	if strings.Contains(body, "district") {
		t.Errorf("did not expect Amex (district) row when filtering by HDFC")
	}
	if !strings.Contains(body, "ready_to_push (1)") {
		t.Errorf("expected filtered count 1, got: %q", snippet(body, "ready_to_push"))
	}
	// The dropdown lists every account present on staged rows, regardless
	// of the active filter.
	if !strings.Contains(body, "HDFC Card") || !strings.Contains(body, "Amex Card") {
		t.Errorf("expected both HDFC Card and Amex Card in the filter dropdown")
	}

	// A bogus (non-numeric) account param degrades to "all accounts".
	respBad := u.do(t, "GET", "/admin/ui/?status=ready_to_push&account=not-a-number", nil)
	defer respBad.Body.Close()
	bodyBad, _ := io.ReadAll(respBad.Body)
	if !strings.Contains(string(bodyBad), "ready_to_push (2)") {
		t.Errorf("bad account param should show all rows, got: %q", snippet(string(bodyBad), "ready_to_push"))
	}

	// Unfiltered: both rows present, count 2.
	resp2 := u.do(t, "GET", "/admin/ui/?status=ready_to_push", nil)
	defer resp2.Body.Close()
	s2, _ := io.ReadAll(resp2.Body)
	body2 := string(s2)
	if !strings.Contains(body2, "zomato") || !strings.Contains(body2, "district") {
		t.Errorf("expected both rows in the unfiltered list")
	}
	if !strings.Contains(body2, "ready_to_push (2)") {
		t.Errorf("expected unfiltered count 2, got: %q", snippet(body2, "ready_to_push"))
	}
}

// TestFilterURL pins the link-builder contract the pagination controls
// rely on: the active filter set (status + account) is preserved and the
// page is appended. If a future filter dimension is added, this is where
// to assert it carries through.
func TestFilterURL(t *testing.T) {
	// url.Values.Encode sorts keys, so the expected order is alphabetical.
	if got, want := string(filterURL(listFilters{Status: "ready_to_push", SourceAccount: "7"}, 50, 3)),
		"/admin/ui/?account=7&page=3&per_page=50&status=ready_to_push"; got != want {
		t.Errorf("filterURL with account = %q, want %q", got, want)
	}
	// No account → no account param; page <= 0 omits the page param.
	if got, want := string(filterURL(listFilters{Status: "pending"}, 50, 0)),
		"/admin/ui/?per_page=50&status=pending"; got != want {
		t.Errorf("filterURL without account = %q, want %q", got, want)
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

// TestUI_Detail_SourceNameFromMirrorNoHistory is the regression test for
// the blank-source display bug: a source resolved to a freshly-created
// firefly asset (id 1314) has NO firefly_txns history, so the id→name
// lookup must fall back to the firefly_accounts mirror. Even with the
// stored proposed_source_account_name left empty, the detail page must
// render "Ixigo AU Bank Credit Card", not blank.
func TestUI_Detail_SourceNameFromMirrorNoHistory(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	// The AU asset exists in the firefly mirror but has NO transactions.
	if _, err := u.db.DB.Exec(`
		INSERT INTO firefly_accounts (firefly_id, name, type, account_role, account_number, active, raw_payload)
		VALUES (1314,'Ixigo AU Bank Credit Card','asset','ccAsset','4069775035029179',1,'{}')`); err != nil {
		t.Fatalf("seed firefly_accounts: %v", err)
	}
	// A staged row whose source resolved to 1314 by id, with the name column
	// deliberately left empty — forcing resolution through the mirror.
	if _, err := u.db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status,
		    classifier_tier, classifier_confidence, classifier_evidence_json,
		    proposed_source_account_id, proposed_destination_account_name, proposed_category_id)
		VALUES ('au-row','{}',21200,'USD','2026-06-26T15:19:00Z',
		        'CARD','OUTGOING','CARD/x/DeepSeek/USD/2.12/OUTGOING','deepseek','needs_review',
		        3, 0.8, '{"tier":3}', 1314, 'DeepSeek', NULL)`); err != nil {
		t.Fatalf("seed staged: %v", err)
	}

	resp := u.do(t, "GET", "/admin/ui/staged/au-row", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `name="source_name" value="Ixigo AU Bank Credit Card"`) {
		t.Errorf("expected source to render 'Ixigo AU Bank Credit Card' from the mirror; got blank or wrong.\nbody excerpt: %s",
			excerptAround(string(body), "source_name"))
	}
}

// excerptAround returns a short window around the first occurrence of
// needle, for readable test failures.
func excerptAround(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "(needle not found)"
	}
	start := i - 40
	if start < 0 {
		start = 0
	}
	end := i + 80
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// TestUI_Save_ResolvesSourceFromMirrorNoHistory is the regression test for
// the push-abort bug: a source whose firefly asset was just created (no
// transaction history) must still resolve to its id via the firefly_accounts
// mirror, instead of coming back unresolved and aborting the push.
func TestUI_Save_ResolvesSourceFromMirrorNoHistory(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	// AU asset exists in the mirror only — no firefly_txns reference it.
	if _, err := u.db.DB.Exec(`
		INSERT INTO firefly_accounts (firefly_id, name, type, account_role, account_number, active, raw_payload)
		VALUES (1314,'Ixigo AU Bank Credit Card','asset','ccAsset','4069775035029179',1,'{}')`); err != nil {
		t.Fatalf("seed firefly_accounts: %v", err)
	}
	form := url.Values{}
	form.Set("destination_name", "Cake Palace") // resolves via firefly_txns (id 12)
	form.Set("source_name", "Ixigo AU Bank Credit Card")
	form.Set("category_name", "Snacks")
	form.Set("description", "test")
	resp := u.do(t, "POST", "/admin/ui/staged/rev-1/save", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var srcID sql.NullInt64
	if err := u.db.DB.QueryRow(
		`SELECT confirmed_source_account_id FROM staged_fold_txns WHERE fold_uuid='rev-1'`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if !srcID.Valid || srcID.Int64 != 1314 {
		t.Errorf("source resolved to %v, want 1314 (from the mirror, no txn history)", srcID)
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

// TestUI_IndexAllStatus: the "all" view lists rows of every status (no
// status filter), renders a per-row colour-coded status icon, and marks
// the "all" nav tab active.
func TestUI_IndexAllStatus(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	// Harness seeds rev-1 (needs_review, dest "Cake Palace"). Add one
	// ready_to_push (high confidence → full battery) and one pushed so
	// "all" spans three statuses.
	if _, err := u.db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp,
		    mode, type, narration, merchant_extracted, status, classifier_tier, classifier_confidence)
		VALUES ('rtp-1','{}',2500,'INR','2026-05-09T10:00:00Z','CARD','OUTGOING','x','zomato','ready_to_push',3,0.92),
		       ('psh-1','{}',3500,'INR','2026-05-09T11:00:00Z','CARD','OUTGOING','x','swiggy','pushed',1,1.0)
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := u.do(t, "GET", "/admin/ui/?status=all", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	s, _ := io.ReadAll(resp.Body)
	body := string(s)

	// All three statuses' rows are present (rev-1's destination resolves
	// to "Cake Palace"; rtp-1/psh-1 fall back to the raw merchant).
	for _, m := range []string{"Cake Palace", "zomato", "swiggy"} {
		if !strings.Contains(body, m) {
			t.Errorf("all view missing destination %q", m)
		}
	}
	if !strings.Contains(body, "all (3)") {
		t.Errorf("expected heading 'all (3)', got: %q", snippet(body, "all ("))
	}
	// Per-row status icons for the mixed statuses.
	for _, cls := range []string{"sicon-needs_review", "sicon-ready_to_push", "sicon-pushed"} {
		if !strings.Contains(body, cls) {
			t.Errorf("all view missing status icon %q", cls)
		}
	}
	// Cockpit columns + the battery gauge (rtp-1 conf 0.92 → high band).
	for _, want := range []string{"<th>Source</th>", "<th>Destination</th>", "<th>Confidence</th>", "batt-high"} {
		if !strings.Contains(body, want) {
			t.Errorf("all view missing %q", want)
		}
	}
	// The "all" nav tab is highlighted as active.
	if !strings.Contains(body, `?status=all" class="active"`) {
		t.Errorf("expected the 'all' nav tab to be marked active")
	}
}

// TestUI_ReclassifySelected: posting selected fold_uuids re-runs the
// classifier on them. rev-1 starts as Tier 4 (no match); with the seeded
// firefly corpus the FTS vote re-tiers it. An empty selection is a
// graceful no-op flash.
func TestUI_ReclassifySelected(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)

	form := url.Values{}
	form.Set("fold_uuids", "rev-1")
	form.Set("back", "/admin/ui/?status=needs_review")
	resp := u.do(t, "POST", "/admin/ui/reclassify", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	// rev-1 was Tier 4; reclassification should move it off 4 (FTS hits
	// the seeded "Cake Palace" rows).
	var tier sql.NullInt64
	if err := u.db.DB.QueryRow(`SELECT classifier_tier FROM staged_fold_txns WHERE fold_uuid='rev-1'`).Scan(&tier); err != nil {
		t.Fatal(err)
	}
	if !tier.Valid || tier.Int64 == 4 {
		t.Errorf("rev-1 should have been re-tiered off Tier 4, got %v", tier)
	}
	var gotFlash bool
	for _, c := range resp.Cookies() {
		if c.Name == "tfe-flash" && strings.Contains(c.Value, "reclassified") {
			gotFlash = true
		}
	}
	if !gotFlash {
		t.Errorf("expected a 'reclassified' flash")
	}

	// Empty selection → graceful flash, no error.
	resp2 := u.do(t, "POST", "/admin/ui/reclassify", url.Values{"back": {"/admin/ui/?status=all"}})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("empty selection: expected 303, got %d", resp2.StatusCode)
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

// TestUI_Index_ShowsDescription: the list renders the transaction title
// (proposed_description) so rows are scannable by their human title.
func TestUI_Index_ShowsDescription(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	resp := u.do(t, "GET", "/admin/ui/?status=needs_review", nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Snack at cake palace") {
		t.Errorf("expected the row's description 'Snack at cake palace' in the list")
	}
}
