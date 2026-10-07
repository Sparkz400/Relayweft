package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sparkz400/relayweft/internal/runner"
)

// mcpClient speaks MCP to a real `rw mcp` over its stdin and stdout, line
// by line, the way Claude Code and Codex do: no SDK on this side, so the
// test checks the wire format itself.
type mcpClient struct {
	t     *testing.T
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan []byte
	seq   int
	notes []map[string]any // notifications from the server
	// exited is closed when rw mcp has exited.
	exited chan struct{}
}

func startMCP(t *testing.T, bin, dir string, env []string) *mcpClient {
	t.Helper()
	cmd := exec.Command(bin, "mcp")
	cmd.Dir, cmd.Env = dir, env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &mcpClient{t: t, cmd: cmd, in: in, lines: make(chan []byte, 64)}
	go func() {
		defer close(c.lines)
		rd := bufio.NewReader(out)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				c.lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	c.exited = make(chan struct{})
	go func() { cmd.Wait(); close(c.exited) }()
	t.Cleanup(func() {
		if !c.close(20 * time.Second) {
			cmd.Process.Kill()
			<-c.exited
		}
		if t.Failed() {
			t.Logf("rw mcp stderr:\n%s", stderr.String())
			for _, kv := range env {
				if k, v, _ := strings.Cut(kv, "="); k == envSelftestDir {
					t.Logf("scripted agent calls:\n%s", fileText(filepath.Join(v, "calls.log")))
				}
			}
		}
	})
	return c
}

// close closes rw mcp's stdin (the client goes away) and reports whether
// it exited within d.
func (c *mcpClient) close(d time.Duration) bool {
	c.in.Close()
	select {
	case <-c.exited:
		return true
	case <-time.After(d):
		return false
	}
}

func (c *mcpClient) send(v map[string]any) {
	c.t.Helper()
	v["jsonrpc"] = "2.0"
	b, _ := json.Marshal(v)
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// call sends a request and returns its result (or fails on an error).
func (c *mcpClient) call(method string, params any) map[string]any {
	c.t.Helper()
	res, rpcErr := c.callRaw(method, params)
	if rpcErr != nil {
		c.t.Fatalf("%s: error %v", method, rpcErr)
	}
	return res
}

func (c *mcpClient) callRaw(method string, params any) (map[string]any, map[string]any) {
	c.t.Helper()
	c.seq++
	id := c.seq
	c.send(map[string]any{"id": id, "method": method, "params": params})
	deadline := time.After(90 * time.Second)
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				c.t.Fatalf("%s: rw mcp closed its output", method)
			}
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				c.t.Fatalf("not JSON on stdout: %q", line)
			}
			if m["jsonrpc"] != "2.0" {
				c.t.Fatalf("not JSON-RPC 2.0: %s", line)
			}
			if _, isReq := m["method"]; isReq {
				c.notes = append(c.notes, m)
				continue
			}
			if m["id"] != float64(id) {
				c.t.Fatalf("answer to id %v, want %d: %s", m["id"], id, line)
			}
			if e, ok := m["error"].(map[string]any); ok {
				return nil, e
			}
			res, _ := m["result"].(map[string]any)
			return res, nil
		case <-deadline:
			c.t.Fatalf("%s: no answer within 90s", method)
		}
	}
}

// tool calls a tool; it returns the structured result, or the error text
// when the tool failed.
func (c *mcpClient) tool(name string, args map[string]any) (map[string]any, string) {
	c.t.Helper()
	return c.toolMeta(name, args, nil)
}

