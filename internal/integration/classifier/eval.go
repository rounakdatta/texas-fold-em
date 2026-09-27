package classifier

// eval.go — the shadow evaluation: how good are the suggestions, really?
//
// A prompt change, a new model or a different reasoning effort is judged on
// the only ground truth there is: transactions the owner already sent to
// firefly. Each sampled row is classified again exactly as a new arrival
// would be, except that every lookup hides the row itself (its firefly
// journal from the FTS hits, the style samples, the handle history and the
// recurring check; its own feedback; a one-sample merchant lookup that could
// only have been learned from it) — so the engine never sees the answer it
// is graded on. The answer is the firefly row as it stands now, after any
// edit made there.
//
// Nothing is written. The old suggestion stored on each row (what the engine
// of the day proposed before the owner corrected it) is scored the same way,
// so every report carries its own baseline.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/rounakdatta/texas-fold-em/internal/integration/feedback"
	"github.com/rounakdatta/texas-fold-em/internal/integration/llm"
)

// EvalOptions picks what an evaluation runs on.
type EvalOptions struct {
	Limit       int      `json:"limit"`       // rows, most recently sent first (default 30, at most 200)
	Offset      int      `json:"offset"`      // skip this many of the most recent
	Model       string   `json:"model"`       // "" = the configured model
	Reasoning   string   `json:"reasoning"`   // "" = the configured effort; "none" turns it off
	UUIDs       []string `json:"uuids"`       // specific sent rows instead of the most recent
	Concurrency int      `json:"concurrency"` // parallel calls (default 2, at most 4)
}

// FieldScore is one field's tally.
type FieldScore struct {
	Right int     `json:"right"`
	Total int     `json:"total"`
	Rate  float64 `json:"rate"`
}

func (f *FieldScore) add(ok bool) {
	f.Total++
	if ok {
		f.Right++
	}
	f.Rate = float64(f.Right) / float64(f.Total)
}

// EvalRow is one graded transaction.
type EvalRow struct {
	UUID       string                `json:"uuid"`
	Narration  string                `json:"narration"`
	Truth      feedback.ReviewValues `json:"truth"`
	Got        feedback.ReviewValues `json:"got"`
	Before     feedback.ReviewValues `json:"before"` // the stored suggestion, the baseline
	Tier       Tier                  `json:"tier"`
	Confidence float64               `json:"confidence"`
	Right      map[string]bool       `json:"right"`
	TitleFit   string                `json:"titleFit"` // exact | fits | overlap | miss
	// NewPayee: the payee's account exists only because this row was sent,
	// so it was hidden — the engine had to name a new one, as it did then.
	NewPayee   bool           `json:"newPayee,omitempty"`
	Unknowns   []string       `json:"unknowns,omitempty"`
	Signals    []string       `json:"signals,omitempty"`
	Reasoning  string         `json:"reasoning,omitempty"`
	Learned    *LearnedCounts `json:"learned,omitempty"`
	DurationMS int64          `json:"durationMs"`
	Error      string         `json:"error,omitempty"`
}

// EvalReport is an evaluation, running or done.
type EvalReport struct {
	ID         string                 `json:"id"`
	Running    bool                   `json:"running"`
	StartedAt  time.Time              `json:"startedAt"`
	FinishedAt time.Time              `json:"finishedAt,omitempty"`
	Model      string                 `json:"model"`
	Reasoning  string                 `json:"reasoning"`
	Sampled    int                    `json:"sampled"`
	Done       int                    `json:"done"`
	Failed     int                    `json:"failed"`
	AvgMS      int64                  `json:"avgMs"`
	Engine     map[string]*FieldScore `json:"engine"`   // this run
	Baseline   map[string]*FieldScore `json:"baseline"` // the stored suggestions, same rows
	Rows       []EvalRow              `json:"rows"`
	Error      string                 `json:"error,omitempty"`
}

var evalFields = []string{"type", "payee", "payeeFits", "category", "budget", "tags", "title", "titleFits"}

func newScores() map[string]*FieldScore {
	m := map[string]*FieldScore{}
	for _, f := range evalFields {
		m[f] = &FieldScore{}
	}
	return m
}

// evalState is the one evaluation that may run at a time.
type evalState struct {
	mu      sync.Mutex
	current *EvalReport
}

// ErrEvalRunning: one evaluation at a time.
var ErrEvalRunning = errors.New("an evaluation is already running")

// evalBudget bounds one evaluation's run.
const evalBudget = 45 * time.Minute

