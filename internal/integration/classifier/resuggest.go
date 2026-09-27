package classifier

// resuggest.go — the continuous half of the engine: waiting cards get a
// better suggestion as soon as there is something to make it better with.
//
// A suggestion used to be made once, when the transaction arrived, and never
// revisited. So correcting the first of ten identical cards left the other
// nine saying the same wrong thing, and a stronger engine only ever helped
// transactions that arrived after it. Each pass here picks, in order:
//
//   - "learned": a waiting card whose merchant got a correction or a send
//     AFTER its current suggestion was made — the new decision is exactly
//     what the prompt now shows the model;
//   - "engine": a card whose suggestion came from an older engine version
//     (or from the deterministic tiers alone while the model was down).
//
// It only ever touches what no human has decided: a card with any confirmed
// field, a hold, or a manual row is never re-suggested. A failed attempt
// keeps the old suggestion — classifyAndApply's reset-to-pending is right for
// a new row and would blank a waiting card — and backs off before retrying.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
)

// ResuggestReport says what one pass did.
type ResuggestReport struct {
	At         time.Time      `json:"at"`
	Examined   int            `json:"examined"`
	Changed    int            `json:"changed"` // the suggestion now says something different
	Same       int            `json:"same"`    // re-suggested, and it still says the same
	Failed     int            `json:"failed"`  // the model failed; the old suggestion stays
	Reasons    map[string]int `json:"reasons"` // learned / engine
	DurationMS int64          `json:"durationMs"`
}

// noHumanDecision is the SQL guard every re-suggest candidate passes: nothing
// on the row was chosen by a person.
const noHumanDecision = `s.hold_reason IS NULL AND s.mode <> 'MANUAL'
	AND s.confirmed_destination_account_id IS NULL AND s.confirmed_destination_account_name IS NULL
	AND s.confirmed_source_account_id IS NULL AND s.confirmed_source_account_name IS NULL
	AND s.confirmed_category_id IS NULL AND s.confirmed_budget_id IS NULL
	AND s.confirmed_description IS NULL AND s.confirmed_tags_json IS NULL
	AND s.confirmed_txn_type IS NULL AND s.confirmed_refund_of IS NULL`

type resuggestCandidate struct {
	uuid, reason string
}

