package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rounakdatta/texas-fold-em/internal/integration"
	"github.com/rounakdatta/texas-fold-em/internal/integration/firefly"
)

// catHarness: a fold whose deck can make a category in a fake Firefly that
// keeps its own category list (which a test can change, break, or fill with a
// name fold hasn't heard of yet). Names are made up.
type catHarness struct {
	db  *integration.DB
	srv *httptest.Server

	mu       sync.Mutex
	cats     []firefly.Category // what Firefly has
	creates  []string           // names POSTed to /api/v1/categories
	down     bool               // Firefly answers everything with a 500
	catsDown bool               // only its category list fails
	nextID   int
	ffURL    string
}

func newCatHarness(t *testing.T, readOnly bool) *catHarness {
	t.Helper()
	idb, err := integration.Open(context.Background(), filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = idb.Close() })
	ch := &catHarness{db: idb, nextID: 901}
	mustExec(t, idb, `INSERT INTO firefly_accounts (firefly_id, name, type, active, raw_payload) VALUES
		(10, 'Harbour Bank', 'asset', 1, '{}'), (30, 'Corner Bakery', 'expense', 1, '{}')`)
	mustExec(t, idb, `
		INSERT INTO firefly_txns (firefly_id, group_id, txn_type, amount_paise, currency, date,
		    source_account_id, source_account_name, destination_account_id, destination_account_name,
		    destination_account_name_normalized, category_id, category_name, description, tags_json, notes)
		VALUES (5001, 5001, 'withdrawal', 12000, 'INR', '2025-11-02T09:00:00Z', 10, 'Harbour Bank',
		        30, 'Corner Bakery', 'corner bakery', 9, 'Groceries', 'Bread and eggs', '[]', '')`)

	ff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch.mu.Lock()
		defer ch.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.api+json")
		if ch.down {
			http.Error(w, `{"message":"down"}`, http.StatusInternalServerError)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/categories":
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ch.creates = append(ch.creates, body.Name)
			for _, c := range ch.cats {
				if strings.EqualFold(c.Attributes.Name, body.Name) {
					http.Error(w, `{"message":"The name has already been taken."}`, http.StatusUnprocessableEntity)
					return
				}
			}
			c := firefly.Category{ID: fmt.Sprint(ch.nextID), Type: "categories", Attributes: firefly.CategoryAttribs{Name: body.Name}}
			ch.nextID++
			ch.cats = append(ch.cats, c)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": c})
		case r.URL.Path == "/api/v1/categories":
			if ch.catsDown {
				http.Error(w, `{"message":"categories unavailable"}`, http.StatusInternalServerError)
				return
			}
			resp := firefly.CategoryListResponse{Data: ch.cats}
			resp.Meta.Pagination.CurrentPage, resp.Meta.Pagination.TotalPages = 1, 1
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/api/v1/accounts":
			resp := firefly.AccountListResponse{}
			resp.Meta.Pagination.CurrentPage, resp.Meta.Pagination.TotalPages = 1, 1
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ff.Close)
	ch.ffURL = ff.URL
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := firefly.NewClient(ff.URL, "p", ff.Client())
	h, err := New(idb.DB, integration.NewPusher(idb, fc, log, readOnly), log, "admin-key", AuthModeBypass)
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}
	h.SetFireflyAccountsSyncer(integration.NewFireflyAccountsSyncer(idb, fc, log))
	mux := http.NewServeMux()
	h.Mount(mux)
	ch.srv = httptest.NewServer(mux)
	t.Cleanup(ch.srv.Close)
	return ch
}

func (ch *catHarness) create(t *testing.T, name string) (int, categoryReply, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequest(http.MethodPost, ch.srv.URL+"/admin/ui/api/categories", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fold-UI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var rep categoryReply
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &rep)
	_ = json.Unmarshal(raw, &msg)
	return resp.StatusCode, rep, msg.Message
}

func (ch *catHarness) createsSeen() []string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return append([]string(nil), ch.creates...)
}

func (ch *catHarness) mirrored(t *testing.T) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	rows, err := ch.db.DB.Query(`SELECT firefly_id, name FROM firefly_categories`)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n string
		_ = rows.Scan(&id, &n)
		out[id] = n
	}
	return out
}

