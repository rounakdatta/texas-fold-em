// Package refundref is the stored form of "which purchase does this refund
// give money back for" — shared by the classifier (which proposes it), the
// review UI (which lets the human change it) and the Pusher (which turns it
// into firefly's native "Refund" transaction link). It lives on its own so
// none of those packages has to import another just for the format.
//
// A reference is one of:
//
//	fold:<fold_uuid>      the purchase is a staged row (pending or pushed)
//	journal:<journal_id>  the purchase is a firefly journal fold never staged
//	none                  the human says there is no purchase to link
package refundref

import (
	"strconv"
	"strings"
)

// Kinds returned by Parse.
const (
	KindFold    = "fold"
	KindJournal = "journal"
	KindNone    = "none"
)

// None is the stored value for "no purchase to link".
const None = "none"

const (
	foldPrefix    = "fold:"
	journalPrefix = "journal:"
)

// Fold references a staged fold row.
func Fold(foldUUID string) string { return foldPrefix + foldUUID }

// Journal references a firefly transaction journal.
func Journal(journalID int64) string { return journalPrefix + strconv.FormatInt(journalID, 10) }

// Parse splits a stored reference. kind is KindFold, KindJournal, KindNone,
// or "" for empty/unrecognised input.
func Parse(s string) (kind, foldUUID string, journalID int64) {
	s = strings.TrimSpace(s)
	switch {
	case s == None:
		return KindNone, "", 0
	case strings.HasPrefix(s, foldPrefix) && len(s) > len(foldPrefix):
		return KindFold, strings.TrimPrefix(s, foldPrefix), 0
	case strings.HasPrefix(s, journalPrefix):
		if id, err := strconv.ParseInt(strings.TrimPrefix(s, journalPrefix), 10, 64); err == nil && id > 0 {
			return KindJournal, "", id
		}
	}
	return "", "", 0
}

// Valid reports whether s is a well-formed reference (including None).
func Valid(s string) bool {
	k, _, _ := Parse(s)
	return k != ""
}
