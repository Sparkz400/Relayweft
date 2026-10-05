package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runTwoRepo runs a task that writes api.txt in the primary and web.txt
// in repo web.
func runTwoRepo(t *testing.T) (api, web string, res TaskResult) {
	t.Helper()
	api, web = gitRepo(t), gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: twoRepoPlan("web")}
		case strings.Contains(s.Prompt, runner.MarkerFinalReview), strings.Contains(s.Prompt, runner.MarkerPlanReview):
			return approve()
		}
		name := map[string]string{"a": "api.txt", "b": "web.txt"}[s.StepID]
		os.WriteFile(filepath.Join(s.Dir, name), []byte(s.StepID+"\n"), 0o644)
		return runner.Result{Final: "ok", Files: []string{name}}
	})
	o, _ := newOrc(t, api, set, nil)
	o.opts.Repos = []Repo{{Name: "web", Dir: web}}
	res = o.Run(context.Background(), longTask)
	if !res.OK {
		t.Fatalf("task: %+v", res)
	}
	return
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// An extra repo that lost its record of the task (pruned there, or cloned
// again) is left out like a missing folder: the primary is still undone.
func TestUndoPrimaryWhenExtraLostItsRecord(t *testing.T) {
	isolateUserConfig(t)
	api, web, res := runTwoRepo(t)
	for _, r := range strings.Fields(gitIn(t, web, "for-each-ref", "--format=%(refname)", "refs/relayweft/")) {
		gitIn(t, web, "update-ref", "-d", r)
	}
	plan, err := Undo(api, "", false, false)
	if err != nil {
		t.Fatalf("undo of the primary refused: %v", err)
	}
	if plan.Task.Key != res.UndoKey || len(plan.Missing) != 1 || !strings.HasPrefix(plan.Missing[0], "web (") || !strings.Contains(plan.Missing[0], "no record") {
		t.Fatalf("plan = %+v", plan)
	}
	if exists(filepath.Join(api, "api.txt")) {
		t.Error("the primary was not undone")
	}
	if !exists(filepath.Join(web, "web.txt")) {
		t.Error("web was touched although it has no record")
	}
	// Redo works the same way.
	if _, err := Undo(api, res.UndoKey, true, false); err != nil || !exists(filepath.Join(api, "api.txt")) {
		t.Errorf("redo: %v", err)
	}
}

// trimUndo keeps an extra repo's part of a multi-repo task beyond the
// newest 30 while the primary keeps its record, and drops it after.
func TestTrimUndoKeepsWorkspacePartWhilePrimaryHasIt(t *testing.T) {
	isolateUserConfig(t)
	api, web, res := runTwoRepo(t)
	webRoot, _ := repoRoot(web)
	apiRoot, _ := repoRoot(api)
	tree := gitIn(t, web, "rev-parse", "HEAD^{tree}")
	for i := 0; i <= undoKeep; i++ { // newer tasks of web's own
		cmd := exec.Command("git", "commit-tree", tree, "-m", "relayweft before: own task")
		cmd.Dir = web
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			fmt.Sprintf("GIT_COMMITTER_DATE=2030-01-01T00:00:%02d", i))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		c := strings.TrimSpace(string(out))
		key := fmt.Sprintf("own-%02d", i)
		gitIn(t, web, "update-ref", undoPrefix(webRoot)+key+"/before", c)
		gitIn(t, web, "update-ref", undoPrefix(webRoot)+key+"/after", c)
	}
	has := func() bool {
		list, _ := UndoList(web)
		for _, x := range list {
			if x.Key == res.UndoKey {
				return true
			}
		}
		return false
	}
	trimUndo(webRoot)
	if !has() {
		t.Fatal("web's part of the task was pruned while the primary still records it")
	}
	if _, err := Undo(api, res.UndoKey, false, false); err != nil || exists(filepath.Join(web, "web.txt")) {
		t.Fatalf("undo of both repos: %v", err)
	}
	gitIn(t, api, "update-ref", "-d", undoPrefix(apiRoot)+res.UndoKey+"/before")
	trimUndo(webRoot)
	if has() {
		t.Error("web's part outlived the primary's record")
	}
}