func TestCategories_ANewOneIsMadeInFireflyAndLandsOnTheCard(t *testing.T) {
	ch := newCatHarness(t, false)
	mustExec(t, ch.db, `
		INSERT INTO staged_fold_txns (fold_uuid, raw_payload, amount_paise, currency, txn_timestamp, mode, type, narration,
		    merchant_extracted, status, classifier_tier, classifier_confidence,
		    proposed_source_account_id, proposed_destination_account_id, proposed_category_id, proposed_description)
		VALUES ('p1', '{}', 45000, 'INR', '2026-01-05T10:00:00Z', 'UPI', 'OUTGOING', 'UPI-CLAY STUDIO',
		        'clay studio', 'needs_review', 3, 0.6, 10, 30, 9, 'Pottery class')`)

	code, rep, msg := ch.create(t, "  Pottery   Classes ")
	if code != 200 || !rep.Created || rep.Name != "Pottery Classes" || rep.ID != 901 {
		t.Fatalf("create = %d %+v %q; want a new category 901 'Pottery Classes'", code, rep, msg)
	}
	if got := ch.createsSeen(); len(got) != 1 || got[0] != "Pottery Classes" {
		t.Fatalf("firefly was asked to create %q; want exactly [Pottery Classes], spaces tidied", got)
	}
	if m := ch.mirrored(t); m[901] != "Pottery Classes" {
		t.Errorf("fold's copy of the categories = %v; the new one must be pickable before the next sync", m)
	}

	resp, err := http.Get(ch.srv.URL + "/admin/ui/api/options")
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	var o struct {
		Categories []string `json:"categories"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&o)
	resp.Body.Close()
	found := false
	for _, c := range o.Categories {
		found = found || c == "Pottery Classes"
	}
	if !found {
		t.Errorf("the picker's categories %v don't offer the one just made", o.Categories)
	}

	req, _ := http.NewRequest(http.MethodPost, ch.srv.URL+"/admin/ui/api/rows/p1/edit", strings.NewReader(`{"category":"pottery classes"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fold-UI", "1")
	er, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	var a actionResponse
	_ = json.NewDecoder(er.Body).Decode(&a)
	er.Body.Close()
	if er.StatusCode != 200 || a.Card == nil || a.Card.Category != "Pottery Classes" {
		t.Fatalf("edit = %d, card category %v; a category no transaction uses yet must still show on the card", er.StatusCode, a.Card)
	}
	var cat int64
	_ = ch.db.DB.QueryRow(`SELECT confirmed_category_id FROM staged_fold_txns WHERE fold_uuid='p1'`).Scan(&cat)
	if cat != 901 {
		t.Errorf("confirmed_category_id = %d; push sends the id, so it must be Firefly's 901", cat)
	}
}

func TestCategories_ANameFoldKnowsIsNotMadeAgain(t *testing.T) {
	ch := newCatHarness(t, false)
	code, rep, _ := ch.create(t, "groceries")
	if code != 200 || rep.Created || rep.ID != 9 || rep.Name != "Groceries" {
		t.Fatalf("create(known name) = %d %+v; want the existing 9 'Groceries', not created", code, rep)
	}
	if got := ch.createsSeen(); len(got) != 0 {
		t.Errorf("firefly was asked to create %q for a name it already has", got)
	}
}

func TestCategories_FireflyAlreadyHasItUnderThatName(t *testing.T) {
	ch := newCatHarness(t, false)
	ch.mu.Lock()
	ch.cats = []firefly.Category{{ID: "77", Type: "categories", Attributes: firefly.CategoryAttribs{Name: "Gardening"}}}
	ch.mu.Unlock()
	// fold's copy hasn't caught up yet, so it asks Firefly to create it
	code, rep, msg := ch.create(t, "Gardening")
	if code != 200 || rep.Created || rep.ID != 77 || rep.Name != "Gardening" {
		t.Fatalf("create = %d %+v %q; want Firefly's existing 77 'Gardening' after a refresh", code, rep, msg)
	}
}

func TestCategories_AReadOnlyFoldMakesNothing(t *testing.T) {
	ch := newCatHarness(t, true)
	code, _, msg := ch.create(t, "Pottery Classes")
	if code != http.StatusForbidden || !strings.Contains(msg, "read-only") {
		t.Fatalf("create on a read-only fold = %d %q; want 403 saying so", code, msg)
	}
	if got := ch.createsSeen(); len(got) != 0 {
		t.Errorf("a read-only fold asked firefly to create %q", got)
	}
}

func TestCategories_FireflyDownSaysSoAndRecordsNothing(t *testing.T) {
	ch := newCatHarness(t, false)
	ch.mu.Lock()
	ch.down = true
	ch.mu.Unlock()
	code, _, msg := ch.create(t, "Pottery Classes")
	if code != http.StatusBadGateway || msg != "Couldn’t reach Firefly — try again in a moment." {
		t.Fatalf("create while firefly is down = %d %q", code, msg)
	}
	if m := ch.mirrored(t); len(m) != 0 {
		t.Errorf("fold recorded %v for a category Firefly never made", m)
	}
}

func TestCategories_ANameIsNeeded(t *testing.T) {
	ch := newCatHarness(t, false)
	if code, _, msg := ch.create(t, "   "); code != http.StatusBadRequest || msg == "" {
		t.Fatalf("create(blank) = %d %q; want 400 with a reason", code, msg)
	}
	if got := ch.createsSeen(); len(got) != 0 {
		t.Errorf("a blank name reached firefly: %q", got)
	}
}

func TestCategories_TheMirrorFollowsFirefly(t *testing.T) {
	ch := newCatHarness(t, false)
	syncer := integration.NewFireflyAccountsSyncer(ch.db, firefly.NewClient(ch.ffURL, "p", http.DefaultClient), slog.New(slog.NewTextHandler(io.Discard, nil)))
	set := func(cats ...firefly.Category) {
		ch.mu.Lock()
		ch.cats = cats
		ch.mu.Unlock()
	}
	cat := func(id, name string) firefly.Category {
		return firefly.Category{ID: id, Type: "categories", Attributes: firefly.CategoryAttribs{Name: name}}
	}

	set(cat("1", "Pottery Classes"), cat("2", "Gardening"))
	if rep, err := syncer.Sync(context.Background()); err != nil || rep.Categories != 2 {
		t.Fatalf("sync = %+v, %v; want 2 categories mirrored", rep, err)
	}
	set(cat("2", "Garden Supplies"))
	if _, err := syncer.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if m := ch.mirrored(t); len(m) != 1 || m[2] != "Garden Supplies" {
		t.Errorf("after a rename and a delete in Firefly, fold's copy = %v; want only {2: Garden Supplies}", m)
	}

	// Firefly's category list failing must not fail the accounts, nor
	// empty fold's copy.
	ch.mu.Lock()
	ch.catsDown = true
	ch.mu.Unlock()
	rep, err := syncer.Sync(context.Background())
	if err != nil || rep.CategoriesError == "" {
		t.Fatalf("sync with the category list down = %+v, %v; want the accounts synced and the category failure reported", rep, err)
	}
	if m := ch.mirrored(t); len(m) != 1 || m[2] != "Garden Supplies" {
		t.Errorf("a failed category sync changed fold's copy to %v; the previous copy must stay", m)
	}
}
