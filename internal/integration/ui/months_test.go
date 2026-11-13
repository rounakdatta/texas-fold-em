package ui

import (
	"net/url"
	"slices"
	"sort"
	"testing"
	"time"
)

// monthDeck stages cards either side of two month ends, at home and on a
// trip, in the shapes fold stores times in.
func monthDeck(t *testing.T) *reviewHarness {
	t.Helper()
	rh := newReviewHarness(t)
	// at home every day is IST's
	rh.stage(t, "aug-last", "needs_review", "Tea", 2000, "2026-08-31T18:29:00Z")           // 31 Aug, 11:59 pm IST
	rh.stage(t, "sep-first", "needs_review", "Tea", 2000, "2026-08-31 18:31:00 +0000 UTC") // 1 Sept, 12:01 am IST (Go's own shape)
	rh.stage(t, "sep-mid", "ready_to_push", "Tea", 2000, "2026-09-15 06:00:00+00:00")      // (the driver's shape)
	rh.stage(t, "oct-first", "needs_review", "Tea", 2000, "2026-09-30T19:00:00Z")          // 1 Oct, 12:30 am IST: still 30 Sept in UTC
	rh.stage(t, "dec-old", "needs_review", "Tea", 2000, "2025-12-24T08:00:00Z")
	// a New York trip; its last dinner is 8:30 pm on 31 Aug there — 6 am on
	// 1 Sept at home — and its card says 31 Aug, so it is August's
	rh.abroad(t, "ny-bagels", "2026-08-30T15:00:00Z", "USD", "Corner Bagels, Midtown, New York")
	rh.abroad(t, "ny-pizza", "2026-08-31T18:00:00Z", "USD", "Pier Pizza, Manhattan, New York")
	rh.abroad(t, "ny-diner", "2026-09-01T00:30:00Z", "USD", "Night Diner, Brooklyn, New York")
	// decided rows are in no month's count
	rh.stage(t, "sep-sent", "pushed", "Tea", 2000, "2026-09-10T06:00:00Z")
	rh.stage(t, "sep-skipped", "skipped", "Tea", 2000, "2026-09-11T06:00:00Z")
	return rh
}

func sorted(xs []string) []string { xs = slices.Clone(xs); sort.Strings(xs); return xs }

// A month holds exactly the cards whose day — the day the card shows — falls
// in it: an IST day at home, the trip's own day abroad. The 12:30 am card on
// 1 Oct is October's though it is 30 Sept in UTC, and the New York dinner is
// August's though it is 1 Sept at home.
func TestDeck_AMonthHoldsTheCardsWhoseDayIsInIt(t *testing.T) {
	rh := monthDeck(t)
	for month, want := range map[string][]string{
		"2026-08": {"aug-last", "ny-bagels", "ny-diner", "ny-pizza"},
		"2026-09": {"sep-first", "sep-mid"},
		"2026-10": {"oct-first"},
		"2025-12": {"dec-old"},
		"2026-07": {},
	} {
		d := rh.deck(t, "month="+month)
		if got := sorted(uuids(d.Cards)); !slices.Equal(got, want) {
			t.Errorf("%s holds %v, want %v", month, got, want)
		}
		if d.Counts.Review != len(want) {
			t.Errorf("%s: the To review count is %d, want %d — the pile under the month", month, d.Counts.Review, len(want))
		}
		for _, c := range d.Cards {
			if c.Month != month {
				t.Errorf("%s in %s says it is %q", c.UUID, month, c.Month)
			}
		}
	}
	diner := map[string]reviewCard{}
	for _, c := range rh.deck(t, "month=2026-08").Cards {
		diner[c.UUID] = c
	}
	if c := diner["ny-diner"]; c.Local == nil || c.Day != "Mon 31 Aug" {
		t.Errorf("the diner reads %q (local %+v): the month must follow the day the card shows", c.Day, c.Local)
	}
}

