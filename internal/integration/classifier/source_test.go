package classifier

import "testing"

// realFireflyAssets mirrors the user's actual firefly asset list (pulled
// from GET /api/v1/accounts), including the freshly-created
// "Ixigo AU Bank Credit Card" (id 1314). Credit-card account_numbers are
// firefly-side statement numbers — deliberately NOT the card last4 — so
// the matcher must fall back to distinctive-token matching for cards.
func realFireflyAssets() []fireflyAsset {
	return []fireflyAsset{
		{ID: 283, Name: "Merrill Lynch (US)", AccountNumber: "IIA 7WA32116", Role: "defaultAsset"},
		{ID: 115, Name: "Public Provident Fund", AccountNumber: "55000003299299", Role: "savingAsset"},
		{ID: 137, Name: "Employee Provident Fund", AccountNumber: "101595206636", Role: "defaultAsset"},
		{ID: 13, Name: "Axis Bank Fixed Deposits", AccountNumber: "", Role: "savingAsset"},
		{ID: 14, Name: "HDFC Bank Fixed Deposits", AccountNumber: "", Role: "savingAsset"},
		{ID: 82, Name: "HDFC Limited Fixed Deposits", AccountNumber: "62751524", Role: "savingAsset"},
		{ID: 4, Name: "Axis Bank", AccountNumber: "91701008014703", Role: "savingAsset"},
		{ID: 12, Name: "HDFC Bank", AccountNumber: "50100304725684", Role: "savingAsset"},
		{ID: 107, Name: "HDFC Securities Funds", AccountNumber: "", Role: "defaultAsset"},
		{ID: 108, Name: "Zerodha Funds", AccountNumber: "IU3830", Role: "defaultAsset"},
		{ID: 106, Name: "WazirX Funds", AccountNumber: "", Role: "defaultAsset"},
		{ID: 163, Name: "Axis Bank Ace Credit Card", AccountNumber: "47001101015328", Role: "ccAsset"},
		{ID: 476, Name: "Tata Neu HDFC Bank Credit Card", AccountNumber: "65292500023589", Role: "ccAsset"},
		{ID: 954, Name: "Scapia Federal Bank Credit Card", AccountNumber: "40298600009717", Role: "ccAsset"},
		{ID: 1314, Name: "Ixigo AU Bank Credit Card", AccountNumber: "40697750350291", Role: "ccAsset"},
	}
}

// TestMatchFoldCardToFireflyAsset_RealData is the regression test for the
// hallucination class: every one of the user's six fold accounts must
// resolve to the correct firefly asset — most importantly, the
// "AU Ixigo ****9179" card must land on "Ixigo AU Bank Credit Card"
// (1314), NEVER on "Axis Bank Ace Credit Card" (163).
func TestMatchFoldCardToFireflyAsset_RealData(t *testing.T) {
	assets := realFireflyAssets()
	cases := []struct {
		name   string
		fold   FoldAccountRef
		wantID int64
	}{
		{"scapia card", FoldAccountRef{Name: "Scapia Scapia ****1743", Provider: "Scapia", LastFour: "1743"}, 954},
		{"tata neu card", FoldAccountRef{Name: "HDFC Tata Neu Plus ****8943", Provider: "HDFC", Network: "RuPay", LastFour: "8943"}, 476},
		{"axis ace card", FoldAccountRef{Name: "AXIS Ace ****2895", Provider: "AXIS", LastFour: "2895"}, 163},
		{"au ixigo card", FoldAccountRef{Name: "AU Ixigo ****9179", Provider: "AU", LastFour: "9179"}, 1314},
		{"hdfc bank (last4)", FoldAccountRef{Name: "HDFC Bank ****5684", Provider: "HDFC Bank", LastFour: "5684"}, 12},
		{"axis bank (exact name)", FoldAccountRef{Name: "Axis Bank ****7037", Provider: "Axis Bank", LastFour: "7037"}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotName, ok := matchFoldCardToFireflyAsset(&tc.fold, assets)
			if !ok {
				t.Fatalf("expected a match for %q, got none", tc.fold.Name)
			}
			if gotID != tc.wantID {
				t.Errorf("%q resolved to id=%d (%q), want id=%d", tc.fold.Name, gotID, gotName, tc.wantID)
			}
		})
	}
}

// TestMatchFoldCardToFireflyAsset_AbsentAssetIsNoMatch proves the safety
// property: when the firefly asset for a card does NOT exist yet, the
// matcher returns ok=false (so the caller proposes the fold card name and
// routes to review) rather than force-fitting the nearest plausible card.
// This is exactly the AU-before-it-was-added situation that produced the
// "Axis Bank Ace" hallucination.
func TestMatchFoldCardToFireflyAsset_AbsentAssetIsNoMatch(t *testing.T) {
	// All assets EXCEPT the Ixigo AU card (id 1314).
	var without []fireflyAsset
	for _, a := range realFireflyAssets() {
		if a.ID != 1314 {
			without = append(without, a)
		}
	}
	fold := FoldAccountRef{Name: "AU Ixigo ****9179", Provider: "AU", LastFour: "9179"}
	if id, name, ok := matchFoldCardToFireflyAsset(&fold, without); ok {
		t.Fatalf("expected NO match for AU Ixigo when its asset is absent, but got id=%d (%q) — that is the hallucination bug", id, name)
	}
}

// TestMatchFoldCardToFireflyAsset_BrandNewCard: a card with no related
// firefly asset at all yields no match.
func TestMatchFoldCardToFireflyAsset_BrandNewCard(t *testing.T) {
	fold := FoldAccountRef{Name: "OneCard Federal ****0000", Provider: "OneCard", LastFour: "0000"}
	if id, name, ok := matchFoldCardToFireflyAsset(&fold, realFireflyAssets()); ok {
		t.Fatalf("expected no match for an unknown card, got id=%d (%q)", id, name)
	}
}

// TestMatchFoldCardToFireflyAsset_Empty: nil fold or empty asset list.
func TestMatchFoldCardToFireflyAsset_Empty(t *testing.T) {
	if _, _, ok := matchFoldCardToFireflyAsset(nil, realFireflyAssets()); ok {
		t.Error("nil fold account should not match")
	}
	fold := FoldAccountRef{Name: "Scapia Scapia ****1743", LastFour: "1743"}
	if _, _, ok := matchFoldCardToFireflyAsset(&fold, nil); ok {
		t.Error("empty asset list should not match")
	}
}
