package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/workflow"
)

func TestWorkflowRequiresApproverOnRunAndResume(t *testing.T) {
	for _, changes := range []bool{false, true} {
		t.Run(map[bool]string{false: "plan", true: "changes"}[changes], func(t *testing.T) {
			var calls atomic.Int32
			set := both(func(s runner.Spec) runner.Result {
				calls.Add(1)
				if r, ok := twoEdits(s); ok {
					return r
				}
				return runner.Result{Final: "done"}
			})
			dir := gitRepo(t)
			o, _ := newOrc(t, dir, set, nil)
			wf := &workflow.Definition{Name: "gated", Description: "approval required", Prompt: "{{task}}", ApprovePlan: !changes, ReviewChanges: changes}
			res := o.RunWith(context.Background(), longTask, TaskOptions{Workflow: wf})
			if res.OK || calls.Load() != 0 || !strings.Contains(res.Summary, "requires an approver") {
				t.Fatalf("run without approver: calls=%d, %+v", calls.Load(), res)
			}
			// Refused new tasks still retain their workflow in saved state.
			history := History(dir, 1)
			if len(history) != 1 {
				t.Fatal("refused task was not saved")
			}
			st, err := LoadTask(history[0].ID)
			if err != nil || st.Workflow == nil {
				t.Fatalf("saved workflow: %+v, %v", st, err)
			}
			// The fresh state on disk supplies the gate even for a stale caller.
			st.Workflow = nil
			res = o.RunWith(context.Background(), "", TaskOptions{Resume: st, Force: true})
			if res.OK || calls.Load() != 0 || !strings.Contains(res.Summary, "requires an approver") {
				t.Fatalf("resume without approver: calls=%d, %+v", calls.Load(), res)
			}
		})
	}
}

func TestFitBudgetPreservesMultiRepoPlan(t *testing.T) {
	o, _ := newOrc(t, "", nil, func(c *config.Config) {
		c.Orchestrator.FitBudget = true
		c.Budget.TaskTokens = 100000
	})
	tk := &task{cfg: o.opts.Store.Get(), text: "Update the API and its caller", repos: []*task{{repoName: "web"}}}
	o.startBudget(context.Background(), tk)
	defer o.endBudget(tk, o.cost(tk))
	p := Plan{Repos: []string{"primary", "web"}, Subtasks: []Subtask{
		{ID: "api", Kind: router.KindEdit, Repo: "primary", Prompt: "change handler", Files: []string{"api.go"}},
		{ID: "web", Kind: router.KindEdit, Repo: "web", Prompt: "change caller", Files: []string{"app.ts"}, DependsOn: []string{"api"}},
	}}
	got, review := o.fitPlan(tk, p, true)
	if review || !reflect.DeepEqual(got, p) {
		t.Fatalf("repository assignments or dependencies lost: %+v (review=%v)", got, review)
	}
}

// A real build succeeds while the named test fails. Neither a reviewer's
// citation nor a tests-first writer's claim proves that the test passed.
func TestAcceptanceDoesNotCertifyUnexecutedTest(t *testing.T) {
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return runner.Result{Final: `{"approve":true,"requirements":[{"id":"R1","requirement":"retry works","met":true,"evidence":"retry.go:2","test_file":"retry_test.go","test_name":"TestRetry"}]}`}
		}
		for f, content := range map[string]string{
			"go.mod":        "module example\ngo 1.26.0\n",
			"retry.go":      "package example\nfunc Retry() bool { return false }\n",
			"retry_test.go": "package example\nimport \"testing\"\nfunc TestRetry(t *testing.T) { if !Retry() { t.Fatal(\"broken\") } }\n",
		} {
			if err := os.WriteFile(filepath.Join(s.Dir, f), []byte(content), 0600); err != nil {
				return runner.Result{Err: err}
			}
		}
		return runner.Result{Final: "done"}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Verify.Commands = []string{"go build ./..."} })
	res := o.Run(context.Background(), "Fix retries")
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "broken") {
		t.Fatalf("expected the cited test to fail: %v\n%s", err, out)
	}
	if !res.OK || res.Acceptance == nil || res.Acceptance.Checks.Status != LevelPass {
		t.Fatalf("build-only task failed: %+v", res)
	}
	if c := res.Acceptance.Criteria[0]; c.Status != CritEvidence || !strings.Contains(c.Note, "execution") {
		t.Fatalf("build certified a failing test: %+v", c)
	}
	for _, name := range []string{"TestRetry", "TestInvented"} {
		a := buildAcceptance(acceptanceInput{edits: true, verifying: true, checksRan: true, verified: true,
			firstTests: []AcceptanceTest{{Requirement: "retry works", File: "retry_test.go", Name: name}},
			testFound:  (&task{dir: dir}).testFoundIn()})
		if a.Requirements.Status == LevelPass || a.Criteria[0].Status == CritVerified || a.Criteria[0].Note == "" {
			t.Errorf("tests-first claim certified without execution: %+v", a)
		}
	}
}