// resuggestCandidates lists up to limit cards worth another look, learned
// ones first, newest first within each reason.
func (c *Classifier) resuggestCandidates(ctx context.Context, limit int) ([]resuggestCandidate, error) {
	var out []resuggestCandidate
	seen := map[string]bool{}
	pick := func(reason, query string, args ...any) error {
		rows, err := c.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() && len(out) < limit {
			var u string
			if err := rows.Scan(&u); err != nil {
				return err
			}
			if !seen[u] {
				seen[u] = true
				out = append(out, resuggestCandidate{uuid: u, reason: reason})
			}
		}
		return rows.Err()
	}
	// learned: a correction or a send for the same merchant that is newer
	// than both the card's suggestion and its last attempt — so a card is
	// looked at once per new decision, and one the model can't improve
	// isn't asked about again until there is something new to learn. The
	// stamps are to the millisecond (julianday compares them; datetime()
	// would drop the fraction), and a decision made in the very instant a
	// card was suggested counts: it may not be in that suggestion.
	if err := pick("learned", `
		SELECT s.fold_uuid FROM staged_fold_txns s
		WHERE s.status IN ('needs_review', 'ready_to_push') AND `+noHumanDecision+`
		  AND COALESCE(s.merchant_extracted, '') <> ''
		  AND EXISTS (SELECT 1 FROM review_feedback f
		              WHERE f.merchant_key = s.merchant_extracted AND f.fold_uuid <> s.fold_uuid
		                AND f.action IN ('edit', 'send')
		                AND julianday(f.at) >= julianday(COALESCE(s.classified_at, '1970-01-01'))
		                AND (s.resuggested_at IS NULL OR julianday(f.at) > julianday(s.resuggested_at)))
		ORDER BY s.txn_timestamp DESC LIMIT ?`, limit); err != nil {
		return nil, err
	}
	// engine: suggested by an older engine; an attempt that failed waits
	// half a day before the next.
	if len(out) < limit {
		if err := pick("engine", `
			SELECT s.fold_uuid FROM staged_fold_txns s
			WHERE s.status IN ('needs_review', 'ready_to_push') AND `+noHumanDecision+`
			  AND s.classifier_version < ?
			  AND (s.resuggested_at IS NULL OR datetime(s.resuggested_at) < datetime('now', '-12 hours'))
			ORDER BY s.txn_timestamp DESC LIMIT ?`, EngineVersion, limit); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Resuggest runs one pass over up to limit cards. Safe to call while the
// periodic classify runs: that one only touches 'pending' rows.
func (c *Classifier) Resuggest(ctx context.Context, limit int) (ResuggestReport, error) {
	start := time.Now()
	rep := ResuggestReport{At: start, Reasons: map[string]int{}}
	if c.llm == nil {
		return rep, errors.New("resuggest: no LLM configured")
	}
	if limit <= 0 {
		limit = 8
	}
	if h := c.llm.Health(); !h.Available {
		// The model is resting (a limit, a refused key, a run of
		// failures): nothing to gain from a pass until it is back.
		return rep, nil
	}
	cands, err := c.resuggestCandidates(ctx, limit)
	if err != nil {
		return rep, fmt.Errorf("resuggest candidates: %w", err)
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, c.concurrency))
	for _, cand := range cands {
		g.Go(func() error {
			outcome := c.resuggestOne(gctx, cand)
			mu.Lock()
			defer mu.Unlock()
			if outcome == "skipped" {
				return nil
			}
			rep.Examined++
			rep.Reasons[cand.reason]++
			switch outcome {
			case "changed":
				rep.Changed++
			case "same":
				rep.Same++
			default:
				rep.Failed++
			}
			return nil
		})
	}
	_ = g.Wait()
	rep.DurationMS = time.Since(start).Milliseconds()
	c.mu.Lock()
	c.lastResuggest = &rep
	c.mu.Unlock()
	if rep.Examined > 0 {
		c.log.Info("resuggest pass", "examined", rep.Examined, "changed", rep.Changed, "same", rep.Same,
			"failed", rep.Failed, "reasons", rep.Reasons, "duration_ms", rep.DurationMS)
	}
	return rep, nil
}

// resuggestOne re-classifies one card and says what happened: "changed",
// "same", "failed" (the old suggestion kept) or "skipped" (the model is
// resting; the card is left for a later pass).
func (c *Classifier) resuggestOne(ctx context.Context, cand resuggestCandidate) string {
	mark := func() {
		_, _ = c.db.ExecContext(ctx, `UPDATE staged_fold_txns SET resuggested_at = strftime('%Y-%m-%d %H:%M:%f', 'now'), resuggest_reason = ?, resuggest_changed = 0 WHERE fold_uuid = ?`,
			cand.reason, cand.uuid)
	}
	rows, err := c.fetchStagedForClassify(ctx, `fold_uuid = ?`, cand.uuid)
	if err != nil || len(rows) != 1 {
		mark()
		return "failed"
	}
	s := rows[0]
	if !c.llm.Health().Available {
		return "skipped" // resting since the pass began: try it next time
	}
	before, _ := feedback.SnapshotReview(ctx, c.db, s.FoldUUID)
	d, err := c.classifyOne(ctx, s, exclusion{})
	if err != nil || d.Tier != TierLLM && d.Tier != TierRefund {
		// The model failed or declined, and a deterministic fallback is
		// no better than what the card already says: keep it. When the
		// model has just started resting, the card waits its turn rather
		// than being put back hours.
		if !c.llm.Health().Available {
			return "skipped"
		}
		mark()
		return "failed"
	}
	c.resolveOwnSide(ctx, s, &d)
	// Re-check under the guard: a person may have decided this card while
	// the model was thinking.
	var decided int
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM staged_fold_txns s WHERE s.fold_uuid = ? AND NOT (`+noHumanDecision+`)`, s.FoldUUID).Scan(&decided)
	if decided > 0 {
		return "failed"
	}
	if err := c.ApplyDecision(ctx, s.FoldUUID, d); err != nil {
		mark()
		return "failed"
	}
	if d.Tier == TierLLM && len(d.Tags) == 0 {
		// ApplyDecision keeps the old tags when a decision has none (so a
		// fallback to the deterministic tiers doesn't drop the model's). A
		// new model decision saying "no tags" means it: the suggestion is
		// one engine's whole view, not a blend of two.
		_, _ = c.db.ExecContext(ctx, `UPDATE staged_fold_txns SET proposed_tags_json = NULL WHERE fold_uuid = ?`, s.FoldUUID)
	}
	mark()
	after, _ := feedback.SnapshotReview(ctx, c.db, s.FoldUUID)
	if after.Suggested.Equal(before.Suggested) {
		return "same"
	}
	_, _ = c.db.ExecContext(ctx, `UPDATE staged_fold_txns SET resuggest_changed = 1 WHERE fold_uuid = ?`, s.FoldUUID)
	return "changed"
}

// LastResuggest is the most recent pass's report, nil before the first.
func (c *Classifier) LastResuggest() *ResuggestReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastResuggest
}