// When an undo fails in one repo and putting an already undone repo back
// fails too, the error names every repo and its state.
func TestUndoRollbackFailureIsReported(t *testing.T) {
	isolateUserConfig(t)
	api, web, res := runTwoRepo(t)
	orig := undoRepo
	t.Cleanup(func() { undoRepo = orig })
	undoRepo = func(dir string, tk UndoTask, redo bool, only []string) error {
		if samePath(dir, web) {
			return errors.New("web is locked")
		}
		if redo {
			return errors.New("primary is locked now")
		}
		return orig(dir, tk, redo, only)
	}
	_, err := Undo(api, res.UndoKey, false, false)
	if err == nil {
		t.Fatal("undo succeeded")
	}
	for _, want := range []string{"repo web: web is locked", "repo primary is still undone", "primary is locked now", "repo web and the repos after it were not changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	// A rollback that works says so.
	undoRepo = func(dir string, tk UndoTask, redo bool, only []string) error {
		if samePath(dir, web) {
			return errors.New("web is locked")
		}
		return orig(dir, tk, redo, only)
	}
	os.WriteFile(filepath.Join(api, "api.txt"), []byte("a\n"), 0o644) // as the task left it
	gitIn(t, api, "update-ref", "-d", undoPrefix(mustRoot(t, api))+res.UndoKey+"/undone")
	if _, err := Undo(api, res.UndoKey, false, false); err == nil || !strings.Contains(err.Error(), "repo primary was put back") {
		t.Errorf("rollback ok: %v", err)
	}
	if !exists(filepath.Join(api, "api.txt")) {
		t.Error("the primary was not put back")
	}
}

func mustRoot(t *testing.T, dir string) string {
	r, err := repoRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A task whose last agent crosses a limit is finished, not stopped: its
// work is applied and the result is OK (an unattended task, nothing left
// to start, so nobody needs to be asked).
func TestBudgetLastAgentCrossingFinishesTask(t *testing.T) {
	dir := gitRepo(t)
	var ran sync.Map
	set := both(func(s runner.Spec) runner.Result {
		r, ok := twoEdits(s)
		if !ok {
			ran.Store(s.StepID, true)
			os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte("x\n"), 0o644)
			r = runner.Result{Final: "done"}
		}
		r.Tokens = event.TokenUsage{Input: 1000}
		return r
	})
	// plan + a + b = 3000: the last agent reaches the limit exactly.
	o, rec := newOrc(t, dir, set, budgetCfg(func(b *config.BudgetCfg) { b.TaskTokens = 3000 }))
	ap := &fakeApprover{budget: func(BudgetRequest) bool { return false }}
	withApprover(o, ap)
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if !res.OK || strings.Contains(res.Summary, "budget") {
		t.Fatalf("finished task reported as %+v", res)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if !exists(filepath.Join(dir, f)) {
			t.Errorf("%s not applied", f)
		}
	}
	evs := rec.all()
	if n := countLogs(evs, event.Error, "stopped by budget"); n != 0 {
		t.Errorf("%d stop errors", n)
	}
	if n := countLogs(evs, event.Log, "budget: this task's tokens"); n != 1 {
		t.Errorf("%d notes about the crossed limit, want 1", n)
	}

	// Attended: nothing is asked when no agent is pending.
	ran = sync.Map{}
	o2, _ := newOrc(t, gitRepo(t), set, budgetCfg(func(b *config.BudgetCfg) { b.TaskTokens = 3000 }))
	withApprover(o2, ap)
	if res := o2.Run(context.Background(), longTask); !res.OK || len(ap.budgets) != 0 {
		t.Errorf("attended: %+v, asked %d times", res, len(ap.budgets))
	}
}

// reviewApprover holds step a's change review until the task's context
// ends and refuses every budget question.
type reviewApprover struct {
	inReview chan struct{}
	once     sync.Once
}

func (r *reviewApprover) ApprovePlan(ctx context.Context, task string, p Plan) (Plan, bool) {
	return p, true
}

func (r *reviewApprover) ReviewChanges(ctx context.Context, cs ChangeSet) ChangeDecision {
	if cs.StepID == "a" {
		r.once.Do(func() { close(r.inReview) })
		<-ctx.Done()
		return ChangeDecision{}
	}
	return ChangeDecision{Apply: cs.AllPaths()}
}

func (r *reviewApprover) ApproveBudget(ctx context.Context, req BudgetRequest) bool { return false }

// A budget stop while a step's changes wait for review keeps those
// (paid-for) changes on a branch.
func TestBudgetStopDuringReviewKeepsBranch(t *testing.T) {
	dir := gitRepo(t)
	ap := &reviewApprover{inReview: make(chan struct{})}
	set := both(func(s runner.Spec) runner.Result {
		r := runner.Result{Tokens: event.TokenUsage{Input: 1000}}
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			r.Final = planJSON(
				map[string]any{"id": "a", "title": "write a", "kind": "edit", "prompt": "write a", "files": []string{"a.txt"}},
				map[string]any{"id": "b", "title": "write b", "kind": "edit", "prompt": "write b", "files": []string{"b.txt"}},
				map[string]any{"id": "c", "title": "write c", "kind": "edit", "prompt": "write c", "files": []string{"c.txt"}, "depends_on": []string{"b"}},
			)
			return r
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return approve()
		}
		if s.StepID == "b" {
			<-ap.inReview // a is waiting for its review
		}
		os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte(s.StepID+"\n"), 0o644)
		r.Final = "wrote " + s.StepID
		return r
	})
	// plan + a + b = 3000 >= 2500: c's start hits the limit, the answer is
	// no, and the task stops while a is still in review.
	o, _ := newOrc(t, dir, set, budgetCfg(func(b *config.BudgetCfg) { b.TaskTokens = 2500 }))
	o.opts.Store.Update(func(c *config.Config) error {
		c.Orchestrator.Parallel = true
		c.Orchestrator.MaxThreads = 3
		c.Orchestrator.ReviewChanges = true
		return nil
	})
	withApprover(o, ap)
	done, _ := runBG(t, func(ctx context.Context) TaskResult { return o.Run(ctx, longTask) }, nil)
	var res TaskResult
	select {
	case res = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("task hangs")
	}
	if res.OK || !strings.HasPrefix(res.Summary, "stopped by budget") {
		t.Fatalf("result = %+v", res)
	}
	if exists(filepath.Join(dir, "a.txt")) || exists(filepath.Join(dir, "c.txt")) {
		t.Error("a or c landed")
	}
	branches := gitIn(t, dir, "branch", "--list", "rw/*")
	var aBranch string
	for _, b := range strings.Fields(strings.ReplaceAll(branches, "*", "")) {
		if strings.HasSuffix(b, "/a") {
			aBranch = b
		}
	}
	if aBranch == "" {
		t.Fatalf("a's reviewed changes were not kept on a branch:\n%s", branches)
	}
	if got := gitIn(t, dir, "show", aBranch+":a.txt"); got != "a" {
		t.Errorf("branch %s has a.txt = %q", aBranch, got)
	}
}

