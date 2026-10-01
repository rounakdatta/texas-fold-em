package ui

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// conf sets a staged row's engine confidence (stage's extra statements).
func conf(v string) string {
	return `UPDATE staged_fold_txns SET classifier_confidence = ` + v + ` WHERE fold_uuid = ?`
}

// surestOrder walks every page of Surest first, two cards a page, so the
// cursor is part of every check.
func (rh *reviewHarness) surestOrder(t *testing.T, query string) []string {
	t.Helper()
	var out []string
	after := ""
	for i := 0; i < 50; i++ {
		q := "order=surest&limit=2" + query
		if after != "" {
			q += "&after=" + url.QueryEscape(after)
		}
		d := rh.deck(t, q)
		out = append(out, uuids(d.Cards)...)
		if d.Next == "" {
			return out
		}
		after = d.Next
	}
	t.Fatal("the pages never ended")
	return nil
}

func TestSurest_TheOnesThatGoWithASwipeAndAreSureComeFirst(t *testing.T) {
	rh := newReviewHarness(t)
	// newest first: fairly, blank-sure, sure-new, held, unsure, sure-old, blank-fairly
	rh.stage(t, "blank-fairly", "ready_to_push", "___ for lunch from Zomato", 1000, "2025-12-20T07:00:00Z", conf("0.7"))
	rh.stage(t, "sure-old", "ready_to_push", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.95"))
	rh.stage(t, "unsure", "needs_review", "Masala chai", 1000, "2025-12-22T07:00:00Z", conf("0.5"))
	rh.stage(t, "held", "ready_to_push", "Masala chai", 1000, "2025-12-23T07:00:00Z", conf("0.99"),
		`UPDATE staged_fold_txns SET hold_reason = 'never billed' WHERE fold_uuid = ?`)
	rh.stage(t, "sure-new", "ready_to_push", "Masala chai", 1000, "2025-12-24T07:00:00Z", conf("0.9"))
	rh.stage(t, "blank-sure", "ready_to_push", "___ for lunch from Zomato", 1000, "2025-12-25T07:00:00Z", conf("0.95"))
	rh.stage(t, "fairly", "needs_review", "Masala chai", 1000, "2025-12-26T07:00:00Z", conf("0.7"))

	want := "sure-new,sure-old,fairly,unsure,blank-sure,blank-fairly,held"
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != want {
		t.Fatalf("surest first = %s\nwant          %s", got, want)
	}
	// …which is not the order time alone gives
	if got := strings.Join(uuids(rh.deck(t, "").Cards), ","); got == want {
		t.Fatalf("newest first gives the same order (%s): the fixture proves nothing", got)
	}
	// the card says which band it is in, every card, every order
	for _, c := range rh.deck(t, "").Cards {
		wantBand := map[string]string{"sure-new": sureYes, "sure-old": sureYes, "fairly": sureFairly, "unsure": sureNot,
			"blank-sure": sureYes, "blank-fairly": sureFairly, "held": sureYes}[c.UUID]
		if c.Sure.Band != wantBand {
			t.Errorf("%s: band %q, want %q", c.UUID, c.Sure.Band, wantBand)
		}
		if c.Sure.Score == nil {
			t.Errorf("%s: no score on a card the engine scored", c.UUID)
		}
	}
}

// The engine's number is its own word, and two sure suggestions are equally
// good ones: within a band the deck keeps to time, so a day's payments stay
// together, rather than scattering them for a 0.86 against a 0.99.
func TestSurest_WithinABandTheDeckKeepsToTime(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "older-surer", "ready_to_push", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.99"))
	rh.stage(t, "newer", "ready_to_push", "Masala chai", 1000, "2025-12-22T07:00:00Z", conf("0.86"))
	rh.stage(t, "older-fairly", "needs_review", "Masala chai", 1000, "2025-12-19T07:00:00Z", conf("0.84"))
	rh.stage(t, "newer-fairly", "needs_review", "Masala chai", 1000, "2025-12-20T07:00:00Z", conf("0.6"))
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != "newer,older-surer,newer-fairly,older-fairly" {
		t.Fatalf("surest first = %s, want each band newest first", got)
	}
}

func TestSurest_BandsFollowTheClassifiersOwnLines(t *testing.T) {
	score := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	declined := sql.NullInt64{Int64: 4, Valid: true}
	for _, tc := range []struct {
		facts sureFacts
		band  string
		why   string
	}{
		{sureFacts{score: score(1)}, sureYes, ""},
		{sureFacts{score: score(0.85)}, sureYes, ""}, // the threshold it accepts at, unasked
		{sureFacts{score: score(0.849)}, sureFairly, ""},
		{sureFacts{score: score(0.6)}, sureFairly, ""},
		{sureFacts{score: score(0.599)}, sureNot, ""},
		{sureFacts{score: score(0)}, sureNot, sureUnsettled}, // no engine at all stores 0
		{sureFacts{}, sureNot, sureUnsettled},
		// it declined: the model's confidence was in what it could say, not who
		{sureFacts{score: score(0.9), tier: declined}, sureNot, sureUnsettled},
		// it would make an account in Firefly: a person checks the name first
		{sureFacts{score: score(0.95), newOther: true}, sureFairly, sureNewPayee},
		{sureFacts{score: score(0.95), newOwn: true}, sureFairly, sureNewAccount},
		{sureFacts{score: score(0.5), newOther: true}, sureNot, sureNewPayee},
		// a person's choice outranks all of it
		{sureFacts{score: score(0.9), tier: declined, chosen: true, newOther: true}, sureYes, sureChosen},
	} {
		s := sureness(reviewCard{}, tc.facts)
		if s.Band != tc.band || s.Why != tc.why {
			t.Errorf("%+v: %q (%q), want %q (%q)", tc.facts, s.Band, s.Why, tc.band, tc.why)
		}
	}
}

func TestSurest_ADoubtTheCardRaisesIsUnsureWhateverTheScore(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "dup", "ready_to_push", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.99"),
		`UPDATE staged_fold_txns SET raw_payload = '{"is_possible_duplicate": true}' WHERE fold_uuid = ?`)
	rh.stage(t, "refund", "ready_to_push", "Refund for lunch", 1000, "2025-12-22T07:00:00Z", conf("0.99"),
		`UPDATE staged_fold_txns SET classifier_tier = 5 WHERE fold_uuid = ?`)
	rh.stage(t, "worth-holding", "ready_to_push", "Masala chai", 1000, "2025-12-23T07:00:00Z", conf("0.99"),
		`UPDATE staged_fold_txns SET classifier_evidence_json = '{"hold_suggestion":"a released authorisation"}' WHERE fold_uuid = ?`)
	rh.stage(t, "plain", "ready_to_push", "Masala chai", 1000, "2025-12-20T07:00:00Z", conf("0.99"))
	why := map[string]string{}
	for _, c := range rh.deck(t, "").Cards {
		why[c.UUID] = c.Sure.Band + "/" + c.Sure.Why
	}
	for uuid, want := range map[string]string{"dup": "unsure/duplicate", "refund": "unsure/refund", "worth-holding": "unsure/hold", "plain": "sure/"} {
		if why[uuid] != want {
			t.Errorf("%s = %s, want %s", uuid, why[uuid], want)
		}
	}
	if got := rh.surestOrder(t, ""); got[0] != "plain" {
		t.Errorf("surest first = %v, want the one without a doubt first, though it is the oldest", got)
	}
}

