package orchestrator

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/workflow"
)

// A workflow's approvals hold on an unattended (queued or scheduled) task,
// even for a small task, and resume keeps them; the workflow changes only
// the task's own copy of the config.
func TestWorkflowApprovalsHoldUnattended(t *testing.T) {
	set := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		return runner.Result{Final: "done"}
	})
	var asked atomic.Int32
	decline := &fakeApprover{plan: func(p Plan) (Plan, bool) { asked.Add(1); return p, false }}
	gated := &workflow.Definition{Name: "gated", Description: "d", Prompt: "{{task}}", ApprovePlan: true, Checks: []string{"git --version"}}

	o, _ := newOrc(t, "", set, nil)
	withApprover(o, decline)
	before := o.opts.Store.Get()
	id := ""
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true, Workflow: gated, Started: func(s string) { id = s }})
	if asked.Load() != 1 || res.OK || !strings.Contains(res.Summary, "not approved") {
		t.Fatalf("unattended workflow task: asked %d, %+v", asked.Load(), res)
	}
	after := o.opts.Store.Get()
	if !reflect.DeepEqual(before.Verify.Commands, after.Verify.Commands) || before.Orchestrator.ApprovePlan != after.Orchestrator.ApprovePlan {
		t.Fatal("the workflow changed the shared config")
	}

	// The saved state remembers the workflow, and a resume asks again.
	st, err := LoadTask(id)
	if err != nil || st.Workflow == nil || st.Workflow.Name != "gated" {
		t.Fatalf("state %+v: %v", st, err)
	}
	o.RunWith(context.Background(), "", TaskOptions{Unattended: true, Resume: st, Force: true})
	if asked.Load() != 2 {
		t.Fatalf("resume asked %d times in all, want 2", asked.Load())
	}

	// Without approvals in the workflow, an unattended task does not ask.
	asked.Store(0)
	o2, _ := newOrc(t, "", set, nil)
	withApprover(o2, decline)
	open := &workflow.Definition{Name: "open", Description: "d", Prompt: "{{task}}"}
	if res := o2.RunWith(context.Background(), longTask, TaskOptions{Unattended: true, Workflow: open}); !res.OK || asked.Load() != 0 {
		t.Fatalf("ungated workflow: asked %d, %+v", asked.Load(), res)
	}

	// A small task skips plan approval, but not under a gated workflow.
	o3, _ := newOrc(t, "", set, func(c *config.Config) { c.Orchestrator.SmallTaskWords = 50 })
	withApprover(o3, decline)
	if res := o3.RunWith(context.Background(), "fix the typo", TaskOptions{Workflow: gated}); res.OK || asked.Load() != 1 {
		t.Fatalf("small gated task: asked %d, %+v", asked.Load(), res)
	}

	// A workflow that requires checks refuses a project without any.
	o4, _ := newOrc(t, "", set, nil)
	strict := &workflow.Definition{Name: "strict", Description: "d", Prompt: "{{task}}", RequireChecks: true}
	if res := o4.RunWith(context.Background(), longTask, TaskOptions{Workflow: strict}); res.OK || !strings.Contains(res.Summary, "workflow strict") {
		t.Fatalf("strict workflow without checks: %+v", res)
	}
}

// A workflow's budget caps bound the live limits, which may still be
// lowered while the task runs.
func TestWorkflowCapsLiveBudget(t *testing.T) {
	o, _ := newOrc(t, "", runner.NewFakeSet(0), func(c *config.Config) { c.Budget.TaskUSD = 5 })
	tk := &task{wf: &workflow.Definition{TaskUSD: 2, TaskTokens: 1000}}
	if b := o.budgetLimits(tk); b.TaskUSD != 2 || b.TaskTokens != 1000 {
		t.Fatalf("limits = %+v", b)
	}
	if err := o.opts.Store.Update(func(c *config.Config) error { c.Budget.TaskUSD = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if b := o.budgetLimits(tk); b.TaskUSD != 1 {
		t.Fatalf("a lower live limit lost: %+v", b)
	}
}
