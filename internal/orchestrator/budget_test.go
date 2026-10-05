package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// budgetSet plans two edit steps (a, b) and makes every agent run cost
// $0.08 (Claude API-equivalent) and 1000 fresh tokens. ran records the
// steps that ran.
func budgetSet(ran *sync.Map) runner.Set {
	return both(func(s runner.Spec) runner.Result {
		r, ok := twoEdits(s)
		if !ok {
			ran.Store(s.StepID, true)
			r = runner.Result{Final: "done"}
		}
		r.Tokens = event.TokenUsage{Input: 1000, CostUSD: 0.09}
		return r
	})
}

func budgetCfg(edit func(b *config.BudgetCfg)) func(*config.Config) {
	return func(c *config.Config) {
		c.Orchestrator.Parallel = false
		c.Orchestrator.ReviewBeforePlan = false
		c.Orchestrator.ReviewBeforeDone = false
		c.Orchestrator.ApprovePlan = false
		c.Budget.WarnAt = 0.8
		edit(&c.Budget)
	}
}

func steps(ran *sync.Map) int {
	n := 0
	ran.Range(func(any, any) bool { n++; return true })
	return n
}

func countLogs(evs []event.Event, kind event.Kind, prefix string) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind && strings.HasPrefix(e.Text, prefix) {
			n++
		}
	}
	return n
}

// An unattended task stops at its budget without asking, warns once on
// the way, and its summary says why.
func TestBudgetStopsUnattendedTask(t *testing.T) {
	var ran sync.Map
	o, rec := newOrc(t, "", budgetSet(&ran), budgetCfg(func(b *config.BudgetCfg) { b.TaskUSD = 0.10 }))
	ap := &fakeApprover{budget: func(BudgetRequest) bool { return true }}
	withApprover(o, ap)
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if res.OK || !strings.HasPrefix(res.Summary, "stopped by budget: this task's cost $0.18 reached the budget of $0.10") {
		t.Fatalf("result = %+v", res)
	}
	if n := steps(&ran); n != 1 {
		t.Errorf("%d steps ran; the budget should stop the task after the first", n)
	}
	if len(ap.budgets) != 0 {
		t.Errorf("an unattended task asked: %+v", ap.budgets)
	}
	evs := rec.all()
	if n := countLogs(evs, event.Log, "budget warning: this task's cost"); n != 1 {
		t.Errorf("%d budget warnings, want 1", n)
	}
	if n := countLogs(evs, event.Error, "stopped by budget"); n != 1 {
		t.Errorf("%d stop errors, want 1", n)
	}
	if strings.Contains(res.Summary, "cancelled") {
		t.Errorf("summary calls it a cancel: %s", res.Summary)
	}
}

// Attended: the person is asked once; yes lets the task finish, no stops it.
func TestBudgetApproverContinueAndStop(t *testing.T) {
	for _, goOn := range []bool{true, false} {
		var ran sync.Map
		o, _ := newOrc(t, "", budgetSet(&ran), budgetCfg(func(b *config.BudgetCfg) { b.TaskTokens = 1500 }))
		ap := &fakeApprover{budget: func(BudgetRequest) bool { return goOn }}
		withApprover(o, ap)
		res := o.Run(context.Background(), longTask)
		if len(ap.budgets) != 1 {
			t.Fatalf("goOn=%v: asked %d times, want once", goOn, len(ap.budgets))
		}
		r := ap.budgets[0]
		if r.Limit != LimitTaskTokens || r.Used != 2000 || r.Max != 1500 || r.Task != longTask || r.Next == "" {
			t.Errorf("request = %+v", r)
		}
		n := steps(&ran)
		if goOn && (!res.OK || n != 2) {
			t.Errorf("continue: %+v (%d steps ran)", res, n)
		}
		if !goOn && (res.OK || n != 1 || !strings.HasPrefix(res.Summary, "stopped by budget: this task's tokens 2.0k tokens")) {
			t.Errorf("stop: %+v (%d steps ran)", res, n)
		}
	}
}

// Without anyone to ask (rw run without --approve) the task stops and the
// error says how to raise the limit.
func TestBudgetWithoutApproverStops(t *testing.T) {
	var ran sync.Map
	o, rec := newOrc(t, "", budgetSet(&ran), budgetCfg(func(b *config.BudgetCfg) { b.TaskUSD = 0.10 }))
	res := o.Run(context.Background(), longTask)
	if res.OK || !strings.HasPrefix(res.Summary, "stopped by budget") {
		t.Fatalf("result = %+v", res)
	}
	found := false
	for _, e := range rec.all() {
		if e.Kind == event.Error && strings.Contains(e.Text, "--budget-task-usd") && strings.Contains(e.Text, "budget.task_usd") {
			found = true
		}
	}
	if !found {
		t.Error("no hint how to raise the budget")
	}
}

