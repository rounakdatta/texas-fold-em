package classifier

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
)

// A small book for the repair: two of the user's accounts that fold knows
// (a bank account and a card), a fold account Firefly has no asset for, and
// the people and shops on the other side. Names are made up.
func seedRepairDB(t *testing.T) *sql.DB {
	t.Helper()
	db := seedTestDB(t)
	for _, q := range []string{
		`INSERT INTO fold_accounts (fold_account_id, kind, name, provider, network, last_four, raw_payload, is_closed) VALUES
		   ('tern-acc','BANK','Tern Bank ****4410','Tern','','4410','{}',0),
		   ('kes-acc','CREDIT_CARD','Kestrel ****7781','Kestrel','Visa','7781','{}',0),
		   ('ghost-acc','BANK','Nowhere Bank ****0000','Nowhere','','0000','{}',0)`,
		`INSERT INTO firefly_accounts (firefly_id, name, type, account_role, account_number, active, raw_payload) VALUES
		   (10,'Tern Bank','asset','defaultAsset','99990004410',1,'{}'),
		   (20,'Kestrel Credit Card','asset','ccAsset','5555007781',1,'{}'),
		   (30,'Corner Bakery','expense',NULL,NULL,1,'{}'),
		   (40,'A Friend','expense',NULL,NULL,1,'{}'),
		   (41,'Payroll Co','revenue',NULL,NULL,1,'{}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed repair: %v", err)
		}
	}
	return db
}

// stageSides stages a row as the classifier (or a save) left it. Each side
// is "" (nothing), "#<id>" (an account) or a plain name, prefixed with "!"
// when it is stored as confirmed.
func stageSides(t *testing.T, db *sql.DB, uuid, typ, account, status, mode, src, dst string) {
	t.Helper()
	cols := map[string]any{}
	put := func(side, v string) {
		level := "proposed"
		if strings.HasPrefix(v, "!") {
			level, v = "confirmed", v[1:]
		}
		switch {
		case v == "":
		case strings.HasPrefix(v, "#"):
			var id int64
			for _, ch := range v[1:] {
				id = id*10 + int64(ch-'0')
			}
			cols[level+"_"+side+"_account_id"] = id
		default:
			cols[level+"_"+side+"_account_name"] = v
		}
	}
	put("source", src)
	put("destination", dst)
	names := []string{"fold_uuid", "raw_payload", "amount_paise", "currency", "txn_timestamp", "mode", "type", "narration", "merchant_extracted", "status"}
	vals := []any{uuid, `{"account_id":"` + account + `"}`, 50000, "INR", "2026-03-01T10:00:00Z", mode, typ, "UPI-SOMEONE-x@bank-REF-UPI", "someone", status}
	for k, v := range cols {
		names = append(names, k)
		vals = append(vals, v)
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")
	if _, err := db.Exec(`INSERT INTO staged_fold_txns (`+strings.Join(names, ",")+`) VALUES (`+ph+`)`, vals...); err != nil {
		t.Fatalf("stage %s: %v", uuid, err)
	}
}

type sidesRow struct {
	status, propType             string
	cSrcID, pSrcID, cDstID, pDst sql.NullInt64
	cSrcName, pSrcName, cDstName sql.NullString
}

func readSides(t *testing.T, db *sql.DB, uuid string) sidesRow {
	t.Helper()
	var r sidesRow
	var pt sql.NullString
	if err := db.QueryRow(`SELECT status, proposed_txn_type,
		confirmed_source_account_id, confirmed_source_account_name, proposed_source_account_id, proposed_source_account_name,
		confirmed_destination_account_id, confirmed_destination_account_name, proposed_destination_account_id
		FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&r.status, &pt,
		&r.cSrcID, &r.cSrcName, &r.pSrcID, &r.pSrcName, &r.cDstID, &r.cDstName, &r.pDst); err != nil {
		t.Fatalf("read %s: %v", uuid, err)
	}
	r.propType = pt.String
	return r
}

func repair(t *testing.T, db *sql.DB, dry bool) RepairReport {
	t.Helper()
	rep, err := New(db, slog.Default(), DefaultConfidenceThreshold, 10).RepairOwnAccounts(context.Background(), dry)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	return rep
}

