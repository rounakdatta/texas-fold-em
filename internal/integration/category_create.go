package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrCategoryName is a name that can't be a category: empty, or too long for
// a label on a card.
var ErrCategoryName = errors.New("a category needs a name of up to 100 characters")

// CategoryByName finds a category fold already knows — in its copy of
// Firefly's categories, or on a mirrored transaction — ignoring case. ok is
// false when there is none.
func CategoryByName(ctx context.Context, db *sql.DB, name string) (id int64, canonical string, ok bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, "", false
	}
	err := db.QueryRowContext(ctx, `
		SELECT id, name FROM (
			SELECT firefly_id AS id, name, 0 AS rank FROM firefly_categories WHERE LOWER(name) = LOWER(?)
			UNION ALL
			SELECT category_id, category_name, 1 FROM firefly_txns
			WHERE LOWER(category_name) = LOWER(?) AND category_id IS NOT NULL
		) ORDER BY rank LIMIT 1`, name, name).Scan(&id, &canonical)
	if err != nil {
		return 0, "", false
	}
	return id, canonical, true
}

// CreateCategory makes a category in Firefly and puts it in fold's copy of
// Firefly's categories straight away, so it can be picked (and shows on a
// card) before the next sync. A name fold already knows, in any case, is not
// created again: that category is returned, with created false. Refused
// while fold is read-only.
func (p *Pusher) CreateCategory(ctx context.Context, name string) (id int64, canonical string, created bool, err error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len([]rune(name)) > 100 {
		return 0, "", false, ErrCategoryName
	}
	if id, canonical, ok := CategoryByName(ctx, p.db.DB, name); ok {
		return id, canonical, false, nil
	}
	if p.readOnly {
		return 0, "", false, PushReadOnlyError
	}
	if p.fc == nil {
		return 0, "", false, fmt.Errorf("firefly isn't set up on this fold")
	}
	id, err = p.fc.CreateCategory(ctx, name)
	if err != nil {
		return 0, "", false, err
	}
	if _, err := p.db.ExecContext(ctx, `
		INSERT INTO firefly_categories (firefly_id, name, last_synced_at) VALUES (?, ?, strftime('%Y-%m-%d %H:%M:%f', 'now'))
		ON CONFLICT(firefly_id) DO UPDATE SET name = excluded.name, last_synced_at = excluded.last_synced_at`, id, name); err != nil {
		return id, name, true, fmt.Errorf("created in firefly (id %d) but not recorded in fold: %w", id, err)
	}
	return id, name, true, nil
}
