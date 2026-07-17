package classifier

import (
	"strings"
	"testing"
)

// TestMealContext_Buckets covers each IST meal/occasion bucket and
// the weekday-vs-weekend distinction. Times are written in UTC so
// the IST conversion is exercised on every case.
func TestMealContext_Buckets(t *testing.T) {
	// All cases below are picked so that the UTC time → IST conversion
	// (+5:30) lands inside the named bucket. Weekday is asserted
	// explicitly so a holiday calendar change can't silently shift the
	// expected "weekend" pattern.
	cases := []struct {
		name        string
		utcInput    string
		wantBucket  string // substring expected in the bucket label
		wantDayKind string // "weekday" or "weekend"
	}{
		{"breakfast", "2026-05-12T01:00:00Z", "breakfast", "weekday"},     // 06:30 IST Tue
		{"snack",     "2026-05-12T05:30:00Z", "mid-morning",  "weekday"},  // 11:00 IST Tue
		{"lunch",     "2026-05-12T08:30:00Z", "lunch",        "weekday"},  // 14:00 IST Tue
		{"tea",       "2026-05-12T11:00:00Z", "tea",          "weekday"},  // 16:30 IST Tue
		{"earlyevening", "2026-05-12T13:00:00Z", "early evening", "weekday"}, // 18:30 IST Tue
		{"dinner",    "2026-05-12T16:00:00Z", "dinner",       "weekday"},  // 21:30 IST Tue
		{"latenight", "2026-05-12T19:00:00Z", "late-night",   "weekday"},  // 00:30 IST Wed
		// Saturday (2026-05-16): 10:00 UTC = 15:30 IST — boundary between
		// tea bucket lower edge. Use 09:00 UTC = 14:30 IST → still lunch.
		{"weekend_lunch", "2026-05-16T09:00:00Z", "lunch",    "weekend"},  // 14:30 IST Sat
		{"weekend_dinner", "2026-05-17T15:00:00Z", "dinner",  "weekend"},  // 20:30 IST Sun
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mealContext(tc.utcInput, "") // domestic (INR): IST is local
			if got == "" {
				t.Fatalf("mealContext returned empty for %q", tc.utcInput)
			}
			if !strings.Contains(got, "IST") {
				t.Errorf("output should mention IST: %q", got)
			}
			if !strings.Contains(got, tc.wantBucket) {
				t.Errorf("expected bucket %q in output, got %q", tc.wantBucket, got)
			}
			if !strings.Contains(got, tc.wantDayKind) {
				t.Errorf("expected day kind %q in output, got %q", tc.wantDayKind, got)
			}
		})
	}
}

// TestMealContext_RejectsGarbage: an unparseable timestamp returns
// empty so the prompt renderer cleanly omits the TIME CONTEXT block
// rather than emitting a confusing partial line.
func TestMealContext_RejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "not-a-date", "12345"} {
		if got := mealContext(s, ""); got != "" {
			t.Errorf("mealContext(%q) = %q, want empty", s, got)
		}
	}
}

// TestMealBucket_Boundaries: explicit boundary cases for the inclusive-
// lower / exclusive-upper bucket edges so a future tweak to the
// boundaries doesn't silently re-bucket many transactions.
func TestMealBucket_Boundaries(t *testing.T) {
	cases := []struct {
		h, m int
		want string
	}{
		// boundary clock positions:
		{6, 0, "breakfast"},     // bucket lower edge
		{10, 29, "breakfast"},   // just inside upper edge
		{10, 30, "mid-morning"}, // bumped to next bucket
		{12, 30, "lunch"},
		{15, 29, "lunch"},
		{15, 30, "tea"},
		{18, 0, "early evening"},
		{20, 0, "dinner"},
		{23, 29, "dinner"},
		{23, 30, "late-night"},
		{0, 0, "late-night"},
		{5, 59, "late-night"},
	}
	for _, tc := range cases {
		got := mealBucket(tc.h, tc.m)
		if !strings.Contains(got, tc.want) {
			t.Errorf("mealBucket(%d:%02d) = %q, want substring %q", tc.h, tc.m, got, tc.want)
		}
	}
}

// TestMealContext_ForeignCurrencyUsesLocalTime: a USD charge is bucketed in
// US local time, not the server's IST. 22:00 UTC = 03:30 IST (late-night) but
// 18:00 EDT in the US — the meal signal must be the local one.
func TestMealContext_ForeignCurrencyUsesLocalTime(t *testing.T) {
	got := mealContext("2026-07-17T22:00:00Z", "USD")
	for _, want := range []string{"likely-local", "2026-07-17 18:00", "EDT", "USD", "US", "home (IST)", "subscription"} {
		if !strings.Contains(got, want) {
			t.Errorf("foreign meal context missing %q:\n%s", want, got)
		}
	}
	// A domestic (INR) charge stays IST-only, no likely-local line.
	if dom := mealContext("2026-07-17T22:00:00Z", ""); strings.Contains(dom, "likely-local") {
		t.Errorf("domestic should have no likely-local line: %s", dom)
	}
}

func TestTimezoneForCurrency(t *testing.T) {
	if _, region, approx, ok := timezoneForCurrency("USD"); !ok || !approx || !strings.Contains(region, "US") {
		t.Errorf("USD: ok=%v approx=%v region=%q; want ok, approx, US", ok, approx, region)
	}
	if _, region, approx, ok := timezoneForCurrency("aed"); !ok || approx || region != "UAE" {
		t.Errorf("AED: ok=%v approx=%v region=%q; want ok, !approx, UAE", ok, approx, region)
	}
	if _, _, _, ok := timezoneForCurrency("INR"); ok {
		t.Error("INR must not map (it's home)")
	}
	if _, _, _, ok := timezoneForCurrency("ZZZ"); ok {
		t.Error("unknown currency must not map")
	}
}

func TestForeignCurrencyOf(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"currency":"INR","source_currency":"USD"}`, "USD"},
		{`{"currency":"INR","source_currency":"inr"}`, ""}, // same as home
		{`{"currency":"INR"}`, ""},                          // no source
		{`{"amount":100}`, ""},                              // neither
		{`not json`, ""},
		{``, ""},
	}
	for _, c := range cases {
		if got := foreignCurrencyOf(c.raw); got != c.want {
			t.Errorf("foreignCurrencyOf(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
