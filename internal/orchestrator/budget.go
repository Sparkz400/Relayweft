package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// errBudget ends an agent that the budget did not let start.
var errBudget = errors.New("stopped by budget")

// Budget limit names (config budget.*).
const (
	LimitTaskTokens = "task_tokens"
	LimitTaskUSD    = "task_usd"
	LimitDayTokens  = "day_tokens"
	LimitDayUSD     = "day_usd"
	// The team budget: every machine's tasks today (team.go).
	LimitTeamDayTokens = "team_day_tokens"
	LimitTeamDayUSD    = "team_day_usd"
)

// BudgetRequest asks a person whether a task may go on past a budget
// limit. Approving lets the task run past that limit until it ends.
type BudgetRequest struct {
	Limit string  `json:"limit"` // task_tokens, task_usd, day_tokens, day_usd, team_day_tokens or team_day_usd
	Used  float64 `json:"used"`  // tokens or dollars so far
	Max   float64 `json:"max"`   // the limit
	Task  string  `json:"task"`
	Next  string  `json:"next"` // what runs if the task goes on, e.g. "start w (worker)"
}

// USD reports whether the limit is in dollars (else in fresh tokens).
func (r BudgetRequest) USD() bool {
	return r.Limit == LimitTaskUSD || r.Limit == LimitDayUSD || r.Limit == LimitTeamDayUSD
}

// What names the limit, e.g. "the task's cost (API-equivalent)".
func (r BudgetRequest) What() string {
	switch r.Limit {
	case LimitTaskTokens:
		return "this task's tokens"
	case LimitTaskUSD:
		return "this task's cost"
	case LimitDayTokens:
		return "today's tokens"
	case LimitTeamDayTokens:
		return "the team's tokens today"
	case LimitTeamDayUSD:
		return "the team's cost today"
	}
	return "today's cost"
}

// Amount formats a value in the limit's unit.
func (r BudgetRequest) Amount(v float64) string {
	if r.USD() {
		return fmt.Sprintf("$%.2f", v)
	}
	return event.HumanTokens(int64(v)) + " tokens"
}

// String is a one-line description, e.g.
// "this task's cost $2.04 reached its budget of $2.00".
func (r BudgetRequest) String() string {
	return fmt.Sprintf("%s %s reached the budget of %s", r.What(), r.Amount(r.Used), r.Amount(r.Max))
}

// Flag is how to raise the limit on the command line ("" when only the
// config file sets it).
func (r BudgetRequest) Flag() string {
	switch r.Limit {
	case LimitTaskTokens:
		return "--budget-task-tokens"
	case LimitTaskUSD:
		return "--budget-task-usd"
	case LimitDayUSD:
		return "--budget-day-usd"
	}
	return ""
}

// RaiseHint says how to raise the limit.
func (r BudgetRequest) RaiseHint() string {
	key := "budget." + r.Limit
	switch r.Limit {
	case LimitTeamDayTokens:
		key = "budget.team.day_tokens"
	case LimitTeamDayUSD:
		key = "budget.team.day_usd"
	}
	s := "raise " + key + " in " + config.FileName
	if f := r.Flag(); f != "" {
		s = "raise it with " + f + " <n> or " + key + " in " + config.FileName
	}
	return s + " (0 = no limit)"
}

// taskBudget is one task's budget state.
type taskBudget struct {
	mu      sync.Mutex
	allowed map[string]bool
	warned  map[string]bool
	noted   map[string]bool // limits crossed by a finishing agent, logged once
	stopped string          // why the budget stopped the task ("" = it did not)
	cancel  context.CancelFunc
}

// BudgetStatus is what the UIs show: today's use (finished tasks plus the
// running one), the running task's use and the limits.
type BudgetStatus struct {
	DayTokens  int64            `json:"day_tokens"`
	DayUSD     float64          `json:"day_usd"`
	TaskTokens int64            `json:"task_tokens"`
	TaskUSD    float64          `json:"task_usd"`
	Running    bool             `json:"running"`
	Limits     config.BudgetCfg `json:"limits"`
}

// dayCache holds today's finished-task totals from the session logs.
type dayCache struct {
	date   string
	tokens int64
	usd    float64
	read   time.Time // when the logs were last read
	err    string    // the last read error
	logged string    // the read error last logged
}

// dayMaxAge is how old the day totals may get before a budget check reads
// the session logs again (tasks of other sy windows finish meanwhile).
var dayMaxAge = time.Minute

