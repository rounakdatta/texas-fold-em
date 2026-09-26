package ui

// paths.go — every URL the UI answers to, in one place.
//
// The app is the review UI; it has nothing else to be. So its pages live at
// the root of the host: the deck at "/", every transaction at
// "/transactions", one of them at "/transactions/{uuid}", the Add form at
// "/new", the deck's JSON at "/api/…" and the assets at "/static/…".
//
// Until 0.19.0 all of it lived under /admin/ui/. Those URLs now answer with a
// 308 to their new home (handleLegacy), so a bookmark, a tab left open on the
// old deck (its fetches follow a 308 with method and body intact) and a
// script keep working. The old static URLs are served in place instead of
// redirected: an installed phone app re-reads its manifest from the old URL.
//
// The host's other paths are not the UI's: /token, /init and /admin/… are
// bearer-key endpoints for other services, and /livez and /health are for the
// cluster. The ingress routes only the paths below to the public host, so
// those stay reachable in-cluster only — keep the two lists in step
// (homelab.setup kubernetes/apps/texas-fold-em/ingress.yaml).

import (
	"net/url"
	"strings"
)

const (
	pathDeck   = "/"
	pathList   = "/transactions"
	pathNew    = "/new"
	pathAPI    = "/api/"
	pathStatic = "/static/"
	pathLogin  = "/login"

	legacyPrefix = "/admin/ui"
)

// txnPath is one transaction's page; the actions on it hang off the same path.
func txnPath(uuid string) string { return pathList + "/" + uuid }

// listPath is the list, filtered by status ("" for every status).
func listPath(status string) string {
	if status == "" {
		return pathList
	}
	return pathList + "?status=" + url.QueryEscape(status)
}

// modernPath maps a pre-0.19.0 /admin/ui path to where it lives now, keeping
// its query. ok is false when p isn't an /admin/ui path at all.
func modernPath(p string) (string, bool) {
	u, err := url.Parse(p)
	if err != nil || (u.Path != legacyPrefix && !strings.HasPrefix(u.Path, legacyPrefix+"/")) {
		return "", false
	}
	rest := strings.TrimPrefix(u.Path, legacyPrefix) // "", "/", "/review", "/staged/x/save", "/api/deck" …
	var to string
	switch {
	case rest == "" || rest == "/":
		to = pathList // the old home was the list; the deck had its own page
	case rest == "/review":
		to = pathDeck
	case strings.HasPrefix(rest, "/staged/"):
		to = pathList + "/" + strings.TrimPrefix(rest, "/staged/")
	case rest == "/reclassify":
		to = pathList + "/reclassify"
	default: // /new, /login, /sync-accounts, /api/…, /static/… keep their names
		to = rest
	}
	if u.RawQuery != "" {
		to += "?" + u.RawQuery
	}
	return to, true
}

// safeBack returns back when it is one of the UI's own pages — the deck, the
// list, a transaction or the Add form — else fallback. A "back" arrives in a
// form or a query string, so anything else (another host, "//host", a
// machine endpoint) is refused: no open redirect. A pre-0.19.0 path from a
// page that was open across the upgrade is translated rather than refused.
func safeBack(back, fallback string) string {
	if m, ok := modernPath(back); ok {
		back = m
	}
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.HasPrefix(back, `/\`) {
		return fallback
	}
	p := back
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == pathDeck || p == pathList || strings.HasPrefix(p, pathList+"/") || p == pathNew {
		return back
	}
	return fallback
}

// isDeckPath reports whether a (safe) back link points at the deck, for the
// nav's active tab.
func isDeckPath(back string) bool {
	p := back
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return p == pathDeck
}