// A person's choice of who is sure; a suggestion they kept while fixing
// something else (the title, the amount from the statement) is still only as
// sure as fold was. Every save writes each side whole, so it is the choice
// that counts, not the save.
func TestSurest_APersonsPickOfWhoIsSureAKeptSuggestionIsNot(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "picked", "needs_review", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.5"))
	rh.stage(t, "named", "needs_review", "Masala chai", 1000, "2025-12-22T07:00:00Z", conf("0.5"))
	rh.stage(t, "kept", "needs_review", "Masala chai", 1000, "2025-12-23T07:00:00Z", conf("0.5"))
	for path, body := range map[string]string{
		"rows/picked/edit": `{"payee":"Daily Mart, Market Road"}`, // another existing payee
		"rows/named/edit":  `{"payee":"Tea stall, Market Road"}`,  // a new one, by name
		"rows/kept/edit":   `{"title":"Masala chai and bun"}`,     // the payee as fold had it
	} {
		if code, a := rh.post(t, path, body); code != 200 || a.Card == nil {
			t.Fatalf("%s = %d %+v", path, code, a)
		}
	}
	got := map[string]cardSure{}
	for _, c := range rh.deck(t, "").Cards {
		got[c.UUID] = c.Sure
	}
	if s := got["picked"]; s.Band != sureYes || s.Why != sureChosen {
		t.Errorf("picked = %+v, want sure: a person chose who", s)
	}
	if s := got["named"]; s.Band != sureYes || s.Why != sureChosen {
		t.Errorf("named = %+v, want sure: a person named who", s)
	}
	if s := got["kept"]; s.Band != sureNot || s.Why != "" {
		t.Errorf("kept = %+v, want unsure: the payee is still fold's 0.5 guess", s)
	}
	if order := strings.Join(rh.surestOrder(t, ""), ","); order != "named,picked,kept" {
		t.Errorf("surest first = %s", order)
	}
}

