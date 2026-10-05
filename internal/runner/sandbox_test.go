package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/sandbox"
)

// runFakeDocker is the test binary acting as docker (SY_FAKE_DOCKER = the
// dump file). `run` records its arguments, a few variables of its
// environment and the container's standard input file, then acts as
// SY_FAKE_DOCKER_MODE says: ok (a Claude run that writes /work/sub/x.txt),
// noimage (the runtime's error), hang (until killed). Other commands are
// appended to <dump>.calls.
func runFakeDocker() int {
	dump := os.Getenv("SY_FAKE_DOCKER")
	args := os.Args[1:]
	if len(args) == 0 || args[0] != "run" {
		f, _ := os.OpenFile(dump+".calls", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
		return 0
	}
	stdin := ""
	for _, a := range args {
		if strings.HasPrefix(a, "type=bind,source=") && strings.HasSuffix(a, ",target=/sy/run,readonly") {
			src := strings.TrimSuffix(strings.TrimPrefix(a, "type=bind,source="), ",target=/sy/run,readonly")
			b, _ := os.ReadFile(filepath.Join(src, "stdin"))
			stdin = string(b)
		}
	}
	env := map[string]string{}
	for _, n := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "SY_TEST_PASS", "SY_TEST_NOT_NAMED"} {
		if v, ok := os.LookupEnv(n); ok {
			env[n] = v
		}
	}
	data, _ := json.Marshal(map[string]any{"args": args, "env": env, "stdin": stdin})
	os.WriteFile(dump, data, 0o600)
	switch os.Getenv("SY_FAKE_DOCKER_MODE") {
	case "tamper":
		// Write a submodule's .git in /work, as an agent could.
		for _, a := range args {
			if src, ok := strings.CutPrefix(a, "type=bind,source="); ok && strings.HasSuffix(src, ",target=/work") {
				src = strings.TrimSuffix(src, ",target=/work")
				os.MkdirAll(filepath.Join(src, "sub"), 0o755)
				os.WriteFile(filepath.Join(src, "sub", ".git"), []byte("gitdir: ../mine\n"), 0o644)
			}
		}
	case "noimage":
		fmt.Fprintln(os.Stderr, "Unable to find image 'switchyard-sandbox:latest' locally")
		fmt.Fprintln(os.Stderr, "docker: Error response from daemon: pull access denied for switchyard-sandbox, repository does not exist or may require 'docker login'")
		return 125
	case "hang":
		fmt.Println(`{"type":"system","subtype":"init","session_id":"s-hang","model":"m"}`)
		io.Copy(io.Discard, os.Stdin) // sy keeps it open: until killed
		time.Sleep(time.Minute)
		return 1
	}
	fmt.Println(`{"type":"system","subtype":"init","session_id":"s-1","model":"m"}`)
	fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/work/sub/x.txt"}}]}}`)
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s-1","usage":{"input_tokens":10,"output_tokens":2}}`)
	return 0
}

type dockerDump struct {
	Args  []string          `json:"args"`
	Env   map[string]string `json:"env"`
	Stdin string            `json:"stdin"`
}

// sandboxedClaude is a Claude runner in a docker sandbox driven by the
// fake docker; it returns the dump file.
func sandboxedClaude(t *testing.T, mode string) (*Exec, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := sandbox.LookPath
	sandbox.LookPath = func(string) (string, error) { return exe, nil }
	t.Cleanup(func() { sandbox.LookPath = old })
	cache := t.TempDir() // sy's sandbox state goes here, not into your cache
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	dump := filepath.Join(t.TempDir(), "dump.json")
	t.Setenv("SY_FAKE_DOCKER", dump)
	t.Setenv("SY_FAKE_DOCKER_MODE", mode)
	cfg := config.Default()
	cfg.Sandbox = config.SandboxCfg{Mode: "docker", Env: []string{"SY_TEST_PASS"}}
	pc := cfg.Providers[event.Claude]
	// Not installed here: in a sandbox the CLI comes from the image.
	pc.Command = `C:\nowhere\claude-not-installed.cmd`
	cfg.Providers[event.Claude] = pc
	x := NewClaude(pc, limits.NewDetector(cfg.LimitPatterns))
	x.Provider, x.Kind, x.Sandbox = event.Claude, event.Claude, cfg.ProviderSandbox(event.Claude)
	return x, dump
}

