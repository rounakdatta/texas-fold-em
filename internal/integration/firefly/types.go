package firefly

// Types covering the subset of Firefly III's API we touch. The full
// schema is much larger; we keep our models tight (only the fields the
// classifier uses) so a Firefly schema bump in an unrelated area can't
// break our parse.
//
// All IDs are strings in the firefly wire format ("123") rather than
// ints — we keep them that way until we drop into SQL, where we parse
// with strconv.ParseInt. Keeping them strings here avoids subtle
// JSON-number truncation and makes empty-vs-zero unambiguous.

// Pagination is the meta block firefly returns on every list endpoint.
type Pagination struct {
	Total       int `json:"total"`
	Count       int `json:"count"`
	PerPage     int `json:"per_page"`
	CurrentPage int `json:"current_page"`
	TotalPages  int `json:"total_pages"`
}

// Meta is the wrapping meta object containing pagination.
type Meta struct {
	Pagination Pagination `json:"pagination"`
}

// User is the subset of /api/v1/about/user we surface (everything else
// — created_at, role, etc. — is logged but not stored).
type User struct {
	Email   string `json:"email"`
	Role    string `json:"role"`
	Blocked bool   `json:"blocked"`
}

type aboutUserEnvelope struct {
	Data struct {
		Attributes User `json:"attributes"`
	} `json:"data"`
}

// TransactionJournal is one *journal* line within a transaction group.
// A withdrawal with one source + one destination = one journal. A split
// with three destinations = three journals under the same group_id.
//
// All money fields are strings in firefly's wire format ("70.00"); the
// classifier code parses them as decimals when storing.
type TransactionJournal struct {
	JournalID       string   `json:"transaction_journal_id"`
	Type            string   `json:"type"` // withdrawal | deposit | transfer
	Amount          string   `json:"amount"`
	CurrencyCode    string   `json:"currency_code"`
	Date            string   `json:"date"` // RFC3339 with offset
	SourceID        string   `json:"source_id"`
	SourceName      string   `json:"source_name"`
	DestinationID   string   `json:"destination_id"`
	DestinationName string   `json:"destination_name"`
	CategoryID      string   `json:"category_id"`
	CategoryName    string   `json:"category_name"`
	BudgetID        string   `json:"budget_id"`
	BudgetName      string   `json:"budget_name"`
	Description     string   `json:"description"`
	Tags            []string `json:"tags"`
	ExternalID      string   `json:"external_id"`
	// Notes is firefly's free-text per-transaction notes field. Months
	// of manual work has gone into populating it with raw fold narrations
	// (e.g. `CARD/.../SHREE VINAYAKA ENTE/...`) — surfacing that into our
	// mirror gives both BM25 (Tier-2) and the LLM (Tier-3) the bridging
	// signal between fold's truncated merchant string and firefly's
	// canonical destination_account_name.
	Notes           string   `json:"notes"`
	// Foreign side of a cross-currency transaction (e.g. "7.24" USD);
	// empty for domestic ones. Read so the review form can pre-fill a
	// correction from firefly's live values.
	ForeignAmount       string `json:"foreign_amount"`
	ForeignCurrencyCode string `json:"foreign_currency_code"`
}

// TransactionGroupResponse is the body returned by GET /api/v1/transactions/{id}.
// Singular wrapper of TransactionGroup (same shape as list endpoint's element).
type TransactionGroupResponse struct {
	Data TransactionGroup `json:"data"`
}

// TransactionGroup is the outer wrapper firefly returns. data[i].attributes
// holds the journal list — usually just one, sometimes more for splits.
type TransactionGroup struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"` // always "transactions" from firefly
	Attributes TransactionGroupAttribs `json:"attributes"`
}

// TransactionGroupAttribs is exported so callers in sibling packages
// (test fixtures, sync orchestrators) can construct groups directly
// without reflection or json round-trips.
type TransactionGroupAttribs struct {
	GroupTitle   string               `json:"group_title"`
	Transactions []TransactionJournal `json:"transactions"`
}

// TransactionListResponse is the body returned by GET /api/v1/transactions.
type TransactionListResponse struct {
	Data []TransactionGroup `json:"data"`
	Meta Meta               `json:"meta"`
}

// Account is the subset of an account's attributes we keep. We use this
// to validate that classifier proposals reference accounts that still
// exist (a defensive check before push).
type Account struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"` // always "accounts"
	Attributes AccountAttribs   `json:"attributes"`
}

type AccountAttribs struct {
	Name           string `json:"name"`
	Type           string `json:"type"` // asset | expense | revenue | etc.
	Active         bool   `json:"active"`
	CurrencyCode   string `json:"currency_code"`
	AccountRole    string `json:"account_role"`
	// AccountNumber often holds the card/account number; for the
	// deterministic source resolver this is where a card's last-four
	// digits live (firefly asset *names* rarely carry them).
	AccountNumber  string `json:"account_number"`
}

type AccountListResponse struct {
	Data []Account `json:"data"`
	Meta Meta      `json:"meta"`
}

// Category and Budget have very similar shapes — keep the duplication
// flat for clarity rather than reaching for generics.

type Category struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Attributes CategoryAttribs   `json:"attributes"`
}

type CategoryAttribs struct {
	Name string `json:"name"`
}

type CategoryListResponse struct {
	Data []Category `json:"data"`
	Meta Meta       `json:"meta"`
}

type Budget struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Attributes BudgetAttribs  `json:"attributes"`
}

type BudgetAttribs struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

type BudgetListResponse struct {
	Data []Budget `json:"data"`
	Meta Meta     `json:"meta"`
}
