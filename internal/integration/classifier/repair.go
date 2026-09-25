package classifier

// repair.go — putting the user's own account back where fold says it is, on
// rows classified before the classifier knew how.
//
// fold's account_id is ground truth for a row's own side: the card that paid
// (OUTGOING), the account the money came into (INCOMING). The classifier
// resolves it that way whenever it classifies a row — but it only classifies
// a row once, and rows classified before it knew how were left behind in two
// shapes:
//
//   - no own account at all: money in whose receiving account no tier
//     proposed, from before the receiving-account rule existed. The card
//     asks "Which account?" of something fold already knows.
//   - the wrong way round: Tier 1 once booked money in like a spend — out of
//     the receiving account, to the person who paid — and a later title edit
//     saved those sides as they stood. (Where the payer was lost on the way,
//     fold's account is moved across and the card asks who paid.)
//
// RepairOwnAccounts fixes exactly those two, from fold's account and nothing
// else. A row whose own side is already one of the user's accounts is never
// moved to another, whoever put it there: that may be a person's choice.
// Idempotent — a repaired row no longer matches either shape — and cheap, so
// it runs every sync cycle.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// RepairReport counts what RepairOwnAccounts changed.
type RepairReport struct {
	// Filled: rows with no own account that now have fold's.
	Filled int
	// Swapped: rows saved the wrong way round, now the right way.
	Swapped int
	// Changes says what changed on each row, in words (for the log and the
	// audit trail).
	Changes []RepairChange
}

// RepairChange is one row RepairOwnAccounts changed (or, on a dry run,
// would change).
type RepairChange struct {
	FoldUUID string
	Kind     string // "filled" | "swapped"
	Note     string
}

// repairSide is one side of a staged row as stored: a confirmed value (a
// person's, or a save that kept the proposal) wins over the proposed one,
// id or name, as a unit — the way the review UI and push read it.
type repairSide struct {
	col                string // "source" | "destination"
	confID             sql.NullInt64
	confName, propName string
	propID             sql.NullInt64
}

func (s repairSide) confirmed() bool {
	return s.confID.Valid || strings.TrimSpace(s.confName) != ""
}

// effective is the side's value as push would send it.
func (s repairSide) effective() (id int64, name string) {
	switch {
	case s.confID.Valid:
		return s.confID.Int64, ""
	case strings.TrimSpace(s.confName) != "":
		return 0, strings.TrimSpace(s.confName)
	case s.propID.Valid:
		return s.propID.Int64, ""
	}
	return 0, strings.TrimSpace(s.propName)
}

func (s repairSide) empty() bool {
	id, name := s.effective()
	return id == 0 && name == ""
}

