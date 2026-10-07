package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
)

// shortcuts turns the task-shape shortcuts on again after newOrc's classic.
func shortcuts(c *config.Config) {
	d := config.Default().Orchestrator
	oc := &c.Orchestrator
	oc.AutoSingle, oc.LightPlanning, oc.FitBudget, oc.ReviewSkipMaxLines = d.AutoSingle, d.LightPlanning, d.FitBudget, d.ReviewSkipMaxLines
	oc.ReviewWhen = d.ReviewWhen
}

// smallSkip is shortcuts with review_when: large (skip the final review of
// a small change whose checks pass).
func smallSkip(c *config.Config) {
	shortcuts(c)
	c.Orchestrator.ReviewWhen = config.ReviewLarge
}

func TestDefaultsTurnShortcutsOn(t *testing.T) {
	oc := config.Default().Orchestrator
	if !oc.AutoSingle || !oc.LightPlanning || !oc.FitBudget || oc.ReviewSkipMaxLines != 80 {
		t.Errorf("defaults: auto_single=%v light_planning=%v fit_budget=%v review_skip_max_lines=%d", oc.AutoSingle, oc.LightPlanning, oc.FitBudget, oc.ReviewSkipMaxLines)
	}
}

func TestMentionedFiles(t *testing.T) {
	for text, want := range map[string][]string{
		"Fix internal/x/y.go and README.md, e.g. the intro.": {"internal/x/y.go", "README.md"},
		"Bump to 1.2.3 and keep v2.0 notes":                  nil,
		"Look at @src/app.ts (and `cmd\\rw\\main.go`).":      {"src/app.ts", "cmd\\rw\\main.go"},
		"See https://example.com/a.html for details":         nil,
		"Edit a.go, then a.go again":                         {"a.go"},
	} {
		if got := mentionedFiles(text); !slices.Equal(got, want) {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
	if n := listItems("Do this:\n- one\n- two\n3. three\nnot - a list"); n != 3 {
		t.Errorf("list items %d, want 3", n)
	}
}

func TestShapeTask(t *testing.T) {
	o, _ := newOrc(t, "", runner.NewFakeSet(0), shortcuts)
	cases := []struct {
		text          string
		single, light bool
		why           string
	}{
		{"Fix the off-by-one in the pagination helper so the last page is no longer dropped", true, true, "one step"},
		{"Update internal/a.go, internal/b.go and cmd/c.go to use the new logger everywhere it is created", false, true, "3 files named"},
		{"Refactor the config loader so environment overrides are applied in one place", false, true, "broad: refactor"},
		{"Make the job scheduler safe for concurrent callers: guard the queue with a lock, keep the existing public API unchanged, and make sure that cancelling a job while it is being dequeued never loses it", false, false, "difficulty"},
		{"Add rate limiting to the login handler so repeated attempts are slowed down", false, false, "sensitive: login"},
		{"Do these things in the CLI:\n- add a --json flag\n- print totals\n- sort the rows by name", false, true, "3 list items"},
		{"Create two small text files in parallel, then combine both of them into a third file", false, true, "several steps: in parallel"},
	}
	for _, c := range cases {
		tk := &task{text: c.text, cfg: o.opts.Store.Get()}
		s := o.shapeTask(tk)
		if s.single != c.single || s.light != c.light || !strings.Contains(s.why, c.why) {
			t.Errorf("%q: single=%v light=%v why=%q; want single=%v light=%v why containing %q", c.text, s.single, s.light, s.why, c.single, c.light, c.why)
		}
	}
	// auto_single off, or a forced plan: planned.
	tk := &task{text: cases[0].text, cfg: o.opts.Store.Get()}
	tk.cfg.Orchestrator.AutoSingle = false
	if s := o.shapeTask(tk); s.single {
		t.Errorf("auto_single off still ran as one step: %q", s.why)
	}
	tk = &task{text: cases[0].text, cfg: o.opts.Store.Get(), forcePlan: true}
	if s := o.shapeTask(tk); s.single {
		t.Errorf("--plan still ran as one step: %q", s.why)
	}
}

// A question that also asks for a file is a writing step (the starter
// bench's explain-low-stock ran read-only and could not write ANSWER.md);
// a plain question stays read-only.
func TestShortcutQuestionThatWrites(t *testing.T) {
	o, _ := newOrc(t, "", runner.NewFakeSet(0), shortcuts)
	for text, want := range map[string]router.Kind{
		"Where is the low-stock threshold defined and which functions use it? Write a short answer with file and function names to ANSWER.md.": router.KindEdit,
		"Where is the low-stock threshold defined and which functions of the inventory package read it at runtime?":                            router.KindExplore,
	} {
		tk := &task{text: text, cfg: o.opts.Store.Get()}
		tk.shape = o.shapeTask(tk)
		p, _, ok, _ := o.shortcutPlan(tk)
		if !ok || p.Subtasks[0].Kind != want {
			t.Errorf("%q: ok=%v plan %+v, want one %s step", text, ok, p.Subtasks, want)
		}
	}
}

// A task that runs as one step probes only the provider that step routes
// to: a Codex worker needs no Claude permission probe (the realistic bench
// spent 51k tokens on one); a Claude worker still gets it.
func TestAutoSinglePreflightsOnlyItsProvider(t *testing.T) {
	for _, worker := range []string{event.Codex, event.Claude} {
		t.Run(worker, func(t *testing.T) {
			dir := gitRepo(t)
			var mu sync.Mutex
			probes, writers := 0, 0
			set := both(func(s runner.Spec) runner.Result {
				mu.Lock()
				defer mu.Unlock()
				if s.CheckOnly {
					probes++
					return runner.Result{Final: "ran", Commands: append([]string(nil), s.AllowedCommands...)}
				}
				if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
					return approve()
				}
				writers++
				os.WriteFile(filepath.Join(s.Dir, "out.txt"), []byte("x\n"), 0o644)
				return runner.Result{Final: "done"}
			})
			o, _ := newOrc(t, dir, set, func(c *config.Config) {
				shortcuts(c)
				rc := c.Roles[event.RoleWorker]
				rc.Prefer = worker
				c.Roles[event.RoleWorker] = rc
				c.Verify.Preflight = []string{"go version"}
				c.Verify.Commands = []string{fileCheck("out.txt")}
			})
			if res := o.Run(context.Background(), oneStepTask); !res.OK {
				t.Fatalf("task: %+v", res)
			}
			want := map[string]int{event.Codex: 0, event.Claude: 1}[worker]
			if probes != want || writers != 1 {
				t.Errorf("worker on %s: probes=%d writers=%d; want %d probes, 1 writer", worker, probes, writers, want)
			}
		})
	}
}

// shapeRun records which kinds of agent a task ran.
type shapeRun struct {
	mu                                  sync.Mutex
	plans, planReviews, finals, workers int
	planPrompts, workPrompts            []string
	writeLines                          int // lines each worker writes to out.txt
}

func (r *shapeRun) set() runner.Set {
	return both(func(s runner.Spec) runner.Result {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlanReview):
			r.planReviews++
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			r.finals++
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			r.plans++
			r.planPrompts = append(r.planPrompts, s.Prompt)
			return runner.Result{Final: planJSON(
				map[string]any{"id": "a", "title": "change a", "kind": "edit", "prompt": "change a.go", "files": []string{"a.go"}},
				map[string]any{"id": "b", "title": "change b", "kind": "edit", "prompt": "change b.go", "files": []string{"b.go"}},
				map[string]any{"id": "c", "title": "change c", "kind": "edit", "prompt": "change c.go", "files": []string{"c.go"}},
			)}
		}
		r.workers++
		r.workPrompts = append(r.workPrompts, s.Prompt)
		n := max(1, r.writeLines)
		name := "out.txt" // a planned step writes its own file
		if s.StepID != "work" {
			name = "out-" + s.StepID + ".txt"
		}
		os.WriteFile(filepath.Join(s.Dir, name), []byte(strings.Repeat("line\n", n)), 0o644)
		return runner.Result{Final: "done", Files: []string{name}}
	})
}

