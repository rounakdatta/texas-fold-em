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
}

// New constructs a UI Handler.
func New(db *sql.DB, pusher *integration.Pusher, log *slog.Logger, adminKey string, auth AuthMode) (*Handler, error) {
	// filterURL is shared by the index pagination + filter controls so
	// every link preserves the active filter set without hand-built
	// querystrings. Registered on both templates for uniformity even
	// though only the index references it today.
	funcs := template.FuncMap{"filterURL": filterURL}
	indexTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(tmplFS, "templates/layout.html", "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse index template: %w", err)
	}
	detailTmpl, err := template.New("layout.html").Funcs(funcs).ParseFS(tmplFS, "templates/layout.html", "templates/detail.html")
	if err != nil {
		return nil, fmt.Errorf("parse detail template: %w", err)
	}
	return &Handler{
		db:         db,
		pusher:     pusher,
		log:        log.With("component", "ui"),
		adminKey:   adminKey,
		auth:       auth,
		indexTmpl:  indexTmpl,
		detailTmpl: detailTmpl,
	}, nil
}

// Mount registers all UI routes on the supplied mux. Routes:
//
//	GET  /admin/ui/                          → index (filtered by ?status=)
//	GET  /admin/ui/staged/{fold_uuid}        → detail
//	POST /admin/ui/staged/{fold_uuid}/save   → save edits
//	POST /admin/ui/staged/{fold_uuid}/push   → save edits + push
//	POST /admin/ui/staged/{fold_uuid}/skip   → mark skipped
//	GET  /admin/ui/login?key=<admin_key>     → set auth cookie (cookie mode only)
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("GET /admin/ui/", h.withAuth(h.handleIndex))
	mux.Handle("GET /admin/ui/staged/{fold_uuid}", h.withAuth(h.handleDetail))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/save", h.withAuth(h.handleSave))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/push", h.withAuth(h.handlePush))
	mux.Handle("POST /admin/ui/staged/{fold_uuid}/skip", h.withAuth(h.handleSkip))
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
	Mode                 string
	Type                 string
	MerchantExtracted    string
	Status               string
	TierLabel            string
	Confidence           float64
	ProposedCategoryName string
	// SourceAccountName is the effective paying account (confirmed
	// overrides proposed), resolved to a firefly name. Empty when the
	// row has no source account yet (common for pending/needs_review).
	SourceAccountName string
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
	clauses := []string{"s.status = ?"}
	args := []any{f.Status}
	if f.SourceAccount != "" {
		// Effective source = confirmed (human) overrides proposed
		// (classifier) — the same precedence the Pusher uses when it
		// resolves which account to send to firefly. A non-numeric value
		// is dropped upstream in handleIndex, so ParseInt should succeed;
		// guard anyway so a bad param degrades to "all" instead of erroring.
		if id, err := strconv.ParseInt(f.SourceAccount, 10, 64); err == nil {
			clauses = append(clauses, "COALESCE(s.confirmed_source_account_id, s.proposed_source_account_id) = ?")
			args = append(args, id)
		}
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
	q := `
		SELECT s.fold_uuid, s.txn_timestamp, s.amount_paise, s.currency, s.mode, s.type,
		       COALESCE(s.merchant_extracted,''), s.status,
		       s.classifier_tier, s.classifier_confidence,
		       (SELECT category_name FROM firefly_txns
		         WHERE category_id = s.proposed_category_id LIMIT 1),
		       (SELECT source_account_name FROM firefly_txns
		         WHERE source_account_id = COALESCE(s.confirmed_source_account_id, s.proposed_source_account_id) LIMIT 1)
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
			tier        sql.NullInt64
			conf        sql.NullFloat64
			catName     sql.NullString
			srcName     sql.NullString
			tsStr       string
		)
		if err := rows.Scan(&r.FoldUUID, &tsStr, &amountPaise, &r.Currency, &r.Mode, &r.Type,
			&r.MerchantExtracted, &r.Status, &tier, &conf, &catName, &srcName); err != nil {
			return nil, err
		}
		// Two views of the timestamp: a server-rendered fallback for
		// no-JS clients, and a canonical RFC3339 UTC string for the
		// client-side <time datetime="..."> conversion to local zone.
		if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			r.TxnTimestamp = t.Format("Jan 02 15:04 UTC")
			r.TxnTimestampUTC = t.UTC().Format(time.RFC3339)
		} else if t, err := time.Parse("2006-01-02 15:04:05+00:00", tsStr); err == nil {
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
			r.ProposedCategoryName = catName.String
		}
		if srcName.Valid {
			r.SourceAccountName = srcName.String
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
	Mode              string
	Type              string
	MerchantExtracted string
	Status            string
	TierLabel         string
	Confidence        float64
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

	h.render(w, h.detailTmpl, map[string]any{
		"Title":           uuid,
		"Row":             row,
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
		pSrcID, pDestID, pCatID, pBudID sql.NullInt64
		pDesc                           sql.NullString
	)
	err := h.db.QueryRowContext(ctx, `
		SELECT fold_uuid, narration, txn_timestamp, amount_paise, currency, mode, type,
		       COALESCE(merchant_extracted,''), status,
		       classifier_tier, classifier_confidence, classifier_evidence_json,
		       confirmed_source_account_id, confirmed_destination_account_id,
		       confirmed_category_id, confirmed_budget_id,
		       confirmed_description, confirmed_tags_json,
		       proposed_source_account_id, proposed_destination_account_id,
		       proposed_category_id, proposed_budget_id, proposed_description,
		       confirmed_destination_account_name, proposed_destination_account_name
		FROM staged_fold_txns
		WHERE fold_uuid = ?
	`, uuid).Scan(
		&r.FoldUUID, &r.Narration, &tsStr, &amountPaise, &r.Currency, &r.Mode, &r.Type,
		&r.MerchantExtracted, &r.Status, &tier, &conf, &evidence,
		&cSrcID, &cDestID, &cCatID, &cBudID, &cDesc, &cTags,
		&pSrcID, &pDestID, &pCatID, &pBudID, &pDesc,
		&cDestName, &pDestName,
	)
	if err != nil {
		return r, editForm{}, "", err
	}

	if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
		r.TxnTimestamp = t.Format("Jan 02, 2006 15:04 UTC")
		r.TxnTimestampUTC = t.UTC().Format(time.RFC3339)
	} else if t, err := time.Parse("2006-01-02 15:04:05+00:00", tsStr); err == nil {
		r.TxnTimestamp = t.Format("Jan 02, 2006 15:04 UTC")
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

	// Edit form values: confirmed_* takes precedence, fall back to proposed_*.
	edit := editForm{
		DestinationAccountID: nullableInt64Str(cDestID, pDestID),
		SourceAccountID:      nullableInt64Str(cSrcID, pSrcID),
		CategoryID:           nullableInt64Str(cCatID, pCatID),
		BudgetID:             nullableInt64Str(cBudID, pBudID),
		Description:          nullableStringValue(cDesc, pDesc),
	}
	if cTags.Valid {
		var tags []string
		_ = json.Unmarshal([]byte(cTags.String), &tags)
		edit.Tags = strings.Join(tags, ", ")
	}

	// Resolve the human-readable names from firefly_txns for display next to id inputs.
	if id, ok := parseInt(edit.DestinationAccountID); ok {
		edit.DestinationAccountName = h.lookupAccountName(ctx, id)
	} else if name := nullableStringValue(cDestName, pDestName); name != "" {
		// Name-only destination: a novel merchant the classifier proposed
		// for firefly to create on push. Pre-fill so the human sees and
		// can confirm/edit the name before it becomes a real account.
		edit.DestinationAccountName = name
	}
	if id, ok := parseInt(edit.SourceAccountID); ok {
		edit.SourceAccountName = h.lookupAccountName(ctx, id)
	}
	if id, ok := parseInt(edit.CategoryID); ok {
		edit.CategoryName = h.lookupCategoryName(ctx, id)
	}
	if id, ok := parseInt(edit.BudgetID); ok {
		edit.BudgetName = h.lookupBudgetName(ctx, id)
	}

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
	http.Redirect(w, r, "/admin/ui/?status=pushed", http.StatusSeeOther)
}

// handleSkip sets status='skipped' so this row is excluded from
// firefly. Uses confirmed_description if present to record a reason
// (form field "skip_reason" optional).
func (h *Handler) handleSkip(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
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
	http.Redirect(w, r, "/admin/ui/?status=needs_review", http.StatusSeeOther)
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
	var dstName any // set instead of dstID for a new expense account (withdrawal)
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

	_, err = h.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET confirmed_source_account_id        = ?,
		    confirmed_destination_account_id   = ?,
		    confirmed_destination_account_name = ?,
		    confirmed_category_id              = ?,
		    confirmed_budget_id                = ?,
		    confirmed_description              = ?,
		    confirmed_tags_json                = ?,
		    reviewed_at                        = CURRENT_TIMESTAMP,
		    updated_at                         = CURRENT_TIMESTAMP,
		    status = CASE WHEN status='needs_review' THEN 'ready_to_push' ELSE status END
		WHERE fold_uuid = ?
	`, srcID, dstID, dstName, catID, budID, nullableStrFromForm(desc), tagsJSON, uuid)
	return unresolved, err
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
