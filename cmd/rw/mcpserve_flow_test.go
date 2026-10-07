package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/proc"
)

// TestMCPReviewEditFollowUp: change review through apply and reject (with
// feedback, a wrong file name, two reviews waiting at once), an edited
// plan, and a follow-up to the agent that wrote it.
func TestMCPReviewEditFollowUp(t *testing.T) {
	p := newMCPProfile(t)
	c := startMCP(t, p.bin, p.repo, p.env)
	c.initialize("2025-06-18")

	id := c.mustTool("run_task", map[string]any{"prompt": selftestTask, "review_changes": true, "approve_plan": false})["task_id"].(string)
	rejected, wrongName := false, false
	deadline := time.Now().Add(mcpTestWait)
	for {
		st := c.mustTool("task_status", map[string]any{"task_id": id, "wait_seconds": 20})
		if st["status"] == "done" {
			break
		}
		if st["status"] != "waiting" && st["status"] != "running" || time.Now().After(deadline) {
			t.Fatalf("status %v", st)
		}
		waiting, _ := st["waiting"].([]any)
		if len(waiting) > 1 {
			if _, e := c.tool("apply", map[string]any{"task_id": id}); !strings.Contains(e, "give request_id") {
				t.Errorf("apply with %d reviews waiting and no request_id: %q", len(waiting), e)
			}
		}
		for _, x := range waiting {
			w := x.(map[string]any)
			if w["type"] != "changes" {
				t.Fatalf("waiting %v", w)
			}
			ch := w["changes"].(map[string]any)
			req := w["request_id"].(string)
			files := ch["files"].([]any)
			if len(files) == 0 || files[0].(map[string]any)["patch"] == "" {
				t.Errorf("changes without files or patch: %v", ch)
			}
			if !wrongName {
				wrongName = true
				if _, e := c.tool("apply", map[string]any{"task_id": id, "request_id": req, "files": []string{"no-such-file.txt"}}); !strings.Contains(e, "not one of the changed files") {
					t.Errorf("apply of a file not in the change: %q", e)
				}
			}
			if ch["step"] == "b" && !rejected {
				rejected = true
				c.mustTool("reject", map[string]any{"task_id": id, "request_id": req, "feedback": "Please check the file once more."})
				continue
			}
			c.mustTool("apply", map[string]any{"task_id": id, "request_id": req})
		}
	}
	if !rejected {
		t.Error("step b's changes were never reviewed")
	}
	if got := readRepo(t, p.repo, stFileC); got != "selftest-a\nselftest-b\n" {
		t.Errorf("combined file %q", got)
	}

	// An edited plan replaces the planner's.
	first := id
	id = c.mustTool("run_task", map[string]any{"prompt": selftestTask, "approve_plan": true})["task_id"].(string)
	c.waitStatus(id, "waiting")
	// Review fix: no undo of the finished task while this one runs in the
	// same tree.
	if _, e := c.tool("undo", map[string]any{"task_id": first}); !strings.Contains(e, "is running in this folder") {
		t.Errorf("undo while a task runs: %q", e)
	}
	bad := map[string]any{"steps": []any{map[string]any{"id": "x", "kind": "edit", "prompt": "write <<d.txt>> containing <<edited>>", "role": "planner"}}}
	if _, e := c.tool("edit_plan", map[string]any{"task_id": id, "plan": bad}); !strings.Contains(e, "role") {
		t.Errorf("edit_plan with role planner: %q", e)
	}
	plan := map[string]any{"summary": "one step", "steps": []any{map[string]any{"id": "only", "kind": "edit", "prompt": "write <<d.txt>> containing <<edited>>"}}}
	c.mustTool("edit_plan", map[string]any{"task_id": id, "plan": plan})
	st := c.waitStatus(id, "done")
	if steps := st["plan"].(map[string]any)["steps"].([]any); len(steps) != 1 {
		t.Errorf("ran %d steps, want the edited plan's 1", len(steps))
	}
	if got := readRepo(t, p.repo, "d.txt"); got != "edited\n" {
		t.Errorf("d.txt %q", got)
	}

	// A follow-up continues the newest agent's session.
	res := c.mustTool("task_result", map[string]any{"task_id": id})
	if agents, _ := res["follow_up_agents"].([]any); len(agents) == 0 {
		t.Fatalf("no follow-up agents: %v", res)
	}
	fu := c.mustTool("follow_up", map[string]any{"message": "write <<e.txt>> containing <<followed>>"})["task_id"].(string)
	c.waitStatus(fu, "done")
	if got := readRepo(t, p.repo, "e.txt"); got != "followed\n" {
		t.Errorf("e.txt %q", got)
	}
	res = c.mustTool("task_result", map[string]any{"task_id": fu})
	if res["kind"] != "follow_up" || !strings.Contains(fmt.Sprint(res["diff"]), "e.txt") {
		t.Errorf("follow-up result %v", res)
	}
}

