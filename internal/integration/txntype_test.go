package integration

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A small book of accounts, the shapes the rules care about: two of the
// user's own accounts (a bank account and a card), a card that also exists
// as a same-named payee (Firefly allows it), a merchant, a payer, and a card
// that has been closed. Names are made up.
func typeTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES
		(10, 'Harbour Bank',                'asset',   1, '{}'),
		(20, 'Kestrel Credit Card',         'asset',   1, '{}'),
		(21, 'Kestrel Credit Card',         'expense', 1, '{}'),
		(30, 'Corner Bakery',               'expense', 1, '{}'),
		(40, 'Payroll Co',                  'revenue', 1, '{}'),
		(50, 'Old Card',                    'asset',   0, '{}')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

func TestCheckType(t *testing.T) {
	db := typeTestDB(t)
	id := func(n int64) Side { return Side{ID: n} }
	name := func(s string) Side { return Side{Name: s} }
	none := Side{}
	cases := []struct {
		test                 string
		dir, confirmed, prop string
		src, dst             Side
		want, problem        string
		explicit             bool
	}{
		// the everyday cases
		{test: "a purchase is a withdrawal", dir: "OUTGOING", src: id(20), dst: id(30), want: TypeWithdrawal},
		{test: "a new merchant by name is a withdrawal", dir: "OUTGOING", src: id(20), dst: name("Night Market Stall"), want: TypeWithdrawal},
		{test: "salary is a deposit", dir: "INCOMING", src: id(40), dst: id(10), want: TypeDeposit},
		{test: "a new payer by name is a deposit", dir: "INCOMING", src: name("A Friend"), dst: id(10), want: TypeDeposit},
		{test: "nobody paid yet: no problem, the card asks", dir: "OUTGOING", src: id(20), dst: none, want: TypeWithdrawal},

		// inferring a transfer — only when nobody has decided
		{test: "bank to card, undecided, is a transfer", dir: "OUTGOING", prop: "withdrawal", src: id(10), dst: id(20), want: TypeTransfer},
		{test: "a card's payee twin still counts as the card", dir: "OUTGOING", src: id(10), dst: id(21), want: TypeTransfer},
		{test: "a payment arriving on the card from the bank is a transfer", dir: "INCOMING", src: name("Harbour Bank"), dst: id(20), want: TypeTransfer},

		// a person's choice is final, and checked
		{test: "a confirmed deposit from an own account is refused, not converted",
			dir: "INCOMING", confirmed: "deposit", src: name("Harbour Bank"), dst: id(20),
			want: TypeDeposit, explicit: true, problem: ProblemOtherIsMine},
		{test: "a confirmed withdrawal to an own account is refused, not converted",
			dir: "OUTGOING", confirmed: "withdrawal", src: id(10), dst: id(20),
			want: TypeWithdrawal, explicit: true, problem: ProblemOtherIsMine},
		{test: "a confirmed transfer to a merchant is refused",
			dir: "OUTGOING", confirmed: "transfer", src: id(10), dst: id(30),
			want: TypeTransfer, explicit: true, problem: ProblemOtherNotMine},
		{test: "a confirmed transfer to a name that isn't an account is refused",
			dir: "OUTGOING", confirmed: "transfer", src: id(10), dst: name("Someone"),
			want: TypeTransfer, explicit: true, problem: ProblemOtherNotMine},
		{test: "a transfer from an account to itself is refused",
			dir: "OUTGOING", confirmed: "transfer", src: id(10), dst: name("harbour bank"),
			want: TypeTransfer, explicit: true, problem: ProblemSameAccount},
		{test: "a confirmed transfer between two own accounts is fine",
			dir: "OUTGOING", confirmed: "transfer", src: id(10), dst: id(21),
			want: TypeTransfer, explicit: true},
		{test: "money that left can't be a deposit",
			dir: "OUTGOING", confirmed: "deposit", src: id(20), dst: id(30),
			want: TypeDeposit, explicit: true, problem: ProblemDirection},

		// what Firefly would refuse anyway
		{test: "a withdrawal to a payer's account", dir: "OUTGOING", src: id(20), dst: id(40),
			want: TypeWithdrawal, problem: ProblemOtherKind},
		{test: "a deposit from a payee's account", dir: "INCOMING", src: id(30), dst: id(10),
			want: TypeDeposit, problem: ProblemOtherKind},
		{test: "money in onto a merchant as its own side", dir: "INCOMING", src: id(40), dst: id(30),
			want: TypeDeposit, problem: ProblemOwnNotMine},
		{test: "a closed card is not a usable account", dir: "OUTGOING", src: id(50), dst: id(30),
			want: TypeWithdrawal, problem: ProblemOwnNotMine},

		// saved the wrong way round: the own account on the other side
		{test: "money in from a payer, saved backwards", dir: "INCOMING", src: id(10), dst: id(40),
			want: TypeDeposit, problem: ProblemSwapped},
		{test: "money in from a new payer, saved backwards", dir: "INCOMING", src: id(10), dst: name("A Friend"),
			want: TypeDeposit, problem: ProblemSwapped},
		{test: "a spend saved backwards", dir: "OUTGOING", src: id(30), dst: id(20),
			want: TypeWithdrawal, problem: ProblemSwapped},
		{test: "a transfer is never read as backwards", dir: "INCOMING", confirmed: "transfer", src: id(10), dst: id(40),
			want: TypeTransfer, explicit: true, problem: ProblemOwnNotMine},

		// a classifier guess that the direction can't carry falls back
		{test: "a proposed deposit on money that left is a withdrawal", dir: "OUTGOING", prop: "deposit", src: id(20), dst: id(30), want: TypeWithdrawal},
		// an account the mirror hasn't seen is not judged
		{test: "an unknown id is left to firefly", dir: "OUTGOING", confirmed: "transfer", src: id(10), dst: id(999),
			want: TypeTransfer, explicit: true},
	}
	for _, c := range cases {
		t.Run(c.test, func(t *testing.T) {
			got := CheckType(context.Background(), db, c.dir, c.confirmed, c.prop, c.src, c.dst)
			if got.Type != c.want || got.Problem != c.problem || got.Explicit != c.explicit {
				t.Errorf("got type=%q problem=%q explicit=%v, want type=%q problem=%q explicit=%v",
					got.Type, got.Problem, got.Explicit, c.want, c.problem, c.explicit)
			}
		})
	}
}

