// Package ui is the server-rendered admin UI for the integration.
// HTML pages at /admin/ui/. The UI lets a human:
//
//	- list staged fold transactions filtered by status
//	- inspect a row's classifier decision and evidence
//	- edit the proposed firefly fields (destination, source, category,
//	  budget, description, tags)
//	- push to firefly (with the same idempotency safety as the JSON
//	  endpoint — the Pusher is the same code path)
//	- skip a transaction (excluded from firefly forever)
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
	db          *sql.DB
	pusher      *integration.Pusher
	log         *slog.Logger
	adminKey    string
	auth        AuthMode
	indexTmpl   *template.Template
	detailTmpl  *template.Template
}

// New constructs a UI Handler.
func New(db *sql.DB, pusher *integration.Pusher, log *slog.Logger, adminKey string, auth AuthMode) (*Handler, error) {
	indexTmpl, err := template.ParseFS(tmplFS, "templates/layout.html", "templates/index.html")
	if err != nil {
		return nil, fmt.Errorf("parse index template: %w", err)
	}
	detailTmpl, err := template.ParseFS(tmplFS, "templates/layout.html", "templates/detail.html")
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
type indexRow struct {
	FoldUUID             string
	TxnTimestamp         string
	AmountDisplay        string
	Currency             string
	Mode                 string
	Type                 string
	MerchantExtracted    string
	Status               string
	TierLabel            string
	Confidence           float64
	ProposedCategoryName string
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "needs_review"
	}
	rows, err := h.listRows(r.Context(), status)
	if err != nil {
		h.log.Error("list rows", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, h.indexTmpl, map[string]any{
		"Title":  "review",
		"Status": status,
		"Rows":   rows,
		"Flash":  flashFromCookie(r, w),
	})
}

func (h *Handler) listRows(ctx context.Context, status string) ([]indexRow, error) {
	q := `
		SELECT s.fold_uuid, s.txn_timestamp, s.amount_paise, s.currency, s.mode, s.type,
		       COALESCE(s.merchant_extracted,''), s.status,
		       s.classifier_tier, s.classifier_confidence,
		       (SELECT category_name FROM firefly_txns
		         WHERE category_id = s.proposed_category_id LIMIT 1)
		FROM staged_fold_txns s
		WHERE s.status = ?
		ORDER BY s.txn_timestamp DESC
		LIMIT 200`
	rows, err := h.db.QueryContext(ctx, q, status)
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
			tsStr       string
		)
		if err := rows.Scan(&r.FoldUUID, &tsStr, &amountPaise, &r.Currency, &r.Mode, &r.Type,
			&r.MerchantExtracted, &r.Status, &tier, &conf, &catName); err != nil {
			return nil, err
		}
		// Format timestamp friendly.
		if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			r.TxnTimestamp = t.Format("Jan 02 15:04")
		} else if t, err := time.Parse("2006-01-02 15:04:05+00:00", tsStr); err == nil {
			r.TxnTimestamp = t.Format("Jan 02 15:04")
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
	h.render(w, h.detailTmpl, map[string]any{
		"Title":          uuid,
		"Row":            row,
		"Edit":           edit,
		"EvidencePretty": prettyJSON(evidence),
		"Flash":          flashFromCookie(r, w),
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
		       proposed_category_id, proposed_budget_id, proposed_description
		FROM staged_fold_txns
		WHERE fold_uuid = ?
	`, uuid).Scan(
		&r.FoldUUID, &r.Narration, &tsStr, &amountPaise, &r.Currency, &r.Mode, &r.Type,
		&r.MerchantExtracted, &r.Status, &tier, &conf, &evidence,
		&cSrcID, &cDestID, &cCatID, &cBudID, &cDesc, &cTags,
		&pSrcID, &pDestID, &pCatID, &pBudID, &pDesc,
	)
	if err != nil {
		return r, editForm{}, "", err
	}

	if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
		r.TxnTimestamp = t.Format("Jan 02, 2006 15:04 MST")
	} else if t, err := time.Parse("2006-01-02 15:04:05+00:00", tsStr); err == nil {
		r.TxnTimestamp = t.Format("Jan 02, 2006 15:04 MST")
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
// firefly call.
func (h *Handler) handleSave(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if err := h.saveEdits(r.Context(), uuid, r.Form); err != nil {
		h.flashErr(w, "save failed: "+err.Error())
		http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
		return
	}
	h.flashOk(w, "edits saved")
	http.Redirect(w, r, "/admin/ui/staged/"+uuid, http.StatusSeeOther)
}

// handlePush saves edits AND pushes the row to firefly.
func (h *Handler) handlePush(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("fold_uuid")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if err := h.saveEdits(r.Context(), uuid, r.Form); err != nil {
		h.flashErr(w, "save failed before push: "+err.Error())
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

// saveEdits writes form values to the confirmed_* columns. Empty
// fields are written as NULL so the push fall-through to proposed_*
// still works as designed. Status is bumped to ready_to_push if it
// was needs_review.
func (h *Handler) saveEdits(ctx context.Context, uuid string, form url.Values) error {
	dst := nullableFromForm(form.Get("destination_account_id"))
	src := nullableFromForm(form.Get("source_account_id"))
	cat := nullableFromForm(form.Get("category_id"))
	bud := nullableFromForm(form.Get("budget_id"))
	desc := strings.TrimSpace(form.Get("description"))
	tagsStr := strings.TrimSpace(form.Get("tags"))

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

	_, err := h.db.ExecContext(ctx, `
		UPDATE staged_fold_txns
		SET confirmed_source_account_id      = ?,
		    confirmed_destination_account_id = ?,
		    confirmed_category_id            = ?,
		    confirmed_budget_id              = ?,
		    confirmed_description            = ?,
		    confirmed_tags_json              = ?,
		    reviewed_at                      = CURRENT_TIMESTAMP,
		    updated_at                       = CURRENT_TIMESTAMP,
		    status = CASE WHEN status='needs_review' THEN 'ready_to_push' ELSE status END
		WHERE fold_uuid = ?
	`, src, dst, cat, bud, nullableStrFromForm(desc), tagsJSON, uuid)
	return err
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