// The day total counts today's finished tasks from the session logs: a
// day already at its limit stops the next task before any agent starts.
func TestBudgetDayTotalFromSessionLog(t *testing.T) {
	var ran sync.Map
	var agents int
	var mu sync.Mutex
	set := budgetSet(&ran)
	counting := both(func(s runner.Spec) runner.Result {
		mu.Lock()
		agents++
		mu.Unlock()
		return set[event.Codex].(scripted).fn(s)
	})
	o, _ := newOrc(t, "", counting, budgetCfg(func(b *config.BudgetCfg) { b.DayUSD = 1 }))
	// An earlier rw (another log file in the same directory) spent $0.95
	// today, and $50 yesterday.
	other, err := sessionlog.Open(filepath.Dir(o.opts.Log.Path()), "/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	other.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, Cost: &event.TaskCost{CostUSD: 0.95}})
	other.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TS: sessionlog.DayStart(time.Now()).Add(-time.Hour), Cost: &event.TaskCost{CostUSD: 50}})
	other.Close()

	st := o.BudgetStatus()
	if st.DayUSD < 0.949 || st.DayUSD > 0.951 || st.Limits.DayUSD != 1 {
		t.Fatalf("status before = %+v", st)
	}
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if res.OK || !strings.Contains(res.Summary, "today's cost $1.04 reached the budget of $1.00") {
		t.Fatalf("result = %+v", res)
	}
	if agents != 1 {
		t.Errorf("%d agents ran, want only the planner", agents)
	}
	// The finished task now counts into today.
	if st := o.BudgetStatus(); st.DayUSD < 1.039 || st.DayUSD > 1.041 || st.Running {
		t.Errorf("status after = %+v", st)
	}
	// The next task does not even start an agent.
	agents = 0
	res = o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if res.OK || agents != 0 {
		t.Errorf("second task: %+v, %d agents", res, agents)
	}
}

// Without limits nothing is checked or logged.
func TestBudgetOffByDefault(t *testing.T) {
	var ran sync.Map
	o, rec := newOrc(t, "", budgetSet(&ran), budgetCfg(func(*config.BudgetCfg) {}))
	if res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if n := countLogs(rec.all(), event.Log, "budget"); n != 0 {
		t.Errorf("%d budget logs without a budget", n)
	}
}

// Quota readings are logged when they change, so `rw run --when-reset`
// can find the reset time later.
func TestQuotaChangesAreLogged(t *testing.T) {
	o, _ := newOrc(t, "", both(func(runner.Spec) runner.Result { return runner.Result{} }), nil)
	resets := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	q := event.QuotaInfo{Utilization: 0.9, Window: "five_hour", ResetsAt: resets}
	o.emit(event.Event{Kind: event.Quota, Provider: event.Claude, Quota: &q})
	o.emit(event.Event{Kind: event.Quota, Provider: event.Claude, Quota: &q}) // unchanged: not logged again
	recs, err := sessionlog.ReadDir(filepath.Dir(o.opts.Log.Path()))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.Type == sessionlog.TypeQuota {
			n++
		}
	}
	at, _, ok := sessionlog.LatestReset(recs, event.Claude)
	if n != 1 || !ok || !at.Equal(resets) {
		t.Errorf("%d quota records, reset %v %v", n, at, ok)
	}
}

func TestBudgetRequestText(t *testing.T) {
	r := BudgetRequest{Limit: LimitDayTokens, Used: 1_200_000, Max: 1_000_000}
	if r.String() != "today's tokens 1.2M tokens reached the budget of 1.0M tokens" || r.Flag() != "" || !strings.Contains(r.RaiseHint(), "budget.day_tokens") {
		t.Errorf("%q %q %q", r.String(), r.Flag(), r.RaiseHint())
	}
}

// With routing.tiers: auto, a filling budget moves later steps to the
// cheaper tier: the first edit starts with 40% of the task budget used
// (the plan's share; no saving yet), the second with 80% and runs on the
// explorer's route.
func TestTiersFollowBudget(t *testing.T) {
	var ran sync.Map
	edit := budgetCfg(func(b *config.BudgetCfg) { b.TaskTokens = 2500 })
	o, rec := newOrc(t, "", budgetSet(&ran), func(c *config.Config) {
		edit(c)
		c.Routing.Tiers = config.TiersAuto
	})
	o.Run(context.Background(), longTask)
	cfg := o.opts.Store.Get()
	var edits []event.Decision
	for _, e := range rec.all() {
		if d := e.Decision; e.Kind == event.Route && d != nil && (d.StepID == "a" || d.StepID == "b") {
			edits = append(edits, *d)
		}
	}
	if len(edits) != 2 {
		t.Fatalf("%d edit decisions: %+v", len(edits), edits)
	}
	first, second := edits[0], edits[1]
	if first.Tier != "standard" || first.Model != cfg.Roles[event.RoleWorker].For(first.Provider).Model ||
		!strings.Contains(first.Reason, "quota left 60%") {
		t.Errorf("first edit: %+v", first)
	}
	if second.Tier != "fast" || second.Role != event.RoleWorker || second.Model != cfg.Roles[event.RoleExplorer].For(second.Provider).Model ||
		!strings.Contains(second.Reason, "quota left 20%") {
		t.Errorf("second edit: %+v", second)
	}
}