func (c *mcpClient) toolMeta(name string, args, meta map[string]any) (map[string]any, string) {
	c.t.Helper()
	params := map[string]any{"name": name, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	res := c.call("tools/call", params)
	text := ""
	if content, ok := res["content"].([]any); ok && len(content) > 0 {
		text, _ = content[0].(map[string]any)["text"].(string)
	}
	if res["isError"] == true {
		return nil, text
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc == nil {
		c.t.Fatalf("%s: no structuredContent: %v", name, res)
	}
	return sc, ""
}

func (c *mcpClient) mustTool(name string, args map[string]any) map[string]any {
	c.t.Helper()
	sc, errText := c.tool(name, args)
	if errText != "" {
		c.t.Fatalf("%s %v: %s", name, args, errText)
	}
	return sc
}

func (c *mcpClient) initialize(version string) map[string]any {
	c.t.Helper()
	res := c.call("initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "rw-test", "version": "1"}})
	c.send(map[string]any{"method": "notifications/initialized"})
	return res
}

// waitStatus polls task_status (with its wait) until the task's status is
// one of want, and returns that status.
func (c *mcpClient) waitStatus(id string, want ...string) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(mcpTestWait)
	var st map[string]any
	for time.Now().Before(deadline) {
		st = c.mustTool("task_status", map[string]any{"task_id": id, "wait_seconds": 20})
		if slices.Contains(want, st["status"].(string)) {
			return st
		}
		switch st["status"] {
		case "failed", "cancelled", "done", "waiting":
			c.t.Fatalf("task %s is %s, want %v: %v", id, st["status"], want, st)
		}
	}
	c.t.Fatalf("task %s still %v after %s, want %v: %v", id, st["status"], mcpTestWait, want, st)
	return nil
}

// mcpTestWait bounds the wait for a task's status.
var mcpTestWait = 3 * time.Minute

// mcpProfile is a user profile with the scripted claude CLI and a git
// repo, for running a built rw.
type mcpProfile struct {
	bin, repo, calls string
	configDir        string // os.UserConfigDir in the profile
	env              []string
}

func newMCPProfile(t *testing.T, extraEnv ...string) mcpProfile {
	t.Helper()
	if testing.Short() {
		t.Skip("builds rw and runs whole tasks")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "rw")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	profile := filepath.Join(root, "home")
	shim, err := writeAgentShim(filepath.Join(root, "npm"), bin, "claude")
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"providers": map[string]any{
			"claude": map[string]any{"command": shim},
			"codex":  map[string]any{"disabled": true},
		},
		"notify": map[string]any{"enabled": false},
		// Scripted steps exercise planning, change review and resumption.
		"orchestrator": map[string]any{"single_worker": false, "review_before_done": true},
	}
	data, _ := yaml.Marshal(cfg)
	cfgFile := filepath.Join(profileConfigDir(profile), "relayweft", "relayweft.yaml")
	os.MkdirAll(filepath.Dir(cfgFile), 0o755)
	if err := os.WriteFile(cfgFile, data, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls")
	os.MkdirAll(calls, 0o755)
	env := profileEnv(profile, append([]string{envSelftestDir + "=" + calls, envNoSetup + "=1"}, extraEnv...)...)
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); strings.EqualFold(k, "TEMP") || strings.EqualFold(k, "TMPDIR") {
			os.MkdirAll(v, 0o755)
		}
	}
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{{"init", "-q"}, {"config", "core.autocrlf", "false"}} {
		gitIn(t, repo, env, args...)
	}
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("# mcp test\n"), 0o644)
	gitIn(t, repo, env, "add", "-A")
	gitIn(t, repo, env, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	return mcpProfile{bin: bin, repo: repo, calls: calls, configDir: profileConfigDir(profile), env: env}
}

