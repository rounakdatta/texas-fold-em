package classifier

// status.go — what the engine is doing, in numbers a person can check: which
// model, whether it is answering, how much it has learned from, and how far
// the re-suggestion of older cards has got. Served at GET /api/engine; the
// deck's side panel reads it.

import (
	"context"
	"net/url"
	"time"

	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// EngineStatus is the engine's state.
type EngineStatus struct {
	Configured bool       `json:"configured"`
	Model      string     `json:"model,omitempty"`
	ModelName  string     `json:"modelName,omitempty"` // "Claude Opus 5.5"
	Host       string     `json:"host,omitempty"`      // the gateway's host; never the key
	Reasoning  string     `json:"reasoning"`
	Version    int        `json:"version"`
	Health     llm.Health `json:"health"`
	Calls      struct {
		OK           int            `json:"ok"`
		Failed       int            `json:"failed"`
		AvgMS        int64          `json:"avgMs,omitempty"`        // over the recent calls that answered
		PromptTokens int            `json:"promptTokens,omitempty"` // mean, same calls
		Recent       []llm.CallStat `json:"recent,omitempty"`
	} `json:"calls"`
	// Feedback is what the engine learns from, by action; Feedback24h the
	// decisions of the last day.
	Feedback    map[string]int `json:"feedback"`
	Feedback24h int            `json:"feedback24h"`
	Waiting     struct {
		Total          int `json:"total"`
		Current        int `json:"current"` // suggested by this engine version
		Older          int `json:"older"`   // queued for a second look
		Decided        int `json:"decided"` // a person already chose something on it
		Held           int `json:"held"`
		HeldByFold     int `json:"heldByFold"`
		Resuggested24h int `json:"resuggested24h"`
		Improved24h    int `json:"improved24h"` // …and now say something different
	} `json:"waiting"`
	LastResuggest *ResuggestReport `json:"lastResuggest,omitempty"`
	LastEval      *EvalHeadline    `json:"lastEval,omitempty"`
}

// EvalHeadline is an evaluation's result in one line per field.
type EvalHeadline struct {
	ID         string             `json:"id"`
	Running    bool               `json:"running"`
	FinishedAt time.Time          `json:"finishedAt,omitempty"`
	Model      string             `json:"model"`
	Reasoning  string             `json:"reasoning"`
	Done       int                `json:"done"`
	Sampled    int                `json:"sampled"`
	Engine     map[string]float64 `json:"engine"`
	Baseline   map[string]float64 `json:"baseline"`
}

// Status reads the engine's state. Every count is best-effort.
func (c *Classifier) Status(ctx context.Context) EngineStatus {
	var s EngineStatus
	s.Version = EngineVersion
	s.Reasoning = "none"
	if c.llm != nil {
		s.Configured = true
		s.Model = c.llm.Model()
		s.ModelName = llm.DisplayName(s.Model)
		if u, err := url.Parse(c.llm.Endpoint()); err == nil {
			s.Host = u.Host
		}
		if e := c.llm.ReasoningEffort(); e != "" {
			s.Reasoning = e
		}
		s.Health = c.llm.Health()
		recent, ok, failed := c.llm.RecentCalls()
		s.Calls.OK, s.Calls.Failed = ok, failed
		var ms int64
		var tokens, n int
		for _, cs := range recent {
			if cs.OK {
				ms += cs.DurationMS
				tokens += cs.PromptTokens
				n++
			}
		}
		if n > 0 {
			s.Calls.AvgMS, s.Calls.PromptTokens = ms/int64(n), tokens/n
		}
		if len(recent) > 12 {
			recent = recent[:12]
		}
		s.Calls.Recent = recent
	}

	s.Feedback = map[string]int{}
	if rows, err := c.db.QueryContext(ctx, `SELECT action, COUNT(*) FROM review_feedback GROUP BY action`); err == nil {
		for rows.Next() {
			var a string
			var n int
			if rows.Scan(&a, &n) == nil {
				s.Feedback[a] = n
			}
		}
		rows.Close()
	}
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_feedback WHERE datetime(at) > datetime('now', '-1 day')`).Scan(&s.Feedback24h)

	w := &s.Waiting
	_ = c.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN s.classifier_version >= ? THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN s.classifier_version < ? AND `+noHumanDecision+` THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN NOT (`+noHumanDecision+`) AND s.hold_reason IS NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN s.hold_reason IS NOT NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN s.hold_reason IS NOT NULL AND s.hold_by = 'fold' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN datetime(s.resuggested_at) > datetime('now', '-1 day') THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN datetime(s.resuggested_at) > datetime('now', '-1 day') AND s.resuggest_changed = 1 THEN 1 ELSE 0 END), 0)
		FROM staged_fold_txns s
		WHERE s.status IN ('needs_review', 'ready_to_push')`, EngineVersion, EngineVersion).
		Scan(&w.Total, &w.Current, &w.Older, &w.Decided, &w.Held, &w.HeldByFold, &w.Resuggested24h, &w.Improved24h)

	s.LastResuggest = c.LastResuggest()
	if e := c.EvalStatus(); e.ID != "" {
		h := &EvalHeadline{ID: e.ID, Running: e.Running, FinishedAt: e.FinishedAt, Model: e.Model, Reasoning: e.Reasoning,
			Done: e.Done, Sampled: e.Sampled, Engine: map[string]float64{}, Baseline: map[string]float64{}}
		for _, f := range evalFields {
			if v := e.Engine[f]; v != nil && v.Total > 0 {
				h.Engine[f] = v.Rate
			}
			if v := e.Baseline[f]; v != nil && v.Total > 0 {
				h.Baseline[f] = v.Rate
			}
		}
		s.LastEval = h
	}
	return s
}