// logDir is the session log directory ("" without a log).
func (o *Orchestrator) logDir() string {
	if p := o.opts.Log.Path(); p != "" {
		return filepath.Dir(p)
	}
	return ""
}

// refreshDay re-reads today's finished tasks from the session logs (all
// sy processes write there, so tasks of other windows count too). A log
// file that cannot be read is left out with a warning, and the total never
// drops below what was known for today: a read error must not turn the
// day limit off.
//
// warn logs a read error (once per error); only task paths warn, since a
// UI reading BudgetStatus may be the one draining the event channel.
func (o *Orchestrator) refreshDay(now time.Time, warn bool) dayCache {
	dc := dayCache{date: now.Local().Format("2006-01-02"), read: now}
	var readErr error
	if dir := o.logDir(); dir != "" {
		recs, err := sessionlog.ReadDirSince(dir, sessionlog.DayStart(now))
		dc.tokens, dc.usd = sessionlog.DayUsage(recs, now)
		readErr = err
	}
	o.mu.Lock()
	prev := o.day
	dc.logged = prev.logged
	if readErr != nil {
		dc.err = readErr.Error()
		if prev.date == dc.date {
			dc.tokens, dc.usd = max(dc.tokens, prev.tokens), max(dc.usd, prev.usd)
		}
	}
	logIt := warn && dc.err != "" && dc.err != dc.logged
	if logIt {
		dc.logged = dc.err
	}
	o.day = dc
	o.mu.Unlock()
	if logIt {
		o.logf("budget: could not read every session log (%v); today's total counts what could be read", readErr)
	}
	return dc
}

// dayTotals returns today's finished-task totals: cached, re-read when the
// day changed or the cache is older than maxAge (0 = only on a new day).
func (o *Orchestrator) dayTotals(now time.Time, maxAge time.Duration, warn bool) dayCache {
	o.mu.Lock()
	dc := o.day
	o.mu.Unlock()
	if dc.date != now.Local().Format("2006-01-02") || (maxAge > 0 && now.Sub(dc.read) >= maxAge) {
		return o.refreshDay(now, warn)
	}
	if warn && dc.err != "" && dc.err != dc.logged {
		return o.refreshDay(now, warn)
	}
	return dc
}

// addDay counts a finished task into today's cached totals (its task_end
// record is in the log too; a later refresh reads it from there).
func (o *Orchestrator) addDay(u event.TokenUsage, usd float64) {
	if o.logDir() != "" {
		o.refreshDay(time.Now(), true)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.day.date == time.Now().Local().Format("2006-01-02") {
		o.day.tokens += u.Total()
		o.day.usd += usd
	}
}

// budgetLimits are the live limits: an edit in the settings applies to
// the running task at its next check.
func (o *Orchestrator) budgetLimits(t *task) config.BudgetCfg {
	if o.opts.Store != nil {
		return o.opts.Store.Budget()
	}
	return t.cfg.Budget
}

// BudgetStatus reports today's and the running task's use against the
// budget. It is cheap (the session logs are read at task start, at budget
// checks and when the day changes), so a UI may call it every frame.
func (o *Orchestrator) BudgetStatus() BudgetStatus {
	cfg := o.opts.Store.Budget()
	dc := o.dayTotals(time.Now(), 0, false)
	st := BudgetStatus{DayTokens: dc.tokens, DayUSD: dc.usd, Limits: cfg}
	o.mu.Lock()
	t := o.cur
	o.mu.Unlock()
	if t != nil {
		u := t.usage()
		st.Running = true
		st.TaskTokens, st.TaskUSD = u.Total(), u.CostUSD
		st.DayTokens += st.TaskTokens
		st.DayUSD += st.TaskUSD
	}
	return st
}

// usage is what the task has used so far.
func (t *task) usage() event.TokenUsage {
	t.tokensMu.Lock()
	defer t.tokensMu.Unlock()
	return t.tokens
}

// startBudget prepares a task's budget: today's totals so far and a way
// to stop the task. It returns the context the task runs under. Tasks,
// single runs and follow-ups all have one.
func (o *Orchestrator) startBudget(ctx context.Context, t *task) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	o.refreshDay(time.Now(), true)
	t.budget = &taskBudget{allowed: map[string]bool{}, warned: map[string]bool{}, noted: map[string]bool{}, cancel: cancel}
	o.mu.Lock()
	o.cur = t
	o.mu.Unlock()
	return ctx
}

