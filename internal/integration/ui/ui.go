// Package ui is the server-rendered admin UI for the integration.
// HTML pages at /admin/ui/. The UI lets a human:
//
//   - list staged fold transactions filtered by status
//   - inspect a row's classifier decision and evidence
//   - edit the proposed firefly fields (destination, source, category,
//     budget, description, tags)
//   - push to firefly (with the same idempotency safety as the JSON
//     endpoint — the Pusher is the same code path)
//   - skip a transaction (excluded from firefly forever)
//
// The UI is intentionally minimal — single-page detail view, dark-mode
// CSS in the template. No client-side framework. No JS beyond what
// browsers do natively for form submission.
//
// Auth model: in production these routes are deployed behind tinyauth
// (Google OAuth forward-auth) at the cluster ingress, so unauthenticated
// requests never reach this code path. For local development /
// testing, an admin-key cookie is accepted (set via /admin/ui/login).
// Both layers are independently optional via SetAuthMode.
package ui

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/classifier"
)

//go:embed templates/*.html
var tmplFS embed.FS

// AuthMode describes how /admin/ui/* routes authenticate human users.
type AuthMode int

const (
	// AuthModeBypass: no auth at this layer. Use ONLY when the routes
	// are deployed behind a trusted reverse-proxy auth layer (e.g.
	// tinyauth ForwardAuth on the cluster ingress).
	AuthModeBypass AuthMode = iota
	// AuthModeCookie: routes require a cookie equal to the admin key.
	// /admin/ui/login?key=<admin_key> sets the cookie. For local
	// development.
	AuthModeCookie
)

// Handler holds the dependencies the UI needs.
//
// Each page has its own *template.Template instance because we use a
// shared base layout with a {{block "content" . }} that each page
// supplies via its own {{define "content"}}. If we parsed all pages
// into one template, the last-parsed `content` definition would win
// for every render.
type Handler struct {
	db         *sql.DB
	pusher     *integration.Pusher
	log        *slog.Logger
	adminKey   string
	auth       AuthMode
	indexTmpl  *template.Template
	detailTmpl *template.Template
	newTmpl    *template.Template // "add a transaction fold never saw"
	// fireflyPublicURL is the user-facing firefly base (e.g.
	// https://firefly.taptappers.club) for deep-linking pushed rows to
	// their firefly transaction. Empty disables the links. Distinct from
	// the integration's internal FIREFLY_BASE (the in-cluster API URL).
	fireflyPublicURL string
	// cls enables the reclassify-selected action. Optional; when nil the
	// review list's reclassify button reports it's unavailable.
	cls *classifier.Classifier
	// fireflyAccounts enables the "sync accounts from firefly" button, which
	// refreshes the firefly_accounts mirror on demand so an account the user
	// just created in firefly is immediately selectable. Optional.
	fireflyAccounts *integration.FireflyAccountsSyncer
}

// SetFireflyPublicURL sets the user-facing firefly base used to link
// pushed rows to their firefly transaction (trailing slash trimmed).
func (h *Handler) SetFireflyPublicURL(u string) { h.fireflyPublicURL = strings.TrimRight(u, "/") }

// SetClassifier wires the classifier so the review UI can reclassify
// selected rows on demand. Optional.
func (h *Handler) SetClassifier(c *classifier.Classifier) { h.cls = c }

// SetFireflyAccountsSyncer wires the firefly-accounts mirror syncer so the
// review UI can refresh it on demand (the "sync accounts" button). Optional.
func (h *Handler) SetFireflyAccountsSyncer(s *integration.FireflyAccountsSyncer) { h.fireflyAccounts = s }

// New constructs a UI Handler.
func New(db *sql.DB, pusher *integration.Pusher, log *slog.Logger, adminKey string, auth AuthMode) (*Handler, error) {
	// filterURL is shared by the index pagination + filter controls so
	// every link preserves the active filter set without hand-built
	// querystrings. Registered on both templates for uniformity even
	// though only the index references it today.
	funcs := template.FuncMap{"filterURL": filterURL, "statusBadge": statusBadge, "confidenceBar": confidenceBar}
	indexTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(tmplFS, "templates/layout.html", "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse index template: %w", err)
	}
	detailTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(tmplFS, "templates/layout.html", "templates/detail.html")
	if err != nil {
		return nil, fmt.Errorf("parse detail template: %w", err)
	}
	newTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(tmplFS, "templates/layout.html", "templates/new.html")
	if err != nil {
		return nil, fmt.Errorf("parse new-transaction template: %w", err)
	}
	return &Handler{
		db:         db,
		pusher:     pusher,
		log:        log.With("component", "ui"),
		adminKey:   adminKey,
		auth:       auth,
		indexTmpl:  indexTmpl,
		detailTmpl: detailTmpl,
		newTmpl:    newTmpl,
	}, nil
}

// Mount registers all UI routes on the supplied mux. Routes:
//
//	GET  /admin/ui/                          → index (filtered by ?status=)
//	GET  /admin/ui/staged/{fold_uuid}        → detail
//	POST /admin/ui/staged/{fold_uuid}/save   → save edits
//	POST /admin/ui/staged/{fold_uuid}/push   → save edits + push
//	POST /admin/ui/staged/{fold_uuid}/skip   → mark skipped
//	GET  /admin/ui/new                       → form: add a transaction fold never saw
//	POST /admin/ui/new                       → create it as a MANUAL staged row
//	GET  /admin/ui/login?key=<admin_key>     → set auth cookie (cookie mode only)
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("GET /admin/ui/", h.withAuth(h.handleIndex))
	mux.Handle("GET /admin/ui/new", h.withAuth(h.handleNewForm))
	mux.Handle("POST /admin/ui/new", h.withAuth(h.handleNewCreate))
	mux.Handle("GET /admin/ui/staged/{fold_uuid}", h.withAuth(h.handleDetail))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/save", h.withAuth(h.handleSave))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/push", h.withAuth(h.handlePush))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/update", h.withAuth(h.handleUpdate))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/skip", h.withAuth(h.handleSkip))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/link", h.withAuth(h.handleLink))
	mux.Handle("POST /admin/ui/reclassify", h.withAuth(h.handleReclassify))
	mux.Handle("POST /admin/ui/sync-accounts", h.withAuth(h.handleSyncAccounts))
	if h.auth == AuthModeCookie {
		mux.HandleFunc("GET /admin/ui/login", h.handleLogin)
	}
}

