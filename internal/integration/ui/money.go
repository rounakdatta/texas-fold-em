package ui

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// manualMode marks a staged row the operator added by hand from a statement
// line fold never received. The classifier skips these rows.
const manualMode = "MANUAL"

// possibleDuplicateSQL extracts fold.money's own is_possible_duplicate flag
// from a raw_payload column as 0/1. Guarded by json_valid so a legacy or
// hand-made payload can never fail the whole list query.
func possibleDuplicateSQL(col string) string {
	return "CASE WHEN json_valid(" + col + ") THEN COALESCE(json_extract(" + col + ", '$.is_possible_duplicate'), 0) ELSE 0 END"
}

// ist is the zone the review form's date/time inputs are expressed in. The
// operator's card statements (and fold's own narrations) are in IST, so a
// statement date typed into the form is an IST date. Stored values stay UTC.
var ist = time.FixedZone("IST", 5*60*60+30*60)

// parseMoneyToPaise turns a human- or firefly-formatted amount into integer
// paise (1/100 of the currency unit). Accepts "1,234.56", "₹1234.5",
// "USD 7.24", "1234", and firefly's long wire form "143.270000000000".
// Rounds half-up past two decimals. Rejects negatives and non-numbers: the
// sign of a transaction is carried by its type, never its amount.
func parseMoneyToPaise(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, cut := range []string{"₹", "$", "€", "£", ","} {
		s = strings.ReplaceAll(s, cut, "")
	}
	s = strings.TrimSpace(s)
	// A leading ISO code ("USD 7.24", "INR 100").
	if len(s) > 3 && s[3] == ' ' && isAlpha(s[:3]) {
		s = strings.TrimSpace(s[4:])
	}
	if s == "" {
		return 0, errors.New("empty amount")
	}
	if strings.HasPrefix(s, "-") {
		return 0, errors.New("amount must not be negative")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	r.Mul(r, big.NewRat(100, 1))
	// Round half-up to whole paise.
	r.Add(r, big.NewRat(1, 2))
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if !q.IsInt64() {
		return 0, fmt.Errorf("amount too large: %q", s)
	}
	return q.Int64(), nil
}

func isAlpha(s string) bool {
	for _, c := range s {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// parseDBTime reads a timestamp in any of the shapes SQLite hands back for
// our DATETIME columns (modernc writes "2006-01-02 15:04:05+00:00"; scanning
// a typed column yields RFC3339).
func parseDBTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST", // Go's time.String()
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// istDateTime splits a timestamp into the review form's IST date and time
// inputs ("2006-01-02", "15:04"). Zero time → empty strings.
func istDateTime(t time.Time) (string, string) {
	if t.IsZero() {
		return "", ""
	}
	l := t.In(ist)
	return l.Format("2006-01-02"), l.Format("15:04")
}

// parseISTForm is the inverse: the form's IST date (required) and optional
// time (default 00:00) → a UTC instant, minute precision.
func parseISTForm(date, clock string) (time.Time, error) {
	date, clock = strings.TrimSpace(date), strings.TrimSpace(clock)
	if date == "" {
		return time.Time{}, errors.New("date is required")
	}
	if clock == "" {
		clock = "00:00"
	}
	if len(clock) > 5 { // tolerate "15:04:05" from some browsers
		clock = clock[:5]
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, ist)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad date/time %q %q: %w", date, clock, err)
	}
	return t.UTC(), nil
}