// writeDayLog records a finished task of another rw in o's log directory.
func writeDayLog(t *testing.T, o *Orchestrator, usd float64) {
	t.Helper()
	other, err := sessionlog.Open(filepath.Dir(o.opts.Log.Path()), "/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	other.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, Cost: &event.TaskCost{CostUSD: usd}})
	other.Close()
}

// Today's total is re-read at budget checks (not only at task start): a
// task another rw window finished meanwhile counts.
func TestBudgetDayTotalRefreshedAtCheck(t *testing.T) {
	old := dayMaxAge
	dayMaxAge = time.Nanosecond
	t.Cleanup(func() { dayMaxAge = old })
	var ran sync.Map
	var o *Orchestrator
	inner := budgetSet(&ran)
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerPlan) {
			writeDayLog(t, o, 5) // finished elsewhere while the planner ran
		}
		return inner[event.Codex].(scripted).fn(s)
	})
	o, _ = newOrc(t, "", set, budgetCfg(func(b *config.BudgetCfg) { b.DayUSD = 1 }))
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if res.OK || !strings.Contains(res.Summary, "today's cost $5.09 reached") || steps(&ran) != 0 {
		t.Fatalf("result = %+v, %d steps ran", res, steps(&ran))
	}
}

// A session log that cannot be read does not make today count as 0: the
// readable logs count, and a warning says so.
func TestBudgetDayKeepsWhatItCanRead(t *testing.T) {
	var ran sync.Map
	o, rec := newOrc(t, "", budgetSet(&ran), budgetCfg(func(b *config.BudgetCfg) { b.DayUSD = 1 }))
	writeDayLog(t, o, 0.95)
	if err := os.Mkdir(filepath.Join(filepath.Dir(o.opts.Log.Path()), "broken.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st := o.BudgetStatus(); st.DayUSD < 0.949 || st.DayUSD > 0.951 {
		t.Fatalf("day with an unreadable log = %+v", st)
	}
	// It keeps the limit: the task stops at the planner's cost, and says
	// why the total may be low (once).
	if res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true}); res.OK || steps(&ran) != 0 {
		t.Errorf("task ran past the day limit: %+v", res)
	}
	if n := countLogs(rec.all(), event.Log, "budget: could not read every session log"); n != 1 {
		t.Errorf("%d warnings about the unreadable log, want 1", n)
	}
}