const oneStepTask = "Fix the off-by-one in the pagination helper so the last page is no longer dropped"
const threeFileTask = "Update a.go, b.go and c.go so each of them logs through the shared logger instead of fmt"

// A task that looks like one step runs without the planner; its checks
// pass, so no final review runs (review_when: untested).
func TestAutoSingleSkipsPlannerAndSmallReview(t *testing.T) {
	dir := gitRepo(t)
	r := &shapeRun{}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		shortcuts(c)
		c.Verify.Commands = []string{fileCheck("out.txt")}
	})
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK {
		t.Fatalf("task: %+v", res)
	}
	if r.plans != 0 || r.planReviews != 0 || r.finals != 0 || r.workers != 1 {
		t.Errorf("plans=%d plan reviews=%d final reviews=%d workers=%d; want 0/0/0/1", r.plans, r.planReviews, r.finals, r.workers)
	}
	if !strings.Contains(res.Summary, "checks pass") {
		t.Errorf("summary %q", res.Summary)
	}
	if !logged(rec, "skipping the planner") || !logged(rec, "final review skipped: the checks pass") {
		t.Errorf("log lacks the shortcut reasons:\n%s", strings.Join(logLines(rec), "\n"))
	}
}

// A task auto_single made one step is still shown for plan approval, and
// a step added there runs; a small task is not shown, as before.
func TestAutoSingleStillAsksApproval(t *testing.T) {
	dir := gitRepo(t)
	r := &shapeRun{}
	o, _ := newOrc(t, dir, r.set(), shortcuts)
	var shown []Plan
	withApprover(o, &fakeApprover{plan: func(p Plan) (Plan, bool) {
		shown = append(shown, p)
		p.Subtasks = append(p.Subtasks, Subtask{ID: "extra", Title: "extra", Kind: router.KindEdit, Prompt: "also this"})
		return p, true
	}})
	if res := o.Run(context.Background(), oneStepTask); !res.OK {
		t.Fatalf("task: %+v", res)
	}
	if len(shown) != 1 || len(shown[0].Subtasks) != 1 || r.plans != 0 || r.workers != 2 {
		t.Errorf("approvals=%d plans=%d workers=%d; want 1 one-step approval, no planner, 2 workers", len(shown), r.plans, r.workers)
	}
	shown = nil
	if res := o.Run(context.Background(), "fix the typo in README"); !res.OK || len(shown) != 0 {
		t.Errorf("small task: approvals=%d: %+v", len(shown), res)
	}
}

