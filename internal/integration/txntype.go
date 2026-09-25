package integration

// txntype.go — which of Firefly's three kinds a staged row is, and the rules
// that tie each kind to the row's two sides.
//
// A row arrives from the bank knowing one thing for certain: money LEFT one of
// the user's accounts (OUTGOING) or ARRIVED in one (INCOMING). Firefly needs
// one thing more — who is on the other side:
//
//	money left an account → a withdrawal (paid to someone: an expense account)
//	                        or a transfer (moved to another of your asset accounts)
//	money arrived         → a deposit (paid by someone: a revenue account)
//	                        or a transfer (moved from another of your asset accounts)
//
// A transfer is between two of the user's own asset accounts and nothing else,
// and a withdrawal or deposit never has one of them on the other side. These
// are Firefly's own rules; checking them here, before anything is sent, is
// what stops a bank's ₹1 verification credit from being booked as money
// moved out of the user's own account at that bank.
//
// CheckType answers the same way for everyone who asks — the review card, the
// editor, the send check and push — so the type a card shows is the type push
// sends. A person's choice (confirmed_txn_type) is final: push never turns it
// into another type behind their back. Only a row nobody has decided is
// inferred, and there both sides being the user's own accounts makes it a
// transfer whatever the classifier guessed (a card bill paid from the bank).

import (
	"context"
	"database/sql"
	"strings"
)

// Firefly's transaction types, as its API spells them.
const (
	TypeWithdrawal = "withdrawal"
	TypeDeposit    = "deposit"
	TypeTransfer   = "transfer"
)

// TypesFor lists the types a bank direction allows, the everyday one first:
// withdrawal or transfer for money that left, deposit or transfer for money
// that arrived.
func TypesFor(direction string) []string {
	if isIncoming(direction) {
		return []string{TypeDeposit, TypeTransfer}
	}
	return []string{TypeWithdrawal, TypeTransfer}
}

// TypeAllowed reports whether t is one of the types direction allows.
func TypeAllowed(direction, t string) bool {
	for _, x := range TypesFor(direction) {
		if x == t {
			return true
		}
	}
	return false
}

// DefaultType is the type a direction has when nothing says otherwise.
func DefaultType(direction string) string { return TypesFor(direction)[0] }

func isIncoming(direction string) bool { return strings.EqualFold(direction, "INCOMING") }

func validType(t string) bool { return t == TypeWithdrawal || t == TypeDeposit || t == TypeTransfer }

// What can be wrong between a row's type and its sides. Stable strings: the
// review UI keys its explanations on them.
const (
	ProblemNone = ""
	// the type can't carry the bank's direction: a deposit of money that left
	ProblemDirection = "direction"
	// a withdrawal or deposit whose other side is one of the user's own accounts
	ProblemOtherIsMine = "other-is-mine"
	// a transfer whose other side isn't one of the user's accounts
	ProblemOtherNotMine = "other-not-mine"
	// a transfer from an account to itself
	ProblemSameAccount = "same-account"
	// a withdrawal to a payer's (revenue) account, a deposit from a payee's
	// (expense) account: Firefly refuses both
	ProblemOtherKind = "other-kind"
	// the row's own side names something that isn't one of the user's accounts
	ProblemOwnNotMine = "own-not-mine"
	// the two sides were saved the wrong way round: the user's own account
	// on the other side, someone else in its place ("from Harbour Bank into
	// A Friend" for money that came from a friend into Harbour Bank)
	ProblemSwapped = "swapped"
)

// Side is one end of a row as stored: an existing account's id, or a name only
// (an account push asks Firefly to find or create by that name).
type Side struct {
	ID   int64
	Name string
}

// Empty reports whether the side names nothing at all.
func (s Side) Empty() bool { return s.ID == 0 && strings.TrimSpace(s.Name) == "" }

// TypeCheck is a row's resolved type and what, if anything, is wrong with it.
type TypeCheck struct {
	Type string // withdrawal | deposit | transfer
	// Explicit: a person chose the type. Push sends it as it is.
	Explicit bool
	// Problem is one of the Problem* codes; ProblemNone when the sides fit.
	// A side that is simply missing is not a problem here — the card asks
	// for it as a step of its own.
	Problem string
	// OwnAsset / OtherAsset: each side as one of the user's asset accounts —
	// itself when it is one, else the same-named asset of a duplicate account
	// (a payee that shares a card's name), else 0.
	OwnAsset, OtherAsset int64
}