// StartEval begins an evaluation in the background and returns its first
// report. The run outlives the request that started it, bounded by
// evalBudget.
func (c *Classifier) StartEval(opt EvalOptions) (EvalReport, error) {
	if c.llm == nil {
		return EvalReport{}, errors.New("no model configured")
	}
	c.eval.mu.Lock()
	if c.eval.current != nil && c.eval.current.Running {
		c.eval.mu.Unlock()
		return EvalReport{}, ErrEvalRunning
	}
	client := c.llm.WithOverrides(opt.Model, opt.Reasoning)
	rep := &EvalReport{
		ID: fmt.Sprintf("eval-%d", time.Now().UnixMilli()), Running: true, StartedAt: time.Now().UTC(),
		Model: client.Model(), Reasoning: client.ReasoningEffort(),
		Engine: newScores(), Baseline: newScores(),
	}
	if rep.Reasoning == "" {
		rep.Reasoning = "none"
	}
	c.eval.current = rep
	c.eval.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), evalBudget)
	targets, err := c.evalTargets(ctx, opt)
	if err == nil && len(targets) == 0 {
		err = errors.New("no sent transactions to grade against yet")
	}
	if err != nil {
		cancel()
		c.finishEval(rep, err)
		return c.EvalStatus(), err
	}
	c.eval.mu.Lock()
	rep.Sampled = len(targets)
	c.eval.mu.Unlock()

	go func() {
		defer cancel()
		c.runEval(ctx, rep, client, targets, opt)
	}()
	return c.EvalStatus(), nil
}

// EvalStatus is the current (or last) evaluation, a deep-enough copy to hand
// to JSON while the run goes on.
func (c *Classifier) EvalStatus() EvalReport {
	c.eval.mu.Lock()
	defer c.eval.mu.Unlock()
	if c.eval.current == nil {
		return EvalReport{}
	}
	out := *c.eval.current
	out.Engine, out.Baseline = copyScores(out.Engine), copyScores(out.Baseline)
	out.Rows = append([]EvalRow(nil), out.Rows...)
	return out
}

func copyScores(m map[string]*FieldScore) map[string]*FieldScore {
	out := map[string]*FieldScore{}
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

func (c *Classifier) finishEval(rep *EvalReport, err error) {
	c.eval.mu.Lock()
	defer c.eval.mu.Unlock()
	rep.Running = false
	rep.FinishedAt = time.Now().UTC()
	if err != nil {
		rep.Error = err.Error()
	}
	if rep.Done > 0 {
		var total int64
		for _, r := range rep.Rows {
			total += r.DurationMS
		}
		rep.AvgMS = total / int64(len(rep.Rows))
	}
	// Misses first: they are what a person reads the report for.
	sort.SliceStable(rep.Rows, func(i, j int) bool { return misses(rep.Rows[i]) > misses(rep.Rows[j]) })
}

func misses(r EvalRow) int {
	n := 0
	for _, f := range evalFields {
		if ok, graded := r.Right[f]; graded && !ok {
			n++
		}
	}
	if r.Error != "" {
		n += 10
	}
	return n
}

// evalTarget is one sent row and its answer.
type evalTarget struct {
	staged    StagedRow
	fireflyID int64
	accountID int64 // the payee's account, when nothing else uses it
	truth     feedback.ReviewValues
	before    feedback.ReviewValues
}

func (c *Classifier) evalTargets(ctx context.Context, opt EvalOptions) ([]evalTarget, error) {
	limit := opt.Limit
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, 200)
	where := `status = 'pushed'`
	var args []any
	if len(opt.UUIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(opt.UUIDs)), ",")
		where += ` AND fold_uuid IN (` + ph + `)`
		for _, u := range opt.UUIDs {
			args = append(args, u)
		}
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT fold_uuid FROM staged_fold_txns
		WHERE `+where+` AND mode <> 'MANUAL'
		ORDER BY COALESCE(pushed_at, updated_at) DESC LIMIT ? OFFSET ?`, append(args, limit, max(opt.Offset, 0))...)
	if err != nil {
		return nil, err
	}
	var uuids []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			uuids = append(uuids, u)
		}
	}
	rows.Close()
	var out []evalTarget
	for _, u := range uuids {
		staged, err := c.fetchStagedForClassify(ctx, `fold_uuid = ?`, u)
		if err != nil || len(staged) != 1 {
			continue
		}
		t := evalTarget{staged: staged[0]}
		var ok bool
		var payeeID int64
		if t.fireflyID, payeeID, t.truth, ok = fireflyTruth(ctx, c.db, u); !ok {
			continue // not mirrored yet: nothing to grade against
		}
		if payeeID != 0 {
			var others int
			_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM firefly_txns
				WHERE firefly_id <> ? AND (destination_account_id = ? OR source_account_id = ?)`, t.fireflyID, payeeID, payeeID).Scan(&others)
			if others == 0 {
				t.accountID = payeeID
			}
		}
		if snap, err := feedback.SnapshotReview(ctx, c.db, u); err == nil {
			t.before = snap.Suggested
		}
		out = append(out, t)
	}
	return out, nil
}