// TestMCPReconnect: the client goes away while an agent works. rw mcp
// stops the task and its agent and exits; the next rw mcp (the client
// reconnecting) finds the task in the history and resumes it.
func TestMCPReconnect(t *testing.T) {
	p := newMCPProfile(t, envSelftestHang+"=1")
	c := startMCP(t, p.bin, p.repo, p.env)
	c.initialize("2025-11-25")
	id := c.mustTool("run_task", map[string]any{"prompt": selftestTask, "approve_plan": false})["task_id"].(string)
	deadline := time.Now().Add(mcpTestWait)
	for !hung(p.calls) {
		if st := c.mustTool("task_status", map[string]any{"task_id": id, "wait_seconds": 3}); st["status"] != "running" || time.Now().After(deadline) {
			t.Fatalf("waiting for the combine step: %v", st)
		}
	}
	pid := 0
	fmt.Sscan(fileText(filepath.Join(p.calls, "agent.pid")), &pid)
	if !c.close(30 * time.Second) {
		t.Fatal("rw mcp did not exit within 30s after its client went away")
	}
	// A dead agent may stay a zombie for a moment until init reaps it.
	for end := time.Now().Add(5 * time.Second); pid != 0 && proc.Alive(pid) && time.Now().Before(end); {
		time.Sleep(100 * time.Millisecond)
	}
	if pid == 0 || proc.Alive(pid) {
		t.Errorf("the agent (pid %d) outlived rw mcp", pid)
	}

	c = startMCP(t, p.bin, p.repo, p.env)
	c.initialize("2025-11-25")
	rows := c.mustTool("list_tasks", map[string]any{})["tasks"].([]any)
	if len(rows) == 0 || rows[0].(map[string]any)["task_id"] != id || rows[0].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("list_tasks after reconnect: %v", rows)
	}
	st := c.mustTool("task_status", map[string]any{"task_id": id})
	if st["status"] != "cancelled" || !strings.Contains(st["next"].(string), "resume_task") {
		t.Errorf("status after reconnect: %v", st)
	}
	if _, e := c.tool("task_result", map[string]any{"task_id": id}); e != "" {
		t.Errorf("task_result of the cancelled task: %s", e)
	}
	// Review fix: another rw holds the task (its lock): resume_task says
	// it did not start, and the task's status stays the one on disk.
	unlock, ok := proc.TryLock(filepath.Join(p.configDir, "relayweft", "tasks", id+".lock"))
	if !ok {
		t.Fatal("could not take the task's lock")
	}
	if _, e := c.tool("resume_task", map[string]any{"task_id": id}); !strings.Contains(e, "not started") || !strings.Contains(e, "another rw") {
		t.Errorf("resume of a task another rw holds: %q", e)
	}
	unlock()
	if st := c.mustTool("task_status", map[string]any{"task_id": id}); st["status"] != "cancelled" {
		t.Errorf("status after a refused resume: %v", st["status"])
	}
	c.mustTool("resume_task", map[string]any{"task_id": id})
	c.waitStatus(id, "done")
	if got := readRepo(t, p.repo, stFileC); got != "selftest-a\nselftest-b\n" {
		t.Errorf("combined file %q", got)
	}
}

