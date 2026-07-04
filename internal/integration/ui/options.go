package ui

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// nameOption is one entry in a UI datalist. ID is included so the
// template can offer it as additional context (e.g. "Zomato (id 505)")
// but the form only sends Name back — IDs get resolved server-side.
type nameOption struct {
	ID   int64
	Name string
}

// listAccountsByKind returns DISTINCT (id, name) pairs from
// firefly_txns for either "destination" (the merchant side, ~1.3k
// expense accounts) or "source" (the user's own asset accounts,
// usually <10). Sorted alphabetically.
func (h *Handler) listAccountsByKind(ctx context.Context, kind string) ([]nameOption, error) {
	idCol, nameCol := "destination_account_id", "destination_account_name"
	if kind == "source" {
		idCol, nameCol = "source_account_id", "source_account_name"
	}
	q := fmt.Sprintf(`
		SELECT DISTINCT %s, %s
		FROM firefly_txns
		WHERE %s IS NOT NULL AND %s IS NOT NULL AND %s <> ''
		ORDER BY %s COLLATE NOCASE
	`, idCol, nameCol, idCol, nameCol, nameCol, nameCol)
	rows, err := h.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nameOption
	for rows.Next() {
		var n nameOption
		if err := rows.Scan(&n.ID, &n.Name); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// listFilterAccounts returns the distinct source accounts that actually
// appear on staged rows (effective source = confirmed ?? proposed),
// resolved to display names and sorted alphabetically. This backs the
// index account-filter dropdown — listing only filterable accounts so
// there are no dead options, unlike a blanket scan of every firefly
// source account (which would include long-gone payers). The set is
// tiny (a handful of cards/banks), so the per-id name lookup is cheap.
func (h *Handler) listFilterAccounts(ctx context.Context) ([]nameOption, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT DISTINCT COALESCE(confirmed_source_account_id, proposed_source_account_id) AS aid
		FROM staged_fold_txns
		WHERE COALESCE(confirmed_source_account_id, proposed_source_account_id) IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]nameOption, 0, len(ids))
	for _, id := range ids {
		name := h.lookupAccountName(ctx, id)
		if name == "" {
			name = fmt.Sprintf("account #%d", id)
		}
		out = append(out, nameOption{ID: id, Name: name})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// listCategories / listBudgets are siblings of listAccountsByKind for
// the smaller dimension tables.
func (h *Handler) listCategories(ctx context.Context) ([]nameOption, error) {
	return h.listSimple(ctx, "category_id", "category_name")
}
func (h *Handler) listBudgets(ctx context.Context) ([]nameOption, error) {
	return h.listSimple(ctx, "budget_id", "budget_name")
}

func (h *Handler) listSimple(ctx context.Context, idCol, nameCol string) ([]nameOption, error) {
	q := fmt.Sprintf(`
		SELECT DISTINCT %s, %s
		FROM firefly_txns
		WHERE %s IS NOT NULL AND %s IS NOT NULL AND %s <> ''
		ORDER BY %s COLLATE NOCASE
	`, idCol, nameCol, idCol, nameCol, nameCol, nameCol)
	rows, err := h.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nameOption
	for rows.Next() {
		var n nameOption
		if err := rows.Scan(&n.ID, &n.Name); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// listAllTags walks firefly_txns.tags_json (stringified JSON arrays),
// flattens, deduplicates, and returns alphabetically. Used as the tag
// autocomplete list in the detail form.
//
// We don't reach for SQLite's json_each extension because we don't
// fully control the build flags in the modernc.org/sqlite driver.
// In-memory dedup over ~7k rows is sub-millisecond.
func (h *Handler) listAllTags(ctx context.Context) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT tags_json FROM firefly_txns
		WHERE tags_json IS NOT NULL AND tags_json <> '' AND tags_json <> '[]'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var arr []string
		if json.Unmarshal([]byte(raw), &arr) != nil {
			continue
		}
		for _, t := range arr {
			t = strings.TrimSpace(t)
			if t != "" {
				seen[t] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out, nil
}

// suggestedTagsForMerchant returns the tags most recently used at the
// given destination_account_id. Surfaces in the UI as click-to-add
// chips — if the user always tags Zomato with "Lunch", we want that
// front-and-centre even though our tag library has 109 entries.
//
// Limited to 8 distinct tags; sorted by recency (latest firefly_txns
// using the tag wins).
func (h *Handler) suggestedTagsForMerchant(ctx context.Context, destinationID int64) ([]string, error) {
	if destinationID == 0 {
		return nil, nil
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT tags_json, date FROM firefly_txns
		WHERE destination_account_id = ?
		  AND tags_json IS NOT NULL AND tags_json <> '' AND tags_json <> '[]'
		ORDER BY date DESC
		LIMIT 50
	`, destinationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]int{} // tag → first-seen-rank, lower = more recent
	rank := 0
	for rows.Next() {
		var raw, _date string
		if err := rows.Scan(&raw, &_date); err != nil {
			return nil, err
		}
		rank++
		var arr []string
		if json.Unmarshal([]byte(raw), &arr) != nil {
			continue
		}
		for _, t := range arr {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if _, ok := seen[t]; !ok {
				seen[t] = rank
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type pair struct {
		t string
		r int
	}
	pairs := make([]pair, 0, len(seen))
	for t, r := range seen {
		pairs = append(pairs, pair{t, r})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].r < pairs[j].r })
	if len(pairs) > 8 {
		pairs = pairs[:8]
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.t)
	}
	return out, nil
}

// resolveAccountID looks up the firefly account-id for a given typed
// name. Case-insensitive. Returns 0 if no match (caller should NULL
// the corresponding column).
//
// On multiple matches we pick the one with the most-recent firefly
// transaction — that's the merchant the user is most actively using.
func (h *Handler) resolveAccountID(ctx context.Context, kind, name string) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	idCol, nameCol := "destination_account_id", "destination_account_name"
	if kind == "source" {
		idCol, nameCol = "source_account_id", "source_account_name"
	}
	q := fmt.Sprintf(`
		SELECT %s FROM firefly_txns
		WHERE LOWER(%s) = LOWER(?) AND %s IS NOT NULL
		ORDER BY date DESC LIMIT 1
	`, idCol, nameCol, idCol)
	var id sql.NullInt64
	_ = h.db.QueryRowContext(ctx, q, name).Scan(&id)
	if id.Valid {
		return id.Int64
	}
	// Fall back to firefly's real account list (the mirror) so a source that
	// resolved to a freshly-created asset — no transaction history yet, e.g.
	// "Ixigo AU Bank Credit Card" — still resolves at push time instead of
	// aborting as "unresolved". A source is always an ASSET, so scope the
	// match to type='asset': that also avoids a name that exists on both
	// sides (a credit card can be both your asset AND an expense payee)
	// resolving to the wrong account.
	if kind == "source" {
		_ = h.db.QueryRowContext(ctx, `
			SELECT firefly_id FROM firefly_accounts
			WHERE LOWER(name) = LOWER(?) AND type = 'asset' AND active = 1
			ORDER BY firefly_id LIMIT 1
		`, name).Scan(&id)
		if id.Valid {
			return id.Int64
		}
	}
	return 0
}

// resolveCategoryID / resolveBudgetID are siblings, smaller scope.
func (h *Handler) resolveCategoryID(ctx context.Context, name string) int64 {
	return h.resolveSimple(ctx, "category_id", "category_name", name)
}
func (h *Handler) resolveBudgetID(ctx context.Context, name string) int64 {
	return h.resolveSimple(ctx, "budget_id", "budget_name", name)
}

func (h *Handler) resolveSimple(ctx context.Context, idCol, nameCol, name string) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	q := fmt.Sprintf(`
		SELECT %s FROM firefly_txns
		WHERE LOWER(%s) = LOWER(?) AND %s IS NOT NULL
		ORDER BY date DESC LIMIT 1
	`, idCol, nameCol, idCol)
	var id sql.NullInt64
	_ = h.db.QueryRowContext(ctx, q, name).Scan(&id)
	if id.Valid {
		return id.Int64
	}
	return 0
}
