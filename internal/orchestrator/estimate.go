package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// Dry-run cost estimate: before a plan is approved, every step gets an
// estimate of its tokens, wall time and API-equivalent $ from earlier runs
// of the same role, kind and route (sessionlog/estimate.go), and the total
// is compared with what is left of the task and day budgets. Approvers that
// implement EstimateApprover show it; `sy run --estimate` prints it and
// stops before anything runs.

// StepEstimate is one planned step's estimate on the route the router
// would pick for it now.
type StepEstimate struct {
	StepID string `json:"step_id"`
	Title  string `json:"title"`
	Role   string `json:"role"`
	Route  string `json:"route"` // provider:model[:effort] ("a + b" for best of N)
	// BestOf is the number of candidates when the step runs as best of N
	// (bestof.go): the estimate covers all of them.
	BestOf int `json:"best_of,omitempty"`
	sessionlog.Estimate
}

// PlanEstimate is a plan's estimate: per step (the final review included
// when it will run) and in total.
type PlanEstimate struct {
	Steps []StepEstimate `json:"steps"`
	// Tokens and USD add up over the steps; Seconds follows the longest
	// chain of dependent steps when steps run in parallel.
	Tokens    sessionlog.Spread `json:"tokens"`
	Seconds   sessionlog.Spread `json:"seconds"`
	USD       sessionlog.Spread `json:"usd"`
	NoHistory int               `json:"no_history"` // steps on fixed defaults
	Budget    []BudgetCheck     `json:"budget,omitempty"`
	Warnings  []string          `json:"warnings,omitempty"`
}

// BudgetCheck compares the estimate with what is left of one budget limit.
type BudgetCheck struct {
	Limit    string  `json:"limit"` // task_tokens, task_usd, day_tokens or day_usd
	Left     float64 `json:"left"`
	Max      float64 `json:"max"`
	Likely   bool    `json:"likely"`   // the median estimate is more than what is left
	Possible bool    `json:"possible"` // the high end is
}

// FinalReviewID is the estimate row of the final review.
const FinalReviewID = "review-final"

// EstimateApprover is an Approver that shows the dry-run estimate with the
// plan. estimate re-estimates an edited plan; it is cheap (the history is
// read once per approval) and safe to call from any goroutine.
type EstimateApprover interface {
	ApprovePlanEstimate(ctx context.Context, task string, p Plan, estimate func(Plan) PlanEstimate) (Plan, bool)
}

// Step returns a step's estimate.
func (e PlanEstimate) Step(id string) (StepEstimate, bool) {
	for _, s := range e.Steps {
		if s.StepID == id {
			return s, true
		}
	}
	return StepEstimate{}, false
}

// Short is a compact estimate for a table cell, e.g. "45k · 2m · $0.21".
func (s StepEstimate) Short() string {
	out := tok(s.Tokens.Mid) + " · " + secs(s.Seconds.Mid)
	if s.USD.High > 0 {
		out += " · " + usd(s.USD.Mid)
	}
	if s.Source == sessionlog.SourceNone {
		out += " ?"
	}
	return out
}

// Line describes a step's estimate with its range and where it comes
// from, e.g. "45k tok (30k-60k) · 2m0s (1m30s-3m0s) · $0.21 ($0.10-$0.40)
// · this repo, 7 runs".
func (s StepEstimate) Line() string {
	src := s.Source
	if s.Samples > 0 {
		src = fmt.Sprintf("%s, %d runs", s.Source, s.Samples)
	}
	return spreadLine(s.Tokens, s.Seconds, s.USD) + " · " + src
}

// TotalLine describes the total, e.g. "~120k tok (80k-200k) · ... · 1 of
// 3 steps without history".
func (e PlanEstimate) TotalLine() string {
	if n := e.NoHistoryNote(); n != "" {
		return e.Totals() + " · " + n
	}
	return e.Totals()
}

// Totals is the total alone, e.g. "~120k tok (80k-200k) · 6m0s (...)".
func (e PlanEstimate) Totals() string { return "~" + spreadLine(e.Tokens, e.Seconds, e.USD) }

// NoHistoryNote says how many steps are on fixed defaults ("" if none).
func (e PlanEstimate) NoHistoryNote() string {
	if e.NoHistory == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d steps without history", e.NoHistory, len(e.Steps))
}

