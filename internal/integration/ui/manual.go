package ui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Adding a transaction fold never saw.
//
// Reconciling a card against its statement turns up lines fold has no row
// for: charges from before the card was linked to fold, a merchant whose
// alert never arrived, a reversal or refund credit, a fee or waiver. Those
// still have to reach firefly for the balance to match, and the review UI is
// where every other correction happens — so they are added here, as MANUAL
// staged rows, and pushed like any other row.
//
// A MANUAL row:
//   - gets fold_uuid "manual-<32 hex>", which push uses as firefly's
//     external_id, so the usual dedup makes a re-push a no-op;
//   - carries the statement line as its narration (firefly notes);
//   - is skipped by the classifier (see fetchStagedForClassify);
//   - lands in ready_to_push, because every field on it is a human choice.

func (h *Handler) handleNewForm(w http.ResponseWriter, r *http.Request) {
	destAccounts, _ := h.listAccountsByKind(r.Context(), "destination")
	srcAccounts, _ := h.listAccountsByKind(r.Context(), "source")
	ownOpts, payeeOpts, payerOpts := h.nameLists(r.Context())
	categories, _ := h.listCategories(r.Context())
	budgets, _ := h.listBudgets(r.Context())
	allTags, _ := h.listAllTags(r.Context())
	date, _ := istDateTime(time.Now())
	h.render(w, h.newTmpl, map[string]any{
		"Title":           "Add a transaction",
		"Status":          "new",
		"Nav":             "new",
		"Date":            date,
		"DestOptions":     destAccounts,
		"SourceOptions":   srcAccounts,
		"OwnOptions":      ownOpts,
		"PayeeOptions":    payeeOpts,
		"PayerOptions":    payerOpts,
		"CategoryOptions": categories,
		"BudgetOptions":   budgets,
		"TagLibrary":      allTags,
		"Flash":           flashFromCookie(r, w),
	})
}

func (h *Handler) handleNewCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	fail := func(msg string) {
		h.flashErr(w, msg)
		http.Redirect(w, r, "/new", http.StatusSeeOther)
	}

	txnType := strings.TrimSpace(r.FormValue("txn_type"))
	foldType := map[string]string{"withdrawal": "OUTGOING", "transfer": "OUTGOING", "deposit": "INCOMING"}[txnType]
	if foldType == "" {
		fail("pick a type: withdrawal, deposit or transfer")
		return
	}
	amount, err := parseMoneyToPaise(r.FormValue("amount"))
	if err != nil || amount <= 0 {
		fail(fmt.Sprintf("amount %q is not a positive number", r.FormValue("amount")))
		return
	}
	when, err := parseISTForm(r.FormValue("date"), r.FormValue("time"))
	if err != nil {
		fail(err.Error())
		return
	}
	if strings.TrimSpace(r.FormValue("description")) == "" {
		fail("a description is required (it becomes the firefly title)")
		return
	}
	var fxPaise, fxCur any
	if cur := strings.ToUpper(strings.TrimSpace(r.FormValue("foreign_currency"))); cur != "" && cur != "INR" {
		p, err := parseMoneyToPaise(r.FormValue("foreign_amount"))
		if err != nil || p <= 0 {
			fail(fmt.Sprintf("foreign amount %q is not a positive number", r.FormValue("foreign_amount")))
			return
		}
		fxPaise, fxCur = p, cur
	}
	narration := strings.TrimSpace(r.FormValue("narration"))
	if narration == "" {
		narration = "manual entry"
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		fail("could not generate an id: " + err.Error())
		return
	}
	uuid := "manual-" + hex.EncodeToString(b)

	if _, err := h.db.ExecContext(r.Context(), `
		INSERT INTO staged_fold_txns (
		    fold_uuid, raw_payload, amount_paise, currency,
		    foreign_amount_paise, foreign_currency, txn_timestamp,
		    mode, type, narration, status, confirmed_txn_type
		) VALUES (?, '{"manual":true}', ?, 'INR', ?, ?, ?, ?, ?, ?, 'ready_to_push', ?)
	`, uuid, amount, fxPaise, fxCur, when, manualMode, foldType, narration, txnType); err != nil {
		fail("could not add the transaction: " + err.Error())
		return
	}

	// Accounts, category, budget, description and tags go through the same
	// resolver as every other row, so names behave identically here. The
	// money/date fields match what was just stored, so they record no
	// override.
	unresolved, err := h.saveEdits(r.Context(), uuid, r.Form)
	if err != nil {
		h.flashErr(w, "added, but saving its fields failed: "+err.Error())
	} else if len(unresolved) > 0 {
		h.flashErr(w, "added, but couldn't resolve: "+strings.Join(unresolved, "; ")+" — fix below before pushing")
	} else {
		h.flashOk(w, "added — review below, then push")
	}
	http.Redirect(w, r, "/transactions/"+uuid, http.StatusSeeOther)
}
