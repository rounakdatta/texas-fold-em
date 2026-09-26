package ui

// accounts_sync.go — refreshing fold's copy of Firefly's accounts, from
// wherever an account turns out to be missing.
//
// fold keeps a mirror of Firefly's accounts (firefly_accounts), refreshed on
// every sync cycle; the pickers offer what the mirror has. An account made in
// Firefly a minute ago isn't in it yet, so the moment a person notices is the
// moment they are looking for it in a picker — and that is where the sync is
// offered: at the end of the deck's pickers, on the editor, on the Add form.
// It runs in place (nothing on the page is lost, unlike a form post that
// reloads a half-edited transaction), and the answer says what arrived.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// syncedAccount is one account a sync brought that fold didn't have (or that
// came back into use).
type syncedAccount struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Short string `json:"short"`
	// Kind is what it is to the user: "account" (one of theirs), "payee"
	// (someone they pay) or "payer" (someone who pays them).
	Kind string `json:"kind"`
}

type accountSnap struct {
	name, typ string
	active    bool
}

var errSyncUnavailable = errors.New("account sync isn't set up on this fold")

// accountSnapshot is the mirror as it stands: id → name, type, active.
func (h *Handler) accountSnapshot(ctx context.Context) map[int64]accountSnap {
	out := map[int64]accountSnap{}
	rows, err := h.db.QueryContext(ctx, `SELECT firefly_id, name, type, active FROM firefly_accounts`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var a accountSnap
		var active int
		if rows.Scan(&id, &a.name, &a.typ, &active) == nil {
			a.active = active == 1
			out[id] = a
		}
	}
	return out
}

// syncAccounts refreshes the mirror from Firefly and returns what arrived:
// the user's own accounts first, then payees, then payers, each by name.
func (h *Handler) syncAccounts(ctx context.Context) ([]syncedAccount, error) {
	if h.fireflyAccounts == nil {
		return nil, errSyncUnavailable
	}
	before := h.accountSnapshot(ctx)
	if _, err := h.fireflyAccounts.Sync(ctx); err != nil {
		return nil, err
	}
	added := []syncedAccount{}
	for id, a := range h.accountSnapshot(ctx) {
		if b, had := before[id]; !a.active || (had && b.active) {
			continue
		}
		kind := map[string]string{"asset": "account", "expense": "payee", "revenue": "payer"}[a.typ]
		if kind == "" {
			continue // cash, liabilities, opening balances: no picker offers them
		}
		s := syncedAccount{ID: id, Name: a.name, Short: a.name, Kind: kind}
		if kind == "account" {
			s.Short = shortAccountName(a.name)
		}
		added = append(added, s)
	}
	rank := map[string]int{"account": 0, "payee": 1, "payer": 2}
	sort.Slice(added, func(i, j int) bool {
		if rank[added[i].Kind] != rank[added[j].Kind] {
			return rank[added[i].Kind] < rank[added[j].Kind]
		}
		return strings.ToLower(added[i].Name) < strings.ToLower(added[j].Name)
	})
	return added, nil
}

// syncMessage says what a sync brought, in one line: the names that arrived,
// or plainly that nothing did.
func syncMessage(added []syncedAccount) string {
	if len(added) == 0 {
		return "Synced — nothing new in Firefly."
	}
	names := []string{}
	for _, a := range added {
		if len(names) == 3 {
			break
		}
		names = append(names, a.Short)
	}
	list := strings.Join(names, ", ")
	if more := len(added) - len(names); more > 0 {
		list += fmt.Sprintf(" and %s more", groupIndian(int64(more)))
	}
	return fmt.Sprintf("Synced — %s new: %s.", groupIndian(int64(len(added))), list)
}

// accountsSyncedAt is when the mirror was last refreshed (its newest row);
// zero when it never was.
func (h *Handler) accountsSyncedAt(ctx context.Context) time.Time {
	var at sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT MAX(last_synced_at) FROM firefly_accounts`).Scan(&at)
	if t, ok := parseDBTime(at.String); ok {
		return t.UTC()
	}
	return time.Time{}
}

// agoText says how long ago an RFC3339 time was, the way the pages do:
// "just now", "14 min ago", "3 hours ago", else the day. Always rounded
// down: a sync 59½ minutes ago is not "1 hour ago".
func agoText(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 2*time.Hour:
		return "1 hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d/time.Hour))
	}
	switch day := spokenDay(t, time.Now()); day {
	case "Today", "Yesterday":
		return strings.ToLower(day)
	default:
		return "on " + day
	}
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleAPISyncAccounts is POST /api/sync-accounts: sync now, and
// say what arrived.
func (h *Handler) handleAPISyncAccounts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	added, err := h.syncAccounts(ctx)
	switch {
	case errors.Is(err, errSyncUnavailable):
		h.apiError(w, http.StatusServiceUnavailable, "Account sync isn’t set up on this fold.")
		return
	case err != nil:
		h.log.Warn("account sync failed", "err", err)
		h.apiError(w, http.StatusBadGateway, "Couldn’t reach Firefly — try again in a moment.")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK       bool            `json:"ok"`
		SyncedAt string          `json:"syncedAt"`
		Added    []syncedAccount `json:"added"`
		Message  string          `json:"message"`
	}{true, rfc3339OrEmpty(h.accountsSyncedAt(ctx)), added, syncMessage(added)})
}

// handleSyncAccounts is the same sync for a page without JavaScript: a form
// post that comes back to the page it was sent from, saying what arrived.
func (h *Handler) handleSyncAccounts(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	// Back to the page the form was on: its own "back", else the page that
	// posted it, else the list.
	fallback := pathList
	if ref, err := url.Parse(r.Referer()); err == nil {
		fallback = safeBack(ref.RequestURI(), pathList)
	}
	back := safeBack(r.FormValue("back"), fallback)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	added, err := h.syncAccounts(ctx)
	switch {
	case errors.Is(err, errSyncUnavailable):
		h.flashErr(w, "Account sync isn’t set up on this fold.")
	case err != nil:
		h.log.Warn("account sync failed", "err", err)
		h.flashErr(w, "Couldn’t reach Firefly — try again in a moment.")
	default:
		h.flashOk(w, syncMessage(added))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
