package classifier

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

// fireflyAsset is one row of the firefly_accounts mirror, filtered to
// the asset side. The deterministic source resolver matches a fold card
// against this set.
type fireflyAsset struct {
	ID            int64
	Name          string
	AccountNumber string
	Role          string
}

// listFireflyAssetsFromMirror reads the user's real firefly asset
// accounts from the firefly_accounts mirror (populated by the firefly
// accounts sync). Unlike listAssetAccounts — which scrapes assets out of
// transaction history and is blind to a freshly-created account — this
// is the authoritative inventory, so an asset the user just added in
// firefly is matchable immediately. Returns an empty slice (not an
// error) when the mirror hasn't been populated yet.
func listFireflyAssetsFromMirror(ctx context.Context, db *sql.DB) ([]fireflyAsset, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT firefly_id, name, COALESCE(account_number,''), COALESCE(account_role,'')
		FROM firefly_accounts
		WHERE type = 'asset' AND active = 1
		ORDER BY name COLLATE NOCASE
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fireflyAsset
	for rows.Next() {
		var a fireflyAsset
		if err := rows.Scan(&a.ID, &a.Name, &a.AccountNumber, &a.Role); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// matchFoldCardToFireflyAsset deterministically resolves which firefly
// asset a fold card maps to. It returns ok=false when there is no
// *unique* confident match — the caller then proposes the fold card name
// and routes the row to review rather than guessing a wrong card (the
// failure mode that put "AU Ixigo" txns on "Axis Bank Ace").
//
// Three tiers, tried in order; each requires a single unambiguous winner:
//
//  1. last-four: fold's last_four equals the trailing digits of a
//     firefly asset's account_number. Exact — the strongest signal when
//     present (e.g. a plain bank account whose number firefly stores).
//  2. exact name: fold's provider or core name, normalised, equals a
//     firefly asset name. Handles accounts whose fold name *is* the
//     firefly name ("Axis Bank" ↔ "Axis Bank").
//  3. distinctive tokens: the most shared brand tokens after dropping
//     generic words (bank/credit/card/…), requiring a strict unique
//     winner. Handles cards whose names are reorderings or supersets
//     ("AU Ixigo ****9179" ↔ "Ixigo AU Bank Credit Card";
//     "HDFC Tata Neu Plus ****8943" ↔ "Tata Neu HDFC Bank Credit Card").
func matchFoldCardToFireflyAsset(fold *FoldAccountRef, assets []fireflyAsset) (int64, string, bool) {
	if fold == nil || len(assets) == 0 {
		return 0, "", false
	}

	// Tier 1 — last-four vs account_number suffix.
	if l4 := digitsOnly(fold.LastFour); len(l4) >= 4 {
		var hits []fireflyAsset
		for _, a := range assets {
			an := digitsOnly(a.AccountNumber)
			if len(an) >= len(l4) && strings.HasSuffix(an, l4) {
				hits = append(hits, a)
			}
		}
		if len(hits) == 1 {
			return hits[0].ID, hits[0].Name, true
		}
	}

	// Tier 2 — exact normalised-name equality against fold name/provider.
	foldNameKeys := map[string]bool{}
	for _, cand := range []string{fold.Name, fold.Provider} {
		if k := normalizeAccountName(cand); k != "" {
			foldNameKeys[k] = true
		}
	}
	if len(foldNameKeys) > 0 {
		var hits []fireflyAsset
		for _, a := range assets {
			if foldNameKeys[normalizeAccountName(a.Name)] {
				hits = append(hits, a)
			}
		}
		if len(hits) == 1 {
			return hits[0].ID, hits[0].Name, true
		}
	}

	// Tier 3 — distinctive-token overlap, strict unique winner.
	foldTokens := tokenSet(strings.Join([]string{fold.Name, fold.Provider, fold.Network}, " "))
	if len(foldTokens) == 0 {
		return 0, "", false
	}
	bestScore, bestIdx := 0, -1
	tie := false
	for i, a := range assets {
		score := overlapCount(foldTokens, tokenSet(a.Name))
		switch {
		case score > bestScore:
			bestScore, bestIdx, tie = score, i, false
		case score == bestScore && score > 0:
			tie = true
		}
	}
	// Accept only a strict unique winner that also covers a MAJORITY of
	// the fold card's distinctive tokens. The majority bar is what stops a
	// single weak shared word (e.g. "Federal" between an unrelated card
	// and "Scapia Federal Bank Credit Card") from resolving — a lone-token
	// coincidence falls through to "propose name + review" instead of a
	// confident-wrong match. All six of the user's real cards share 100%
	// of their fold tokens with the right asset, so they sail through.
	if bestScore >= 1 && bestIdx >= 0 && !tie && bestScore*2 > len(foldTokens) {
		return assets[bestIdx].ID, assets[bestIdx].Name, true
	}
	return 0, "", false
}

// --- normalisation helpers -------------------------------------------------

// maskRe matches a masked card/account suffix like "****1743" or
// "xxxx1743" (optionally spaced), which is noise for name matching.
var maskRe = regexp.MustCompile(`(?:\*{2,}|x{2,})\s*\d{2,}`)

// nonAlnumRe collapses any run of non-alphanumeric characters to a space.
var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeAccountName lowercases, strips a masked-suffix, and collapses
// punctuation/whitespace so two spellings of the same account compare
// equal: "Axis Bank" and "AXIS  Bank" → "axis bank".
func normalizeAccountName(s string) string {
	s = strings.ToLower(s)
	s = maskRe.ReplaceAllString(s, " ")
	s = nonAlnumRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// accountStopwords are the generic words that carry no brand signal —
// dropped before token matching so "HDFC Bank ****5684" doesn't tie with
// every other "… Bank …" asset on the shared word "bank".
var accountStopwords = map[string]bool{
	"bank": true, "credit": true, "card": true, "cards": true, "the": true,
	"a": true, "an": true, "account": true, "accounts": true, "savings": true,
	"saving": true, "plus": true, "prime": true, "fixed": true, "deposit": true,
	"deposits": true, "fund": true, "funds": true, "ltd": true, "limited": true,
	"co": true, "corp": true, "india": true, "indian": true, "of": true,
	"and": true, "debit": true, "wallet": true, "us": true,
}

// tokenSet returns the distinctive (non-stopword, non-numeric) tokens of
// a name as a set.
func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.Fields(normalizeAccountName(s)) {
		if accountStopwords[tok] {
			continue
		}
		if digitsOnly(tok) == tok { // pure-digit token (a number fragment)
			continue
		}
		out[tok] = true
	}
	return out
}

func overlapCount(a, b map[string]bool) int {
	n := 0
	for t := range a {
		if b[t] {
			n++
		}
	}
	return n
}
