package classifier

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// With hundreds of payees the prompt shows a part of the list. The part must
// hold the payee this transaction is about, even one late in the alphabet —
// cut alphabetically, "Zen Tea House" never reached the model, which then
// proposed a new payee for an account that already existed.
func TestTheRelevantPayeeIsShownEvenLateInTheAlphabet(t *testing.T) {
	db := learnDB(t)
	for i := range 400 {
		mustExec(t, db, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES (?, ?, 'expense', 1, '{}')`,
			1000+i, fmt.Sprintf("Aardvark Shop %03d", i))
	}
	mustExec(t, db, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES (2000, 'Zen Tea House, Jayanagar', 'expense', 1, '{}')`)
	// a payee used often, but not about this transaction
	for i := range 5 {
		fireflyRow(t, db, int64(3000+i), time.Now().AddDate(0, -1, -i), 1377, "Aardvark Shop 377", "", "x", "", "", 100)
	}

	fake := newCapturingLLM(t, `{"txn_type":"withdrawal","destination_account_id":2000,"confidence":0.9,"reasoning":"r"}`)
	c := New(db, slog.Default(), DefaultConfidenceThreshold, 10)
	c.SetLLM(llm.NewClient("k", "m", fake.URL, fake.Client()))
	d, err := c.ClassifyOne(context.Background(), StagedRow{FoldUUID: "z", Type: "OUTGOING", Mode: "UPI",
		Narration: "UPI/zentea@okaxis/ZEN TEA HOUSE", MerchantExtracted: "zen tea house", RawPayload: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	p := fake.last()
	if !strings.Contains(p, `"Zen Tea House, Jayanagar"`) {
		t.Error("the payee this transaction names was not in the inventory the model saw")
	}
	if !strings.Contains(p, "250 of your 405 payees") {
		t.Errorf("the prompt doesn't say the list is partial; it should, so the model knows to look in the examples too")
	}
	zen, used := strings.Index(p, `"Zen Tea House, Jayanagar"`), strings.Index(p, `"Aardvark Shop 377"`)
	if used < 0 || zen > used {
		t.Errorf("order: the relevant payee (at %d) should come before the most-used one (at %d)", zen, used)
	}
	if d.DestinationAccountID == nil || *d.DestinationAccountID != 2000 {
		t.Errorf("the model's pick of an existing account shown to it was refused: %+v", d.DestinationAccountID)
	}
}

// A category the owner created in the deck a minute ago has no transaction
// yet; it is still one the model may pick.
func TestACategoryWithNoHistoryYetIsOffered(t *testing.T) {
	db := learnDB(t) // firefly_categories: Books, Eating out, Subscriptions, Household help
	cats, err := listCategories(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range cats {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "Books,Eating out,Household help,Subscriptions" {
		t.Errorf("categories = %v", names)
	}
}

// Firefly's own name wins over one remembered from history (a rename).
func TestAnAccountReadsWithItsCurrentName(t *testing.T) {
	db := learnDB(t)
	fireflyRow(t, db, 900, time.Now(), 41, "Tea Trail (old name)", "", "x", "", "", 100)
	accs, _ := listExpenseAccounts(context.Background(), db)
	for _, a := range accs {
		if a.ID == 41 && a.Name != "Tea Trail Cafe, Koramangala" {
			t.Errorf("account 41 reads %q, want firefly's current name", a.Name)
		}
	}
	n := 0
	for _, a := range accs {
		if a.ID == 41 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("account 41 listed %d times", n)
	}
}

func TestSharesWord(t *testing.T) {
	words := promptWords(StagedRow{MerchantExtracted: "swiggy instamart", Narration: "UPI/swiggy.instamart@icici/Payment from Phone"})
	for name, want := range map[string]bool{
		"Swiggy":                   true,
		"Swiggy Instamart":         true,
		"SwiggyOne":                true,  // prefix
		"Phone Repairs, Jayanagar": false, // "phone" is rail boilerplate
		"State Bank of India":      false,
		"Big Basket":               false,
	} {
		if got := sharesWord(name, words); got != want {
			t.Errorf("sharesWord(%q) = %v, want %v", name, got, want)
		}
	}
}