func gitIn(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func readRepo(t *testing.T, repo, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// TestMCPServer drives a built `rw mcp` over real stdio with a scripted
// claude CLI and real git: initialize, the tool list, a task with plan
// approval polled to the end, its result, a busy refusal, cancel while an
// agent works, resume, undo, a read-only question and list_tasks.
func TestMCPServer(t *testing.T) {
	p := newMCPProfile(t, envSelftestHang+"=1", "CLAUDE_CODE_MESSAGING_TOKEN=caller-secret")
	c := startMCP(t, p.bin, p.repo, p.env)

	hello := c.initialize("2025-11-25")
	if hello["protocolVersion"] != "2025-11-25" {
		t.Errorf("protocolVersion %v", hello["protocolVersion"])
	}
	if info, _ := hello["serverInfo"].(map[string]any); info["name"] != "relayweft" {
		t.Errorf("serverInfo %v", hello["serverInfo"])
	}
	if s, _ := hello["instructions"].(string); !strings.Contains(s, "task_status") {
		t.Errorf("instructions %q", s)
	}
	var names []string
	for _, tl := range c.call("tools/list", map[string]any{})["tools"].([]any) {
		tool := tl.(map[string]any)
		names = append(names, tool["name"].(string))
		if tool["description"] == "" || tool["inputSchema"] == nil {
			t.Errorf("tool %v has no description or schema", tool["name"])
		}
	}
	for _, want := range []string{"run_task", "task_status", "task_result", "list_tasks", "cancel_task", "resume_task",
		"follow_up", "approve_plan", "edit_plan", "apply", "reject", "undo"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools/list lacks %s: %v", want, names)
		}
	}

	// No path from a tool reaches rw: a task id is not a path either.
	if _, e := c.tool("task_status", map[string]any{"task_id": "../../etc/passwd"}); !strings.Contains(e, "invalid task id") {
		t.Errorf("path as task id: %q", e)
	}
	if _, e := c.tool("run_task", map[string]any{"prompt": "  "}); e == "" {
		t.Error("an empty prompt started a task")
	}

	// A task with plan approval. The scripted planner plans four steps;
	// the combine step hangs until it is cancelled (RW_SELFTEST_HANG).
	sc := c.mustTool("run_task", map[string]any{"prompt": selftestTask, "approve_plan": true})
	id := sc["task_id"].(string)
	if id == "" || !validTaskIDForTest(id) {
		t.Fatalf("task id %q", id)
	}
	st := c.waitStatus(id, "waiting")
	waiting := st["waiting"].([]any)
	w := waiting[0].(map[string]any)
	if w["type"] != "plan" || len(w["plan"].(map[string]any)["steps"].([]any)) != 4 {
		t.Fatalf("waiting %v", w)
	}
	if _, e := c.tool("run_task", map[string]any{"prompt": "another task"}); !strings.Contains(e, "one task at a time") {
		t.Errorf("a second task while one runs: %q", e)
	}
	// Review fix: a wait on a task that already waits for an answer
	// returns at once, not after wait_seconds.
	began := time.Now()
	if st := c.mustTool("task_status", map[string]any{"task_id": id, "wait_seconds": 30}); st["status"] != "waiting" || time.Since(began) > 10*time.Second {
		t.Errorf("task_status waited %s on a waiting task: %v", time.Since(began), st["status"])
	}
	if _, e := c.tool("apply", map[string]any{"task_id": id}); !strings.Contains(e, "does not wait for a change review") {
		t.Errorf("apply without a change review: %q", e)
	}
	c.mustTool("approve_plan", map[string]any{"task_id": id})

	// Wait (with a progress token) until the combine step's agent hangs.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		st, _ = c.toolMeta("task_status", map[string]any{"task_id": id, "wait_seconds": 6}, map[string]any{"progressToken": "p1"})
		if hung(p.calls) {
			break
		}
		if st["status"] != "running" || time.Now().After(deadline) {
			t.Fatalf("waiting for the combine step: %v", st)
		}
	}
	if readRepo(t, p.repo, stFileA) != "selftest-a\n" || readRepo(t, p.repo, stFileB) != "selftest-b\n" {
		t.Errorf("the finished steps' files are not in the repo")
	}
	plan := st["plan"].(map[string]any)["steps"].([]any)
	if s := plan[1].(map[string]any); s["status"] != "done" {
		t.Errorf("step a: %v", s)
	}
	if cost, _ := st["cost"].(map[string]any); cost == nil || cost["fresh_tokens"].(float64) <= 0 {
		t.Errorf("no cost so far: %v", st["cost"])
	}

	c.mustTool("cancel_task", map[string]any{"task_id": id})
	st = c.waitStatus(id, "cancelled")
	if !strings.Contains(st["next"].(string), "resume_task") {
		t.Errorf("next after cancel: %v", st["next"])
	}
	if !slices.ContainsFunc(c.notes, func(m map[string]any) bool { return m["method"] == "notifications/progress" }) {
		t.Error("no progress notification while waiting with a progress token")
	}

	// Resume continues the combine step's own session, without the hang.
	if sc := c.mustTool("resume_task", map[string]any{"task_id": id}); sc["task_id"] != id {
		t.Fatalf("resume: %v", sc)
	}
	c.waitStatus(id, "done")
	if got := readRepo(t, p.repo, stFileC); got != "selftest-a\nselftest-b\n" {
		t.Errorf("combined file %q", got)
	}
	res := c.mustTool("task_result", map[string]any{"task_id": id})
	diff, _ := res["diff"].(map[string]any)
	if diff == nil || !strings.Contains(fmt.Sprint(diff["files"]), stFileC) || res["undo_key"] == "" {
		t.Errorf("result: %v", res)
	}
	if rp, _ := res["report"].(string); rp == "" || fileText(rp) == "" {
		t.Errorf("no report: %v", res["report"])
	}

	// Undo removes what the task created.
	c.mustTool("undo", map[string]any{"task_id": id})
	if _, err := os.Stat(filepath.Join(p.repo, stFileC)); !os.IsNotExist(err) {
		t.Errorf("undo left %s: %v", stFileC, err)
	}

	// A read-only question: one agent answers, nothing changes. Its
	// agent checks the recursion guard and the variables it got.
	sc = c.mustTool("run_task", map[string]any{"prompt": "Explain the repo " + stNestedMCP, "read_only": true})
	ro := sc["task_id"].(string)
	c.waitStatus(ro, "done")
	res = c.mustTool("task_result", map[string]any{"task_id": ro})
	answer, _ := res["answer"].(string)
	if !strings.Contains(answer, "rw_agent=set") {
		t.Errorf("the agent did not get %s: %q", runner.EnvAgent, answer)
	}
	if !strings.Contains(answer, "caller_token=unset") {
		t.Errorf("the calling session's token reached rw's agent: %q", answer)
	}
	// Without RW_AGENT the process tree tells: rw, the agent, rw mcp.
	if !strings.Contains(answer, "refuses here") || !strings.Contains(answer, "an rw that started the agent") || !strings.Contains(answer, `"isError":true`) {
		t.Errorf("an rw mcp under rw's agent (without RW_AGENT) did not refuse: %q", answer)
	}

	rows := c.mustTool("list_tasks", map[string]any{})["tasks"].([]any)
	if len(rows) < 2 || rows[0].(map[string]any)["task_id"] != ro {
		t.Errorf("list_tasks: %v", rows)
	}
}

