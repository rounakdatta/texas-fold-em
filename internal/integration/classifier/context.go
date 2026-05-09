package classifier

import (
	"context"
	"database/sql"
	"encoding/json"
)

// FoldAccountRef carries the human-readable details of a fold-side
// asset (credit card or bank account) that the classifier joins onto
// the staged transaction's raw_payload.account_id. This is the
// keystone signal for source-account inference: without it, the LLM
// sees only an opaque UUID; with it, "Tata Neu Plus 8943 (HDFC,
// RuPay)" — directly text-matchable against firefly's asset list.
type FoldAccountRef struct {
	ID        string // fold's per-account UUID
	Kind      string // "BANK" | "CREDIT_CARD"
	Name      string // composed display name, e.g. "HDFC Tata Neu Plus ****8943"
	Provider  string // "HDFC", "Axis Bank"
	Network   string // credit-card-only: "Visa", "RuPay"
	LastFour  string
	IsClosed  bool
}

// lookupFoldAccountForStaged extracts account_id from the staged row's
// raw_payload and returns the matching fold_accounts row, if any.
// Returns (nil, nil) when raw_payload has no account_id, or when the
// id isn't yet mirrored. Callers should always check for nil.
func lookupFoldAccountForStaged(ctx context.Context, db *sql.DB, rawPayload string) (*FoldAccountRef, error) {
	if rawPayload == "" {
		return nil, nil
	}
	var probe struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal([]byte(rawPayload), &probe); err != nil {
		return nil, nil // non-JSON or older shape — silently skip
	}
	if probe.AccountID == "" {
		return nil, nil
	}
	var (
		ref   FoldAccountRef
		prov  sql.NullString
		net   sql.NullString
		lf    sql.NullString
		closedInt int
	)
	err := db.QueryRowContext(ctx, `
		SELECT fold_account_id, kind, name, provider, network, last_four, is_closed
		FROM fold_accounts
		WHERE fold_account_id = ?
	`, probe.AccountID).Scan(&ref.ID, &ref.Kind, &ref.Name, &prov, &net, &lf, &closedInt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if prov.Valid {
		ref.Provider = prov.String
	}
	if net.Valid {
		ref.Network = net.String
	}
	if lf.Valid {
		ref.LastFour = lf.String
	}
	ref.IsClosed = closedInt == 1
	return &ref, nil
}

// Context-gathering helpers for Tier-3 LLM RAG. None of these touch
// firefly's API — every query reads our local mirror in firefly_txns.
// The mirror gets refreshed by /admin/firefly/sync; on a fresh deploy
// these queries return empty until that's run once.

// AccountRef is a (id, name) pair used to seed Tier-3 prompts with
// the user's asset / expense / revenue account inventory and the
// hallucination-guard whitelist.
type AccountRef struct {
	ID   int64
	Name string
}

// listAssetAccounts returns the user's own bank/card/wallet accounts
// — the asset side of the firefly book. We derive it as:
//
//	source side of withdrawals  (user paid → user's account is source)
//	+ destination side of deposits (someone paid user → user's account is destination)
//
// Deduped by id. For a typical user this is ≤ 20 entries — small
// enough to inline in every Tier-3 prompt.
func listAssetAccounts(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name FROM (
		    SELECT source_account_id      AS id, source_account_name      AS name
		    FROM firefly_txns
		    WHERE txn_type = 'withdrawal' AND source_account_id IS NOT NULL AND source_account_name IS NOT NULL
		    UNION
		    SELECT destination_account_id AS id, destination_account_name AS name
		    FROM firefly_txns
		    WHERE txn_type = 'deposit'    AND destination_account_id IS NOT NULL AND destination_account_name IS NOT NULL
		)
		GROUP BY id
		ORDER BY name COLLATE NOCASE
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountRef
	for rows.Next() {
		var a AccountRef
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// listRevenueAccounts is the deposit-side counterpart: the source side
// of deposits (employer, refunds, etc.). The LLM uses these when the
// fold transaction is INCOMING and we need to pick where the money
// came from.
func listRevenueAccounts(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT source_account_id, source_account_name
		FROM firefly_txns
		WHERE txn_type = 'deposit' AND source_account_id IS NOT NULL AND source_account_name IS NOT NULL
		ORDER BY source_account_name COLLATE NOCASE
	`)
}

// listExpenseAccounts is the withdrawal-side merchant inventory.
// Used as the destination_account candidate set for OUTGOING fold
// transactions.
func listExpenseAccounts(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT destination_account_id, destination_account_name
		FROM firefly_txns
		WHERE txn_type = 'withdrawal' AND destination_account_id IS NOT NULL AND destination_account_name IS NOT NULL
		ORDER BY destination_account_name COLLATE NOCASE
	`)
}

// listCategories / listBudgets are the dimension tables, also derived
// from firefly_txns.
func listCategories(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT category_id, category_name
		FROM firefly_txns
		WHERE category_id IS NOT NULL AND category_name IS NOT NULL
		ORDER BY category_name COLLATE NOCASE
	`)
}

func listBudgets(ctx context.Context, db *sql.DB) ([]AccountRef, error) {
	return listDistinct(ctx, db, `
		SELECT DISTINCT budget_id, budget_name
		FROM firefly_txns
		WHERE budget_id IS NOT NULL AND budget_name IS NOT NULL
		ORDER BY budget_name COLLATE NOCASE
	`)
}

// fireflyTxnTypeFor maps fold's INCOMING/OUTGOING to firefly's
// withdrawal/deposit. Mirror of integration.foldTypeToFireflyType,
// duplicated here to avoid an import cycle with package integration
// (whose tests import the classifier subpackage).
func fireflyTxnTypeFor(foldType string) string {
	switch foldType {
	case "INCOMING":
		return "deposit"
	case "OUTGOING":
		return "withdrawal"
	default:
		return "withdrawal" // safety default — rare on fold
	}
}

func listDistinct(ctx context.Context, db *sql.DB, query string) ([]AccountRef, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountRef
	for rows.Next() {
		var a AccountRef
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