func TestSurest_ARowAddedFromAStatementIsSure(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "manual-0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a", "ready_to_push", "Annual fee", 1000, "2025-12-21T07:00:00Z",
		`UPDATE staged_fold_txns SET mode = 'MANUAL', classifier_tier = NULL, classifier_confidence = NULL WHERE fold_uuid = ?`)
	c := rh.deck(t, "").Cards[0]
	if c.Sure.Band != sureYes || c.Sure.Why != sureManual || c.Sure.Score != nil {
		t.Errorf("manual row = %+v, want sure, every field a person's, and no score claimed", c.Sure)
	}
}

func TestSurest_PagesCoverEveryCardOnceEvenAsCardsAreDecided(t *testing.T) {
	rh := newReviewHarness(t)
	// timestamps as the live database writes them, a "+" and spaces in each
	// (the cursor carries them)
	for i, s := range []string{"0.9", "0.7", "0.95", "0.5", "0.88", "0.65", "0.3"} {
		rh.stage(t, "c"+s, "ready_to_push", "Masala chai", 1000, "2025-12-2"+string(rune('1'+i))+" 07:00:00 +0000 UTC", conf(s))
	}
	all := strings.Join(rh.surestOrder(t, ""), ",")
	if all != "c0.88,c0.95,c0.9,c0.65,c0.7,c0.3,c0.5" {
		t.Fatalf("surest first = %s", all)
	}
	// a card decided between pages must not move the rest
	first := rh.deck(t, "order=surest&limit=2")
	rh.post(t, "rows/"+first.Cards[0].UUID+"/later", `{"later":true}`)
	second := rh.deck(t, "order=surest&limit=2&after="+url.QueryEscape(first.Next))
	if got := strings.Join(uuids(second.Cards), ","); got != "c0.9,c0.65" {
		t.Errorf("the page after a decision = %s, want the next two in order", got)
	}
}

func TestSurest_CountsTheSureOnesItsFirstPageLeadsWith(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "a", "ready_to_push", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.9"))
	rh.stage(t, "b", "ready_to_push", "Masala chai", 1000, "2025-12-22T07:00:00Z", conf("0.97"))
	rh.stage(t, "c-blank", "ready_to_push", "___ from Zomato", 1000, "2025-12-23T07:00:00Z", conf("0.97"))
	rh.stage(t, "d", "needs_review", "Masala chai", 1000, "2025-12-24T07:00:00Z", conf("0.7"))
	d := rh.deck(t, "order=surest&limit=1")
	if d.Counts.Sure == nil || *d.Counts.Sure != 2 {
		t.Fatalf("counts.sure = %v, want 2: the ready, sure ones (a sure card with a blank waits on you)", d.Counts.Sure)
	}
	if more := rh.deck(t, "order=surest&limit=1&after="+d.Next); more.Counts.Sure != nil {
		t.Errorf("counts.sure on a later page = %d, want it on the first only", *more.Counts.Sure)
	}
	if d := rh.deck(t, ""); d.Counts.Sure != nil {
		t.Errorf("counts.sure in time order = %d, want none: nothing asked", *d.Counts.Sure)
	}
	if d := rh.deck(t, "order=surest&account=12"); d.Counts.Sure == nil || *d.Counts.Sure != 0 {
		t.Errorf("counts.sure on an account with nothing waiting = %v, want 0", d.Counts.Sure)
	}
}