// TestMCPStatelessProtocol speaks MCP 2026-07-28 the way Claude Code
// 2.1.288 starts: server/discover, then requests that carry the version
// in _meta (no initialize).
func TestMCPStatelessProtocol(t *testing.T) {
	p := newMCPProfile(t)
	c := startMCP(t, p.bin, p.repo, p.env)
	meta := map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "rw-test", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	res := c.call("server/discover", map[string]any{"_meta": meta})
	if !strings.Contains(fmt.Sprint(res["supportedVersions"]), "2026-07-28") {
		t.Fatalf("discover: %v", res)
	}
	tools := c.call("tools/list", map[string]any{"_meta": meta})["tools"].([]any)
	if len(tools) != 12 {
		t.Errorf("%d tools", len(tools))
	}
	if sc, e := c.toolMeta("list_tasks", map[string]any{}, meta); e != "" || sc["tasks"] == nil {
		t.Errorf("list_tasks: %v %s", sc, e)
	}
}

// TestMCPDisconnectDuringReview (review fix): the client goes away while a
// change review waits. The task is cancelled first; the closed approver's
// "no" must not count as the person rejecting the changes.
func TestMCPDisconnectDuringReview(t *testing.T) {
	p := newMCPProfile(t)
	c := startMCP(t, p.bin, p.repo, p.env)
	c.initialize("2025-11-25")
	id := c.mustTool("run_task", map[string]any{"prompt": selftestTask, "approve_plan": false, "review_changes": true})["task_id"].(string)
	if st := c.waitStatus(id, "waiting"); !strings.Contains(fmt.Sprint(st["waiting"]), "changes") {
		t.Fatalf("waiting %v", st["waiting"])
	}
	if !c.close(30 * time.Second) {
		t.Fatal("rw mcp did not exit within 30s after its client went away")
	}
	c = startMCP(t, p.bin, p.repo, p.env)
	c.initialize("2025-11-25")
	st := c.mustTool("task_status", map[string]any{"task_id": id})
	if st["status"] != "cancelled" {
		t.Errorf("status %v", st["status"])
	}
	if s := fmt.Sprint(st["plan"]); strings.Contains(s, "rejected by you") {
		t.Errorf("a step counts as rejected by the person: %s", s)
	}
}

// TestMCPWorkflowKeepsApprovals: run_task under a saved workflow waits for
// the workflow's plan approval even when the caller says approve_plan false.
func TestMCPWorkflowKeepsApprovals(t *testing.T) {
	p := newMCPProfile(t)
	wf := filepath.Join(p.configDir, "relayweft", "workflows", "gated.yaml")
	os.MkdirAll(filepath.Dir(wf), 0o700)
	if err := os.WriteFile(wf, []byte("name: gated\ndescription: test workflow\nprompt: '{{task}}'\napprove_plan: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := startMCP(t, p.bin, p.repo, p.env)
	c.initialize("2025-06-18")
	for _, bad := range []map[string]any{
		{"prompt": selftestTask, "workflow": "nope"},
		{"prompt": selftestTask, "workflow": "gated", "read_only": true},
	} {
		if _, e := c.tool("run_task", bad); e == "" {
			t.Errorf("run_task %v accepted", bad)
		}
	}
	id := c.mustTool("run_task", map[string]any{"prompt": selftestTask, "workflow": "gated", "approve_plan": false})["task_id"].(string)
	st := c.waitStatus(id, "waiting")
	waiting, _ := st["waiting"].([]any)
	if len(waiting) != 1 || waiting[0].(map[string]any)["type"] != "plan" {
		t.Fatalf("waiting %v", st["waiting"])
	}
	c.mustTool("cancel_task", map[string]any{"task_id": id})
}