// endBudget forgets the running task and counts its use into today.
func (o *Orchestrator) endBudget(t *task, cost event.TaskCost) {
	o.mu.Lock()
	if o.cur == t {
		o.cur = nil
	}
	o.mu.Unlock()
	if t.budget == nil {
		return
	}
	t.budget.cancel()
	o.addDay(t.usage(), cost.CostUSD)
	o.writeTeam()
}

// budgetStopped returns why the budget stopped the task ("" = it did not).
func (t *task) budgetStopped() string {
	if t.budget == nil {
		return ""
	}
	t.budget.mu.Lock()
	defer t.budget.mu.Unlock()
	return t.budget.stopped
}

// checkBudget compares the task's and today's use with the budget before
// an agent starts (next says which). It warns once per limit at warn_at.
// At a limit it asks the person, unless the task is unattended or nobody
// can be asked: then the task stops. It returns false when the task must
// stop.
func (o *Orchestrator) checkBudget(ctx context.Context, t *task, next string) bool {
	return o.budgetCheck(ctx, t, next, false)
}

// noteBudget is the check after an agent finished: its work is done and
// paid for, so crossing a limit there only warns. Whether the task goes
// on is decided when (if) the next agent is about to start.
func (o *Orchestrator) noteBudget(t *task, agentID string) {
	o.budgetCheck(context.Background(), t, agentID, true)
}

func (o *Orchestrator) budgetCheck(ctx context.Context, t *task, what string, after bool) bool {
	b := t.budget
	if b == nil {
		return true // bench helpers without budget state
	}
	cfg := o.budgetLimits(t)
	if !cfg.Any() {
		return true
	}
	// One question at a time: parallel agents wait for the first answer.
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped != "" {
		return false
	}
	u := t.usage()
	var day dayCache
	if cfg.DayTokens > 0 || cfg.DayUSD > 0 || cfg.Team.Limited() {
		day = o.dayTotals(time.Now(), dayMaxAge, true)
	}
	// The team's day: the other machines' files plus this machine's day.
	var team teamCache
	teamTokens, teamUSD := float64(0), float64(0)
	if cfg.Team.Limited() {
		team = o.teamTotals(time.Now(), dayMaxAge, true)
		teamTokens, teamUSD = float64(cfg.Team.DayTokens), cfg.Team.DayUSD
	}
	type lim struct {
		name      string
		used, max float64
	}
	limits := []lim{
		{LimitTaskTokens, float64(u.Total()), float64(cfg.TaskTokens)},
		{LimitTaskUSD, u.CostUSD, cfg.TaskUSD},
		{LimitDayTokens, float64(day.tokens + u.Total()), float64(cfg.DayTokens)},
		{LimitDayUSD, day.usd + u.CostUSD, cfg.DayUSD},
		{LimitTeamDayTokens, float64(team.tokens + day.tokens + u.Total()), teamTokens},
		{LimitTeamDayUSD, team.usd + day.usd + u.CostUSD, teamUSD},
	}
	for _, l := range limits {
		if l.max <= 0 || b.allowed[l.name] {
			continue
		}
		req := BudgetRequest{Limit: l.name, Used: l.used, Max: l.max, Task: t.text, Next: what}
		if l.used < l.max {
			if w := cfg.WarnAt; w > 0 && l.used >= w*l.max && !b.warned[l.name] {
				b.warned[l.name] = true
				o.emit(event.Event{Kind: event.Log, Text: fmt.Sprintf("budget warning: %s is at %s of %s (%.0f%%)",
					req.What(), req.Amount(l.used), req.Amount(l.max), l.used/l.max*100)})
			}
			continue
		}
		if after {
			if !b.noted[l.name] {
				b.noted[l.name] = true
				o.logf("budget: %s after %s finished; its work is kept, and no further agent starts without your ok", req, what)
			}
			continue
		}
		ok := false
		switch {
		case t.unattended:
			o.logf("budget: %s; unattended tasks stop at a budget", req)
		case o.opts.Approver == nil:
			o.logf("budget: %s; nobody to ask (sy run --approve asks on the terminal)", req)
		default:
			o.logf("budget: %s - waiting for you to decide whether the task goes on", req)
			ok = o.opts.Approver.ApproveBudget(ctx, req)
			if ctx.Err() != nil {
				return false
			}
		}
		if ok {
			b.allowed[l.name] = true
			o.logf("budget: you let the task go on past %s until it ends", req.Amount(l.max))
			continue
		}
		b.stopped = req.String()
		o.emit(event.Event{Kind: event.Error, Text: "stopped by budget: " + req.String() + "; " + req.RaiseHint()})
		b.cancel()
		return false
	}
	return true
}