// queryRower is what CheckType reads through: *sql.DB, *DB or a *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CheckType resolves a row's type from its bank direction ("INCOMING" |
// "OUTGOING"), the confirmed and proposed types, and its two sides as
// stored — src is where the money came from, dst where it went.
func CheckType(ctx context.Context, q queryRower, direction, confirmedType, proposedType string, src, dst Side) TypeCheck {
	own, other := src, dst
	if isIncoming(direction) {
		own, other = dst, src
	}
	tc := TypeCheck{}
	confirmedType, proposedType = strings.ToLower(strings.TrimSpace(confirmedType)), strings.ToLower(strings.TrimSpace(proposedType))
	switch {
	case validType(confirmedType):
		tc.Type, tc.Explicit = confirmedType, true
	case validType(proposedType) && TypeAllowed(direction, proposedType):
		tc.Type = proposedType
	default:
		tc.Type = DefaultType(direction)
	}
	var ownKnown, otherKnown bool
	tc.OwnAsset, ownKnown = sideAsset(ctx, q, own)
	tc.OtherAsset, otherKnown = sideAsset(ctx, q, other)
	if !tc.Explicit && tc.OwnAsset != 0 && tc.OtherAsset != 0 {
		// both sides are the user's own accounts: a transfer, whatever the
		// classifier guessed (a card bill paid from the bank account)
		tc.Type = TypeTransfer
	}

	// Only what the accounts mirror can vouch for is a problem: an account
	// firefly has but the mirror hasn't caught up with is left to firefly.
	switch {
	case !TypeAllowed(direction, tc.Type):
		tc.Problem = ProblemDirection
	case tc.Type != TypeTransfer && !own.Empty() && ownKnown && tc.OwnAsset == 0 && tc.OtherAsset != 0:
		tc.Problem = ProblemSwapped
	case own.ID != 0 && ownKnown && tc.OwnAsset == 0:
		tc.Problem = ProblemOwnNotMine
	case other.Empty():
		// nothing on the other side yet: the card asks for it
	case tc.Type == TypeTransfer && otherKnown && tc.OtherAsset == 0:
		tc.Problem = ProblemOtherNotMine
	case tc.Type == TypeTransfer && tc.OtherAsset != 0 && tc.OtherAsset == tc.OwnAsset:
		tc.Problem = ProblemSameAccount
	case tc.Type != TypeTransfer && tc.OtherAsset != 0:
		tc.Problem = ProblemOtherIsMine
	case tc.Type != TypeTransfer && other.ID != 0:
		kind := accountKind(ctx, q, other.ID)
		if (tc.Type == TypeWithdrawal && kind == "revenue") || (tc.Type == TypeDeposit && kind == "expense") {
			tc.Problem = ProblemOtherKind
		}
	}
	return tc
}

// accountKind is the mirror's type for an account id: "asset", "expense",
// "revenue", … — "" when the mirror doesn't know the id.
func accountKind(ctx context.Context, q queryRower, id int64) string {
	if q == nil || id == 0 {
		return ""
	}
	var kind sql.NullString
	_ = q.QueryRowContext(ctx, `SELECT type FROM firefly_accounts WHERE firefly_id = ?`, id).Scan(&kind)
	return kind.String
}

// AssetByName is the user's active asset account with this name (any case),
// 0 when there is none.
func AssetByName(ctx context.Context, q queryRower, name string) int64 {
	name = strings.TrimSpace(name)
	if q == nil || name == "" {
		return 0
	}
	var id sql.NullInt64
	_ = q.QueryRowContext(ctx,
		`SELECT firefly_id FROM firefly_accounts WHERE LOWER(name) = LOWER(?) AND type = 'asset' AND active = 1 ORDER BY firefly_id LIMIT 1`,
		name).Scan(&id)
	return id.Int64
}

// sideAsset returns a side as one of the user's asset accounts: the id itself
// when it is an asset; the same-named asset when the id is a duplicate
// account that shares its name (Firefly lets an expense account be called
// "Kestrel Credit Card" too); the asset a name-only side is named
// after; else 0. known reports whether the mirror could tell — false for an
// id it has never seen, which is then not judged.
func sideAsset(ctx context.Context, q queryRower, s Side) (asset int64, known bool) {
	if q == nil {
		return 0, false
	}
	if s.ID != 0 {
		var name, kind sql.NullString
		var active sql.NullInt64
		err := q.QueryRowContext(ctx, `SELECT name, type, active FROM firefly_accounts WHERE firefly_id = ?`, s.ID).Scan(&name, &kind, &active)
		if err == nil {
			if kind.String == "asset" && active.Int64 == 1 {
				return s.ID, true
			}
			return AssetByName(ctx, q, name.String), true
		}
		// an id the mirror hasn't seen: its stored name may still say
		if a := AssetByName(ctx, q, s.Name); a != 0 {
			return a, true
		}
		return 0, false
	}
	return AssetByName(ctx, q, s.Name), true
}