// A rank is kept between pages (sureness.go's memo): what it is kept against
// must make it anew when the card changes.
func TestSurest_ACardsPlaceFollowsItsEdits(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "blank", "ready_to_push", "___ for lunch from Zomato", 1000, "2025-12-25T07:00:00Z", conf("0.95"))
	rh.stage(t, "fairly", "needs_review", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.7"))
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != "fairly,blank" {
		t.Fatalf("before = %s, want the card with a blank after the one that can go", got)
	}
	rh.post(t, "rows/blank/edit", `{"title":"Chicken biryani for lunch from Zomato"}`)
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != "blank,fairly" {
		t.Errorf("after filling the blank = %s, want it with the sure ones", got)
	}
}

func TestSurest_AnAccountClosedInFireflyIsNoticed(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "on-closed", "ready_to_push", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.95"),
		`UPDATE staged_fold_txns SET proposed_source_account_id = 12 WHERE fold_uuid = ?`) // HDFC Bank
	rh.stage(t, "other", "needs_review", "Masala chai", 1000, "2025-12-25T07:00:00Z", conf("0.7"))
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != "on-closed,other" {
		t.Fatalf("before = %s", got)
	}
	mustExec(t, rh.db, `UPDATE firefly_accounts SET active = 0 WHERE firefly_id = 12`)
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != "other,on-closed" {
		t.Errorf("after the account closed = %s, want the card that can't go after the one that can", got)
	}
}

// The deck asks for pages while the Show sheet asks for its count: the kept
// ranks are shared between requests.
func TestSurest_PagesAskedAtOnceAgree(t *testing.T) {
	rh := newReviewHarness(t)
	for i, s := range []string{"0.9", "0.7", "0.95", "0.5", "0.88", "0.65"} {
		rh.stage(t, "c"+s, "ready_to_push", "Masala chai", 1000, "2025-12-2"+string(rune('1'+i))+"T07:00:00Z", conf(s))
	}
	want := strings.Join(rh.surestOrder(t, ""), ",")
	got := make(chan string, 6)
	for range 6 {
		go func() { // (no t.Fatal off the test's goroutine: errors come back as text)
			resp, err := http.Get(rh.srv.URL + "/api/deck?order=surest")
			if err != nil {
				got <- err.Error()
				return
			}
			defer resp.Body.Close()
			var d deckResponse
			if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
				got <- err.Error()
				return
			}
			got <- strings.Join(uuids(d.Cards), ",")
		}()
	}
	for range 6 {
		if g := <-got; g != want {
			t.Errorf("a page asked at the same time = %s, want %s", g, want)
		}
	}
}

