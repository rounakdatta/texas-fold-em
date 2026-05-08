package fold

import "testing"

// TestExtractMerchant covers the narration shapes we see in production.
// Adding rows to this table is the right way to teach the extractor
// new patterns — failing test, then code change, then green test.
func TestExtractMerchant(t *testing.T) {
	cases := []struct {
		name      string
		narration string
		mode      string
		want      string
	}{
		{
			name:      "card with simple merchant",
			narration: "CARD/19e0654f754c92ad/Zomato/₹/978.33/OUTGOING/08-05-2026 at 06:44",
			mode:      "CARD",
			want:      "Zomato",
		},
		{
			name:      "card with multi-word merchant",
			narration: "CARD/19df3db9a446595e/CAKE PALACE AND OM GANESH FRUIT JUICE/Rs./50.00/OUTGOING",
			mode:      "CARD",
			want:      "CAKE PALACE AND OM GANESH FRUIT JUICE",
		},
		{
			name:      "card with razorpay-style prefix",
			narration: "CARD/19e05d47fb45edba/Raz*sprintpro Bengaluru Ukain/₹/824.00/OUTGOING",
			mode:      "CARD",
			want:      "Raz*sprintpro Bengaluru Ukain",
		},
		{
			name:      "upi to a person",
			narration: "UPI-GOPIKRISHNAN K-9597855159@UPI-HDFC0003636-612427291",
			mode:      "OTHERS",
			want:      "GOPIKRISHNAN K",
		},
		{
			name:      "upi to a vpa-only handle",
			narration: "UPI-rajesh@okaxis-HDFC-12345",
			mode:      "OTHERS",
			want:      "rajesh@okaxis",
		},
		{
			name:      "empty narration",
			narration: "",
			mode:      "CARD",
			want:      "",
		},
		{
			name:      "unrecognised",
			narration: "some random string with no pattern",
			mode:      "OTHERS",
			want:      "",
		},
		{
			name:      "case-insensitive mode",
			narration: "CARD/x/Test/Rs/1/OUTGOING",
			mode:      "card",
			want:      "Test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractMerchant(tc.narration, tc.mode)
			if got != tc.want {
				t.Errorf("ExtractMerchant(%q, %q) = %q, want %q", tc.narration, tc.mode, got, tc.want)
			}
		})
	}
}
