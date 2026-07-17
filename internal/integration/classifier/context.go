package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FoldAccountRef carries the human-readable details of a fold-side
// asset (credit card or bank account) that the classifier joins onto
// the staged transaction's raw_payload.account_id. This is the
// keystone signal for source-account inference: without it, the LLM
// sees only an opaque UUID; with it, "Tata Neu Plus 8943 (HDFC,
// RuPay)" — directly text-matchable against firefly's asset list.
type FoldAccountRef struct {
	ID        string // fold's per-account UUID
	Kind      string // "BANK" | "CREDIT_CARD"
	Name      string // composed display name, e.g. "HDFC Tata Neu Plus ****8943"
	Provider  string // "HDFC", "Axis Bank"
	Network   string // credit-card-only: "Visa", "RuPay"
	LastFour  string
	IsClosed  bool
}

// lookupFoldAccountForStaged extracts account_id from the staged row's
// raw_payload and returns the matching fold_accounts row, if any.
// Returns (nil, nil) when raw_payload has no account_id, or when the
// id isn't yet mirrored. Callers should always check for nil.
func lookupFoldAccountForStaged(ctx context.Context, db *sql.DB, rawPayload string) (*FoldAccountRef, error) {
	if rawPayload == "" {
		return nil, nil
	}
	var probe struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal([]byte(rawPayload), &probe); err != nil {
		return nil, nil // non-JSON or older shape — silently skip
	}
	if probe.AccountID == "" {
		return nil, nil
	}
	var (
		ref   FoldAccountRef
		prov  sql.NullString
		net   sql.NullString
		lf    sql.NullString
		closedInt int
	)
	err := db.QueryRowContext(ctx, `
		SELECT fold_account_id, kind, name, provider, network, last_four, is_closed
		FROM fold_accounts
		WHERE fold_account_id = ?
	`, probe.AccountID).Scan(&ref.ID, &ref.Kind, &ref.Name, &prov, &net, &lf, &closedInt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if prov.Valid {
		ref.Provider = prov.String
	}
	if net.Valid {
		ref.Network = net.String
	}
	if lf.Valid {
		ref.LastFour = lf.String
	}
	ref.IsClosed = closedInt == 1
	return &ref, nil
}

// Context-gathering helpers for Tier-3 LLM RAG. None of these touch
// firefly's API — every query reads our local mirror in firefly_txns.
// The mirror gets refreshed by /admin/firefly/sync; on a fresh deploy
// these queries return empty until that's run once.

// AccountRef is a (id, name) pair used to seed Tier-3 prompts with
// the user's asset / expense / revenue account inventory and the
// hallucination-guard whitelist.
type AccountRef struct {
	ID   int64
	Name string
}

// listAssetAccounts returns the user's own bank/card/wallet accounts
// — the asset side of the firefly book.
//
// Preferred source is the firefly_accounts mirror (firefly's real
// account list), which includes assets the user created but hasn't
// transacted on yet. When that mirror is empty (fresh deploy before the
// first accounts sync, or a test that doesn't seed it) we fall back to
// deriving assets from transaction history:
//
//	source side of withdrawals  (user paid → user's account is source)
//	+ destination side of deposits (someone paid user → user's account is destination)
//
// Deduped by id. For a typical user this is ≤ 20 entries — small
// enough to inline in every Tier-3 prompt.
func listAssetAccounts(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	if mirror, err := listDistinct(ctx, db, `
		SELECT firefly_id, name FROM firefly_accounts
		WHERE type = 'asset' AND active = 1
		ORDER BY name COLLATE NOCASE
	`); err == nil && len(mirror) > 0 {
		return mirror, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, name FROM (
		    SELECT source_account_id      AS id, source_account_name      AS name
		    FROM firefly_txns
		    WHERE txn_type = 'withdrawal' AND source_account_id IS NOT NULL AND source_account_name IS NOT NULL
		    UNION
		    SELECT destination_account_id AS id, destination_account_name AS name
		    FROM firefly_txns
		    WHERE txn_type = 'deposit'    AND destination_account_id IS NOT NULL AND destination_account_name IS NOT NULL
		)
		GROUP BY id
		ORDER BY name COLLATE NOCASE
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountRef
	for rows.Next() {
		var a AccountRef
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// listRevenueAccounts is the deposit-side counterpart: the source side
// of deposits (employer, refunds, etc.). The LLM uses these when the
// fold transaction is INCOMING and we need to pick where the money
// came from.
func listRevenueAccounts(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT source_account_id, source_account_name
		FROM firefly_txns
		WHERE txn_type = 'deposit' AND source_account_id IS NOT NULL AND source_account_name IS NOT NULL
		ORDER BY source_account_name COLLATE NOCASE
	`)
}

// listExpenseAccounts is the withdrawal-side merchant inventory.
// Used as the destination_account candidate set for OUTGOING fold
// transactions.
func listExpenseAccounts(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT destination_account_id, destination_account_name
		FROM firefly_txns
		WHERE txn_type = 'withdrawal' AND destination_account_id IS NOT NULL AND destination_account_name IS NOT NULL
		ORDER BY destination_account_name COLLATE NOCASE
	`)
}

// listCategories / listBudgets are the dimension tables, also derived
// from firefly_txns.
func listCategories(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT category_id, category_name
		FROM firefly_txns
		WHERE category_id IS NOT NULL AND category_name IS NOT NULL
		ORDER BY category_name COLLATE NOCASE
	`)
}

func listBudgets(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT budget_id, budget_name
		FROM firefly_txns
		WHERE budget_id IS NOT NULL AND budget_name IS NOT NULL
		ORDER BY budget_name COLLATE NOCASE
	`)
}

// fireflyTxnTypeFor maps fold's INCOMING/OUTGOING to firefly's
// withdrawal/deposit. Mirror of integration.foldTypeToFireflyType,
// duplicated here to avoid an import cycle with package integration
// (whose tests import the classifier subpackage).
func fireflyTxnTypeFor(foldType string) string {
	switch foldType {
	case "INCOMING":
		return "deposit"
	case "OUTGOING":
		return "withdrawal"
	default:
		return "withdrawal" // safety default — rare on fold
	}
}

// styleSample is one of the user's past description strings for a
// particular merchant, with the date it was written. Fed into the
// Tier-3 prompt so the LLM mirrors the user's voice for this merchant
// rather than inventing a generic title. Distinct from the FTS
// HISTORICAL EXAMPLES block (which is multi-merchant BM25 nearest) —
// these are tightly scoped to the same destination.
type styleSample struct {
	Description string
	Date        time.Time
}

// recentSameMerchantDescriptions returns up to n of the user's most
// recent description strings for the given destination_account_id,
// falling back to a name match if the id is unknown. Used to seed the
// description-generation prompt with the user's writing voice for
// THIS merchant specifically.
//
// Returns nil when neither lookup yields anything (e.g., first-time
// merchant) — the prompt skips the STYLE SAMPLES block in that case.
func recentSameMerchantDescriptions(ctx context.Context, db *sql.DB, destAccountID int64, merchantNormalized string, n int) ([]styleSample, error) {
	if n <= 0 {
		n = 10
	}
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case destAccountID != 0:
		rows, err = db.QueryContext(ctx, `
			SELECT description, date FROM firefly_txns
			WHERE destination_account_id = ?
			  AND description IS NOT NULL AND TRIM(description) <> ''
			ORDER BY date DESC
			LIMIT ?
		`, destAccountID, n)
	case merchantNormalized != "":
		rows, err = db.QueryContext(ctx, `
			SELECT description, date FROM firefly_txns
			WHERE destination_account_name_normalized = ?
			  AND description IS NOT NULL AND TRIM(description) <> ''
			ORDER BY date DESC
			LIMIT ?
		`, merchantNormalized, n)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []styleSample
	for rows.Next() {
		var (
			s       styleSample
			dateStr string
		)
		if err := rows.Scan(&s.Description, &dateStr); err != nil {
			return nil, err
		}
		// firefly_txns.date appears in a few formats depending on which
		// migration created the row — try them in order, fall through
		// to a zero Date if none match (the renderer handles the zero).
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05+00:00", "2006-01-02"} {
			if t, err := time.Parse(layout, dateStr); err == nil {
				s.Date = t
				break
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// istLocation returns the IST time zone, with a hardcoded UTC+5:30
// fallback for environments where tzdata is unavailable (e.g. the
// distroless image build path some day in the future). IST has no DST
// so the fixed offset is correct year-round.
func istLocation() *time.Location {
	if loc, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return loc
	}
	return time.FixedZone("IST", 5*3600+30*60)
}

// mealContext returns a compact human-readable description of WHEN a
// transaction happened — in the local time of where it most likely
// happened, so the LLM can guess meal/occasion correctly even when the
// user is abroad.
//
// The server clock is IST, but a foreign-currency charge was most likely
// made in that currency's region (foreignCurrency is the ORIGINAL charge
// currency, e.g. "USD"/"AED"; empty for a domestic INR transaction). For a
// foreign charge we surface the LIKELY-LOCAL time in that region as the
// primary signal, keep IST as a secondary reference, and tell the LLM to
// apply LOCAL dining norms and to ignore time entirely for online /
// subscription merchants (an Anthropic charge in USD from India is not a
// US meal).
//
// Domestic example:
//
//	"2026-05-12 21:42 IST Mon — dinner (20:00-23:30) — weekday"
//
// Empty string when the timestamp can't be parsed (caller skips the block).
func mealContext(txnTimestamp, foreignCurrency string) string {
	t, ok := parseTxnTimestamp(txnTimestamp)
	if !ok {
		return ""
	}
	home := t.In(istLocation())
	homeLine := fmt.Sprintf("%s IST %s — %s — %s",
		home.Format("2006-01-02 15:04"), home.Format("Mon"),
		mealBucket(home.Hour(), home.Minute()), weekdayCategory(home.Weekday()))

	loc, region, approx, ok := timezoneForCurrency(foreignCurrency)
	if !ok {
		// Domestic, or a currency we can't place — IST IS the local time.
		return homeLine
	}
	local := t.In(loc)
	approxNote := ""
	if approx {
		approxNote = "; timezone approximate — refine from any city in the merchant/narration"
	}
	return fmt.Sprintf(
		"likely-local: %s %s %s (charge currency %s → likely %s%s)\n"+
			"  home (IST):   %s\n"+
			"  For meal/occasion use the LIKELY-LOCAL time and LOCAL dining norms "+
			"(e.g. dinner is ~18:00-21:00 in the US/Europe, later in India). "+
			"If the merchant is an online service / subscription, IGNORE time & location entirely.",
		local.Format("2006-01-02 15:04"), local.Format("MST"), local.Format("Mon"),
		foreignCurrency, region, approxNote,
		homeLine,
	)
}

// foreignCurrencyOf returns the ORIGINAL charge currency of a fold txn when
// it genuinely differs from the home currency (a cross-border charge), else
// "". Read from raw_payload: source_currency is the charge currency, currency
// is the home-billed one (see migration 00010 / fold_sync).
func foreignCurrencyOf(rawPayload string) string {
	if rawPayload == "" {
		return ""
	}
	var p struct {
		Currency       string `json:"currency"`
		SourceCurrency string `json:"source_currency"`
	}
	if json.Unmarshal([]byte(rawPayload), &p) != nil {
		return ""
	}
	src := strings.ToUpper(strings.TrimSpace(p.SourceCurrency))
	home := strings.ToUpper(strings.TrimSpace(p.Currency))
	if src == "" || src == home {
		return ""
	}
	return src
}

// timezoneForCurrency maps an ISO currency to a representative IANA timezone
// — a proxy for WHERE a foreign charge physically happened, for meal/occasion
// inference. Single-timezone currencies (AED, GBP, SGD…) are exact; wide ones
// (USD across four US zones, EUR/AUD/CAD) return approx=true so the prompt
// flags the local time as approximate and lets the LLM refine from a city in
// the merchant/narration. ok=false ⇒ no confident mapping; caller falls back
// to home (IST). INR is intentionally absent (that IS home).
func timezoneForCurrency(code string) (loc *time.Location, region string, approx, ok bool) {
	z, found := currencyZones[strings.ToUpper(strings.TrimSpace(code))]
	if !found {
		return nil, "", false, false
	}
	l, err := time.LoadLocation(z.tz)
	if err != nil {
		return nil, "", false, false
	}
	return l, z.region, z.approx, true
}

var currencyZones = map[string]struct {
	tz     string
	region string
	approx bool
}{
	"USD": {"America/New_York", "US (Eastern shown)", true},
	"EUR": {"Europe/Paris", "Europe (CET shown)", true},
	"GBP": {"Europe/London", "UK", false},
	"AED": {"Asia/Dubai", "UAE", false},
	"SAR": {"Asia/Riyadh", "Saudi Arabia", false},
	"QAR": {"Asia/Qatar", "Qatar", false},
	"BHD": {"Asia/Bahrain", "Bahrain", false},
	"KWD": {"Asia/Kuwait", "Kuwait", false},
	"OMR": {"Asia/Muscat", "Oman", false},
	"SGD": {"Asia/Singapore", "Singapore", false},
	"THB": {"Asia/Bangkok", "Thailand", false},
	"MYR": {"Asia/Kuala_Lumpur", "Malaysia", false},
	"IDR": {"Asia/Jakarta", "Indonesia (WIB shown)", true},
	"JPY": {"Asia/Tokyo", "Japan", false},
	"KRW": {"Asia/Seoul", "South Korea", false},
	"VND": {"Asia/Ho_Chi_Minh", "Vietnam", false},
	"HKD": {"Asia/Hong_Kong", "Hong Kong", false},
	"CNY": {"Asia/Shanghai", "China", false},
	"LKR": {"Asia/Colombo", "Sri Lanka", false},
	"NPR": {"Asia/Kathmandu", "Nepal", false},
	"AUD": {"Australia/Sydney", "Australia (Eastern shown)", true},
	"NZD": {"Pacific/Auckland", "New Zealand", false},
	"CAD": {"America/Toronto", "Canada (Eastern shown)", true},
	"CHF": {"Europe/Zurich", "Switzerland", false},
	"TRY": {"Europe/Istanbul", "Turkey", false},
}

// parseTxnTimestamp accepts the three timestamp shapes we observe in
// staged_fold_txns.txn_timestamp and firefly_txns.date.
func parseTxnTimestamp(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05+00:00", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// mealBucket maps a 24h IST clock position to a coarse meal/occasion
// bucket. Boundaries are inclusive-lower, exclusive-upper.
func mealBucket(h, m int) string {
	mins := h*60 + m
	switch {
	case mins >= 6*60 && mins < 10*60+30:
		return "breakfast (06:00-10:30)"
	case mins >= 10*60+30 && mins < 12*60+30:
		return "mid-morning / snack (10:30-12:30)"
	case mins >= 12*60+30 && mins < 15*60+30:
		return "lunch (12:30-15:30)"
	case mins >= 15*60+30 && mins < 18*60:
		return "tea / afternoon (15:30-18:00)"
	case mins >= 18*60 && mins < 20*60:
		return "early evening (18:00-20:00)"
	case mins >= 20*60 && mins < 23*60+30:
		return "dinner (20:00-23:30)"
	default:
		return "late-night (23:30-06:00)"
	}
}

func weekdayCategory(d time.Weekday) string {
	if d == time.Saturday || d == time.Sunday {
		return "weekend"
	}
	return "weekday"
}

func listDistinct(ctx context.Context, db *sql.DB, query string) ([]AccountRef, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountRef
	for rows.Next() {
		var a AccountRef
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
