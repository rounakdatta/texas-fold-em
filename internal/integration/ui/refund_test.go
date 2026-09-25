package ui

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// seedRefundPair stages a Zomato purchase on the Scapia card and its refund,
// with the classifier's proposal pointing the refund at the purchase.
func seedRefundPair(t *testing.T, u *uiTestHarness, purchaseStatus, refundStatus string) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO firefly_accounts (firefly_id, name, type, account_role, account_number, active, raw_payload) VALUES
		   (954,'Scapia Federal Bank Credit Card','asset','ccAsset','40298600001743',1,'{}'),
		   (11,'Zomato','expense',NULL,NULL,1,'{}'),
		   (2011,'Zomato','revenue',NULL,NULL,1,'{}')`,
		`INSERT INTO fold_accounts (fold_account_id, kind, name, provider, network, last_four, raw_payload, is_closed)
		 VALUES ('scapia-acc','CREDIT_CARD','Scapia ****1743','Federal Bank','Visa','1743','{}',0)`,
	} {
		if _, err := u.db.DB.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	purchaseJournal, refundJournal := any(nil), any(nil)
	if purchaseStatus == "pushed" {
		purchaseJournal = 5001
	}
	if refundStatus == "pushed" {
		refundJournal = 6001
	}
	if _, err := u.db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration,
		    merchant_extracted, status, firefly_txn_id, proposed_source_account_id, proposed_destination_account_id,
		    proposed_description)
		VALUES ('buy-1', '{"account_id":"scapia-acc"}', 48620, 'INR', '2026-03-13T07:06:00Z', 'CARD', 'OUTGOING',
		        'CARD/x/Zomato/₹/486.20/OUTGOING', 'zomato', ?, ?, 954, 11, '___ from Zomato for lunch')`,
		purchaseStatus, purchaseJournal); err != nil {
		t.Fatal(err)
	}
	if _, err := u.db.DB.Exec(`
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration,
		    merchant_extracted, status, firefly_txn_id, classifier_tier, classifier_confidence,
		    proposed_source_account_id, proposed_source_account_name, proposed_destination_account_id,
		    proposed_txn_type, proposed_description, proposed_refund_of)
		VALUES ('ref-1', '{"account_id":"scapia-acc","refund_group_id":null}', 48620, 'INR', '2026-03-14T11:59:14Z', 'CARD',
		        'INCOMING', 'CARD/y/Zomato/₹/486.20/INCOMING/Refund Received!', 'zomato', ?, ?, 5, 1.0,
		        2011, 'Zomato', 954, 'deposit', 'Refund for ___ from Zomato for lunch', 'fold:buy-1')`,
		refundStatus, refundJournal); err != nil {
		t.Fatal(err)
	}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestUI_Detail_RefundPickerAndRefundedBy(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	seedRefundPair(t, u, "ready_to_push", "ready_to_push")

	page := body(t, u.do(t, http.MethodGet, "/admin/ui/staged/ref-1", nil))
	for _, want := range []string{
		`name="refund_of"`,
		`<option value="fold:buy-1" selected>`,
		`exact amount`,
		`value="none"`,
		`the classifier's pick`,
		"fold.money's raw payload",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("refund detail missing %q", want)
		}
	}
	purchase := body(t, u.do(t, http.MethodGet, "/admin/ui/staged/buy-1", nil))
	if !strings.Contains(purchase, "refunded by") || !strings.Contains(purchase, `/admin/ui/staged/ref-1`) {
		t.Errorf("purchase page should list the refund under 'refunded by'")
	}
	if strings.Contains(purchase, `name="refund_of"`) {
		t.Errorf("a purchase shouldn't get a 'refund of' picker")
	}
}

func TestUI_Save_RefundOfChoice(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	seedRefundPair(t, u, "ready_to_push", "ready_to_push")
	confirmed := func() sql.NullString {
		var v sql.NullString
		_ = u.db.DB.QueryRow(`SELECT confirmed_refund_of FROM staged_fold_txns WHERE fold_uuid='ref-1'`).Scan(&v)
		return v
	}
	form := url.Values{"destination_name": {"Scapia Federal Bank Credit Card"}, "source_name": {"Zomato"},
		"description": {"Refund for lunch"}, "refund_of": {"none"}}
	if kind, _ := flashOf(t, u.do(t, http.MethodPost, "/admin/ui/staged/ref-1/save", form)); kind != "ok" {
		t.Fatalf("save flash = %q", kind)
	}
	if v := confirmed(); v.String != "none" {
		t.Errorf("confirmed_refund_of = %v, want none", v)
	}
	form.Set("refund_of", "garbage")
	if kind, msg := flashOf(t, u.do(t, http.MethodPost, "/admin/ui/staged/ref-1/save", form)); kind != "err" || !strings.Contains(msg, "refund of") {
		t.Errorf("bad reference flash = %q %q, want an unresolved 'refund of'", kind, msg)
	}
	if v := confirmed(); v.String != "none" {
		t.Errorf("a bad reference overwrote the stored one: %v", v)
	}
	form.Set("refund_of", "")
	u.do(t, http.MethodPost, "/admin/ui/staged/ref-1/save", form)
	if v := confirmed(); v.Valid {
		t.Errorf("empty choice should clear to NULL, got %v", v)
	}
	// A form without the picker (another client) keeps what's stored.
	if _, err := u.db.DB.Exec(`UPDATE staged_fold_txns SET confirmed_refund_of='fold:buy-1' WHERE fold_uuid='ref-1'`); err != nil {
		t.Fatal(err)
	}
	form.Del("refund_of")
	u.do(t, http.MethodPost, "/admin/ui/staged/ref-1/save", form)
	if v := confirmed(); v.String != "fold:buy-1" {
		t.Errorf("save without the picker changed it to %v", v)
	}
}