func readDockerDump(t *testing.T, path string) dockerDump {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake docker did not run: %v", err)
	}
	var d dockerDump
	json.Unmarshal(data, &d)
	return d
}

// An agent in a sandbox: the CLI comes from the image, the prompt goes in
// through the run folder, only the named variables reach the container
// (their values never on the command line), the forge token stays out, and
// the paths the agent prints come back as host paths.
func TestExecSandboxed(t *testing.T) {
	x, dump := sandboxedClaude(t, "ok")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-1")
	t.Setenv("SY_TEST_PASS", "pass-1")
	t.Setenv("SY_TEST_NOT_NAMED", "nope")
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	dir := t.TempDir()
	var c collector
	res := x.Run(context.Background(), Spec{AgentID: "a1", Role: event.RoleWorker, Model: "sonnet", Prompt: "multi\nline", Dir: dir}, c.emit)
	if !res.OK() || res.Final != "done" || res.SessionID != "s-1" {
		t.Fatalf("result = %+v", res)
	}
	want := filepath.ToSlash(filepath.Join(dir, "sub", "x.txt"))
	if !slices.Equal(res.Files, []string{want}) {
		t.Errorf("files = %v, want %s", res.Files, want)
	}
	var edits []string
	for _, e := range c.evs {
		if e.Kind == event.FileEdit {
			edits = append(edits, e.Text)
		}
	}
	if !slices.Equal(edits, []string{want}) {
		t.Errorf("edit events = %v", edits)
	}
	d := readDockerDump(t, dump)
	if d.Stdin != "multi\nline" {
		t.Errorf("prompt = %q", d.Stdin)
	}
	joined := strings.Join(d.Args, " ")
	for _, w := range []string{"--entrypoint sh", "-e SY_TEST_PASS", "-e ANTHROPIC_API_KEY", "sy-sandbox /sy/run/stdin claude-not-installed -p --output-format stream-json", "target=/work"} {
		if !strings.Contains(joined, w) {
			t.Errorf("args lack %q: %s", w, joined)
		}
	}
	for _, bad := range []string{"sk-ant-1", "pass-1", "ghs_secret", "GITHUB_TOKEN", "SY_TEST_NOT_NAMED", "target=/work,readonly"} {
		if strings.Contains(joined, bad) {
			t.Errorf("args have %q: %s", bad, joined)
		}
	}
	if d.Env["SY_TEST_PASS"] != "pass-1" || d.Env["ANTHROPIC_API_KEY"] != "sk-ant-1" {
		t.Errorf("named variables did not reach the runtime: %v", d.Env)
	}
	if _, ok := d.Env["GITHUB_TOKEN"]; ok {
		t.Error("the forge token reached the runtime client")
	}
}

// A read-only agent gets its folder read-only; a role the sandbox does not
// cover runs on this machine (here: the CLI is missing there).
func TestExecSandboxRoles(t *testing.T) {
	x, dump := sandboxedClaude(t, "ok")
	var c collector
	if res := x.Run(context.Background(), Spec{AgentID: "e", Role: event.RoleExplorer, ReadOnly: true, Prompt: "p", Dir: t.TempDir()}, c.emit); !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	if d := readDockerDump(t, dump); !slices.ContainsFunc(d.Args, func(a string) bool { return strings.HasSuffix(a, "target=/work,readonly") }) {
		t.Errorf("read-only agent's folder is writable: %q", d.Args)
	}
	x.Sandbox.Roles = []string{event.RoleWorker}
	res := x.Run(context.Background(), Spec{AgentID: "p", Role: event.RolePlanner, Prompt: "p", Dir: t.TempDir()}, c.emit)
	if res.OK() || !strings.Contains(res.Err.Error(), "not found on PATH") {
		t.Errorf("a role outside the sandbox did not run on this machine: %+v", res)
	}
}

// Fail closed: no runtime, or no image, fails the step with what to do;
// nothing runs on this machine instead.
func TestExecSandboxFailsClosed(t *testing.T) {
	x, _ := sandboxedClaude(t, "noimage")
	var c collector
	res := x.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Prompt: "p", Dir: t.TempDir()}, c.emit)
	if res.OK() || res.LimitHit || !strings.Contains(res.Err.Error(), "build it with") {
		t.Errorf("missing image: %+v", res)
	}
	sandbox.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	res = x.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Prompt: "p", Dir: t.TempDir()}, c.emit)
	if res.OK() || !strings.Contains(res.Err.Error(), "docker is not installed") {
		t.Errorf("missing runtime: %+v", res)
	}
}