func spreadLine(t, w, u sessionlog.Spread) string {
	s := fmt.Sprintf("%s tok (%s-%s) · %s (%s-%s)", tok(t.Mid), tok(t.Low), tok(t.High), secs(w.Mid), secs(w.Low), secs(w.High))
	if u.High > 0 {
		s += fmt.Sprintf(" · %s (%s-%s)", usd(u.Mid), usd(u.Low), usd(u.High))
	}
	return s
}

func tok(v float64) string  { return event.HumanTokens(int64(v)) }
func usd(v float64) string  { return fmt.Sprintf("$%.2f", v) }
func secs(v float64) string { return (time.Duration(v) * time.Second).Round(time.Second).String() }

// estimator returns the estimate function of a task's plan approval: the
// session logs are read once here.
func (o *Orchestrator) estimator(t *task) func(Plan) PlanEstimate {
	root := t.root
	if root == "" && !o.opts.NoGit && isRepo(o.opts.Dir) {
		root, _ = repoRoot(o.opts.Dir)
	}
	var hist *sessionlog.History
	if dir := o.logDir(); dir != "" {
		recs, _ := sessionlog.ReadDir(dir) // unreadable logs only make it less precise
		hist = sessionlog.NewHistory(recs, root)
	}
	return func(p Plan) PlanEstimate { return o.estimatePlan(t, hist, p) }
}

// estimatePlan estimates a plan on the routes the router picks now.
func (o *Orchestrator) estimatePlan(t *task, hist *sessionlog.History, p Plan) PlanEstimate {
	cfg := o.opts.Store.Get()
	var e PlanEstimate
	wall := map[string]sessionlog.Spread{}
	add := func(id, title string, step router.Step) StepEstimate {
		d := o.router.Route(step)
		se := StepEstimate{StepID: id, Title: title, Role: d.Role, Route: config.RouteSpec(d.Provider, config.Route{Model: d.Model, Effort: d.Effort}),
			Estimate: hist.Estimate(d.Role, string(step.Kind), sessionlog.RouteKey{Provider: d.Provider, Model: d.Model, Effort: d.Effort}, step.Kind.ReadOnly())}
		e.Steps = append(e.Steps, se)
		e.Tokens, e.USD = e.Tokens.Add(se.Tokens), e.USD.Add(se.USD)
		if se.Source == sessionlog.SourceNone {
			e.NoHistory++
		}
		return se
	}
	for _, st := range p.Subtasks {
		step := routerStep(t, st)
		if on, _ := o.wantBestOf(t, st); on {
			if routes, _ := o.bestOfRoutes(t, step); len(routes) >= 2 {
				se := o.estimateBestOf(hist, st, step.Kind, routes, cfg.Orchestrator.Parallel && cfg.Orchestrator.MaxThreads > 1)
				e.Steps = append(e.Steps, se)
				e.Tokens, e.USD = e.Tokens.Add(se.Tokens), e.USD.Add(se.USD)
				if se.Source == sessionlog.SourceNone {
					e.NoHistory++
				}
				wall[st.ID] = se.Seconds
				continue
			}
		}
		se := add(st.ID, st.Title, step)
		wall[st.ID] = se.Seconds
	}
	e.Seconds = planWall(p, wall, cfg.Orchestrator.Parallel && cfg.Orchestrator.MaxThreads > 1)
	if cfg.Orchestrator.ReviewBeforeDone && hasEdits(p) {
		// The final review runs after every step.
		se := add(FinalReviewID, "final review", router.Step{ID: FinalReviewID, Title: "final review", Kind: router.KindReview, MainProvider: t.mainProv})
		e.Seconds = e.Seconds.Add(se.Seconds)
	}
	o.checkEstimate(t, &e)
	return e
}

