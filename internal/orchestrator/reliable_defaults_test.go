package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
)

// An unavailable gate cannot approve work or trigger speculative code repairs.
func TestUnavailableFinalReviewDoesNotApprove(t *testing.T) {
	for _, checks := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchecked", true: "passing checks"}[checks], func(t *testing.T) {
			fixes := 0
			set := both(func(s runner.Spec) runner.Result {
				if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
					return runner.Result{Err: errors.New("review service unavailable")}
				}
				if strings.Contains(s.Prompt, runner.MarkerFix) {
					fixes++
				}
				return runner.Result{Final: "worker finished"}
			})
			o, rec := newOrc(t, "", set, func(c *config.Config) {
				c.Verify.Auto = false
				c.Verify.Commands = nil
				if checks {
					c.Verify.Commands = []string{"git --version"}
				}
				c.Orchestrator.ReviewBeforeDone = true
				c.Orchestrator.ReviewWhen = config.ReviewAlways
				c.Orchestrator.MaxFixRounds = 1
			})
			res := o.Run(context.Background(), "Fix formatting")
			if res.OK || strings.Contains(res.Summary, "reviewer approved") || !strings.Contains(res.Summary, "review unavailable") {
				t.Fatalf("unavailable reviewer approved work: %+v", res)
			}
			if fixes != 0 {
				t.Fatalf("review outage triggered %d code repairs", fixes)
			}
			if res.Acceptance == nil || res.Acceptance.Requirements.Status != LevelUnchecked {
				t.Fatalf("requirements: %+v", res.Acceptance)
			}
			if !logged(rec, "rw doctor") {
				t.Fatal("missing recovery instruction")
			}
		})
	}
}

func TestDefaultWorkerRetainsChecksRepairAndRecovery(t *testing.T) {
	dir := gitRepo(t)
	r := &checksRun{fixWrites: true}
	o, _ := newOrc(t, dir, r.set(), func(c *config.Config) {
		c.Orchestrator = config.Default().Orchestrator
		c.Verify.Auto = false
		c.Verify.Commands = []string{fileCheck("fixed.txt")}
	})
	res := o.Run(context.Background(), "Refactor the whole repository, update internal/a.go, internal/b.go and cmd/c.go, then add regression tests and finally update the docs")
	if !res.OK || r.workers != 1 || r.fixes != 1 || r.finals != 0 {
		t.Fatalf("default pipeline: workers=%d repairs=%d reviews=%d result=%+v", r.workers, r.fixes, r.finals, res)
	}
	if res.UndoKey == "" || res.Acceptance == nil || res.Acceptance.Checks.Status != LevelPass || res.Acceptance.Requirements.Status != LevelUnchecked {
		t.Fatalf("missing recovery or inaccurate acceptance: %+v", res)
	}
}

func TestSingleWorkerHonorsPlanAndResume(t *testing.T) {
	o, _ := newOrc(t, "", nil, func(c *config.Config) { c.Orchestrator = config.Default().Orchestrator })
	for _, tc := range []struct {
		name string
		task *task
	}{
		{"explicit", &task{forcePlan: true}},
		{"resume", &task{resumed: true, state: &TaskState{Plan: &Plan{}}}},
		{"multiple repos", &task{repos: []*task{{repoName: "other"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.task.text, tc.task.cfg = "Update the service and its callers", o.opts.Store.Get()
			if shape := o.shapeTask(tc.task); shape.single {
				t.Fatalf("lost plan boundary: %+v", shape)
			}
		})
	}
}