// Cancel stops the container (docker kill), not only the client; a
// timeout does the same.
func TestExecSandboxCancel(t *testing.T) {
	x, dump := sandboxedClaude(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	var c collector
	done := make(chan Result, 1)
	go func() {
		done <- x.Run(ctx, Spec{AgentID: "a", Role: event.RoleWorker, Prompt: "p", Dir: t.TempDir()}, c.emit)
	}()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(dump); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	var res Result
	select {
	case res = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not end after cancel")
	}
	if !res.Killed {
		t.Errorf("result = %+v", res)
	}
	calls, _ := os.ReadFile(dump + ".calls")
	if !strings.Contains(string(calls), "kill sy-") || !strings.Contains(string(calls), "rm -f sy-") {
		t.Errorf("no docker kill and rm on cancel: %q", calls)
	}

	os.Remove(dump)
	os.Remove(dump + ".calls")
	res = x.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Prompt: "p", Dir: t.TempDir(), Timeout: time.Second}, c.emit)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "timed out") {
		t.Errorf("timeout: %+v", res)
	}
	if calls, _ := os.ReadFile(dump + ".calls"); !strings.Contains(string(calls), "kill sy-") {
		t.Errorf("no docker kill on timeout: %q", calls)
	}
}

// Claude's MCP config file is mounted read-only and named by its path in
// the container.
func TestExecSandboxMCP(t *testing.T) {
	x, dump := sandboxedClaude(t, "ok")
	x.MCP = config.MCPCfg{Servers: map[string]config.MCPServer{"docs": {Command: "npx"}}}
	var c collector
	if res := x.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Prompt: "p", Dir: t.TempDir()}, c.emit); !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	d := readDockerDump(t, dump)
	if !hasSeq(d.Args, "--mcp-config", sandbox.MCPDir+"/mcp.json") || !slices.ContainsFunc(d.Args, func(a string) bool { return strings.HasSuffix(a, ",target=/sy/mcp,readonly") }) {
		t.Errorf("MCP config not in the container: %q", d.Args)
	}
}

func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// An agent that writes a submodule's .git (git on this machine would
// follow it) fails its run, and the file is gone, even though the CLI
// itself reported success.
func TestExecSandboxSubmoduleTamperFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	x, _ := sandboxedClaude(t, "tamper")
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if out, err := exec.Command("git", "-C", dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(string(head))+",sub").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var c collector
	res := x.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Prompt: "p", Dir: dir}, c.emit)
	if res.OK() || res.Err == nil || !strings.Contains(res.Err.Error(), "sub/.git") {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub", ".git")); !os.IsNotExist(err) {
		t.Error("the agent's sub/.git is still there")
	}
}

// An MCP server that names sy's forge token would take it into the
// container (in its config file or environment): refused there.
func TestExecSandboxRefusesMCPForgeToken(t *testing.T) {
	x, dump := sandboxedClaude(t, "ok")
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	x.MCP = config.MCPCfg{Servers: map[string]config.MCPServer{"gh": {Command: "npx", Env: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}"}}}}
	var c collector
	res := x.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Prompt: "p", Dir: t.TempDir()}, c.emit)
	if res.OK() || !strings.Contains(res.Err.Error(), "GITHUB_TOKEN") {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(dump); err == nil {
		t.Error("the container was started")
	}
}

// In sy's sandbox Codex's own sandbox is off: it needs kernel features
// containers block, and the container is the sandbox.
func TestCodexArgsSandboxed(t *testing.T) {
	pc := config.Default().Providers[event.Codex]
	for _, ro := range []bool{false, true} {
		args := CodexArgs(pc, Spec{ReadOnly: ro, Sandboxed: true})
		if !hasSeq(args, "--sandbox", "danger-full-access") {
			t.Errorf("readonly=%v: %q", ro, args)
		}
		args = CodexArgs(pc, Spec{ReadOnly: ro, Sandboxed: true, Resume: "s"})
		if !slices.Contains(args, "sandbox_mode=danger-full-access") {
			t.Errorf("resume readonly=%v: %q", ro, args)
		}
	}
	if args := CodexArgs(pc, Spec{ReadOnly: true}); !hasSeq(args, "--sandbox", "read-only") {
		t.Errorf("without sy's sandbox: %q", args)
	}
}