// withAuth wraps a handler with the configured auth check.
func (h *Handler) withAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch h.auth {
		case AuthModeBypass:
			// Trusted upstream proxy responsible for auth.
		case AuthModeCookie:
			c, err := r.Cookie("tfe-admin")
			if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(h.adminKey)) != 1 {
				http.Error(w, "unauthorised — visit /admin/ui/login?key=<admin-key> first", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleLogin sets the auth cookie. Single-step: set + redirect.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if subtle.ConstantTimeCompare([]byte(key), []byte(h.adminKey)) != 1 {
		http.Error(w, "wrong key", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "tfe-admin",
		Value:    key,
		Path:     "/admin/ui",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin/ui/", http.StatusSeeOther)
}

// indexRow is the shape rendered by index.html. Slim view of
// staged_fold_txns plus a few derived fields.
//
// TxnTimestampUTC is the canonical RFC3339 string that goes into a
// <time datetime="..."> attribute. The browser-side script in
// layout.html replaces the textContent with the device's local-time
// rendering. TxnTimestamp keeps a server-side fallback for clients
// without JS (still UTC, but readable).
type indexRow struct {
	FoldUUID             string
	TxnTimestamp         string
	TxnTimestampUTC      string
	AmountDisplay        string
	Currency             string
	ForeignDisplay       string // e.g. "AED 25.00"; empty for domestic
	Mode                 string
	Type                 string
	MerchantExtracted    string
	Status               string
	TierLabel            string
	Confidence           float64
	// CategoryName is the category push will send: firefly's live one for a
	// pushed row, else the human's choice (blank for an explicit "none"),
	// else the classifier's suggestion.
	CategoryName string
	// Incoming: money coming INTO the account being viewed — fold's
	// INCOMING rows, plus, under an account filter, rows whose destination
	// is that account (e.g. a bill payment into a card). Drives the +/-.
	Incoming bool
	// SourceAccountName is the effective paying account (confirmed
	// overrides proposed), resolved to a firefly name. Empty when the
	// row has no source account yet (common for pending/needs_review).
	SourceAccountName string
	// DestinationName is the effective destination: the confirmed/proposed
	// destination account name, else a C2 proposed-new-account name, else
	// the raw extracted merchant as a fallback.
	DestinationName string
	// Description is the transaction title (confirmed edit else the LLM's
	// proposed title). Shown in the list so rows are scannable by their
	// human title, not just merchant/amount. Empty when neither is set.
	Description string
	// FireflyURL deep-links a pushed row to the firefly transaction it
	// created. Empty unless the row is pushed, the public firefly URL is
	// configured, and the group id is resolvable from the mirror.
	FireflyURL string
	// Edited: the human corrected the amount, foreign amount or date (the
	// list shows the corrected values; FoldAmountDisplay is what fold saw).
	Edited            bool
	FoldAmountDisplay string
	// Manual: added by hand from a statement line fold never received.
	Manual bool
	// PossibleDuplicate: fold.money itself flags this as a likely duplicate
	// alert (e.g. the same charge alerted twice) — usually one to skip.
	PossibleDuplicate bool
	// IsRefund: gives money back for a purchase (RefundLinked: firefly
	// records it as a Refund link). Refunded: some refund points at THIS
	// purchase.
	IsRefund     bool
	RefundLinked bool
	Refunded     bool
}

// listFilters is the set of WHERE constraints the index list honours.
// It is the seed of a small filter framework: each dimension contributes
// one optional predicate to where(), is preserved across pagination by
// filterURL, and surfaces as one control in index.html. Adding a new
// filter (mode, tier, txn type, destination/merchant) is three local
// edits — a field here, a clause in where(), a control in the template —
// and nothing else has to change.
type listFilters struct {
	Status        string // always set; the status tab
	SourceAccount string // firefly account id (as string); "" = all accounts
}

// where renders the filter set into a SQL predicate (against the
// staged_fold_txns alias `s`) plus its positional args. Always at least
// the status clause; optional dimensions append when set.
func (f listFilters) where() (string, []any) {
	var clauses []string
	var args []any
	// "all" (and empty) means no status constraint — the overview view
	// that lists every transaction regardless of status.
	if f.Status != "" && f.Status != "all" {
		clauses = append(clauses, "s.status = ?")
		args = append(args, f.Status)
	}
	if f.SourceAccount != "" {
		// Every row that moves money on the account: where it PAID (the
		// effective source) and where money CAME IN (the effective
		// destination) — a credit card's bill payments, refunds, reversals
		// and waivers all land on it as a destination, and reconciling the
		// card needs to see them next to its spends. "Effective" resolves as
		// a unit, the same way the Pusher does (see effectiveAccountIDSQL).
		// A non-numeric value is dropped upstream in handleIndex, so
		// ParseInt should succeed; guard anyway so a bad param degrades to
		// "all" instead of erroring.
		if id, err := strconv.ParseInt(f.SourceAccount, 10, 64); err == nil {
			clauses = append(clauses, "("+effectiveAccountIDSQL("source")+" = ? OR "+effectiveAccountIDSQL("destination")+" = ?)")
			args = append(args, id, id)
		}
	}
	if len(clauses) == 0 {
		return "1=1", nil // no constraints (e.g. status=all, no account) → every row
	}
	return strings.Join(clauses, " AND "), args
}

// filterURL builds an index URL that preserves the active filters and
// targets a specific page. Registered as a template func so pagination
// links and filter controls never hand-assemble query strings: add a
// dimension to listFilters.where and to this builder, and every link
// carries it. page <= 0 omits the page param (lands on page 1).
func filterURL(f listFilters, perPage, page int) template.URL {
	v := url.Values{}
	v.Set("status", f.Status)
	if f.SourceAccount != "" {
		v.Set("account", f.SourceAccount)
	}
	v.Set("per_page", strconv.Itoa(perPage))
	if page > 0 {
		v.Set("page", strconv.Itoa(page))
	}
	return template.URL("/admin/ui/?" + v.Encode())
}

// statusBadge renders a compact, colour-coded status glyph for the index
// table. It matters most in the "all" view, where rows of every status
// mix and a per-row indicator is the only at-a-glance signal. The
// staged_fold_txns.status CHECK constraint keeps the input to a known
// enum, so the inline class/title are safe; the default arm escapes
// anything unexpected.
func statusBadge(status string) template.HTML {
	var glyph, label string
	switch status {
	case "pending":
		glyph, label = "○", "pending"
	case "needs_review":
		glyph, label = "⚠", "needs review"
	case "ready_to_push":
		glyph, label = "➤", "ready to push"
	case "pushed":
		glyph, label = "✓", "pushed"
	case "skipped":
		glyph, label = "⊘", "skipped"
	default:
		return template.HTML(fmt.Sprintf(`<span class="sicon" title="%s">•</span>`,
			template.HTMLEscapeString(status)))
	}
	return template.HTML(fmt.Sprintf(`<span class="sicon sicon-%s" title="%s">%s</span>`, status, label, glyph))
}

// confidenceBar renders the classifier confidence (0..1) as a small
// battery-style gauge — a fill proportional to the score, coloured by
// band (high/mid/low), with the numeric value alongside. Zero/absent
// confidence (e.g. a Tier-4 row with no score) shows a dash.
func confidenceBar(conf float64) template.HTML {
	if conf <= 0 {
		return template.HTML(`<span class="conf-na" title="no score">—</span>`)
	}
	pct := int(conf*100 + 0.5)
	if pct > 100 {
		pct = 100
	}
	level := "low"
	switch {
	case conf >= 0.85:
		level = "high"
	case conf >= 0.6:
		level = "mid"
	}
	return template.HTML(fmt.Sprintf(
		`<span class="batt batt-%s" title="confidence %.2f"><span class="batt-fill" style="width:%d%%"></span></span><span class="conf-num">%.2f</span>`,
		level, conf, pct, conf))
}

// defaultPerPage / maxPerPage bound the list query. 50 keeps the
// review UI scroll-friendly; the operator can override per-request
// via ?per_page=N up to maxPerPage when they want a wider view.
const (
	defaultPerPage = 50
	maxPerPage     = 500
)

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "needs_review"
	}
	// Ignore a non-numeric account param rather than erroring — a stale
	// or hand-edited link just falls back to "all accounts".
	account := strings.TrimSpace(r.URL.Query().Get("account"))
	if account != "" {
		if _, err := strconv.ParseInt(account, 10, 64); err != nil {
			account = ""
		}
	}
	filters := listFilters{Status: status, SourceAccount: account}

	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	perPage := parsePositiveInt(r.URL.Query().Get("per_page"), defaultPerPage)
	if perPage > maxPerPage {
		perPage = maxPerPage
	}

	total, err := h.countRows(r.Context(), filters)
	if err != nil {
		h.log.Error("count rows", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Clamp page to the available range so a stale ?page=99 link
	// after rows have been pushed/skipped lands somewhere sane.
	numPages := (total + perPage - 1) / perPage
	if numPages == 0 {
		numPages = 1
	}
	if page > numPages {
		page = numPages
	}

	rows, err := h.listRows(r.Context(), filters, perPage, (page-1)*perPage)
	if err != nil {
		h.log.Error("list rows", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Account filter dropdown — only accounts that actually appear on
	// staged rows, so there are no dead options. Resolve the selected
	// account's name for the heading too.
	accountOptions, _ := h.listFilterAccounts(r.Context())
	selectedAccountName := ""
	for _, o := range accountOptions {
		if strconv.FormatInt(o.ID, 10) == account {
			selectedAccountName = o.Name
			break
		}
	}

	h.render(w, h.indexTmpl, map[string]any{
		"Title":               "review",
		"Status":              status,
		"Filters":             filters,
		"AccountOptions":      accountOptions,
		"SelectedAccount":     account,
		"SelectedAccountName": selectedAccountName,
		"Rows":                rows,
		"Page":                page,
		"PerPage":             perPage,
		"NumPages":            numPages,
		"TotalCount":          total,
		"HasPrev":             page > 1,
		"HasNext":             page < numPages,
		"PrevPage":            page - 1,
		"NextPage":            page + 1,
		"Flash":               flashFromCookie(r, w),
	})
}

// countRows returns the total number of staged_fold_txns rows matching
// the filter set. Drives pagination — must apply the exact same WHERE as
// listRows or the page count drifts from the rows actually shown.
func (h *Handler) countRows(ctx context.Context, f listFilters) (int, error) {
	where, args := f.where()
	var n int
	err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM staged_fold_txns s WHERE `+where, args...,
	).Scan(&n)
	return n, err
}

// parsePositiveInt parses a positive integer from a string, returning
// the default on empty / parse error / non-positive.
func parsePositiveInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return def
	}
	return n
}

func (h *Handler) listRows(ctx context.Context, f listFilters, limit, offset int) ([]indexRow, error) {
	where, args := f.where()
	// Money and time shown are the EFFECTIVE values — a statement-reconciled
	// correction (confirmed_*) wins over what the card alert said — because
	// that is what push will send. s.amount_paise (fold's own) rides along
	// for the "edited" hint.
	q := `
		SELECT s.fold_uuid, COALESCE(s.confirmed_txn_timestamp, s.txn_timestamp),
		       COALESCE(s.confirmed_amount_paise, s.amount_paise), s.currency,
		       COALESCE(s.confirmed_foreign_amount_paise, s.foreign_amount_paise), s.foreign_currency, s.mode, s.type,
		       COALESCE(s.merchant_extracted,''), s.status,
		       s.classifier_tier, s.classifier_confidence,
		       -- Category as push will send it: a pushed row's live firefly
		       -- category (via the mirror); else the human's choice as a
		       -- unit — an explicit none (0) resolves to nothing and must
		       -- not fall back to the suggestion; else the suggestion.
		       COALESCE(
		         CASE WHEN s.status='pushed'
		              THEN (SELECT f.category_name FROM firefly_txns f WHERE f.firefly_id = s.firefly_txn_id LIMIT 1) END,
		         CASE WHEN s.confirmed_category_id IS NOT NULL
		              THEN (SELECT category_name FROM firefly_txns WHERE category_id = s.confirmed_category_id LIMIT 1)
		              ELSE (SELECT category_name FROM firefly_txns WHERE category_id = s.proposed_category_id LIMIT 1) END
		       ),
		       -- Source name, resolved to match what's actually in firefly:
		       --  1. pushed rows → the pushed journal's live source (via the
		       --     firefly_txns mirror by journal id) so the list mirrors
		       --     firefly (incl. corrections/manual edits);
		       --  2. else the human's confirmed choice as a UNIT (id → name,
		       --     else a name-only account) — never falling back to a stale
		       --     proposed id when a name was confirmed;
		       --  3. else the classifier's proposal as a unit.
		       COALESCE(
		         CASE WHEN s.status='pushed'
		              THEN (SELECT f.source_account_name FROM firefly_txns f WHERE f.firefly_id = s.firefly_txn_id LIMIT 1) END,
		         CASE WHEN s.confirmed_source_account_id IS NOT NULL
		              THEN (SELECT f.source_account_name FROM firefly_txns f WHERE f.source_account_id = s.confirmed_source_account_id LIMIT 1)
		              ELSE NULLIF(s.confirmed_source_account_name, '') END,
		         CASE WHEN s.proposed_source_account_id IS NOT NULL
		              THEN (SELECT f.source_account_name FROM firefly_txns f WHERE f.source_account_id = s.proposed_source_account_id LIMIT 1)
		              ELSE NULLIF(s.proposed_source_account_name, '') END
		       ),
		       COALESCE(
		         CASE WHEN s.status='pushed'
		              THEN (SELECT f.destination_account_name FROM firefly_txns f WHERE f.firefly_id = s.firefly_txn_id LIMIT 1) END,
		         CASE WHEN s.confirmed_destination_account_id IS NOT NULL
		              THEN (SELECT f.destination_account_name FROM firefly_txns f WHERE f.destination_account_id = s.confirmed_destination_account_id LIMIT 1)
		              ELSE NULLIF(s.confirmed_destination_account_name, '') END,
		         CASE WHEN s.proposed_destination_account_id IS NOT NULL
		              THEN (SELECT f.destination_account_name FROM firefly_txns f WHERE f.destination_account_id = s.proposed_destination_account_id LIMIT 1)
		              ELSE NULLIF(s.proposed_destination_account_name, '') END,
		         NULLIF(s.merchant_extracted, '')
		       ),
		       COALESCE(NULLIF(s.confirmed_description, ''), NULLIF(s.proposed_description, ''), ''),
		       COALESCE(s.firefly_group_id,
		                (SELECT group_id FROM firefly_txns WHERE firefly_id = s.firefly_txn_id LIMIT 1)),
		       s.amount_paise,
		       (s.confirmed_amount_paise IS NOT NULL OR s.confirmed_foreign_amount_paise IS NOT NULL
		        OR s.confirmed_txn_timestamp IS NOT NULL),
		       ` + possibleDuplicateSQL("s.raw_payload") + `,
		       ` + effectiveAccountIDSQL("source") + `, ` + effectiveAccountIDSQL("destination") + `,
		       s.classifier_tier = 5 OR COALESCE(NULLIF(s.confirmed_refund_of,''), s.proposed_refund_of, '') LIKE 'fold:%'
		         OR COALESCE(NULLIF(s.confirmed_refund_of,''), s.proposed_refund_of, '') LIKE 'journal:%',
		       COALESCE(s.firefly_link_id, 0) <> 0,
		       EXISTS (SELECT 1 FROM staged_fold_txns r
		               WHERE r.fold_uuid <> s.fold_uuid AND r.status <> 'skipped'
		                 AND COALESCE(NULLIF(r.confirmed_refund_of,''), r.proposed_refund_of)
		                     IN ('fold:' || s.fold_uuid, 'journal:' || COALESCE(s.firefly_txn_id, -1)))
		FROM staged_fold_txns s
		WHERE ` + where + `
		ORDER BY s.txn_timestamp DESC
		LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []indexRow
	for rows.Next() {
		var (
			r           indexRow
			amountPaise int64
			fAmt        sql.NullInt64
			fCur        sql.NullString
			tier        sql.NullInt64
			conf        sql.NullFloat64
			catName     sql.NullString
			srcName     sql.NullString
			destName    sql.NullString
			groupID     sql.NullInt64
			tsStr       string
			foldPaise   int64
			edited, dup int
			effSrc      sql.NullInt64
			effDst      sql.NullInt64
			isRefund    sql.NullBool
		)
		if err := rows.Scan(&r.FoldUUID, &tsStr, &amountPaise, &r.Currency, &fAmt, &fCur, &r.Mode, &r.Type,
			&r.MerchantExtracted, &r.Status, &tier, &conf, &catName, &srcName, &destName, &r.Description, &groupID,
			&foldPaise, &edited, &dup, &effSrc, &effDst, &isRefund, &r.RefundLinked, &r.Refunded); err != nil {
			return nil, err
		}
		// Direction as seen from the account being viewed: under an account
		// filter, money INTO that account (it's the destination, not the
		// source) is incoming even when fold saw it as an OUTGOING debit on
		// the paying side — a card's bill payment reads as a credit there.
		r.Incoming = r.Type != "OUTGOING"
		if id, err := strconv.ParseInt(f.SourceAccount, 10, 64); err == nil {
			switch {
			case effSrc.Valid && effSrc.Int64 == id:
				r.Incoming = false
			case effDst.Valid && effDst.Int64 == id:
				r.Incoming = true
			}
		}
		r.Edited, r.Manual, r.PossibleDuplicate = edited == 1, r.Mode == manualMode, dup == 1
		r.IsRefund = isRefund.Valid && isRefund.Bool
		r.FoldAmountDisplay = paiseToDecimal(foldPaise)
		r.ForeignDisplay = foreignDisplay(fAmt, fCur)
		// Two views of the timestamp: a server-rendered fallback for
		// no-JS clients, and a canonical RFC3339 UTC string for the
		// client-side <time datetime="..."> conversion to local zone.
		if t, ok := parseDBTime(tsStr); ok {
			r.TxnTimestamp = t.Format("Jan 02 15:04 UTC")
			r.TxnTimestampUTC = t.UTC().Format(time.RFC3339)
		} else {
			r.TxnTimestamp = tsStr
		}
		r.AmountDisplay = paiseToDecimal(amountPaise)
		if tier.Valid {
			r.TierLabel = tierLabel(int(tier.Int64))
		}
		if conf.Valid {
			r.Confidence = conf.Float64
		}
		if catName.Valid {
			r.CategoryName = catName.String
		}
		if srcName.Valid {
			r.SourceAccountName = srcName.String
		}
		if destName.Valid {
			r.DestinationName = destName.String
		}
		// Deep-link pushed rows to their firefly transaction (group id
		// resolved from the mirror). firefly's web route is
		// /transactions/show/{groupId}.
		if r.Status == "pushed" && h.fireflyPublicURL != "" && groupID.Valid {
			r.FireflyURL = h.fireflyPublicURL + "/transactions/show/" + strconv.FormatInt(groupID.Int64, 10)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// detailRow is the slim view rendered by detail.html (the "fold
// transaction" + "classifier decision" cards).
type detailRow struct {
	FoldUUID          string
	Narration         string
	TxnTimestamp      string
	TxnTimestampUTC   string
	AmountDisplay     string
	Currency          string
	ForeignDisplay    string // e.g. "AED 25.00"; empty for domestic
	Mode              string
	Type              string
	MerchantExtracted string
	Status            string
	TierLabel         string
	Confidence        float64
	Edited            bool // amount/foreign/date corrected by the human
	Manual            bool // added by hand from a statement line
	PossibleDuplicate bool // fold.money flags it as a likely duplicate alert
	// RawPayload is fold.money's own JSON for the transaction, pretty-printed
	// (its refund_group_id, notes, merchant …), for the operator to inspect.
	RawPayload string
	IsRefund   bool // the row gives money back for a purchase (see the refund card)
}

// editForm is the editable subset of the row, in form-field shape.
type editForm struct {
	DestinationAccountID   string
	DestinationAccountName string
	SourceAccountID        string
	SourceAccountName      string
	CategoryID             string
	CategoryName           string
	BudgetID               string
	BudgetName             string
	Description            string
	Tags                   string
	// Money and time as push will send them: the human's correction when
	// there is one, else fold's value. Amount is INR; ForeignAmount is in
	// ForeignCurrency (empty for a domestic row). Date/Time are IST.
	Amount          string
	ForeignAmount   string
	ForeignCurrency string
	Date            string
	Time            string
}

func (h *Handler) handleDetail(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	row, edit, evidence, err := h.fetchDetail(r.Context(), uuid)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("fetch detail", "fold_uuid", uuid, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Datalists for the form's name-with-autocomplete inputs. Cheap
	// queries — runs on every detail page load. If this becomes a hot
	// path we'll cache, but for personal-scale traffic it's fine.
	destAccounts, _ := h.listAccountsByKind(r.Context(), "destination")
	srcAccounts, _ := h.listAccountsByKind(r.Context(), "source")
	categories, _ := h.listCategories(r.Context())
	budgets, _ := h.listBudgets(r.Context())
	allTags, _ := h.listAllTags(r.Context())

	// Per-merchant tag suggestions (clickable chips). Resolves the
	// destination_account_id from the edit form's preferred ID
	// (confirmed > proposed) so we suggest tags that actually fit
	// THIS merchant rather than from the whole library.
	var merchantID int64
	if id, ok := parseInt(edit.DestinationAccountID); ok {
		merchantID = id
	}
	suggested, _ := h.suggestedTagsForMerchant(r.Context(), merchantID)

	// Where to return after save&push / skip: the exact list (status +
	// filters + page) the user came from, passed as ?back=. Falls back to
	// this row's status list when opened directly. Validated to our own UI
	// paths so it can't be an open redirect.
	back := r.URL.Query().Get("back")
	if !strings.HasPrefix(back, "/admin/ui/") {
		back = "/admin/ui/?status=" + row.Status
	}

	// Correcting an already-pushed row: pull firefly's CURRENT transaction
	// and pre-fill the form from it, so the correction builds on firefly's
	// truth (including edits made directly in firefly). Saving then UPDATES
	// the transaction in place rather than creating a duplicate.
	isPushed := row.Status == "pushed"
	fireflyPulled := false
	if isPushed && h.pusher != nil {
		if v, ok := h.pusher.CurrentFireflyView(r.Context(), uuid); ok {
			edit.DestinationAccountName = v.DestinationName
			edit.SourceAccountName = v.SourceName
			edit.CategoryName = v.CategoryName
			edit.BudgetName = v.BudgetName
			if v.Description != "" {
				edit.Description = v.Description
			}
			if len(v.Tags) > 0 {
				edit.Tags = strings.Join(v.Tags, ", ")
			}
			// Money/time from firefly's live values too, so a correction
			// starts from what firefly really holds (an amount already fixed
			// there by hand is carried, not clobbered, by "save & update").
			if p, err := parseMoneyToPaise(v.Amount); err == nil && p > 0 {
				edit.Amount = paiseToDecimal(p)
			}
			if p, err := parseMoneyToPaise(v.ForeignAmount); err == nil && p > 0 && edit.ForeignCurrency != "" {
				edit.ForeignAmount = paiseToDecimal(p)
			}
			if t, ok := parseDBTime(v.Date); ok {
				edit.Date, edit.Time = istDateTime(t)
			}
			fireflyPulled = true
		}
	}

	refund, refundedBy := h.refundCardFor(r.Context(), uuid)
	row.IsRefund = refund.Show

	h.render(w, h.detailTmpl, map[string]any{
		"Title":           uuid,
		"Refund":          refund,
		"RefundedBy":      refundedBy,
		"Status":          row.Status, // for the shared nav's active-state highlight
		"Row":             row,
		"Back":            back,
		"IsPushed":        isPushed,
		"FireflyPulled":   fireflyPulled,
		"Edit":            edit,
		"EvidencePretty":  prettyJSON(evidence),
		"Flash":           flashFromCookie(r, w),
		"DestOptions":     destAccounts,
		"SourceOptions":   srcAccounts,
		"CategoryOptions": categories,
		"BudgetOptions":   budgets,
		"TagLibrary":      allTags,
		"SuggestedTags":   suggested,
	})
}

func (h *Handler) fetchDetail(ctx context.Context, uuid string) (detailRow, editForm, string, error) {
	var (
		r           detailRow
		amountPaise int64
		tier        sql.NullInt64
		conf        sql.NullFloat64
		evidence    sql.NullString
		tsStr       string

		cSrcID, cDestID, cCatID, cBudID sql.NullInt64
		cDesc, cTags                    sql.NullString
		cDestName, pDestName            sql.NullString
		cSrcName, pSrcName              sql.NullString
		pSrcID, pDestID, pCatID, pBudID sql.NullInt64
		pDesc                           sql.NullString
		fAmt                            sql.NullInt64
		fCur                            sql.NullString
		cAmt, cFx                       sql.NullInt64
		cTs                             sql.NullString
		dup                             int
	)
	err := h.db.QueryRowContext(ctx, `
		SELECT fold_uuid, narration, txn_timestamp, amount_paise, currency,
		       foreign_amount_paise, foreign_currency, mode, type,
		       COALESCE(merchant_extracted,''), status,
		       classifier_tier, classifier_confidence, classifier_evidence_json,
		       confirmed_source_account_id, confirmed_destination_account_id,
		       confirmed_category_id, confirmed_budget_id,
		       confirmed_description, confirmed_tags_json,
		       proposed_source_account_id, proposed_destination_account_id,
		       proposed_category_id, proposed_budget_id, proposed_description,
		       confirmed_destination_account_name, proposed_destination_account_name,
		       confirmed_source_account_name, proposed_source_account_name,
		       confirmed_amount_paise, confirmed_foreign_amount_paise, confirmed_txn_timestamp,
		       `+possibleDuplicateSQL("raw_payload")+`, raw_payload
		FROM staged_fold_txns
		WHERE fold_uuid = ?
	`, uuid).Scan(
		&r.FoldUUID, &r.Narration, &tsStr, &amountPaise, &r.Currency, &fAmt, &fCur, &r.Mode, &r.Type,
		&r.MerchantExtracted, &r.Status, &tier, &conf, &evidence,
		&cSrcID, &cDestID, &cCatID, &cBudID, &cDesc, &cTags,
		&pSrcID, &pDestID, &pCatID, &pBudID, &pDesc,
		&cDestName, &pDestName,
		&cSrcName, &pSrcName,
		&cAmt, &cFx, &cTs, &dup, &r.RawPayload,
	)
	if err != nil {
		return r, editForm{}, "", err
	}

	// The "fold transaction" card shows what fold SAW (the alert); the edit
	// form below shows what push will SEND (corrections applied).
	foldTime, ok := parseDBTime(tsStr)
	if ok {
		r.TxnTimestamp = foldTime.Format("Jan 02, 2006 15:04 UTC")
		r.TxnTimestampUTC = foldTime.Format(time.RFC3339)
	} else {
		r.TxnTimestamp = tsStr
	}
	r.AmountDisplay = paiseToDecimal(amountPaise)
	r.ForeignDisplay = foreignDisplay(fAmt, fCur)
	r.Edited = cAmt.Valid || cFx.Valid || cTs.Valid
	r.Manual = r.Mode == manualMode
	r.PossibleDuplicate = dup == 1
	if r.RawPayload == "{}" || r.RawPayload == `{"manual":true}` {
		r.RawPayload = ""
	}
	r.RawPayload = prettyJSON(r.RawPayload)
	if tier.Valid {
		r.TierLabel = tierLabel(int(tier.Int64))
	}
	if conf.Valid {
		r.Confidence = conf.Float64
	}

	edit := editForm{
		CategoryID:  nullableInt64Str(cCatID, pCatID),
		BudgetID:    nullableInt64Str(cBudID, pBudID),
		Description: nullableStringValue(cDesc, pDesc),
	}
	// Destination & source resolve as a UNIT so a name-only correction
	// (confirmed name, null id) is never displayed — nor re-saved — as the
	// stale proposed account: confirmed id → its firefly name; else the
	// confirmed name-only; else proposed id → its name; else proposed name.
	switch {
	case cDestID.Valid:
		edit.DestinationAccountID = strconv.FormatInt(cDestID.Int64, 10)
		edit.DestinationAccountName = h.lookupAccountName(ctx, cDestID.Int64)
	case strings.TrimSpace(cDestName.String) != "":
		edit.DestinationAccountName = strings.TrimSpace(cDestName.String)
	case pDestID.Valid:
		edit.DestinationAccountID = strconv.FormatInt(pDestID.Int64, 10)
		edit.DestinationAccountName = h.lookupAccountName(ctx, pDestID.Int64)
	default:
		edit.DestinationAccountName = strings.TrimSpace(pDestName.String)
	}
	switch {
	case cSrcID.Valid:
		edit.SourceAccountID = strconv.FormatInt(cSrcID.Int64, 10)
		edit.SourceAccountName = h.lookupAccountName(ctx, cSrcID.Int64)
	case strings.TrimSpace(cSrcName.String) != "":
		edit.SourceAccountName = strings.TrimSpace(cSrcName.String)
	case pSrcID.Valid:
		edit.SourceAccountID = strconv.FormatInt(pSrcID.Int64, 10)
		edit.SourceAccountName = h.lookupAccountName(ctx, pSrcID.Int64)
	default:
		edit.SourceAccountName = strings.TrimSpace(pSrcName.String)
	}
	if cTags.Valid {
		var tags []string
		_ = json.Unmarshal([]byte(cTags.String), &tags)
		edit.Tags = strings.Join(tags, ", ")
	}
	if id, ok := parseInt(edit.CategoryID); ok {
		edit.CategoryName = h.lookupCategoryName(ctx, id)
	}
	if id, ok := parseInt(edit.BudgetID); ok {
		edit.BudgetName = h.lookupBudgetName(ctx, id)
	}

	// Money and time: the correction when there is one, else fold's value.
	effAmt := amountPaise
	if cAmt.Valid {
		effAmt = cAmt.Int64
	}
	edit.Amount = paiseToDecimal(effAmt)
	if fCur.Valid && fCur.String != "" {
		edit.ForeignCurrency = fCur.String
		switch {
		case cFx.Valid:
			edit.ForeignAmount = paiseToDecimal(cFx.Int64)
		case fAmt.Valid:
			edit.ForeignAmount = paiseToDecimal(fAmt.Int64)
		}
	}
	effTime := foldTime
	if cTs.Valid {
		if t, ok := parseDBTime(cTs.String); ok {
			effTime = t
		}
	}
	edit.Date, edit.Time = istDateTime(effTime)

	ev := ""
	if evidence.Valid {
		ev = evidence.String
	}
	return r, edit, ev, nil
}

// handleSave writes the edited fields to the confirmed_* columns. No
// firefly call. Surfaces any unresolved names as a flash warning so
// the user knows which fields were dropped to NULL.
func (h *Handler) handleSave(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	unresolved, err := h.saveEdits(r.Context(), uuid, r.Form)
	if err != nil {
		h.flashErr(w, "save failed: "+err.Error())
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	if len(unresolved) > 0 {
		h.flashErr(w, "saved, but couldn't resolve: "+strings.Join(unresolved, "; ")+
			" — pick from the autocomplete suggestions or create the account in firefly first")
	} else {
		h.flashOk(w, "edits saved")
	}
	http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
}

// handlePush saves edits AND pushes the row to firefly. If any name
// failed to resolve, we DON'T push (would send NULL fields to firefly
// and 422). User is sent back to the form with a flash explaining.
func (h *Handler) handlePush(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// On success, return to the list the user came from (the hidden "back"
	// field carries the origin list URL); on any failure we stay on the
	// detail page so they can fix and retry.
	back := backOr(r.FormValue("back"), "/admin/ui/?status=pushed")
	unresolved, err := h.saveEdits(r.Context(), uuid, r.Form)
	if err != nil {
		h.flashErr(w, "save failed before push: "+err.Error())
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	if len(unresolved) > 0 {
		h.flashErr(w, "push aborted — couldn't resolve: "+strings.Join(unresolved, "; ")+
			". Edits were saved; fix the unresolved names and try again.")
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	report, err := h.pusher.Push(r.Context(), uuid, true)
	if err != nil {
		h.flashErr(w, "push failed: "+err.Error())
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	h.flashOk(w, fmt.Sprintf("pushed (%s) — firefly id %d", report.Action, report.FireflyTxnID))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// backOr returns back when it's a safe in-app UI path, else the fallback.
// Guards against open redirects (only our own /admin/ui/ paths).
func backOr(back, fallback string) string {
	if strings.HasPrefix(back, "/admin/ui/") {
		return back
	}
	return fallback
}

// handleUpdate corrects an ALREADY-PUSHED row: it saves the edited fields
// then UPDATES the existing firefly transaction in place (PUT) instead of
// creating a duplicate. The detail page pre-filled the form from firefly's
// current state, so this "clubs" the operator's correction with firefly's
// live values (including manual edits made there). Failures keep the
// operator on the detail page.
func (h *Handler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	back := backOr(r.FormValue("back"), "/admin/ui/?status=pushed")
	if h.pusher == nil {
		h.flashErr(w, "update is unavailable")
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	unresolved, err := h.saveEdits(r.Context(), uuid, r.Form)
	if err != nil {
		h.flashErr(w, "save failed before update: "+err.Error())
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	if len(unresolved) > 0 {
		h.flashErr(w, "update aborted — couldn't resolve: "+strings.Join(unresolved, "; ")+
			". Edits were saved; fix the unresolved names and try again.")
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	report, err := h.pusher.Update(r.Context(), uuid)
	if err != nil {
		h.flashErr(w, "firefly update failed: "+err.Error())
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	h.flashOk(w, fmt.Sprintf("updated firefly transaction in place (id %d)", report.FireflyTxnID))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleSkip sets status='skipped' so this row is excluded from
// firefly. Uses confirmed_description if present to record a reason
// (form field "skip_reason" optional).
func (h *Handler) handleSkip(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	_ = r.ParseForm()
	back := backOr(r.FormValue("back"), "/admin/ui/?status=needs_review")
	_, err := h.db.ExecContext(r.Context(), `
		UPDATE staged_fold_txns
		SET status='skipped', updated_at=CURRENT_TIMESTAMP
		WHERE fold_uuid = ?
	`, uuid)
	if err != nil {
		h.flashErr(w, "skip failed: "+err.Error())
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	h.flashOk(w, "marked skipped")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleSyncAccounts refreshes the firefly_accounts mirror on demand — the
// "sync accounts from firefly" button — so an account the user JUST created
// in firefly becomes selectable in the destination/source autocompletes
// without waiting for the hourly cron refresh.
func (h *Handler) handleSyncAccounts(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/admin/ui/") {
		back = "/admin/ui/"
	}
	if h.fireflyAccounts == nil {
		h.flashErr(w, "account sync is unavailable (no syncer configured)")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	report, err := h.fireflyAccounts.Sync(ctx)
	if err != nil {
		h.flashErr(w, "account sync failed: "+err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	h.flashOk(w, fmt.Sprintf("synced %d firefly accounts (%d assets) — new ones are now selectable", report.Upserted, report.Assets))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleReclassify re-runs the classifier on the selected fold_uuids —
// the review list's "reclassify selected" action, for backfilling
// already-processed transactions on demand. Skips pushed rows and
// preserves human-confirmed fields (see classifier.ReclassifyUUIDs).
func (h *Handler) handleReclassify(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// "back" returns the user to the list+filters they came from. Only
	// honour our own relative paths (no open redirect).
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/admin/ui/") {
		back = "/admin/ui/"
	}
	if h.cls == nil {
		h.flashErr(w, "reclassify is unavailable (no classifier configured)")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	uuids := r.Form["fold_uuids"]
	if len(uuids) == 0 {
		h.flashErr(w, "no transactions selected to reclassify")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	// Each row is one LLM call (the classifier fans them out concurrently).
	// Budget generously and extend the write deadline so even a large
	// selection finishes and still returns a response.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(35 * time.Minute))
	}
	report, err := h.cls.ReclassifyUUIDs(ctx, uuids)
	if err != nil {
		h.flashErr(w, "reclassify failed: "+err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	h.flashOk(w, fmt.Sprintf("reclassified %d — %d ready, %d need review",
		report.Examined, report.AutoClassified, report.NeedsReview))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// saveEdits writes form values to the confirmed_* columns. Names from
// the form (destination_name, source_name, category_name, budget_name)
// are resolved to IDs against firefly_txns. Unresolved names produce
// a NULL in the corresponding column AND an UnresolvedNames slice
// returned to the caller for surfacing as a flash warning.
//
// Empty fields are written as NULL so the eventual push falls through
// to proposed_* (or fails firefly validation cleanly). Status is
// bumped to ready_to_push if it was needs_review.
func (h *Handler) saveEdits(ctx context.Context, uuid string, form url.Values) (unresolved []string, err error) {
	destName := strings.TrimSpace(form.Get("destination_name"))
	srcName := strings.TrimSpace(form.Get("source_name"))
	catName := strings.TrimSpace(form.Get("category_name"))
	budName := strings.TrimSpace(form.Get("budget_name"))
	desc := strings.TrimSpace(form.Get("description"))
	tagsStr := strings.TrimSpace(form.Get("tags"))

	// Effective firefly type governs whether an unmatched destination name
	// is allowed. A withdrawal's expense account is auto-created by firefly
	// from the name, so an unmatched name is a NEW account, not an error.
	// A deposit/transfer destination must be an existing asset, so there an
	// unmatched name is a genuine unresolved field.
	var effType string
	_ = h.db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(confirmed_txn_type,''), NULLIF(proposed_txn_type,''),
		                CASE WHEN type='INCOMING' THEN 'deposit' ELSE 'withdrawal' END)
		FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&effType)

	var dstID, srcID, catID, budID any
	var dstName any    // set instead of dstID for a new expense account (withdrawal)
	var srcNameVal any // set instead of srcID for a new revenue account (deposit)
	if destName != "" {
		if id := h.resolveAccountID(ctx, "destination", destName); id != 0 {
			dstID = id
		} else if effType == "withdrawal" {
			// Novel merchant: keep the typed name so the push creates the
			// firefly expense account by that name (not "unresolved").
			dstName = destName
		} else {
			unresolved = append(unresolved, fmt.Sprintf("destination %q", destName))
		}
	}
	if srcName != "" {
		if id := h.resolveAccountID(ctx, "source", srcName); id != 0 {
			srcID = id
		} else if effType == "deposit" {
			// Deposit payer: keep the typed name so the push creates the
			// firefly REVENUE account by that name — symmetric to a
			// withdrawal's new expense destination. (A withdrawal/transfer
			// source must be an existing asset, so there it stays unresolved.)
			srcNameVal = srcName
		} else {
			unresolved = append(unresolved, fmt.Sprintf("source %q", srcName))
		}
	}
	if catName != "" {
		if id := h.resolveCategoryID(ctx, catName); id != 0 {
			catID = id
		} else {
			unresolved = append(unresolved, fmt.Sprintf("category %q", catName))
		}
	}
	if budName != "" {
		if id := h.resolveBudgetID(ctx, budName); id != 0 {
			budID = id
		} else {
			unresolved = append(unresolved, fmt.Sprintf("budget %q", budName))
		}
	}

	var tagsJSON any
	if tagsStr != "" {
		parts := strings.Split(tagsStr, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		b, _ := json.Marshal(out)
		tagsJSON = string(b)
	}

	// Explicit "none". The form pre-fills category and budget with the
	// current choice (or the classifier's suggestion), so an EMPTY submitted
	// field means the human cleared it. Store 0: push sends nothing, and the
	// suggestion can't leak back in through the confirmed-else-proposed
	// fallback (the way a wrong "Eating outside" on a hostel stay used to).
	if catName == "" && formHas(form, "category_name") {
		catID = int64(0)
	}
	if budName == "" && formHas(form, "budget_name") {
		budID = int64(0)
	}

	// Money and time corrections (statement reconciliation).
	amtOv, fxOv, tsOv, bad, err := h.moneyOverrides(ctx, uuid, form)
	if err != nil {
		return unresolved, err
	}
	unresolved = append(unresolved, bad...)

	_, err = h.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET confirmed_source_account_id        = ?,
		    confirmed_source_account_name      = ?,
		    confirmed_destination_account_id   = ?,
		    confirmed_destination_account_name = ?,
		    confirmed_category_id              = ?,
		    confirmed_budget_id                = ?,
		    confirmed_description              = ?,
		    confirmed_tags_json                = ?,
		    confirmed_amount_paise             = ?,
		    confirmed_foreign_amount_paise     = ?,
		    confirmed_txn_timestamp            = ?,
		    reviewed_at                        = CURRENT_TIMESTAMP,
		    updated_at                         = CURRENT_TIMESTAMP,
		    status = CASE WHEN status='needs_review' THEN 'ready_to_push' ELSE status END
		WHERE fold_uuid = ?
	`, srcID, srcNameVal, dstID, dstName, catID, budID, nullableStrFromForm(desc), tagsJSON,
		amtOv, fxOv, tsOv, uuid)
	if err != nil {
		return unresolved, err
	}
	// Which purchase a refund refunds (the "refund of" picker). Only when
	// the form carried it, so other clients keep what is stored.
	if formHas(form, "refund_of") {
		bad, err := h.saveRefundOf(ctx, uuid, form.Get("refund_of"))
		if err != nil {
			return unresolved, err
		}
		unresolved = append(unresolved, bad...)
	}
	return unresolved, nil
}

// formHas reports whether the submitted form carried a field at all. It lets
// a new form field change behaviour (e.g. "empty budget = none") without
// changing what happens for a client that never sends that field.
func formHas(form url.Values, key string) bool {
	_, ok := form[key]
	return ok
}

// moneyOverrides turns the form's amount / foreign amount / date+time into
// the confirmed_* override values to store. Rules, per field:
//   - field absent from the form      → keep whatever is stored now;
//   - empty                           → no override (use fold's value);
//   - equal to fold's own value       → no override (NULL, not a copy);
//   - different                       → store it;
//   - unparseable / not positive      → keep stored, report as unresolved
//     (so a bad amount also blocks a push).
//
// Foreign amount only applies when fold recorded a foreign side; the date is
// entered in IST at minute precision.
func (h *Handler) moneyOverrides(ctx context.Context, uuid string, form url.Values) (amt, fx, ts any, bad []string, err error) {
	var (
		baseAmt       int64
		baseFx        sql.NullInt64
		baseCur       sql.NullString
		baseTs        string
		curAmt, curFx sql.NullInt64
		curTs         sql.NullTime
	)
	err = h.db.QueryRowContext(ctx, `
		SELECT amount_paise, foreign_amount_paise, foreign_currency, txn_timestamp,
		       confirmed_amount_paise, confirmed_foreign_amount_paise, confirmed_txn_timestamp
		FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).
		Scan(&baseAmt, &baseFx, &baseCur, &baseTs, &curAmt, &curFx, &curTs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, nil, nil // the UPDATE that follows is a no-op too
	}
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("read money for overrides: %w", err)
	}
	if curAmt.Valid {
		amt = curAmt.Int64
	}
	if curFx.Valid {
		fx = curFx.Int64
	}
	if curTs.Valid {
		ts = curTs.Time.UTC()
	}

	if formHas(form, "amount") {
		v := strings.TrimSpace(form.Get("amount"))
		p, perr := parseMoneyToPaise(v)
		switch {
		case v == "":
			amt = nil
		case perr != nil || p <= 0:
			bad = append(bad, fmt.Sprintf("amount %q", v))
		case p == baseAmt:
			amt = nil
		default:
			amt = p
		}
	}
	if formHas(form, "foreign_amount") && baseCur.Valid && baseCur.String != "" {
		v := strings.TrimSpace(form.Get("foreign_amount"))
		p, perr := parseMoneyToPaise(v)
		switch {
		case v == "":
			fx = nil
		case perr != nil || p <= 0:
			bad = append(bad, fmt.Sprintf("foreign amount %q", v))
		case baseFx.Valid && p == baseFx.Int64:
			fx = nil
		default:
			fx = p
		}
	}
	if formHas(form, "date") {
		d := strings.TrimSpace(form.Get("date"))
		t, perr := parseISTForm(d, form.Get("time"))
		base, baseOK := parseDBTime(baseTs)
		switch {
		case d == "":
			ts = nil
		case perr != nil:
			bad = append(bad, fmt.Sprintf("date %q %q", d, form.Get("time")))
		case baseOK && base.Truncate(time.Minute).Equal(t):
			ts = nil
		default:
			ts = t
		}
	}
	return amt, fx, ts, bad, nil
}

// helpers ////////////////////////////////////////////////////////////////

func (h *Handler) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		h.log.Warn("render", "err", err)
	}
}

func (h *Handler) flashOk(w http.ResponseWriter, msg string)  { setFlash(w, "ok", msg) }
func (h *Handler) flashErr(w http.ResponseWriter, msg string) { setFlash(w, "err", msg) }

type flashCookie struct {
	Kind    string
	Message string
}

func setFlash(w http.ResponseWriter, kind, msg string) {
	v, _ := json.Marshal(flashCookie{Kind: kind, Message: msg})
	http.SetCookie(w, &http.Cookie{
		Name:     "tfe-flash",
		Value:    url.QueryEscape(string(v)),
		Path:     "/admin/ui",
		HttpOnly: true,
		MaxAge:   60,
	})
}

func flashFromCookie(r *http.Request, w http.ResponseWriter) *flashCookie {
	c, err := r.Cookie("tfe-flash")
	if err != nil {
		return nil
	}
	v, _ := url.QueryUnescape(c.Value)
	var f flashCookie
	if err := json.Unmarshal([]byte(v), &f); err != nil {
		return nil
	}
	// Consume.
	http.SetCookie(w, &http.Cookie{Name: "tfe-flash", Path: "/admin/ui", MaxAge: -1})
	return &f
}

func tierLabel(t int) string {
	switch t {
	case 1:
		return "1 (lookup)"
	case 2:
		return "2 (FTS5)"
	case 3:
		return "3 (LLM)"
	case 4:
		return "4 (review)"
	case 5:
		return "5 (refund)"
	default:
		return fmt.Sprintf("%d", t)
	}
}

func paiseToDecimal(p int64) string {
	if p < 0 {
		p = -p
	}
	return fmt.Sprintf("%d.%02d", p/100, p%100)
}

// foreignDisplay formats the original foreign charge for display, e.g.
// "AED 25.00". Returns "" for a domestic transaction (no foreign side), so
// templates can render it conditionally.
func foreignDisplay(paise sql.NullInt64, currency sql.NullString) string {
	if !paise.Valid || !currency.Valid || currency.String == "" {
		return ""
	}
	return currency.String + " " + paiseToDecimal(paise.Int64)
}

func nullableInt64Str(confirmed, proposed sql.NullInt64) string {
	if confirmed.Valid {
		return strconv.FormatInt(confirmed.Int64, 10)
	}
	if proposed.Valid {
		return strconv.FormatInt(proposed.Int64, 10)
	}
	return ""
}

func nullableStringValue(confirmed, proposed sql.NullString) string {
	if confirmed.Valid && confirmed.String != "" {
		return confirmed.String
	}
	if proposed.Valid {
		return proposed.String
	}
	return ""
}

func nullableFromForm(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return n
}

func nullableStrFromForm(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func parseInt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func prettyJSON(s string) string {
	if s == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(out)
}

// lookupAccountName/lookupCategoryName/lookupBudgetName: cheap
// firefly_txns scans for a friendly name. Cached implicitly by SQLite
// page cache for a corpus this small.

func (h *Handler) lookupAccountName(ctx context.Context, id int64) string {
	var name sql.NullString
	_ = h.db.QueryRowContext(ctx, `
		SELECT destination_account_name FROM firefly_txns WHERE destination_account_id = ?
		UNION SELECT source_account_name FROM firefly_txns WHERE source_account_id = ?
		LIMIT 1
	`, id, id).Scan(&name)
	if name.Valid && name.String != "" {
		return name.String
	}
	// No transaction history for this account yet — e.g. a firefly asset the
	// user just created ("Ixigo AU Bank Credit Card"). Fall back to the
	// firefly_accounts mirror, which holds firefly's real account list
	// independent of whether anything has been booked against it.
	_ = h.db.QueryRowContext(ctx,
		`SELECT name FROM firefly_accounts WHERE firefly_id = ? LIMIT 1`, id).Scan(&name)
	if name.Valid {
		return name.String
	}
	return ""
}

func (h *Handler) lookupCategoryName(ctx context.Context, id int64) string {
	var name sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT category_name FROM firefly_txns WHERE category_id = ? LIMIT 1`, id).Scan(&name)
	if name.Valid {
		return name.String
	}
	return ""
}

func (h *Handler) lookupBudgetName(ctx context.Context, id int64) string {
	var name sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT budget_name FROM firefly_txns WHERE budget_id = ? LIMIT 1`, id).Scan(&name)
	if name.Valid {
		return name.String
	}
	return ""
}