// Every month's count, for the picker: per month, in this pile, and every
// month together — the deck unfiltered.
func TestDeck_TheMonthPickerCountsEachMonth(t *testing.T) {
	rh := monthDeck(t)
	d := rh.deck(t, "")
	want := map[string]int{"2026-08": 4, "2026-09": 2, "2026-10": 1, "2025-12": 1}
	if len(d.Counts.ByMonth) != len(want) {
		t.Errorf("by month = %v, want %v", d.Counts.ByMonth, want)
	}
	for m, n := range want {
		if d.Counts.ByMonth[m] != n {
			t.Errorf("%s counts %d, want %d (all: %v)", m, d.Counts.ByMonth[m], n, d.Counts.ByMonth)
		}
	}
	if d.Counts.AllMonths == nil || *d.Counts.AllMonths != 8 || *d.Counts.AllMonths != d.Counts.Review {
		t.Errorf("every month = %v, want 8, the whole pile", d.Counts.AllMonths)
	}
	// later pages don't pay for the counts again
	if next := rh.deck(t, "limit=1&after="+url.QueryEscape(d.Cards[0].When+"|"+d.Cards[0].UUID)); next.Counts.ByMonth != nil {
		t.Errorf("a later page recounted the months: %v", next.Counts.ByMonth)
	}
}

// Each picker counts under the other's choice and never its own: the
// accounts say how many each has in the month picked, the months how many
// each has on the account picked — so every number is what that choice
// would show.
func TestDeck_EachPickerCountsUnderTheOthersChoice(t *testing.T) {
	rh := monthDeck(t)
	// two of August's and one of September's are paid from the bank instead
	for _, u := range []string{"aug-last", "ny-pizza", "sep-mid"} {
		mustExec(t, rh.db, `UPDATE staged_fold_txns SET proposed_source_account_id = 12 WHERE fold_uuid = ?`, u)
	}

	d := rh.deck(t, "month=2026-08")
	if d.Counts.All == nil || *d.Counts.All != 4 {
		t.Errorf("all accounts in August = %v, want 4", d.Counts.All)
	}
	if d.Counts.ByAccount["12"] != 2 || d.Counts.ByAccount["476"] != 2 {
		t.Errorf("accounts in August = %v, want the bank 2 and the card 2", d.Counts.ByAccount)
	}
	if *d.Counts.AllMonths != 8 || d.Counts.ByMonth["2026-09"] != 2 {
		t.Errorf("the months under no account = %v / %v: the month picked mustn't narrow its own picker", *d.Counts.AllMonths, d.Counts.ByMonth)
	}

	d = rh.deck(t, "account=12")
	if *d.Counts.AllMonths != 3 || d.Counts.ByMonth["2026-08"] != 2 || d.Counts.ByMonth["2026-09"] != 1 || d.Counts.ByMonth["2026-10"] != 0 {
		t.Errorf("the bank's months = %v / %v, want 3: Aug 2, Sept 1", *d.Counts.AllMonths, d.Counts.ByMonth)
	}
	if *d.Counts.All != 8 {
		t.Errorf("all accounts = %d, want 8: the account picked mustn't narrow its own picker", *d.Counts.All)
	}

	d = rh.deck(t, "account=12&month=2026-08")
	if got := sorted(uuids(d.Cards)); !slices.Equal(got, []string{"aug-last", "ny-pizza"}) || d.Counts.Review != 2 {
		t.Errorf("the bank in August = %v (count %d), want both filters at once", got, d.Counts.Review)
	}
}

// The Later pile has months of its own; the counts follow the pile asked
// for, and the other pile's count follows the filters too.
func TestDeck_ThePilesCountUnderTheMonth(t *testing.T) {
	rh := monthDeck(t)
	mustExec(t, rh.db, `UPDATE staged_fold_txns SET later_at = CURRENT_TIMESTAMP WHERE fold_uuid IN ('aug-last', 'sep-mid')`)
	d := rh.deck(t, "pile=later&month=2026-09")
	if got := uuids(d.Cards); !slices.Equal(got, []string{"sep-mid"}) || d.Counts.Later != 1 || d.Counts.Review != 1 {
		t.Errorf("September's Later = %v, counts review %d later %d; want sep-mid, 1 and 1", got, d.Counts.Review, d.Counts.Later)
	}
	if d.Counts.ByMonth["2026-08"] != 1 || d.Counts.ByMonth["2026-09"] != 1 || *d.Counts.AllMonths != 2 {
		t.Errorf("Later's months = %v / %v", d.Counts.ByMonth, *d.Counts.AllMonths)
	}
	if r := rh.deck(t, "month=2026-08"); r.Counts.Review != 3 || r.Counts.Later != 1 {
		t.Errorf("August: review %d later %d, want 3 and 1", r.Counts.Review, r.Counts.Later)
	}
}