// The classifier sends a card to a person, however sure it is, when sending
// it would make an account in Firefly: the name is for a person to check.
// Surest first must not lead with those.
func TestSurest_ACardThatWouldMakeAnAccountIsForAPersonToCheck(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "new-payee", "needs_review", "Masala chai", 1000, "2025-12-25T07:00:00Z", conf("0.92"),
		`UPDATE staged_fold_txns SET proposed_destination_account_id = NULL, proposed_destination_account_name = 'Tea Stall, Market Road' WHERE fold_uuid = ?`)
	rh.stage(t, "new-card", "needs_review", "Masala chai", 1000, "2025-12-24T07:00:00Z", conf("0.95"),
		`UPDATE staged_fold_txns SET proposed_source_account_id = NULL, proposed_source_account_name = 'Kestrel Travel Credit Card' WHERE fold_uuid = ?`)
	rh.stage(t, "known-by-name", "ready_to_push", "Masala chai", 1000, "2025-12-23T07:00:00Z", conf("0.9"),
		`UPDATE staged_fold_txns SET proposed_destination_account_id = NULL, proposed_destination_account_name = 'daily mart, market road' WHERE fold_uuid = ?`)
	rh.stage(t, "fairly", "needs_review", "Masala chai", 1000, "2025-12-22T07:00:00Z", conf("0.7"))
	got := map[string]string{}
	for _, c := range rh.deck(t, "").Cards {
		got[c.UUID] = c.Sure.Band + "/" + c.Sure.Why
	}
	for uuid, want := range map[string]string{
		"new-payee":     "fairly/new-payee",
		"new-card":      "fairly/new-account",
		"known-by-name": "sure/", // named, but Firefly has it: push finds it
		"fairly":        "fairly/",
	} {
		if got[uuid] != want {
			t.Errorf("%s = %s, want %s", uuid, got[uuid], want)
		}
	}
	if d := rh.deck(t, "order=surest&limit=1"); d.Counts.Sure == nil || *d.Counts.Sure != 1 {
		t.Errorf("counts.sure = %v, want 1: only the payee Firefly has", d.Counts.Sure)
	}
	// once a person names who, it is theirs
	rh.post(t, "rows/new-payee/edit", `{"payee":"Chai Corner, Market Road"}`)
	if order := strings.Join(rh.surestOrder(t, ""), ","); order != "new-payee,known-by-name,new-card,fairly" {
		t.Errorf("surest first = %s", order)
	}
}

func TestSurest_ACardFoldDeclinedIsUnsureWhateverItsScore(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "declined", "needs_review", "Masala chai", 1000, "2025-12-25T07:00:00Z", conf("0.9"),
		`UPDATE staged_fold_txns SET classifier_tier = 4, proposed_destination_account_id = NULL WHERE fold_uuid = ?`)
	rh.stage(t, "blank", "ready_to_push", "___ from Chai Corner", 1000, "2025-12-21T07:00:00Z", conf("0.7"))
	c := rh.deck(t, "").Cards[0]
	if c.UUID != "declined" || c.Sure.Band != sureNot || c.Sure.Why != sureUnsettled {
		t.Fatalf("declined = %s %+v, want unsure: fold couldn't settle who", c.UUID, c.Sure)
	}
	if order := strings.Join(rh.surestOrder(t, ""), ","); order != "blank,declined" {
		t.Errorf("surest first = %s, want the declined card after one fold is fairly sure of", order)
	}
}

