package fold

import "time"

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
// of fields the classifier and staging layer actually use; fold's
// response carries many more (current_balance, category from fold,
// merchant logos, etc.) that we deliberately ignore.
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
	Data struct {
		Transactions []Transaction `json:"transactions"`
	} `json:"data"`
	Meta struct {
		RequestID string `json:"request_id"`
		Timestamp string `json:"timestamp"`
		URI       string `json:"uri"`
	} `json:"meta"`
}