// With review_when: large the final review still runs for a change over
// review_skip_max_lines or on a sensitive path; it runs without checks,
// and with --plan the planner runs.
func TestFinalReviewRunsWhenNotSkippable(t *testing.T) {
	t.Run("large diff", func(t *testing.T) {
		dir := gitRepo(t)
		r := &shapeRun{writeLines: 81}
		o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
			smallSkip(c)
			c.Verify.Commands = []string{fileCheck("out.txt")}
		})
		if res := o.Run(context.Background(), oneStepTask); !res.OK || r.finals != 1 {
			t.Fatalf("final reviews=%d: %+v", r.finals, res)
		}
		if !logged(rec, "final review runs: the diff has 81 lines (> 80)") {
			t.Errorf("log:\n%s", strings.Join(logLines(rec), "\n"))
		}
	})
	t.Run("no checks", func(t *testing.T) {
		dir := gitRepo(t)
		r := &shapeRun{}
		o, _ := newOrc(t, dir, r.set(), shortcuts)
		if res := o.Run(context.Background(), oneStepTask); !res.OK || r.finals != 1 {
			t.Fatalf("final reviews=%d: %+v", r.finals, res)
		}
	})
	t.Run("sensitive file", func(t *testing.T) {
		dir := gitRepo(t)
		set := both(func(s runner.Spec) runner.Result {
			if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
				return approve()
			}
			os.MkdirAll(filepath.Join(s.Dir, "auth"), 0o755)
			os.WriteFile(filepath.Join(s.Dir, "auth", "out.txt"), []byte("x\n"), 0o644)
			return runner.Result{Final: "done"}
		})
		o, rec := newOrc(t, dir, set, func(c *config.Config) {
			smallSkip(c)
			c.Verify.Commands = []string{fileCheck(filepath.Join("auth", "out.txt"))}
		})
		o.Run(context.Background(), oneStepTask)
		if !logged(rec, "final review runs: the change touches a sensitive path: auth") {
			t.Errorf("log:\n%s", strings.Join(logLines(rec), "\n"))
		}
	})
	t.Run("--plan", func(t *testing.T) {
		dir := gitRepo(t)
		r := &shapeRun{}
		o, _ := newOrc(t, dir, r.set(), shortcuts)
		if res := o.RunWith(context.Background(), oneStepTask, TaskOptions{Plan: true}); !res.OK || r.plans != 1 {
			t.Fatalf("plans=%d: %+v", r.plans, res)
		}
	})
}

// A light task's planner and reviewer run on the worker route; a hard
// one's keep their own.
func TestLightPlanningRoutes(t *testing.T) {
	dir := gitRepo(t)
	r := &shapeRun{}
	o, rec := newOrc(t, dir, r.set(), shortcuts)
	if res := o.Run(context.Background(), threeFileTask); !res.OK {
		t.Fatalf("task: %+v", res)
	}
	cfg := config.Default()
	for _, e := range rec.all() {
		if e.Kind != event.Route || e.Decision == nil {
			continue
		}
		d := e.Decision
		if d.Role != event.RolePlanner && d.Role != event.RoleReviewer {
			continue
		}
		w := cfg.Roles[event.RoleWorker].For(d.Provider)
		if d.Model != w.Model || d.Effort != w.Effort || !strings.Contains(d.Reason, "light: worker route") {
			t.Errorf("%s on %s:%s:%s (%s), want the worker route %s:%s", d.Role, d.Provider, d.Model, d.Effort, d.Reason, w.Model, w.Effort)
		}
	}

	rt := o.Router()
	hard := rt.Route(router.Step{ID: "plan", Kind: router.KindPlan, Light: false})
	if hard.Effort != cfg.Roles[event.RolePlanner].For(hard.Provider).Effort {
		t.Errorf("non-light planner moved: %+v", hard)
	}
}

