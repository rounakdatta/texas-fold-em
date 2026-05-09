package fold

import (
	"encoding/json"
	"fmt"
	"time"
)

// AccessToken is the bundle the broker hands us when we ask. Mirrors
// `main.AccessToken` shape but lives here to avoid a circular import
// (the fold subpackage cannot depend on package main).
type AccessToken struct {
	AccessToken string
	DeviceHash  string
	UserUUID    string
	ExpiresAt   time.Time
}

// Transaction is a single fold-side transaction. We model the subset
// of fields the staging layer reads directly (UUID, amount, mode,
// narration, type, timestamp). Fold returns more — account_id,
// merchant, category, current_balance — that the *classifier* feeds
// to the LLM but no Go code reads structurally; those live in
// raw_payload, captured verbatim via ListTransactionsData's custom
// unmarshal.
//
// Money fields (Amount, SourceAmount) come back as JSON numbers, not
// strings — fold's API differs from firefly here. We use float64 in
// the wire model and convert to integer paise at the SQL boundary.
// For our use-case (INR amounts that occasionally have 2 decimals)
// the float64 round-trip is safe within ±0.01.
type Transaction struct {
	UUID           string  `json:"uuid"`
	Amount         float64 `json:"amount"`
	SourceAmount   float64 `json:"source_amount"`
	Currency       string  `json:"currency"`
	SourceCurrency string  `json:"source_currency"`
	TxnTimestamp   string  `json:"txn_timestamp"` // RFC3339 UTC
	Mode           string  `json:"mode"`          // CARD | UPI | OTHERS | …
	Type           string  `json:"type"`          // INCOMING | OUTGOING
	Narration      string  `json:"narration"`
}

// ListTransactionsResponse mirrors the JSON shape of
// GET /api/v3/users/{userId}/transactions.
type ListTransactionsResponse struct {
	Data ListTransactionsData `json:"data"`
	Meta struct {
		RequestID string `json:"request_id"`
		Timestamp string `json:"timestamp"`
		URI       string `json:"uri"`
	} `json:"meta"`
}

// ListTransactionsData carries the parsed transactions plus, in
// parallel, the verbatim JSON bytes for each transaction, plus the
// opaque pagination cursor fold attaches to every page.
//
// RawTransactions is the source of truth for raw_payload persistence:
// passing the typed Transaction back through json.Marshal would drop
// every field we don't model (account_id, merchant, category, …),
// silently starving the classifier's LLM of grounding signals. By
// holding onto the bytes that came off the wire, we let the classifier
// see exactly what fold sent.
//
// After is fold's own next-page cursor. Pass it as `after=...` on the
// following request to get the page strictly older than the current
// one. Empty string means "this was the last page". The format is
// opaque (currently base64 of `DESC:::time:::<ts>:::NULL:::<uuid>`)
// — never construct one locally, always echo what fold supplied.
type ListTransactionsData struct {
	Transactions    []Transaction     `json:"transactions"`
	After           string            `json:"after"`
	RawTransactions []json.RawMessage `json:"-"`
}

// UnmarshalJSON populates Transactions (typed), RawTransactions
// (verbatim bytes) and After (pagination cursor) from the same wire
// payload.
func (d *ListTransactionsData) UnmarshalJSON(b []byte) error {
	var aux struct {
		Transactions []json.RawMessage `json:"transactions"`
		After        string            `json:"after"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	d.After = aux.After
	d.RawTransactions = aux.Transactions
	d.Transactions = make([]Transaction, len(aux.Transactions))
	for i, raw := range aux.Transactions {
		if err := json.Unmarshal(raw, &d.Transactions[i]); err != nil {
			return fmt.Errorf("fold: transaction %d: %w", i, err)
		}
	}
	return nil
}