// The deposit the review deck asked "Which account?" about: fold knows the
// account the money came into, so it is filled in — and nothing else moves.
func TestRepair_MoneyInWithNoAccountGetsFoldsAccount(t *testing.T) {
	db := seedRepairDB(t)
	stageSides(t, db, "in-1", "INCOMING", "tern-acc", "needs_review", "UPI", "A Friend", "")
	stageSides(t, db, "in-2", "INCOMING", "tern-acc", "ready_to_push", "UPI", "#41", "")
	rep := repair(t, db, false)
	if rep.Filled != 2 || rep.Swapped != 0 {
		t.Fatalf("report = %+v, want 2 filled", rep)
	}
	r := readSides(t, db, "in-1")
	if !r.pDst.Valid || r.pDst.Int64 != 10 || r.cDstID.Valid {
		t.Errorf("in-1 destination = proposed %v / confirmed %v, want proposed Tern Bank (10)", r.pDst, r.cDstID)
	}
	if r.pSrcName.String != "A Friend" || r.status != "needs_review" {
		t.Errorf("in-1 payer/status changed: %+v", r)
	}
	if r := readSides(t, db, "in-2"); r.pDst.Int64 != 10 || r.pSrcID.Int64 != 41 || r.status != "ready_to_push" {
		t.Errorf("in-2 = %+v, want Tern Bank filled in and the rest as it was", r)
	}
}

// Money in, saved the way Tier 1 once booked it — out of the receiving
// account, to the person who paid — and confirmed by a later title edit. It
// is swapped where it is stored (confirmed stays confirmed), the person
// becomes the payer by name (they have no payer account yet), and it goes
// back for a look before it can be sent.
func TestRepair_MoneyInSavedBackwardsIsSwapped(t *testing.T) {
	db := seedRepairDB(t)
	stageSides(t, db, "back-1", "INCOMING", "tern-acc", "ready_to_push", "UPI", "!#10", "!#40")
	rep := repair(t, db, false)
	if rep.Swapped != 1 || rep.Filled != 0 {
		t.Fatalf("report = %+v, want 1 swapped", rep)
	}
	r := readSides(t, db, "back-1")
	if r.cDstID.Int64 != 10 || r.cDstName.String != "Tern Bank" {
		t.Errorf("destination = %v %q, want Tern Bank (10), confirmed", r.cDstID, r.cDstName.String)
	}
	if r.cSrcID.Valid || r.cSrcName.String != "A Friend" {
		t.Errorf("source = %v %q, want the payer A Friend by name", r.cSrcID, r.cSrcName.String)
	}
	if r.status != "needs_review" {
		t.Errorf("status = %s, want needs_review", r.status)
	}
	var note string
	_ = db.QueryRow(`SELECT notes FROM audit_log WHERE action = 'repair_own_account' AND fold_uuid = 'back-1'`).Scan(&note)
	if !strings.Contains(note, "was: source Tern Bank (#10, confirmed), destination A Friend (#40, confirmed)") {
		t.Errorf("audit note %q doesn't say what it was", note)
	}
}

// A payer that already has an account keeps it (by id), and a guess of
// "withdrawal" on money in becomes the deposit it is.
func TestRepair_BackwardsPayerKeepsItsAccount(t *testing.T) {
	db := seedRepairDB(t)
	stageSides(t, db, "back-2", "INCOMING", "tern-acc", "needs_review", "ACH", "#10", "#41")
	if _, err := db.Exec(`UPDATE staged_fold_txns SET proposed_txn_type = 'withdrawal' WHERE fold_uuid = 'back-2'`); err != nil {
		t.Fatal(err)
	}
	repair(t, db, false)
	r := readSides(t, db, "back-2")
	if r.pDst.Int64 != 10 || r.pSrcID.Int64 != 41 || r.propType != "deposit" {
		t.Errorf("got %+v, want from Payroll Co (41) into Tern Bank (10), a deposit", r)
	}
}

// fold's account on the payer's side and nobody on the other: the account
// moves across, and the payer is left for the card to ask — money can't come
// from the account it came into.
func TestRepair_AccountOnTheWrongSideWithNoPayer(t *testing.T) {
	db := seedRepairDB(t)
	stageSides(t, db, "lost-1", "INCOMING", "tern-acc", "ready_to_push", "UPI", "#10", "")
	rep := repair(t, db, false)
	if rep.Swapped != 1 {
		t.Fatalf("report = %+v, want 1 swapped", rep)
	}
	r := readSides(t, db, "lost-1")
	if r.pDst.Int64 != 10 || r.pSrcID.Valid || r.pSrcName.Valid || r.cSrcID.Valid || r.status != "needs_review" {
		t.Errorf("got %+v, want Tern Bank as the destination, no source, needs_review", r)
	}
}

