package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func reservationTask(t *testing.T, tokens int64) (*Orchestrator, *task, context.Context) {
	t.Helper()
	o, _ := newOrc(t, "", nil, func(c *config.Config) {
		c.Budget.Reserve, c.Budget.TaskTokens = true, tokens
		c.Orchestrator.ReviewBeforeDone = true
	})
	task := &task{cfg: o.opts.Store.Get(), text: "reservation test"}
	ctx := o.startBudget(context.Background(), task)
	t.Cleanup(func() { o.endBudget(task, o.cost(task)) })
	return o, task, ctx
}

func TestBenchBudgetDoesNotLearnFromPreviousModes(t *testing.T) {
	o, task, _ := reservationTask(t, 100000)
	o.endBudget(task, o.cost(task))
	for i := 0; i < 3; i++ {
		o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, Step: "edit", Kind: "edit", Role: event.RoleWorker,
			Provider: event.Codex, Model: "model", OK: sessionlog.Bool(true), Tokens: &event.TokenUsage{Input: 1000}})
	}
	o.opts.Bench = "historical-case"
	ctx := o.startBudget(context.Background(), task)
	d := event.Decision{Provider: event.Codex, Model: "model", Role: event.RoleWorker}
	finish, ok := o.admitAgent(ctx, task, router.Step{Kind: router.KindEdit}, d, "writer")
	if !ok {
		t.Fatal("fixed estimate did not fit")
	}
	if task.budget.reserved.Total() != 60000 {
		t.Fatal("previous modes changed the experiment's estimate", task.budget.reserved)
	}
	finish(event.TokenUsage{})
}

func TestBudgetStatusDoesNotWaitForApprovalLock(t *testing.T) {
	o, task, _ := reservationTask(t, 100000)
	// An approver can hold this lock until the dashboard answers the prompt.
	task.budget.mu.Lock()
	done := make(chan struct{})
	go func() { o.BudgetStatus(); close(done) }()
	select {
	case <-done:
		task.budget.mu.Unlock()
	case <-time.After(200 * time.Millisecond):
		task.budget.mu.Unlock()
		<-done
		t.Fatal("dashboard state waits for its own budget approval")
	}
}

func TestBudgetBestOfDeclinesExtraCandidateButLandsWinner(t *testing.T) {
	dir := gitRepo(t)
	var writers atomic.Int32
	set := both(func(s runner.Spec) runner.Result {
		if s.ReadOnly {
			r := approve()
			r.Tokens = event.TokenUsage{Input: 1000}
			return r
		}
		writers.Add(1)
		if err := os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte("winner\n"), 0600); err != nil {
			return runner.Result{Err: err}
		}
		return runner.Result{Final: "done", Files: []string{"greet.txt"}, Tokens: event.TokenUsage{Input: 60000}}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		bestOfOn(c)
		c.Budget.Reserve, c.Budget.TaskTokens = true, 100000
		c.Orchestrator.ApprovePlan, c.Orchestrator.ReviewBeforePlan = false, false
		c.Orchestrator.ReviewBeforeDone = true
	})
	res := o.Run(context.Background(), bestOfTask)
	if !res.OK || writers.Load() != 1 {
		t.Fatalf("paid candidate lost or budget double booked: %+v writers=%d", res, writers.Load())
	}
	if read(t, filepath.Join(dir, "greet.txt")) != "winner\n" {
		t.Fatal("winner did not land")
	}
	if countLogs(rec.all(), event.Log, "budget: skipping best-of candidate") != 1 {
		t.Fatal("budget-reduced comparison not disclosed")
	}
}

func TestBudgetReservationParallelWaitAndReconcile(t *testing.T) {
	o, task, ctx := reservationTask(t, 100000)
	d := event.Decision{Provider: event.Codex, Role: event.RoleWorker}
	step := router.Step{Kind: router.KindEdit}
	first, ok := o.admitAgent(ctx, task, step, d, "first")
	if !ok {
		t.Fatal("first writer should fit")
	}
	type admission struct {
		done func(event.TokenUsage)
		ok   bool
	}
	second := make(chan admission, 1)
	go func() { done, ok := o.admitAgent(ctx, task, step, d, "second"); second <- admission{done, ok} }()
	select {
	case <-second:
		t.Fatal("parallel writers spent the same remaining budget")
	case <-time.After(30 * time.Millisecond):
	}
	first(event.TokenUsage{Input: 10000})
	select {
	case next := <-second:
		if !next.ok {
			t.Fatal("unused estimate was not released")
		}
		next.done(event.TokenUsage{Input: 5000})
	case <-time.After(2 * time.Second):
		t.Fatal("reservation waiter stuck")
	}
	if task.usage().Total() != 15000 || task.budget.active != 0 || task.budget.reserved.Total() != 0 {
		t.Fatal("bad reconciliation", task.usage(), task.budget)
	}
}

func TestBudgetReservationPreservesReviewAndIncompleteUsage(t *testing.T) {
	o, task, ctx := reservationTask(t, 100000)
	d := event.Decision{Provider: event.Codex, Role: event.RoleWorker}
	write := router.Step{Kind: router.KindEdit, Pin: &d}
	finish, ok := o.admitAgent(ctx, task, write, d, "candidate-A")
	if !ok {
		t.Fatal("first candidate denied")
	}
	finish(event.TokenUsage{Input: 10000, Incomplete: true})
	if task.budget.unknown.Total() != 50000 {
		t.Fatal("missing usage was treated as free", task.budget.unknown)
	}
	if _, ok := o.admitAgent(ctx, task, write, d, "candidate-B"); ok {
		t.Fatal("optional candidate consumed finishing reserve")
	}
	if ctx.Err() != nil || task.budgetStopped() != "" {
		t.Fatal("declining optional candidate cancelled paid work")
	}
	review, ok := o.admitAgent(ctx, task, router.Step{Kind: router.KindReview}, d, "review-final")
	if !ok {
		t.Fatal("review reserve unavailable")
	}
	review(event.TokenUsage{Input: 15000})
	if !strings.Contains(o.cost(task).Summary(), "at least") || task.usage().Total() != 25000 {
		t.Fatal("estimates leaked into reported cost", o.cost(task))
	}
}

func TestBudgetReservationWaitCancellation(t *testing.T) {
	o, task, parent := reservationTask(t, 100000)
	d := event.Decision{Provider: event.Codex}
	finish, ok := o.admitAgent(parent, task, router.Step{Kind: router.KindEdit}, d, "running")
	if !ok {
		t.Fatal("not admitted")
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan bool, 1)
	go func() { _, ok := o.admitAgent(ctx, task, router.Step{Kind: router.KindEdit}, d, "waiting"); done <- ok }()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("cancelled waiter admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not cancel")
	}
	finish(event.TokenUsage{Input: 10})
	finish(event.TokenUsage{Input: 10})
	if task.usage().Total() != 10 {
		t.Fatal("completion accounted twice")
	}
}