// A card bill paid to the payee that shares the card's name reads as a
// transfer to the card; any save writes that side back by name, and it lands
// on the card itself. That is the same who, not a person's choice.
func TestSurest_ASaveOnATwinOfYourCardIsNotAChoice(t *testing.T) {
	rh := newReviewHarness(t)
	mustExec(t, rh.db, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES (900, 'Tata Neu HDFC Bank Credit Card', 'expense', 1, '{}')`)
	rh.stage(t, "bill", "needs_review", "Card bill", 100000, "2025-12-25T07:00:00Z", conf("0.5"),
		`UPDATE staged_fold_txns SET proposed_source_account_id = 12, proposed_destination_account_id = 900 WHERE fold_uuid = ?`)
	before := rh.deck(t, "").Cards[0]
	if before.Type != "transfer" || before.Sure.Band != sureNot {
		t.Fatalf("before = %s %+v", before.Type, before.Sure)
	}
	if code, a := rh.post(t, "rows/bill/edit", `{"title":"Tata Neu card bill"}`); code != 200 || a.Card == nil {
		t.Fatalf("edit = %d %+v", code, a)
	}
	var saved sql.NullInt64
	_ = rh.db.DB.QueryRow(`SELECT confirmed_destination_account_id FROM staged_fold_txns WHERE fold_uuid = 'bill'`).Scan(&saved)
	after := rh.deck(t, "").Cards[0]
	if after.Sure.Band != sureNot || after.Sure.Why == sureChosen {
		t.Errorf("after a title edit (saved destination %v) = %+v, want still fold's 0.5 guess", saved, after.Sure)
	}
}

// What a rank reads of the mirror is more than your own accounts: who someone
// is decides a payee's place too.
func TestSurest_AnAccountChangedInFireflyIsNoticed(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "card", "ready_to_push", "Masala chai", 1000, "2025-12-25T07:00:00Z", conf("0.95"))
	rh.stage(t, "other", "needs_review", "Masala chai", 1000, "2025-12-21T07:00:00Z", conf("0.7"),
		`UPDATE staged_fold_txns SET proposed_destination_account_id = 421 WHERE fold_uuid = ?`)
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != "card,other" {
		t.Fatalf("before = %s", got)
	}
	// Firefly now has the payee as someone who pays you: not who a spend goes to
	mustExec(t, rh.db, `UPDATE firefly_accounts SET type = 'revenue' WHERE firefly_id = 979`)
	if got := strings.Join(rh.surestOrder(t, ""), ","); got != "other,card" {
		t.Errorf("after = %s, want the card that can't go after the one that can", got)
	}
}

// A choice of who is what a person did, not what the row's columns say: they
// move without one.
func TestSurest_OnlyAPersonsEditIsAChoiceOfWho(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "kept", "needs_review", "Masala chai", 1000, "2025-12-25T07:00:00Z", conf("0.5"))
	rh.stage(t, "swapped", "needs_review", "Masala chai", 1000, "2025-12-24T07:00:00Z", conf("0.5"))
	rh.post(t, "rows/kept/edit", `{"title":"Masala chai and bun"}`)
	// a reclassify rewrites the suggestion under the kept save
	mustExec(t, rh.db, `UPDATE staged_fold_txns SET proposed_destination_account_id = 421 WHERE fold_uuid = 'kept'`)
	// the repair stores a backwards row the right way round, without a person
	mustExec(t, rh.db, `UPDATE staged_fold_txns SET confirmed_source_account_id = 979, confirmed_destination_account_id = 476 WHERE fold_uuid = 'swapped'`)
	for _, c := range rh.deck(t, "").Cards {
		if c.Sure.Why == sureChosen || c.Sure.Band == sureYes {
			t.Errorf("%s = %+v: nobody chose who", c.UUID, c.Sure)
		}
	}
}

// Firefly finds or makes the account a move needs by name and kind: a payee
// you have only ever paid has no account to pay you back from yet.
func TestSurest_AMerchantsFirstRefundMakesItsPayer(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "refund", "ready_to_push", "Refund for Masala chai", 20000, "2025-12-25T07:00:00Z", conf("0.95"),
		`UPDATE staged_fold_txns SET type = 'INCOMING', classifier_tier = 5, proposed_refund_of = 'none',
		     proposed_source_account_id = NULL, proposed_source_account_name = 'Chai Corner, Market Road',
		     proposed_destination_account_id = 476 WHERE fold_uuid = ?`)
	c := rh.deck(t, "").Cards[0]
	if c.Type != "deposit" || len(c.Blockers) != 0 || c.Sure.Band != sureFairly || c.Sure.Why != sureNewPayee {
		t.Errorf("first refund = %s %v %+v, want fairly sure: sending it makes the payer", c.Type, c.Blockers, c.Sure)
	}
	if d := rh.deck(t, "order=surest&limit=1"); d.Counts.Sure == nil || *d.Counts.Sure != 0 {
		t.Errorf("counts.sure = %v, want 0", d.Counts.Sure)
	}
}

// Picking who was paid says nothing about the account that paid.
func TestSurest_APickOfWhoDoesNotVouchForANewAccountOfYours(t *testing.T) {
	rh := newReviewHarness(t)
	rh.stage(t, "new-card", "needs_review", "Masala chai", 1000, "2025-12-25T07:00:00Z", conf("0.95"),
		`UPDATE staged_fold_txns SET proposed_source_account_id = NULL, proposed_source_account_name = 'Kestrel Travel Credit Card' WHERE fold_uuid = ?`)
	rh.post(t, "rows/new-card/edit", `{"payee":"Daily Mart, Market Road"}`)
	c := rh.deck(t, "").Cards[0]
	if c.Sure.Band != sureFairly || c.Sure.Why != sureNewAccount {
		t.Errorf("after picking who = %+v, want fairly sure: the paying account is still new", c.Sure)
	}
}