// Spends: the paying card is filled in, or swapped back, the same way.
func TestRepair_SpendsToo(t *testing.T) {
	db := seedRepairDB(t)
	stageSides(t, db, "out-1", "OUTGOING", "kes-acc", "needs_review", "CARD", "", "#30")
	stageSides(t, db, "out-2", "OUTGOING", "kes-acc", "needs_review", "CARD", "#30", "#20")
	rep := repair(t, db, false)
	if rep.Filled != 1 || rep.Swapped != 1 {
		t.Fatalf("report = %+v, want 1 filled and 1 swapped", rep)
	}
	if r := readSides(t, db, "out-1"); r.pSrcID.Int64 != 20 {
		t.Errorf("out-1 source = %v, want Kestrel Credit Card (20)", r.pSrcID)
	}
	if r := readSides(t, db, "out-2"); r.pSrcID.Int64 != 20 || r.pDst.Int64 != 30 {
		t.Errorf("out-2 = %+v, want from Kestrel (20) to Corner Bakery (30)", r)
	}
}

// What a person could have meant, what fold can't place, and what is no
// longer waiting are all left exactly as they are.
func TestRepair_LeavesAloneWhatItCantBeSureOf(t *testing.T) {
	db := seedRepairDB(t)
	// another of the user's accounts as the receiving one: maybe a person's choice
	stageSides(t, db, "keep-own", "INCOMING", "tern-acc", "needs_review", "UPI", "A Friend", "!#20")
	// fold's account has no asset in Firefly: nothing to fill in
	stageSides(t, db, "keep-ghost", "INCOMING", "ghost-acc", "needs_review", "UPI", "A Friend", "")
	// added by hand from a statement
	stageSides(t, db, "keep-manual", "INCOMING", "tern-acc", "needs_review", "MANUAL", "A Friend", "")
	// already in Firefly
	stageSides(t, db, "keep-pushed", "INCOMING", "tern-acc", "pushed", "UPI", "A Friend", "")
	// an account the mirror hasn't seen yet is not judged
	stageSides(t, db, "keep-unknown", "INCOMING", "tern-acc", "needs_review", "UPI", "#10", "#999")
	before := map[string]sidesRow{}
	for _, u := range []string{"keep-own", "keep-ghost", "keep-manual", "keep-pushed", "keep-unknown"} {
		before[u] = readSides(t, db, u)
	}
	if rep := repair(t, db, false); len(rep.Changes) != 0 {
		t.Fatalf("changed %+v, want nothing", rep.Changes)
	}
	for u, b := range before {
		if a := readSides(t, db, u); a != b {
			t.Errorf("%s changed: %+v → %+v", u, b, a)
		}
	}
}

// A dry run says what it would do and does none of it; a second real run
// finds nothing left.
func TestRepair_DryRunAndIdempotent(t *testing.T) {
	db := seedRepairDB(t)
	stageSides(t, db, "in-1", "INCOMING", "tern-acc", "needs_review", "UPI", "A Friend", "")
	stageSides(t, db, "back-1", "INCOMING", "tern-acc", "ready_to_push", "UPI", "!#10", "!#40")
	b1, b2 := readSides(t, db, "in-1"), readSides(t, db, "back-1")
	if rep := repair(t, db, true); rep.Filled != 1 || rep.Swapped != 1 {
		t.Fatalf("dry run = %+v, want 1 filled, 1 swapped", rep)
	}
	if readSides(t, db, "in-1") != b1 || readSides(t, db, "back-1") != b2 {
		t.Fatal("a dry run wrote to the database")
	}
	var audits int
	_ = db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'repair_own_account'`).Scan(&audits)
	if audits != 0 {
		t.Fatalf("a dry run wrote %d audit rows", audits)
	}
	repair(t, db, false)
	if rep := repair(t, db, false); len(rep.Changes) != 0 {
		t.Errorf("second run changed %+v, want nothing", rep.Changes)
	}
}
