package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// estApprover is a fakeApprover that also takes the estimate.
type estApprover struct {
	fakeApprover
	mu    sync.Mutex
	first PlanEstimate // the estimate of the plan as proposed
	again PlanEstimate // re-estimated after an edit
	edit  func(Plan) Plan
}

func (a *estApprover) ApprovePlanEstimate(ctx context.Context, task string, p Plan, est func(Plan) PlanEstimate) (Plan, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.first = est(p)
	if a.edit != nil {
		p = a.edit(p)
		a.again = est(p)
	}
	return p, true
}

// seedHistory writes n worker edit runs on the default worker route into
// the orchestrator's session log folder, as an earlier sy in cwd would.
func seedHistory(t *testing.T, o *Orchestrator, cwd string, toks ...int64) {
	t.Helper()
	w, err := sessionlog.Open(o.logDir(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i, tok := range toks {
		w.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TS: time.Now().Add(-time.Hour), TaskID: "old", Step: string(rune('a' + i)), Kind: "edit",
			Attempt: 1, Role: event.RoleWorker, Provider: event.Codex, Model: "gpt-6.1-sol", Effort: "medium", OK: sessionlog.Bool(true),
			Tokens: &event.TokenUsage{Input: tok}, DurationMS: tok})
	}
}

// The approver sees an estimate per step from the history, the total, a
// warning when the task budget is likely too small, and a new estimate for
// the edited plan.
func TestPlanApprovalGetsEstimate(t *testing.T) {
	set := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, "", set, func(c *config.Config) {
		c.Orchestrator.ReviewBeforePlan = false
		c.Orchestrator.Parallel = true
		c.Budget.TaskTokens = 30_000
	})
	seedHistory(t, o, t.TempDir(), 10_000, 20_000, 30_000)
	ap := &estApprover{edit: func(p Plan) Plan {
		p.Subtasks[1].Role = event.RoleWorkerHigh
		return p
	}}
	withApprover(o, ap)
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	e := ap.first
	a, okA := e.Step("a")
	fin, okF := e.Step(FinalReviewID)
	if !okA || !okF || len(e.Steps) != 3 {
		t.Fatalf("steps = %+v", e.Steps)
	}
	if a.Role != event.RoleWorker || a.Route != "codex:gpt-6.1-sol:medium" || a.Source != sessionlog.SourceAll || a.Samples != 3 ||
		a.Tokens != (sessionlog.Spread{Low: 15_000, Mid: 20_000, High: 25_000}) {
		t.Fatalf("a = %+v", a)
	}
	if fin.Role != event.RoleReviewer || fin.Source != sessionlog.SourceNone || e.NoHistory != 1 {
		t.Fatalf("final review = %+v (no history %d)", fin, e.NoHistory)
	}
	if e.Tokens.Mid != 40_000+fin.Tokens.Mid {
		t.Fatalf("total tokens = %+v", e.Tokens)
	}
	// a and b run in parallel: the wall time is one step plus the review.
	if e.Seconds.Mid != 20+fin.Seconds.Mid {
		t.Fatalf("wall = %+v", e.Seconds)
	}
	if len(e.Warnings) != 1 || !strings.HasPrefix(e.Warnings[0], "likely over this task's tokens") || len(e.Budget) != 1 || !e.Budget[0].Likely {
		t.Fatalf("budget = %+v %v", e.Budget, e.Warnings)
	}
	if b, _ := ap.again.Step("b"); b.Role != event.RoleWorkerHigh || b.Source != sessionlog.SourceNone {
		t.Fatalf("re-estimate of b = %+v", b)
	}
	if countLogs(rec.all(), event.Log, "estimate: ~") != 1 || countLogs(rec.all(), event.Log, "estimate: likely over") != 1 {
		t.Error("the estimate is not in the task log")
	}
}

func TestPlanWall(t *testing.T) {
	p := Plan{Subtasks: []Subtask{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}, {ID: "c"}, {ID: "d", DependsOn: []string{"b", "c", "gone"}}}}
	w := map[string]sessionlog.Spread{"a": {Low: 1, Mid: 2, High: 3}, "b": {Low: 10, Mid: 10, High: 10}, "c": {Low: 30, Mid: 30, High: 30}, "d": {Low: 1, Mid: 1, High: 1}}
	if got := planWall(p, w, true); got != (sessionlog.Spread{Low: 31, Mid: 31, High: 31}) {
		t.Fatalf("parallel = %+v", got) // c then d
	}
	if got := planWall(p, w, false); got != (sessionlog.Spread{Low: 42, Mid: 43, High: 44}) {
		t.Fatalf("one at a time = %+v", got)
	}
	// A cycle (an edit in progress) does not hang.
	p.Subtasks[0].DependsOn = []string{"d"}
	planWall(p, w, true)
}

// `sy run --estimate`: only the planner runs, nothing is snapshotted, no
// step runs, and the working tree and refs stay as they were.
func TestEstimateRunsNothing(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("mine\n"), 0o644)
	var mu sync.Mutex
	var ran []string
	set := both(func(s runner.Spec) runner.Result {
		mu.Lock()
		ran = append(ran, s.StepID)
		mu.Unlock()
		if !s.ReadOnly {
			os.WriteFile(filepath.Join(s.Dir, "written.txt"), []byte("x"), 0o644)
		}
		if r, ok := twoEdits(s); ok {
			r.Tokens = event.TokenUsage{Input: 900}
			return r
		}
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, dir, set, nil)
	before, _ := (git{dir}).out("status", "--porcelain")
	p, e, err := o.Estimate(context.Background(), longTask)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Subtasks) != 2 || len(e.Steps) != 3 || strings.Join(ran, ",") != "plan" {
		t.Fatalf("plan %+v, estimate %+v, ran %v", p, e.Steps, ran)
	}
	after, _ := (git{dir}).out("status", "--porcelain")
	refs, _ := (git{dir}).out("for-each-ref", "refs/switchyard")
	if after != before || refs != "" {
		t.Fatalf("tree or refs changed:\n%s\n--\n%s\nrefs: %s", before, after, refs)
	}
	if hist := History(dir, 10); len(hist) != 0 {
		t.Fatalf("an estimate became a task: %+v", hist)
	}
	recs, _ := sessionlog.ReadDir(o.logDir())
	var end *sessionlog.Record
	for i, r := range recs {
		if r.Type == sessionlog.TypeTaskEnd {
			end = &recs[i]
		}
	}
	if end == nil || end.Mode != "estimate" || end.Cost == nil || end.Cost.PerProvider[event.Codex].Total() != 900 {
		t.Fatalf("task_end = %+v", end)
	}
	if countLogs(rec.all(), event.TaskDone, "estimate only") != 1 {
		t.Error("no TaskDone")
	}
	// A short task skips the planner, as in a real run: nothing runs.
	ran = nil
	if p, _, err := o.Estimate(context.Background(), "where is the parser"); err != nil || len(ran) != 0 || p.Subtasks[0].Kind != router.KindExplore {
		t.Fatalf("small task: %+v %v ran %v", p, err, ran)
	}
}