// estimateBestOf estimates a best-of step: every candidate's tokens and $
// add up; the wall time is the longest candidate's when they run at once,
// else the sum.
func (o *Orchestrator) estimateBestOf(hist *sessionlog.History, st Subtask, kind router.Kind, routes []event.Decision, parallel bool) StepEstimate {
	se := StepEstimate{StepID: st.ID, Title: st.Title, Role: routes[0].Role, BestOf: len(routes), Estimate: sessionlog.Estimate{Source: sessionlog.SourceRepo}}
	var labels []string
	for i, d := range routes {
		ce := hist.Estimate(d.Role, string(kind), sessionlog.RouteKey{Provider: d.Provider, Model: d.Model, Effort: d.Effort}, kind.ReadOnly())
		labels = append(labels, config.RouteSpec(d.Provider, config.Route{Model: d.Model, Effort: d.Effort}))
		se.Tokens, se.USD = se.Tokens.Add(ce.Tokens), se.USD.Add(ce.USD)
		if parallel {
			se.Seconds = se.Seconds.Max(ce.Seconds)
		} else {
			se.Seconds = se.Seconds.Add(ce.Seconds)
		}
		// The least certain candidate says where the whole comes from.
		if i == 0 || ce.Samples < se.Samples {
			se.Samples = ce.Samples
		}
		if ce.Source == sessionlog.SourceNone || (ce.Source == sessionlog.SourceAll && se.Source == sessionlog.SourceRepo) {
			se.Source = ce.Source
		}
	}
	se.Route = strings.Join(labels, " + ")
	return se
}

// planWall is the plan's wall time: the longest chain of dependent steps
// when steps run in parallel, else the sum.
func planWall(p Plan, wall map[string]sessionlog.Spread, parallel bool) sessionlog.Spread {
	var total sessionlog.Spread
	if !parallel {
		for _, st := range p.Subtasks {
			total = total.Add(wall[st.ID])
		}
		return total
	}
	deps := map[string][]string{}
	for _, st := range p.Subtasks {
		deps[st.ID] = st.DependsOn
	}
	done := map[string]sessionlog.Spread{}
	visiting := map[string]bool{}
	var finish func(id string) sessionlog.Spread
	finish = func(id string) sessionlog.Spread {
		if f, ok := done[id]; ok {
			return f
		}
		if visiting[id] {
			return sessionlog.Spread{} // a cycle (NormalizePlan refuses those)
		}
		visiting[id] = true
		var start sessionlog.Spread
		for _, d := range deps[id] {
			if _, ok := deps[d]; ok {
				start = start.Max(finish(d))
			}
		}
		done[id] = start.Add(wall[id])
		return done[id]
	}
	for _, st := range p.Subtasks {
		total = total.Max(finish(st.ID))
	}
	return total
}

// checkEstimate compares the estimate with what is left of the task and
// day budgets (the task's own use so far, e.g. its planner, counts).
func (o *Orchestrator) checkEstimate(t *task, e *PlanEstimate) {
	lim := o.budgetLimits(t)
	if !lim.Any() {
		return
	}
	u := t.usage()
	var day dayCache
	if lim.DayTokens > 0 || lim.DayUSD > 0 {
		day = o.dayTotals(time.Now(), dayMaxAge, false)
	}
	checks := []struct {
		name      string
		used, max float64
		est       sessionlog.Spread
	}{
		{LimitTaskTokens, float64(u.Total()), float64(lim.TaskTokens), e.Tokens},
		{LimitTaskUSD, u.CostUSD, lim.TaskUSD, e.USD},
		{LimitDayTokens, float64(day.tokens + u.Total()), float64(lim.DayTokens), e.Tokens},
		{LimitDayUSD, day.usd + u.CostUSD, lim.DayUSD, e.USD},
	}
	for _, c := range checks {
		if c.max <= 0 {
			continue
		}
		left := max(0, c.max-c.used)
		bc := BudgetCheck{Limit: c.name, Left: left, Max: c.max, Likely: c.est.Mid > left, Possible: c.est.High > left}
		e.Budget = append(e.Budget, bc)
		if w := bc.Warning(c.est); w != "" {
			e.Warnings = append(e.Warnings, w)
		}
	}
}

// Warning says when the estimate is likely (or could be) over the limit,
// e.g. "likely over this task's tokens: ~120k (80k-200k), 50k left of 100k
// tokens" ("" when it fits).
func (b BudgetCheck) Warning(est sessionlog.Spread) string {
	if !b.Likely && !b.Possible {
		return ""
	}
	r := BudgetRequest{Limit: b.Limit}
	head := "could go over"
	if b.Likely {
		head = "likely over"
	}
	amount := func(v float64) string {
		if r.USD() {
			return usd(v)
		}
		return tok(v)
	}
	return fmt.Sprintf("%s %s: ~%s (%s-%s), %s left of %s", head, r.What(), amount(est.Mid), amount(est.Low), amount(est.High),
		amount(b.Left), r.Amount(b.Max))
}

