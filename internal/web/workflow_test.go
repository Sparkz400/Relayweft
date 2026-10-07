package web

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Saved workflows in the web UI: listed with what keeps them from running,
// validated when submitted, and their approvals hold for scheduled and
// queued tasks, which otherwise run unattended.
func TestWorkflowTasksKeepTheirApprovals(t *testing.T) {
	env := newEnv(t, nil)
	var wv workflowsView
	env.call("GET", "/api/workflows", nil, &wv)
	byName := map[string]workflowView{}
	for _, w := range wv.Workflows {
		byName[w.Name] = w
	}
	if len(byName) != 4 || byName["bugfix"].Problem == "" || byName["review"].Problem != "" || !byName["review"].ApprovePlan {
		t.Fatalf("workflows = %+v", wv)
	}
	for _, bad := range []map[string]string{
		{"workflow": "bugfix", "text": "fix it"}, // requires checks; none configured
		{"workflow": "nope", "text": "fix it"},
		{"workflow": "review", "text": "  "},
		{"workflow": "review", "text": "x", "single": "codex:gpt-5"},
	} {
		if res, data := env.do("POST", "/api/task", bad, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, res.StatusCode, data)
		}
	}
	if res, data := env.do("POST", "/api/schedule", map[string]string{"when": "in 2h", "workflow": "nope", "text": "x"}, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown scheduled workflow: %d %s", res.StatusCode, data)
	}

	// Scheduled: the job runs unattended, but the workflow's plan approval holds.
	var r submitResult
	env.call("POST", "/api/schedule", map[string]string{"when": "in 2h", "workflow": "review", "text": "@parser the CSV changes"}, &r)
	if r.Status != "scheduled" || !strings.Contains(r.Message, "waits for your approval") {
		t.Fatalf("schedule = %+v", r)
	}
	q := env.state().Queue
	if len(q) != 1 || q[0].Workflow != "review" || !q[0].Gated || !strings.HasPrefix(q[0].Label, "review: @parser") {
		t.Fatalf("queue = %+v", q)
	}
	env.srv.mu.Lock()
	env.srv.queue[0].at = time.Now().Add(-time.Second)
	env.srv.mu.Unlock()
	var req *Request
	waitFor(t, "the scheduled workflow's plan approval", func() bool {
		if a := env.state().Approvals; len(a) > 0 {
			req = a[0]
		}
		return req != nil
	})
	st := env.state()
	if req.Type != "plan" || !strings.Contains(req.Task, "Review: @parser the CSV changes") || st.Workflow != "review" {
		t.Fatalf("request %+v, running workflow %q", req, st.Workflow)
	}

	// Queued behind it: the same holds.
	env.call("POST", "/api/task", map[string]string{"workflow": "review", "text": "the docs"}, &r)
	if r.Status != "queued" || !strings.Contains(r.Message, "waits for your approval") {
		t.Fatalf("queue = %+v", r)
	}
	env.call("POST", "/api/approvals/"+req.ID+"/plan", map[string]any{"ok": false}, nil)
	req = nil
	waitFor(t, "the queued workflow's plan approval", func() bool {
		if a := env.state().Approvals; len(a) > 0 {
			req = a[0]
		}
		return req != nil
	})
	if !strings.Contains(req.Task, "Review: the docs") {
		t.Fatalf("request = %+v", req)
	}
	env.call("POST", "/api/approvals/"+req.ID+"/plan", map[string]any{"ok": false}, nil)
	waitFor(t, "both tasks", func() bool { s := env.state(); return !s.Running && len(s.Queue) == 0 })

	// A plain queued task still runs unattended.
	env.call("POST", "/api/schedule", map[string]string{"when": "in 2h", "text": "plain"}, &r)
	if !strings.Contains(r.Message, "unattended (no approvals)") {
		t.Fatalf("plain schedule = %+v", r)
	}
}
