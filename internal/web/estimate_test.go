package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// A plan request carries the estimate per step and in total, the page can
// ask for a new one after an edit, and the budget warning comes along.
func TestPlanApprovalEstimate(t *testing.T) {
	env := newEnv(t, func(c *config.Config) {
		c.Orchestrator.ApprovePlan = true
		c.Orchestrator.ReviewBeforePlan = false // no budget question before the plan
		c.Budget.TaskTokens = 1000
	})
	var sub submitResult
	env.call("POST", "/api/task", map[string]string{"text": "Make the parser keep trailing empty fields and add a --strict flag"}, &sub)
	var req *Request
	waitFor(t, "a plan approval", func() bool {
		if st := env.state(); len(st.Approvals) > 0 {
			req = st.Approvals[0]
		}
		return req != nil
	})
	if req.Type != "plan" {
		t.Fatalf("first request is a %s request", req.Type)
	}
	e := req.Estimate
	if e == nil || len(e.Steps) != len(req.Plan.Subtasks)+1 {
		t.Fatalf("estimate = %+v for %d subtasks", e, len(req.Plan.Subtasks))
	}
	for _, st := range req.Plan.Subtasks {
		se, ok := e.Step(st.ID)
		if !ok || se.Route == "" || se.Source != sessionlog.SourceNone || se.Tokens.Mid <= 0 {
			t.Fatalf("step %s: %+v", st.ID, se)
		}
	}
	if _, ok := e.Step(orchestrator.FinalReviewID); !ok || e.Tokens.Mid <= 0 || len(e.Warnings) == 0 || !strings.Contains(e.Warnings[0], "this task's tokens") {
		t.Fatalf("total/warnings = %+v %v", e.Tokens, e.Warnings)
	}
	// Re-estimate an edited plan: one step left, pinned to worker_high.
	p := *req.Plan
	p.Subtasks = p.Subtasks[:1]
	p.Subtasks[0].DependsOn = nil
	p.Subtasks[0].Role = event.RoleWorkerHigh
	var again orchestrator.PlanEstimate
	env.call("POST", "/api/approvals/"+req.ID+"/estimate", map[string]any{"plan": p}, &again)
	if se, ok := again.Step(p.Subtasks[0].ID); !ok || se.Role != event.RoleWorkerHigh || len(again.Steps) > 2 {
		t.Fatalf("re-estimate = %+v", again.Steps)
	}
	env.call("POST", "/api/approvals/"+req.ID+"/plan", map[string]any{"ok": false}, nil)
	// The request is gone now.
	if res, _ := env.do("POST", "/api/approvals/"+req.ID+"/estimate", map[string]any{"plan": p}, nil); res.StatusCode != http.StatusGone {
		t.Fatalf("estimate of an answered plan: %d", res.StatusCode)
	}
	waitFor(t, "the task to end", func() bool { return !env.state().Running })
}
