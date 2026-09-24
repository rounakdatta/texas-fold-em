package ui

import (
	"testing"
	"time"
)

func TestParseMoneyToPaise(t *testing.T) {
	for in, want := range map[string]int64{
		"70": 7000, "70.5": 7050, "1,234.56": 123456, "₹9,070.69": 907069, "USD 94.21": 9421,
		"143.270000000000": 14327, "0.005": 1, " 12.345 ": 1235,
	} {
		if got, err := parseMoneyToPaise(in); err != nil || got != want {
			t.Errorf("parseMoneyToPaise(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-5", "abc", "12,3a"} {
		if _, err := parseMoneyToPaise(in); err == nil {
			t.Errorf("parseMoneyToPaise(%q) should fail", in)
		}
	}
}

func TestISTFormRoundTrip(t *testing.T) {
	utc := time.Date(2026, 7, 25, 18, 30, 0, 0, time.UTC) // 26 Jul 00:00 IST
	d, c := istDateTime(utc)
	if d != "2026-07-26" || c != "00:00" {
		t.Errorf("istDateTime = %s %s", d, c)
	}
	back, err := parseISTForm(d, c)
	if err != nil || !back.Equal(utc) {
		t.Errorf("parseISTForm = %v, %v; want %v", back, err, utc)
	}
	if _, err := parseISTForm("", "10:00"); err == nil {
		t.Error("a missing date should fail")
	}
}

func TestParseDBTime(t *testing.T) {
	want := time.Date(2026, 5, 8, 12, 59, 18, 0, time.UTC)
	for _, in := range []string{"2026-05-08T12:59:18Z", "2026-05-08 12:59:18+00:00", "2026-05-08 12:59:18 +0000 UTC", "2026-05-08T18:29:18+05:30"} {
		if got, ok := parseDBTime(in); !ok || !got.Equal(want) {
			t.Errorf("parseDBTime(%q) = %v, %v", in, got, ok)
		}
	}
}