// Within a month, every order pages through every card once — the month's
// clause rides along with each cursor.
func TestDeck_AMonthPagesThroughEveryCardOnce(t *testing.T) {
	rh := monthDeck(t)
	for _, order := range []string{orderNewest, orderOldest, orderSurest} {
		var seen []string
		after := ""
		for i := 0; i < 10; i++ {
			q := "month=2026-08&limit=1&order=" + order
			if after != "" {
				q += "&after=" + url.QueryEscape(after)
			}
			d := rh.deck(t, q)
			seen = append(seen, uuids(d.Cards)...)
			if after = d.Next; after == "" {
				break
			}
		}
		if got := sorted(seen); !slices.Equal(got, []string{"aug-last", "ny-bagels", "ny-diner", "ny-pizza"}) {
			t.Errorf("%s first, a page at a time, saw %v", order, seen)
		}
	}
}

// What isn't a month shows every month — never an empty deck that looks
// like a finished one.
func TestDeck_WhatIsntAMonthShowsEveryMonth(t *testing.T) {
	rh := monthDeck(t)
	all := len(rh.deck(t, "").Cards)
	for _, m := range []string{"2026-13", "2026-9", "september", "2026-09-01", "'; DROP TABLE x; --"} {
		if d := rh.deck(t, "month="+url.QueryEscape(m)); len(d.Cards) != all {
			t.Errorf("month=%q showed %d cards, want every month's %d", m, len(d.Cards), all)
		}
	}
}

// A card names the accounts the Show sheet counts it under, so the deck can
// keep those counts true as it leaves: the same two the server counts.
func TestDeck_ACardNamesTheAccountsItIsCountedUnder(t *testing.T) {
	rh := monthDeck(t)
	d := rh.deck(t, "month=2026-09")
	for _, c := range d.Cards {
		if !slices.Equal(c.AccountIDs, []int64{476, 979}) {
			t.Errorf("%s names %v, want the card and the merchant (476, 979)", c.UUID, c.AccountIDs)
		}
	}
	// a side with no account yet (a payee push will create) names nothing
	for _, c := range rh.deck(t, "month=2026-08").Cards {
		if c.UUID == "ny-diner" && !slices.Equal(c.AccountIDs, []int64{476}) {
			t.Errorf("the diner names %v, want only the card", c.AccountIDs)
		}
	}
}

func TestCardMonth(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	at := func(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
	for _, c := range []struct {
		t    time.Time
		zone *time.Location
		want string
	}{
		{at("2026-08-31T18:29:59Z"), nil, "2026-08"},
		{at("2026-08-31T18:30:00Z"), nil, "2026-09"}, // midnight IST
		{at("2026-12-31T18:30:00Z"), nil, "2027-01"},
		{at("2026-09-01T00:30:00Z"), ny, "2026-08"},
		{at("2026-09-01T04:00:00Z"), ny, "2026-09"}, // midnight EDT
	} {
		if got := cardMonth(c.t, c.zone); got != c.want {
			t.Errorf("cardMonth(%s, %v) = %s, want %s", c.t, c.zone, got, c.want)
		}
	}
	for s, want := range map[string]string{"2026-09": "2026-09", "2026-12": "2026-12", "2026-00": "", "2026-13": "", "26-09": "", "": "", " 2026-09": ""} {
		if got := parseMonth(s); got != want {
			t.Errorf("parseMonth(%q) = %q, want %q", s, got, want)
		}
	}
}