// RepairOwnAccounts repairs the own side of every row still waiting for
// review (see the file comment). With dryRun it only reports.
func (c *Classifier) RepairOwnAccounts(ctx context.Context, dryRun bool) (RepairReport, error) {
	var rep RepairReport
	assets, err := listFireflyAssetsFromMirror(ctx, c.db)
	if err != nil || len(assets) == 0 {
		return rep, err // no accounts mirror yet: nothing to repair against
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT fold_uuid, type, status, raw_payload, COALESCE(proposed_txn_type, ''),
		       confirmed_source_account_id, COALESCE(confirmed_source_account_name, ''),
		       proposed_source_account_id, COALESCE(proposed_source_account_name, ''),
		       confirmed_destination_account_id, COALESCE(confirmed_destination_account_name, ''),
		       proposed_destination_account_id, COALESCE(proposed_destination_account_name, '')
		FROM staged_fold_txns
		WHERE status IN ('needs_review', 'ready_to_push') AND mode <> 'MANUAL'
		  AND type IN ('INCOMING', 'OUTGOING')
		ORDER BY txn_timestamp`)
	if err != nil {
		return rep, fmt.Errorf("repair own accounts: %w", err)
	}
	type staged struct {
		uuid, typ, status, raw, propType string
		src, dst                         repairSide
	}
	var list []staged
	for rows.Next() {
		s := staged{src: repairSide{col: "source"}, dst: repairSide{col: "destination"}}
		if err := rows.Scan(&s.uuid, &s.typ, &s.status, &s.raw, &s.propType,
			&s.src.confID, &s.src.confName, &s.src.propID, &s.src.propName,
			&s.dst.confID, &s.dst.confName, &s.dst.propID, &s.dst.propName); err != nil {
			rows.Close()
			return rep, err
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	for _, s := range list {
		fa, _ := lookupFoldAccountForStaged(ctx, c.db, s.raw)
		if fa == nil || fa.Name == "" {
			continue
		}
		foldID, foldName, ok := matchFoldCardToFireflyAsset(fa, assets)
		if !ok {
			continue // no unique match: a person has to say, as on any new row
		}
		incoming := s.typ == "INCOMING"
		own, other := s.src, s.dst
		if incoming {
			own, other = s.dst, s.src
		}

		var ch RepairChange
		var sets []string
		var args []any
		setSide := func(side repairSide, id int64, name string) {
			level := "proposed"
			if side.confirmed() {
				level = "confirmed" // keep the side where it is stored
			}
			sets = append(sets, level+"_"+side.col+"_account_id = ?", level+"_"+side.col+"_account_name = ?")
			args = append(args, nullID(id), name)
		}
		// clearSide empties a side at both levels: the proposal underneath a
		// cleared confirmation would otherwise come back
		clearSide := func(side repairSide) {
			sets = append(sets, "confirmed_"+side.col+"_account_id = NULL", "confirmed_"+side.col+"_account_name = NULL",
				"proposed_"+side.col+"_account_id = NULL", "proposed_"+side.col+"_account_name = NULL")
		}
		switch {
		case own.empty() && c.sideIs(ctx, other, foldID, foldName):
			// fold's account on the wrong side, and nobody on the right one:
			// money can't come from the account it came into
			setSide(own, foldID, foldName)
			clearSide(other)
			if s.status == "ready_to_push" {
				sets = append(sets, "status = 'needs_review'")
			}
			ch = RepairChange{FoldUUID: s.uuid, Kind: "swapped",
				Note: fmt.Sprintf("%s was on the wrong side; moved across, and the other side left for review", foldName)}

		case own.empty():
			setSide(own, foldID, foldName)
			ch = RepairChange{FoldUUID: s.uuid, Kind: "filled",
				Note: fmt.Sprintf("%s account set to %s, the account fold has it on (%s)", ownWord(incoming), foldName, fa.Name)}

		case !c.isOwnSide(ctx, own) && c.sideIs(ctx, other, foldID, foldName):
			// the wrong way round: fold's account on the other side, someone
			// else in its place
			who := c.sideName(ctx, own)
			if who == "" {
				continue
			}
			kind := "expense"
			if incoming {
				kind = "revenue" // a deposit comes from someone who pays you
			}
			whoID := c.accountOfKind(ctx, kind, who)
			setSide(own, foldID, foldName)
			setSide(other, whoID, who)
			if want := defaultTypeFor(s.typ); s.propType != "" && s.propType != want {
				sets = append(sets, "proposed_txn_type = ?")
				args = append(args, want)
			}
			if s.status == "ready_to_push" {
				// sent as it was, it would have been booked backwards: a
				// person looks at it before it goes
				sets = append(sets, "status = 'needs_review'")
			}
			from, into := who, foldName
			if !incoming {
				from, into = foldName, who
			}
			ch = RepairChange{FoldUUID: s.uuid, Kind: "swapped",
				Note: fmt.Sprintf("saved the wrong way round; now from %s into %s", from, into)}

		default:
			continue
		}

		ch.Note += fmt.Sprintf(" (was: source %s, destination %s)", c.describeSide(ctx, s.src), c.describeSide(ctx, s.dst))
		rep.Changes = append(rep.Changes, ch)
		if ch.Kind == "filled" {
			rep.Filled++
		} else {
			rep.Swapped++
		}
		if dryRun {
			continue
		}
		args = append(args, s.uuid)
		if _, err := c.db.ExecContext(ctx, `UPDATE staged_fold_txns SET `+strings.Join(sets, ", ")+
			`, updated_at = CURRENT_TIMESTAMP WHERE fold_uuid = ?`, args...); err != nil {
			return rep, fmt.Errorf("repair %s: %w", s.uuid, err)
		}
		if _, err := c.db.ExecContext(ctx,
			`INSERT INTO audit_log (actor, action, fold_uuid, notes) VALUES ('system', 'repair_own_account', ?, ?)`,
			s.uuid, ch.Kind+": "+ch.Note); err != nil {
			c.log.Warn("repair audit insert failed", "fold_uuid", s.uuid, "err", err)
		}
	}
	if !dryRun && len(rep.Changes) > 0 {
		c.log.Info("repaired own accounts", "filled", rep.Filled, "swapped", rep.Swapped)
	}
	return rep, nil
}

func ownWord(incoming bool) string {
	if incoming {
		return "receiving"
	}
	return "paying"
}

func defaultTypeFor(foldType string) string {
	if foldType == "INCOMING" {
		return "deposit"
	}
	return "withdrawal"
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// isOwnSide: the side is one of the user's active asset accounts — by id, by
// the name of a same-named twin (a payee called after a card), or by name.
func (c *Classifier) isOwnSide(ctx context.Context, s repairSide) bool {
	id, name := s.effective()
	if id != 0 {
		var typ, fname sql.NullString
		var active sql.NullInt64
		if err := c.db.QueryRowContext(ctx, `SELECT type, name, active FROM firefly_accounts WHERE firefly_id = ?`, id).
			Scan(&typ, &fname, &active); err != nil {
			return true // an account the mirror hasn't seen: not ours to judge
		}
		if typ.String == "asset" && active.Int64 == 1 {
			return true
		}
		name = fname.String
	}
	return c.assetByName(ctx, name) != 0
}

// sideIs: the side is this asset — its id, or a same-named twin, or its name.
func (c *Classifier) sideIs(ctx context.Context, s repairSide, assetID int64, assetName string) bool {
	id, name := s.effective()
	if id != 0 {
		if id == assetID {
			return true
		}
		name = c.accountName(ctx, id)
	}
	return name != "" && strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(assetName))
}

// describeSide is a side as stored, for the audit trail: "Harbour Bank (#10,
// confirmed)", "A Friend (by name, proposed)", "nothing".
func (c *Classifier) describeSide(ctx context.Context, s repairSide) string {
	id, name := s.effective()
	level := "proposed"
	if s.confirmed() {
		level = "confirmed"
	}
	switch {
	case id != 0:
		return fmt.Sprintf("%s (#%d, %s)", c.accountName(ctx, id), id, level)
	case name != "":
		return fmt.Sprintf("%s (by name, %s)", name, level)
	}
	return "nothing"
}

// sideName is the side's name as Firefly spells it.
func (c *Classifier) sideName(ctx context.Context, s repairSide) string {
	id, name := s.effective()
	if id != 0 {
		return c.accountName(ctx, id)
	}
	return name
}

func (c *Classifier) accountName(ctx context.Context, id int64) string {
	var name sql.NullString
	_ = c.db.QueryRowContext(ctx, `SELECT name FROM firefly_accounts WHERE firefly_id = ?`, id).Scan(&name)
	if name.String == "" {
		_ = c.db.QueryRowContext(ctx, `SELECT COALESCE(
			(SELECT source_account_name FROM firefly_txns WHERE source_account_id = ? AND source_account_name <> '' LIMIT 1),
			(SELECT destination_account_name FROM firefly_txns WHERE destination_account_id = ? AND destination_account_name <> '' LIMIT 1))`,
			id, id).Scan(&name)
	}
	if strings.TrimSpace(name.String) == "(no name)" {
		return ""
	}
	return strings.TrimSpace(name.String)
}

func (c *Classifier) assetByName(ctx context.Context, name string) int64 {
	if strings.TrimSpace(name) == "" {
		return 0
	}
	var id sql.NullInt64
	_ = c.db.QueryRowContext(ctx, `SELECT firefly_id FROM firefly_accounts
		WHERE LOWER(name) = LOWER(?) AND type = 'asset' AND active = 1 ORDER BY firefly_id LIMIT 1`, strings.TrimSpace(name)).Scan(&id)
	return id.Int64
}

// accountOfKind is an existing account of one kind ("revenue": someone who
// pays you; "expense": someone you pay) by name, 0 when there is none — then
// push names it and Firefly creates it.
func (c *Classifier) accountOfKind(ctx context.Context, kind, name string) int64 {
	var id sql.NullInt64
	_ = c.db.QueryRowContext(ctx, `SELECT firefly_id FROM firefly_accounts
		WHERE LOWER(name) = LOWER(?) AND type = ? AND active = 1 ORDER BY firefly_id LIMIT 1`, name, kind).Scan(&id)
	return id.Int64
}