// A limit edited while a task runs (settings, Store) applies at its next
// check.
func TestBudgetLiveLimitEditApplies(t *testing.T) {
	var ran sync.Map
	var o *Orchestrator
	set := both(func(s runner.Spec) runner.Result {
		r, ok := twoEdits(s)
		if !ok {
			ran.Store(s.StepID, true)
			o.opts.Store.Update(func(c *config.Config) error { c.Budget.TaskTokens = 1500; return nil })
			r = runner.Result{Final: "done"}
		}
		r.Tokens = event.TokenUsage{Input: 1000}
		return r
	})
	o, _ = newOrc(t, "", set, budgetCfg(func(*config.BudgetCfg) {}))
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if res.OK || !strings.HasPrefix(res.Summary, "stopped by budget: this task's tokens") || steps(&ran) != 1 {
		t.Fatalf("result = %+v, %d steps", res, steps(&ran))
	}
}

// Single-agent runs and follow-ups keep the budget too.
func TestBudgetCoversSingleRunsAndFollowUps(t *testing.T) {
	var agents atomic.Int32
	set := both(func(s runner.Spec) runner.Result {
		agents.Add(1)
		return runner.Result{Final: "ok", SessionID: "s1", Tokens: event.TokenUsage{Input: 1000, CostUSD: 0.09}}
	})
	o, _ := newOrc(t, "", set, budgetCfg(func(b *config.BudgetCfg) { b.DayUSD = 1 }))
	writeDayLog(t, o, 1.5)
	res := o.RunSingle(context.Background(), "do it", event.Claude, config.Route{Model: "m"})
	if res.OK || !strings.HasPrefix(res.Summary, "stopped by budget: today's cost") || agents.Load() != 0 {
		t.Errorf("single run: %+v, %d agents", res, agents.Load())
	}
	s := AgentSession{AgentID: "w", Provider: event.Claude, Model: "m", Role: event.RoleWorker, SessionID: "s1", Task: "t"}
	res = o.FollowUpSession(context.Background(), s, "and more")
	if res.OK || !strings.HasPrefix(res.Summary, "stopped by budget: today's cost") || agents.Load() != 0 {
		t.Errorf("follow-up: %+v, %d agents", res, agents.Load())
	}
	if o.BudgetStatus().Running {
		t.Error("a finished run is still the running task")
	}
}

