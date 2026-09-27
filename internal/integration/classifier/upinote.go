package classifier

// upinote.go — the note on a UPI payment, in the owner's own words.
//
// Paying by UPI, the payer types a note ("CAB", "ROLL DINNER", "JUNE RENT
// SPLITWISE"), and the bank's narration carries it, cut to about eighteen
// characters, at the end:
//
//	UPI-<name>-<handle>-<IFSC>-<reference>-<note>      (HDFC)
//	UPI/P2A/<reference>/<name>/<note>/<bank>          (P2A, P2M)
//
// For money out the owner wrote it, minutes after paying: the best evidence
// there is of what a payment was for, and the specifics a title wants. The
// Rapido app fills in "RAPIDO" when a ride is paid to its driver. Apps fill in
// their own boilerplate too ("SENT USING PAYTM U", "PAY", "UPI"), and a
// merchant's order reference can sit in the same place; neither is a note.

import (
	"regexp"
	"strings"
)

var (
	upiDashRe  = regexp.MustCompile(`(?i)^UPI-.+-[A-Z]{4}0[A-Z0-9]{6}-\d{9,14}-(.*)$`)
	upiSlashRe = regexp.MustCompile(`(?i)^UPI/P2[AM]/\d{9,14}/[^/]*/([^/]*)/`)
	// an order or payment reference, not words: long, and mixed with digits
	upiRefRe = regexp.MustCompile(`^[A-Z0-9]{12,}$`)
)

// upiBoilerplate are notes apps write, not people.
var upiBoilerplate = map[string]bool{
	"": true, "UPI": true, "PAY": true, "NA": true, "NIL": true, "PAYMENT": true, "COLLECT": true,
	"SENT USING PAYTM U": true, "SENT USING PAYTM UPI": true, "PAYVIARAZORPAY": true, "PAID VIA CRED": true,
	"PAYMENT FROM PHONE": true, "PAYMENT FROM PH": true, "PAYMENT FROM PHON": true, "FULLPAYMENTFORORDE": true,
	"PAY TO MERCHANT": true, "UPI PAYMENT": true, "NO REMARKS": true, "YES BANK LIMITED YBS": true,
}

// upiNote returns the note the payer typed on a UPI payment, "" when the
// narration carries none worth reading.
func upiNote(narration string) string {
	n := strings.TrimSpace(narration)
	var note string
	if m := upiDashRe.FindStringSubmatch(n); m != nil {
		note = m[1]
	} else if m := upiSlashRe.FindStringSubmatch(n); m != nil {
		note = m[1]
	}
	note = strings.Join(strings.Fields(note), " ")
	up := strings.ToUpper(note)
	if upiBoilerplate[up] || len(note) < 3 {
		return ""
	}
	if upiRefRe.MatchString(up) && strings.ContainsAny(up, "0123456789") {
		return ""
	}
	digits := 0
	for _, r := range up {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits*2 > len(up) { // mostly digits: a reference
		return ""
	}
	return note
}