// hung reports whether the scripted combine step is waiting to be killed.
func hung(calls string) bool {
	return strings.Contains(fileText(filepath.Join(calls, "calls.log")), "hang")
}

func validTaskIDForTest(id string) bool {
	return !strings.ContainsAny(id, `/\ `) && !strings.Contains(id, "..")
}

// TestMCPNestedRefuses: an rw mcp started with RW_AGENT set (under one of
// rw's agents) starts no task and says why, over the protocol.
func TestMCPNestedRefuses(t *testing.T) {
	// The pid must be alive (a stale RW_AGENT is ignored): the test's own.
	marker := runner.EnvAgent + "=" + strconv.Itoa(os.Getpid())
	p := newMCPProfile(t, marker)
	c := startMCP(t, p.bin, p.repo, p.env)
	hello := c.initialize("2025-06-18")
	if s, _ := hello["instructions"].(string); !strings.Contains(s, "refuses") {
		t.Errorf("instructions %q", s)
	}
	if tools := c.call("tools/list", map[string]any{})["tools"]; tools != nil && len(tools.([]any)) != 0 {
		t.Errorf("nested rw mcp offers tools: %v", tools)
	}
	_, e := c.tool("run_task", map[string]any{"prompt": "do something"})
	if !strings.Contains(e, marker) || !strings.Contains(e, "recurse") {
		t.Errorf("refusal %q", e)
	}
	if b, _ := os.ReadFile(filepath.Join(p.calls, "calls.log")); len(b) > 0 {
		t.Errorf("an agent ran: %s", b)
	}
}