// A resume whose extra repo is away for now does not fail the task: it
// stays interrupted (with its undo key), and a later resume works without
// --force.
func TestResumeWithMissingRepoStaysInterrupted(t *testing.T) {
	isolateUserConfig(t)
	api, web := gitRepo(t), gitRepo(t)
	away := web + ".away"
	if err := os.Rename(web, away); err != nil {
		t.Fatal(err)
	}
	set := both(func(s runner.Spec) runner.Result {
		if !strings.Contains(s.Prompt, runner.MarkerStep) {
			return approve()
		}
		os.WriteFile(filepath.Join(s.Dir, "web.txt"), []byte("b\n"), 0o644)
		return runner.Result{Final: "done " + s.StepID}
	})
	o, _ := newOrc(t, api, set, nil)
	st := &TaskState{ID: "resume-away", Task: longTask, Dir: api, Status: "running", Created: time.Now(), UndoKey: "orig-key",
		Repos: []Repo{{Name: "web", Dir: web}},
		Plan: &Plan{Summary: "p", Repos: []string{PrimaryRepo, "web"}, Subtasks: []Subtask{
			{ID: "a", Title: "a", Kind: router.KindEdit, Prompt: "a"},
			{ID: "b", Title: "b", Kind: router.KindEdit, Prompt: "b", DependsOn: []string{"a"}, Repo: "web"},
		}},
		Results: map[string]StepState{"a": {OK: true, Final: "a was done"}}}
	st.save()
	res := o.RunWith(context.Background(), "", TaskOptions{Resume: st})
	if res.OK || !strings.Contains(res.Summary, "not a git repository") {
		t.Fatalf("resume without the repo: %+v", res)
	}
	fresh, err := LoadTask(st.ID)
	if err != nil || fresh.Status != "running" || fresh.UndoKey != "orig-key" {
		t.Fatalf("state after the failed resume: %+v %v", fresh, err)
	}
	if err := os.Rename(away, web); err != nil {
		t.Fatal(err)
	}
	res = o.RunWith(context.Background(), "", TaskOptions{Resume: fresh})
	if !res.OK || !exists(filepath.Join(web, "web.txt")) {
		t.Fatalf("resume once the repo is back: %+v", res)
	}
}

// A planner that keeps naming unknown repos is asked again at most twice.
// A task that ends during planning must not return while the pool prewarm
// is still adding worktrees: git kept writing into the repo after Run
// returned (and the test's temp dir cleanup failed on macOS CI).
func TestEarlyEndWaitsForPrewarm(t *testing.T) {
	isolateUserConfig(t)
	dir := gitRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerPlan) {
			cancel()
			return runner.Result{Err: context.Canceled}
		}
		return runner.Result{Final: "done"}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.MaxThreads = 6 })
	res := o.Run(ctx, longTask)
	if !strings.Contains(res.Summary, "planning cancelled") {
		t.Fatalf("summary = %q, want planning cancelled", res.Summary)
	}
	worktrees := func() []string {
		es, _ := os.ReadDir(filepath.Join(dir, ".git", "worktrees"))
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		return names
	}
	before := worktrees()
	pool := poolDir(dir)
	for i := 0; i < 6; i++ {
		unlock, ok := lockSlot(filepath.Join(pool, fmt.Sprint(i)))
		if !ok {
			t.Fatalf("slot %d still locked after Run returned", i)
		}
		unlock()
	}
	time.Sleep(500 * time.Millisecond)
	if after := worktrees(); len(after) != len(before) {
		t.Errorf("worktrees changed after Run returned: %v -> %v", before, after)
	}
}

func TestPlannerUnknownRepoRetriesCapped(t *testing.T) {
	isolateUserConfig(t)
	api, web := gitRepo(t), gitRepo(t)
	var plans atomic.Int32
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			n := plans.Add(1)
			if n > 10 {
				return runner.Result{Final: twoRepoPlan("web")}
			}
			return runner.Result{Final: twoRepoPlan(fmt.Sprintf("nope%d", n))}
		case strings.Contains(s.Prompt, "[RW:") && !strings.Contains(s.Prompt, runner.MarkerStep):
			return approve()
		}
		return runner.Result{Final: "done"}
	})
	o, _ := newOrc(t, api, set, func(c *config.Config) { c.Orchestrator.ReviewBeforePlan = false })
	o.opts.Repos = []Repo{{Name: "web", Dir: web}}
	o.Run(context.Background(), longTask)
	if n := plans.Load(); n != 1+maxRepoRetries {
		t.Errorf("planner ran %d times, want %d", n, 1+maxRepoRetries)
	}
}