func TestUI_Index_RefundPills(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	seedRefundPair(t, u, "ready_to_push", "ready_to_push")
	page := body(t, u.do(t, http.MethodGet, "/admin/ui/?status=all&per_page=50", nil))
	if r := rowHTML(page, "ref-1"); !strings.Contains(r, ">refund<") {
		t.Errorf("refund row lacks the refund pill: %s", r)
	}
	if r := rowHTML(page, "buy-1"); !strings.Contains(r, ">refunded<") {
		t.Errorf("purchase row lacks the refunded pill: %s", r)
	}
}

// The "link in firefly" button on a pushed refund whose purchase is pushed.
func TestUI_LinkButton(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	seedRefundPair(t, u, "pushed", "pushed")

	var linkBody string
	ff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/link-types"):
			_, _ = w.Write([]byte(`{"data":[{"id":"2","attributes":{"name":"Refund"}}]}`))
		case strings.Contains(r.URL.Path, "/transaction-journals/"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/transaction-links") && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			linkBody = string(b)
			_, _ = w.Write([]byte(`{"data":{"id":"44"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ff.Close)
	pusher := integration.NewPusher(u.db, firefly.NewClient(ff.URL, "p", ff.Client()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	h, err := New(u.db.DB, pusher, slog.New(slog.NewTextHandler(io.Discard, nil)), "k", AuthModeBypass)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	lu := &uiTestHarness{server: srv, db: u.db}

	detail := body(t, lu.do(t, http.MethodGet, "/admin/ui/staged/ref-1", nil))
	if !strings.Contains(detail, "link in firefly as a refund") {
		t.Fatalf("pushed pair should offer the link button")
	}
	if kind, msg := flashOf(t, lu.do(t, http.MethodPost, "/admin/ui/staged/ref-1/link", url.Values{})); kind != "ok" || !strings.Contains(msg, "link 44") {
		t.Fatalf("link flash = %q %q", kind, msg)
	}
	if !strings.Contains(linkBody, `"inward_id":6001`) || !strings.Contains(linkBody, `"outward_id":5001`) {
		t.Errorf("link body = %s, want inward 6001 (refund), outward 5001 (purchase)", linkBody)
	}
	after := body(t, lu.do(t, http.MethodGet, "/admin/ui/staged/ref-1", nil))
	if !strings.Contains(after, "linked in firefly (link 44)") || strings.Contains(after, "link in firefly as a refund") {
		t.Errorf("after linking, the page should show the link and drop the button")
	}
}

// A busy merchant has hundreds of larger orders; the picker lists every
// exact one but only the five most recent larger ones.
func TestUI_RefundPicker_CapsLargerCandidates(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	seedRefundPair(t, u, "ready_to_push", "ready_to_push")
	for i := 1; i <= 8; i++ {
		if _, err := u.db.DB.Exec(`
			INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration,
			    merchant_extracted, status, proposed_source_account_id, proposed_destination_account_id, proposed_description)
			VALUES (?, '{"account_id":"scapia-acc"}', ?, 'INR', ?, 'CARD', 'OUTGOING', 'CARD/x/Zomato/x/OUTGOING', 'zomato',
			        'ready_to_push', 954, 11, ?)`,
			"big-"+string(rune('0'+i)), 90000+int64(i), "2026-03-0"+string(rune('0'+i))+"T07:00:00Z", "order "+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	page := body(t, u.do(t, http.MethodGet, "/admin/ui/staged/ref-1", nil))
	if n := strings.Count(page, "larger — a partial refund?"); n != 5 {
		t.Errorf("larger candidates listed = %d, want 5", n)
	}
	if !strings.Contains(page, `<option value="fold:buy-1" selected>`) || !strings.Contains(page, "1 day before") {
		t.Errorf("the exact purchase (1 day before) should still be listed and selected")
	}
}

// A refund with no purchase chosen yet is badged for picking; one with a
// purchase (or marked none) isn't.
func TestUI_Index_RefundNeedsPickBadge(t *testing.T) {
	u := newUITestHarness(t, AuthModeBypass)
	seedRefundPair(t, u, "ready_to_push", "ready_to_push")
	page := body(t, u.do(t, http.MethodGet, "/admin/ui/?status=all&per_page=50", nil))
	if r := rowHTML(page, "ref-1"); strings.Contains(r, "pick purchase") || !strings.Contains(r, ">refund<") {
		t.Errorf("a refund with its purchase shouldn't need a pick: %s", r)
	}
	if _, err := u.db.DB.Exec(`UPDATE staged_fold_txns SET proposed_refund_of = NULL WHERE fold_uuid = 'ref-1'`); err != nil {
		t.Fatal(err)
	}
	page = body(t, u.do(t, http.MethodGet, "/admin/ui/?status=all&per_page=50", nil))
	if r := rowHTML(page, "ref-1"); !strings.Contains(r, "refund · pick purchase") {
		t.Errorf("a refund with no purchase should be badged for picking: %s", r)
	}
	if _, err := u.db.DB.Exec(`UPDATE staged_fold_txns SET confirmed_refund_of = 'none' WHERE fold_uuid = 'ref-1'`); err != nil {
		t.Fatal(err)
	}
	page = body(t, u.do(t, http.MethodGet, "/admin/ui/?status=all&per_page=50", nil))
	if r := rowHTML(page, "ref-1"); strings.Contains(r, "pick purchase") {
		t.Errorf("a refund marked none shouldn't need a pick: %s", r)
	}
}