// approvePlan asks the approver, with the estimate when it can show one.
func (o *Orchestrator) approvePlan(ctx context.Context, t *task, plan Plan) (Plan, bool) {
	ea, ok := o.opts.Approver.(EstimateApprover)
	if !ok {
		return o.opts.Approver.ApprovePlan(ctx, t.text, plan)
	}
	est := o.estimator(t)
	e := est(plan)
	o.logf("estimate: %s", e.TotalLine())
	for _, w := range e.Warnings {
		o.logf("estimate: %s", w)
	}
	return ea.ApprovePlanEstimate(ctx, t.text, plan, est)
}

// errRunning refuses a second task in one orchestrator.
var errRunning = errors.New("a task is already running")

// Estimate plans a task and estimates it without running it (`sy run
// --estimate`). Only the planner runs, read-only, in the project folder;
// there is no snapshot, task state or undo entry, and nothing in the
// working tree changes. The planner's use is logged like any agent's and
// counts towards today's budget.
func (o *Orchestrator) Estimate(ctx context.Context, text string) (Plan, PlanEstimate, error) {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return Plan{}, PlanEstimate{}, errRunning
	}
	o.running = true
	o.taskSeq++
	seq := o.taskSeq
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.running = false
		o.mu.Unlock()
	}()
	cfg := o.opts.Store.Get()
	t := &task{id: fmt.Sprintf("%sestimate-%d", o.opts.TaskIDPrefix, seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg), dir: o.opts.Dir}
	t.key = o.opts.Log.Session() + "-" + t.id
	t.unattended = true
	if !o.opts.NoGit && isRepo(o.opts.Dir) {
		t.root, _ = repoRoot(o.opts.Dir)
	}
	if err := o.setupWorkspace(t, o.Repos()); err != nil {
		return Plan{}, PlanEstimate{}, err
	}
	if cfg.Orchestrator.Handoff {
		for _, r := range t.allRepos() {
			if r.root != "" {
				r.repoMap, r.repoNotes = repoMap(r.root), repoNotes(r.root)
			}
		}
	}
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: "estimate"})
	o.emit(event.Event{Kind: event.TaskStart, Text: "estimate: " + text})
	t.quotaBefore = o.quotaNow()
	bctx := o.startBudget(ctx, t)
	began := time.Now()
	o.emit(event.Event{Kind: event.Phase, Text: "plan"})
	var plan Plan
	ok := true
	if words := len(strings.Fields(text)); cfg.Orchestrator.SmallTaskWords > 0 && words < cfg.Orchestrator.SmallTaskWords && len(t.repos) == 0 {
		// The same shortcut as a real run: one step, no planner.
		plan = Plan{Summary: "small task: one worker step", Subtasks: []Subtask{{ID: "work", Title: firstWords(text, 6), Kind: router.KindEdit, Prompt: text}}}
		if looksRead(text) {
			plan.Subtasks[0].Kind, plan.Subtasks[0].ID = router.KindExplore, "explore"
		}
		t.mainProv = o.router.Route(router.Step{ID: "plan", Kind: router.KindPlan}).Provider
	} else {
		plan, ok = o.plan(bctx, t, "", nil)
	}
	plan.Repos = t.workspaceNames()
	var e PlanEstimate
	var err error
	switch {
	case ctx.Err() != nil:
		err = errors.New("cancelled while planning")
	case !ok:
		err = errors.New("planning failed: " + plan.Summary)
	default:
		e = o.estimator(t)(plan)
	}
	cost := o.cost(t)
	tk := t.usage()
	summary := "estimate only: nothing was run"
	if err != nil {
		summary = err.Error()
	}
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: "estimate",
		OK: sessionlog.Bool(err == nil), Text: summary, Tokens: &tk, DurationMS: time.Since(began).Milliseconds(), Cost: &cost})
	o.endBudget(t, cost)
	o.emit(event.Event{Kind: event.Phase, Text: "done"})
	o.emit(event.Event{Kind: event.TaskDone, OK: err == nil, Text: summary, Tokens: tk, Cost: &cost})
	return plan, e, err
}