func TestTypesFor(t *testing.T) {
	if got := strings.Join(TypesFor("OUTGOING"), ","); got != "withdrawal,transfer" {
		t.Errorf("money out: %s", got)
	}
	if got := strings.Join(TypesFor("INCOMING"), ","); got != "deposit,transfer" {
		t.Errorf("money in: %s", got)
	}
	if TypeAllowed("INCOMING", TypeWithdrawal) || !TypeAllowed("INCOMING", TypeTransfer) {
		t.Error("TypeAllowed disagrees with TypesFor")
	}
}

// The ₹1 a bank sends to check an account: money INTO the bank account, from
// the bank's payout system, which a person named after the bank. The card
// used to show it as a deposit while push quietly made it a transfer out of
// the user's own account at that bank. A chosen deposit must go as a deposit
// from a payer — and one naming an own account must not go at all.
func TestPush_ConfirmedDepositIsNeverConvertedToATransfer(t *testing.T) {
	s := newPushTestSetup(t)
	if _, err := s.db.Exec(`INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES
		(10, 'Harbour Bank', 'asset', 1, '{}'), (60, 'Tern Bank', 'asset', 1, '{}')`); err != nil {
		t.Fatal(err)
	}
	p := NewPusher(s.db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	row := pushableRow{
		FoldUUID: "u-verify", AmountPaise: 100, Currency: "INR", Type: "INCOMING", TxnTimestamp: time.Now().UTC(),
		ConfirmedTxnType:              sql.NullString{String: "deposit", Valid: true},
		ConfirmedSourceAccountName:    sql.NullString{String: "Tern Bank", Valid: true}, // an own account's name
		ConfirmedDestinationAccountID: sql.NullInt64{Int64: 10, Valid: true},
		ConfirmedDescription:          sql.NullString{String: "Account verification credit", Valid: true},
	}
	if err := p.typeFits(context.Background(), row); err == nil || !strings.Contains(err.Error(), "own accounts") {
		t.Fatalf("a deposit from an own account must be refused before firefly, got %v", err)
	}

	row.ConfirmedSourceAccountName = sql.NullString{String: "Tern Small Finance Bank", Valid: true} // the payer
	if err := p.typeFits(context.Background(), row); err != nil {
		t.Fatalf("a deposit from a payer must be allowed: %v", err)
	}
	line := p.buildCreateRequest(context.Background(), row).Transactions[0]
	if line.Type != TypeDeposit || line.SourceName != "Tern Small Finance Bank" || line.SourceID != "" || line.DestinationID != "10" {
		t.Errorf("got %s from id=%q name=%q into %q; want a deposit from the named payer into 10",
			line.Type, line.SourceID, line.SourceName, line.DestinationID)
	}
}

// A withdrawal's own side goes by its asset id even when the classifier
// stored the card's same-named payee twin there.
func TestBuildCreateRequest_OwnSideGoesByItsAsset(t *testing.T) {
	s := newPushTestSetup(t)
	if _, err := s.db.Exec(`INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES
		(20, 'Kestrel Credit Card', 'asset', 1, '{}'), (21, 'Kestrel Credit Card', 'expense', 1, '{}'),
		(30, 'Corner Bakery', 'expense', 1, '{}')`); err != nil {
		t.Fatal(err)
	}
	p := NewPusher(s.db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	row := pushableRow{
		FoldUUID: "u-twin", AmountPaise: 25000, Currency: "INR", Type: "OUTGOING", TxnTimestamp: time.Now().UTC(),
		ProposedSourceAccountID:      sql.NullInt64{Int64: 21, Valid: true}, // the payee twin
		ProposedDestinationAccountID: sql.NullInt64{Int64: 30, Valid: true},
	}
	line := p.buildCreateRequest(context.Background(), row).Transactions[0]
	if line.Type != TypeWithdrawal || line.SourceID != "20" {
		t.Errorf("got %s from %q, want a withdrawal from the card's asset (20)", line.Type, line.SourceID)
	}
}
