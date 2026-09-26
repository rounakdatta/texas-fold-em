package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every pre-0.19.0 address has a home now, query and all — including the
// POST endpoints a deck left open across the upgrade still calls.
func TestPaths_EveryOldAddressLeadsToItsNewHome(t *testing.T) {
	for old, want := range map[string]string{
		"/admin/ui":                                  "/transactions",
		"/admin/ui/":                                 "/transactions",
		"/admin/ui/?status=pushed&account=12":        "/transactions?status=pushed&account=12",
		"/admin/ui/review":                           "/",
		"/admin/ui/review?account=1314":              "/?account=1314",
		"/admin/ui/staged/abc":                       "/transactions/abc",
		"/admin/ui/staged/abc?back=%2Fadmin%2Fui%2F": "/transactions/abc?back=%2Fadmin%2Fui%2F",
		"/admin/ui/staged/abc/save":                  "/transactions/abc/save",
		"/admin/ui/reclassify":                       "/transactions/reclassify",
		"/admin/ui/new":                              "/new",
		"/admin/ui/sync-accounts":                    "/sync-accounts",
		"/admin/ui/api/rows/abc/edit":                "/api/rows/abc/edit",
		"/admin/ui/api/deck?pile=later&order=oldest": "/api/deck?pile=later&order=oldest",
		"/admin/ui/static/app.css?v=1":               "/static/app.css?v=1",
	} {
		if got, ok := modernPath(old); !ok || got != want {
			t.Errorf("modernPath(%q) = %q, %v; want %q", old, got, ok, want)
		}
	}
	for _, notOld := range []string{"/", "/transactions", "/admin/uix", "/admin/push/abc", "/token"} {
		if got, ok := modernPath(notOld); ok {
			t.Errorf("modernPath(%q) = %q; it isn't an old UI address", notOld, got)
		}
	}
}

// A "back" comes from a form or a query string, so it may only ever lead to
// one of the UI's own pages: not another host, and not the machine endpoints
// that share this host (they are kept off the public ingress on purpose).
func TestPaths_BackLinksStayOnTheUIsOwnPages(t *testing.T) {
	const fallback = "/transactions"
	for back, want := range map[string]string{
		"/":                          "/",
		"/?account=1314":             "/?account=1314",
		"/transactions?status=all":   "/transactions?status=all",
		"/transactions/abc":          "/transactions/abc",
		"/new":                       "/new",
		"/admin/ui/?status=pushed":   "/transactions?status=pushed", // a page open across the upgrade
		"/admin/ui/review":           "/",
		"":                           fallback,
		"https://elsewhere.example/": fallback,
		"//elsewhere.example/":       fallback,
		`/\elsewhere.example`:        fallback,
		"javascript:alert(1)":        fallback,
		"/token":                     fallback,
		"/init":                      fallback,
		"/admin/push/abc":            fallback,
		"/health":                    fallback,
		"/transactionsX":             fallback,
	} {
		if got := safeBack(back, fallback); got != want {
			t.Errorf("safeBack(%q) = %q, want %q", back, got, want)
		}
	}
}

// The deck is the home screen, and "/" must not swallow the rest of the host:
// an unknown path is a 404, not the deck.
func TestUI_TheDeckIsTheHomeScreenAndNotACatchAll(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	resp := u.do(t, "GET", "/", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "/static/review.js") {
		t.Fatalf("GET / = %d, want the review deck (%.200s)", resp.StatusCode, body)
	}
	for _, p := range []string{"/nope", "/transactionz", "/admin"} {
		resp := u.do(t, "GET", p, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 — the deck must not answer for other paths", p, resp.StatusCode)
		}
	}
}

// An old bookmark, an installed app, or a deck tab left open across the
// upgrade: each is sent to the new address with a 308, which keeps a POST a
// POST (a 301/302/303 would turn the tab's "send" into a GET that does
// nothing). Assets stay where the installed app looks for them.
func TestUI_OldAddressesAreRedirectedWithTheirMethodKept(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	for _, c := range []struct{ method, path, to string }{
		{"GET", "/admin/ui/review", "/"},
		{"GET", "/admin/ui/?status=pushed", "/transactions?status=pushed"},
		{"GET", "/admin/ui/staged/rev-1", "/transactions/rev-1"},
		{"POST", "/admin/ui/staged/rev-1/save", "/transactions/rev-1/save"},
		{"POST", "/admin/ui/api/rows/rev-1/edit", "/api/rows/rev-1/edit"},
	} {
		resp := u.do(t, c.method, c.path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != c.to {
			t.Errorf("%s %s = %d → %q, want 308 → %q", c.method, c.path, resp.StatusCode, resp.Header.Get("Location"), c.to)
		}
	}
	resp := u.do(t, "GET", "/admin/ui/static/manifest.webmanifest", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the old manifest URL = %d, want it served in place (200)", resp.StatusCode)
	}
}

// Not one link on any page still points at the old addresses — each would
// cost a redirect, and a stray one is how the old prefix creeps back.
func TestUI_NoPageLinksToTheOldAddresses(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	for _, p := range []string{"/", "/transactions?status=all", "/transactions/rev-1", "/new"} {
		resp := u.do(t, "GET", p, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", p, resp.StatusCode)
		}
		if i := strings.Index(string(body), "/admin/ui"); i >= 0 {
			t.Errorf("GET %s still links to the old prefix: …%s…", p, body[max(0, i-60):min(len(body), i+60)])
		}
	}
}

// The phone app was installed from the old start URL. The manifest keeps
// that as the app's id, so the move updates the installed app instead of
// leaving it pointing at a redirect (and a second app beside it).
func TestUI_TheInstalledAppKeepsItsIdentity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("static", "manifest.webmanifest"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID       string `json:"id"`
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "/admin/ui/review" || m.StartURL != "/" || m.Scope != "/" {
		t.Errorf("manifest id=%q start_url=%q scope=%q; want the old start URL as the id, and / for both", m.ID, m.StartURL, m.Scope)
	}
	if len(m.Icons) == 0 {
		t.Fatal("manifest has no icons")
	}
	for _, ic := range m.Icons {
		if !strings.HasPrefix(ic.Src, "/static/") {
			t.Errorf("manifest icon %q isn't under /static/", ic.Src)
		}
	}
}

// A flash message is set on a POST and read on the page it redirects to, so
// its cookie must cover every page, not only the old prefix.
func TestUI_AFlashMessageReachesThePageItRedirectsTo(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	resp := u.do(t, "POST", "/transactions/rev-1/save", nil)
	resp.Body.Close()
	var flash *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "tfe-flash" {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("the save set no flash cookie")
	}
	if flash.Path != "/" {
		t.Errorf("tfe-flash cookie path = %q, want / — it must reach %s", flash.Path, resp.Header.Get("Location"))
	}
}
