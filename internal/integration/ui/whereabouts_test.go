package ui

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// abroad stages a payment in a foreign currency, at a payee the engine named.
func (rh *reviewHarness) abroad(t *testing.T, uuid, when, cur, payee string) {
	t.Helper()
	rh.stage(t, uuid, "needs_review", "Lunch", 60000, when,
		`UPDATE staged_fold_txns SET foreign_currency = '`+cur+`', foreign_amount_paise = 1250,
		   proposed_destination_account_id = NULL, proposed_destination_account_name = '`+payee+`' WHERE fold_uuid = ?`)
}

// A trip's payments read at the time they were where they were made — their
// own day too — and say whose time that is; home time stays in the small
// print. A foreign charge from home, and every payment at home, read as
// ever.
func TestDeck_ATripsPaymentsReadAtTheirOwnTime(t *testing.T) {
	rh := newReviewHarness(t)
	rh.abroad(t, "sg-lunch", "2026-03-07T05:30:00Z", "SGD", "Lantern Noodles, Chinatown, Singapore") // 1:30 pm SGT, 11:00 am IST
	rh.abroad(t, "sg-late", "2026-03-07T16:45:00Z", "SGD", "Night Market, Singapore")                // 12:45 am SGT Sun, 10:15 pm IST Sat
	rh.abroad(t, "api", "2026-02-01T06:00:00Z", "USD", "Cloud API Credits")                          // from home
	rh.stage(t, "chai", "needs_review", "Chai", 2000, "2026-03-07T10:00:00Z")

	cards := map[string]reviewCard{}
	for _, c := range rh.deck(t, "").Cards {
		cards[c.UUID] = c
	}
	lunch := cards["sg-lunch"]
	if lunch.Local == nil || lunch.Clock != "1:30 pm" || lunch.Local.Place != "Singapore" || lunch.Local.Zone != "Asia/Singapore" || lunch.Local.HomeClock != "11:00 am" {
		t.Errorf("lunch in Singapore: clock %q, local %+v", lunch.Clock, lunch.Local)
	}
	late := cards["sg-late"]
	if late.Local == nil || late.Clock != "12:45 am" || !strings.HasPrefix(late.Day, "Sun 8 Mar") || !strings.HasPrefix(late.Local.HomeDay, "Sat 7 Mar") {
		t.Errorf("after midnight in Singapore: %q %q, local %+v — its own day, and home's in the small print", late.Day, late.Clock, late.Local)
	}
	for _, u := range []string{"api", "chai"} {
		if c := cards[u]; c.Local != nil {
			t.Errorf("%s read at a foreign time: %+v", u, c.Local)
		}
	}

	// the editor's summary says both; the list says where, for the browser to read
	get := func(path string) string {
		resp, err := http.Get(rh.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if page := get("/transactions/sg-late"); !strings.Contains(page, "12:45 am Singapore time") || !strings.Contains(page, "(Sat 7 Mar, 10:15 pm IST)") {
		t.Errorf("the editor doesn't say where and when at home")
	}
	list := get("/transactions?status=needs_review")
	if !strings.Contains(list, `data-tz="Asia/Singapore" data-place="Singapore"`) {
		t.Errorf("the list doesn't say where a trip's payment was made")
	}
	if strings.Count(list, `data-tz=`) != 2*2 { // the two Singapore rows, wide and narrow
		t.Errorf("%d rows carry a zone, want the two Singapore ones (twice each)", strings.Count(list, `data-tz=`))
	}
}

// The list, read first — before anything has read the trips — renders: the
// trips are read before its rows, not while they hold the one connection
// (read inside the loop, the page never answered).
func TestList_ReadFirstItStillReadsTripsAtTheirTime(t *testing.T) {
	rh := newReviewHarness(t)
	rh.abroad(t, "sg-lunch", "2026-03-07T05:30:00Z", "SGD", "Lantern Noodles, Chinatown, Singapore")
	rh.abroad(t, "sg-tea", "2026-03-07T08:00:00Z", "SGD", "Harbour Tea, Marina Bay, Singapore")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(rh.srv.URL + "/transactions?status=needs_review")
	if err != nil {
		t.Fatalf("the list didn't answer: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `data-tz="Asia/Singapore"`) {
		t.Error("the list doesn't say where a trip's payment was made")
	}
}