// fireflyTruth is what firefly holds for a sent row now, in the review's
// terms (the other side as the payee), and that payee's account id.
func fireflyTruth(ctx context.Context, db *sql.DB, uuid string) (int64, int64, feedback.ReviewValues, bool) {
	var (
		id                           int64
		srcID, dstID                 sql.NullInt64
		typ, desc                    string
		src, dst, cat, bud, tagsJSON sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT t.firefly_id, t.txn_type, t.description, t.source_account_id, t.source_account_name,
		       t.destination_account_id, t.destination_account_name, t.category_name, t.budget_name, t.tags_json
		FROM firefly_txns t
		WHERE t.external_id = ?
		   OR t.group_id = (SELECT firefly_txn_id FROM staged_fold_txns WHERE fold_uuid = ? AND firefly_txn_id IS NOT NULL)
		ORDER BY (t.external_id = ?) DESC LIMIT 1`, uuid, uuid, uuid).Scan(&id, &typ, &desc, &srcID, &src, &dstID, &dst, &cat, &bud, &tagsJSON)
	if err != nil {
		return 0, 0, feedback.ReviewValues{}, false
	}
	v := feedback.ReviewValues{Type: typ, Title: desc, Category: cat.String, Budget: bud.String, Tags: parseTags(tagsJSON.String)}
	var foldType string
	_ = db.QueryRowContext(ctx, `SELECT type FROM staged_fold_txns WHERE fold_uuid = ?`, uuid).Scan(&foldType)
	v.Payee = otherSide(typ, foldType, src.String, dst.String)
	var payeeID int64 // the other side's account; a transfer's is the owner's own, never hidden
	switch {
	case typ == "deposit":
		payeeID = srcID.Int64
	case typ == "withdrawal":
		payeeID = dstID.Int64
	}
	return id, payeeID, v, true
}

// otherSide is the review's payee: who was paid on money out, who paid on
// money in, and the other account on a transfer.
func otherSide(fireflyType, foldType, src, dst string) string {
	switch {
	case fireflyType == "deposit":
		return src
	case fireflyType == "transfer" && foldType == "INCOMING":
		return src
	}
	return dst
}

func (c *Classifier) runEval(ctx context.Context, rep *EvalReport, client *llm.Client, targets []evalTarget, opt EvalOptions) {
	cc := &Classifier{db: c.db, log: c.log, threshold: c.threshold, ftsTopK: c.ftsTopK, llm: client}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(min(max(opt.Concurrency, 2), 4))
	for _, t := range targets {
		g.Go(func() error {
			row := cc.evalOne(gctx, t)
			c.eval.mu.Lock()
			defer c.eval.mu.Unlock()
			rep.Rows = append(rep.Rows, row)
			if row.Error != "" {
				rep.Failed++
				return nil
			}
			rep.Done++
			for f, ok := range row.Right {
				rep.Engine[f].add(ok)
			}
			for f, ok := range gradeValues(t.before, t.truth) {
				rep.Baseline[f].add(ok)
			}
			return nil
		})
	}
	_ = g.Wait()
	c.finishEval(rep, ctx.Err())
	final := c.EvalStatus()
	args := []any{"id", final.ID, "model", final.Model, "reasoning", final.Reasoning, "done", final.Done, "failed", final.Failed, "avg_ms", final.AvgMS}
	for _, f := range evalFields {
		args = append(args, f, fmt.Sprintf("%.0f%% (was %.0f%%)", 100*final.Engine[f].Rate, 100*final.Baseline[f].Rate))
	}
	c.log.Info("shadow evaluation finished", args...)
}

func (c *Classifier) evalOne(ctx context.Context, t evalTarget) EvalRow {
	row := EvalRow{UUID: t.staged.FoldUUID, Narration: t.staged.Narration, Truth: t.truth, Before: t.before}
	start := time.Now()
	row.NewPayee = t.accountID != 0
	d, err := c.classifyOne(ctx, t.staged, exclusion{foldUUID: t.staged.FoldUUID, fireflyID: t.fireflyID, accountID: t.accountID})
	row.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		row.Error = truncate(err.Error(), 300)
		return row
	}
	row.Tier, row.Confidence = d.Tier, d.Confidence
	row.Got = decisionValues(d, t.staged.Type)
	row.Right = gradeValues(row.Got, t.truth)
	row.TitleFit = titleFit(row.Got.Title, t.truth.Title)
	row.Unknowns, row.Signals, row.Learned = d.Evidence.Unknowns, d.Evidence.Signals, d.Evidence.Learned
	row.Reasoning = truncate(d.Evidence.Note, 600)
	return row
}

// decisionValues is a decision in the review's terms.
func decisionValues(d Decision, foldType string) feedback.ReviewValues {
	typ := d.TxnType
	if typ == "" {
		typ = fireflyTxnTypeFor(foldType)
	}
	return feedback.ReviewValues{
		Type: typ, Title: d.Description, Category: d.CategoryName, Budget: d.BudgetName, Tags: d.Tags,
		Payee: otherSide(typ, foldType, d.SourceAccountName, d.DestinationAccountName),
	}
}

// gradeValues says, field by field, whether got is what the owner booked.
func gradeValues(got, truth feedback.ReviewValues) map[string]bool {
	fit := titleFit(got.Title, truth.Title)
	return map[string]bool{
		"type":      norm(got.Type) == norm(truth.Type),
		"payee":     norm(got.Payee) == norm(truth.Payee),
		"payeeFits": samePlace(got.Payee, truth.Payee),
		"category":  norm(noneToEmpty(got.Category)) == norm(noneToEmpty(truth.Category)),
		"budget":    norm(noneToEmpty(got.Budget)) == norm(noneToEmpty(truth.Budget)),
		"tags":      sameTags(got.Tags, truth.Tags),
		"title":     fit == "exact",
		"titleFits": fit == "exact" || fit == "fits",
	}
}

// samePlace: the same merchant, however its area and city are written —
// "Lantern Kopi" for "Lantern Kopi, River Quay, Harbourtown".
func samePlace(got, truth string) bool {
	first := func(s string) string {
		s, _, _ = strings.Cut(s, ",")
		return strings.Join(nameWords(s), " ")
	}
	g, t := first(got), first(truth)
	return g != "" && (g == t || strings.HasPrefix(t, g+" ") || strings.HasPrefix(g, t+" "))
}

var spaceRun = regexp.MustCompile(`\s+`)

func norm(s string) string {
	return strings.TrimRight(spaceRun.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), " "), ".")
}

func noneToEmpty(s string) string {
	if s == feedback.NoneValue {
		return ""
	}
	return s
}

func sameTags(a, b []string) bool {
	set := func(l []string) map[string]bool {
		m := map[string]bool{}
		for _, t := range l {
			if t = norm(t); t != "" {
				m[t] = true
			}
		}
		return m
	}
	x, y := set(a), set(b)
	if len(x) != len(y) {
		return false
	}
	for t := range x {
		if !y[t] {
			return false
		}
	}
	return true
}

// titleFit grades a suggested title against the one the owner booked:
//
//	exact    the same words
//	fits     a title with blanks whose every written part is in the booked
//	         one, in order ("___ from Zepto" for "Milk and bread from
//	         Zepto") — one tap from right, and honest about what it
//	         didn't know
//	overlap  shares at least half its words
//	miss     anything else
func titleFit(got, truth string) string {
	g, t := norm(got), norm(truth)
	switch {
	case g == "" && t == "":
		return "exact"
	case g == "":
		return "miss"
	case g == t:
		return "exact"
	}
	if strings.Contains(g, titleBlankMark) {
		parts := strings.Split(g, titleBlankMark)
		var re strings.Builder
		re.WriteString("^")
		for i, p := range parts {
			re.WriteString(regexp.QuoteMeta(p))
			if i < len(parts)-1 {
				re.WriteString(".+")
			}
		}
		re.WriteString("$")
		if ok, _ := regexp.MatchString(re.String(), t); ok {
			return "fits"
		}
	}
	words := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') }) {
			if len(w) > 2 {
				m[w] = true
			}
		}
		return m
	}
	gw, tw := words(strings.ReplaceAll(g, titleBlankMark, " ")), words(t)
	shared := 0
	for w := range gw {
		if tw[w] {
			shared++
		}
	}
	if len(gw) > 0 && shared*2 >= len(gw) {
		return "overlap"
	}
	return "miss"
}

// titleBlankMark is how a title marks what the engine could not know.
const titleBlankMark = "___"