func TestLightPlanningKeepsPinnedRole(t *testing.T) {
	cfg := config.Default()
	st := config.NewStore(cfg, filepath.Join(t.TempDir(), "rw.yaml"))
	rt := &router.Router{Cfg: st.Get, Pinned: func(role string) bool { return role == event.RolePlanner }}
	d := rt.Route(router.Step{ID: "plan", Kind: router.KindPlan, Light: true})
	want := cfg.Roles[event.RolePlanner].For(d.Provider)
	if d.Model != want.Model || d.Effort != want.Effort {
		t.Errorf("pinned planner moved to %s:%s", d.Model, d.Effort)
	}
}

// With a budget, the planner is told what is left, and a plan over it is
// merged into one step that still carries every step's work.
func TestFitBudgetMergesPlan(t *testing.T) {
	dir := gitRepo(t)
	r := &shapeRun{}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		shortcuts(c)
		// Without history: planner 20k, writer 60k, review 20k, fix 60k.
		// Planner + writer + finish = 160k fits; three writers (260k) do not.
		c.Budget.TaskTokens = 250_000
	})
	res := o.Run(context.Background(), threeFileTask)
	if !res.OK {
		t.Fatalf("task: %+v", res)
	}
	if r.plans != 1 || r.planReviews != 0 || r.workers != 1 {
		t.Fatalf("plans=%d plan reviews=%d workers=%d; want 1/0/1", r.plans, r.planReviews, r.workers)
	}
	if !strings.Contains(r.planPrompts[0], "BUDGET: about 250k fresh tokens are left") {
		t.Errorf("planner prompt lacks the budget:\n%s", r.planPrompts[0])
	}
	w := r.workPrompts[0]
	for _, want := range []string{"Work through this plan in order, all of it:", "change a.go", "change b.go", "change c.go"} {
		if !strings.Contains(w, want) {
			t.Errorf("merged step lacks %q:\n%s", want, w)
		}
	}
	if !logged(rec, "budget: the 3-step plan") {
		t.Errorf("log:\n%s", strings.Join(logLines(rec), "\n"))
	}
}

// A plan that fits keeps its steps; one that fits only without the plan
// review skips that.
func TestFitBudgetKeepsPlanThatFits(t *testing.T) {
	for _, c := range []struct {
		name          string
		tokens        int64
		reviews, work int
	}{
		{"room for all", 1_000_000, 1, 3},
		{"no room for the plan review", 270_000, 0, 3}, // 3x60k + 80k finish = 260k; +20k plan review does not fit
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := gitRepo(t)
			r := &shapeRun{}
			o, rec := newOrc(t, dir, r.set(), func(cf *config.Config) {
				shortcuts(cf)
				cf.Budget.TaskTokens = c.tokens
			})
			if res := o.Run(context.Background(), threeFileTask); !res.OK {
				t.Fatalf("task: %+v", res)
			}
			if r.planReviews != c.reviews || r.workers != c.work {
				t.Errorf("plan reviews=%d workers=%d; want %d/%d\n%s", r.planReviews, r.workers, c.reviews, c.work, strings.Join(logLines(rec), "\n"))
			}
		})
	}
}

// When the budget cannot fund the planner as well as the work and the
// finish, a planned-looking task runs as one step.
func TestShapeSingleWhenBudgetCannotFundPlanning(t *testing.T) {
	dir := gitRepo(t)
	r := &shapeRun{}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		shortcuts(c)
		c.Budget.TaskTokens = 150_000 // planner 20k + writer 60k + finish 80k = 160k
	})
	if res := o.Run(context.Background(), threeFileTask); !res.OK {
		t.Fatalf("task: %+v", res)
	}
	if r.plans != 0 || r.workers != 1 {
		t.Errorf("plans=%d workers=%d; want 0/1", r.plans, r.workers)
	}
	if !logged(rec, "the budget left cannot fund planning") {
		t.Errorf("log:\n%s", strings.Join(logLines(rec), "\n"))
	}
}

func TestChangedLinesAndDiffFiles(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1,2 @@\n-old\n+new\n+more\n diff --git a/no b/no\ndiff --git a/n.txt b/n.txt\nnew file mode 100644\n+++ b/n.txt\n+a\n"
	if n := changedLines(diff); n != 4 {
		t.Errorf("changed lines %d, want 4", n)
	}
	if f := diffFiles(diff); !slices.Equal(f, []string{"x.go", "n.txt"}) {
		t.Errorf("files %q", f)
	}
}

// logLines are the activity log's lines.
func logLines(rec *recorder) []string {
	var out []string
	for _, e := range rec.all() {
		if e.Kind == event.Log || e.Kind == event.Error {
			out = append(out, e.Text)
		}
	}
	return out
}

func logged(rec *recorder, sub string) bool {
	for _, l := range logLines(rec) {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
