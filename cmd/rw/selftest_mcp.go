package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/runner"
)

// stNestedMCP in a prompt makes the scripted agent check the recursion
// guard the way a real agent CLI would meet it: it starts `rw mcp` under
// itself, without RW_AGENT (as Codex does, which passes MCP servers only a
// short list of variables), and asks it for a task. Its answer reports
// what that rw mcp said and which variables the agent itself got.
const stNestedMCP = "<<nested rw mcp check>>"

// selftestNestedMCP runs the check and returns the agent's answer.
func selftestNestedMCP() string {
	self, err := os.Executable()
	if err != nil {
		return "nested: " + err.Error()
	}
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !strings.EqualFold(name, runner.EnvAgent) {
			env = append(env, kv)
		}
	}
	set := func(name string) string {
		if os.Getenv(name) != "" {
			return "set"
		}
		return "unset"
	}
	report := fmt.Sprintf("rw_agent=%s caller_token=%s nested=", set(runner.EnvAgent), set("CLAUDE_CODE_MESSAGING_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// A real agent CLI is another program between rw and its MCP
	// servers; here the agent is rw itself, so on Unix a shell stands in
	// (`; exit` keeps it from exec'ing rw in its place). On Windows the
	// .cmd shim's cmd.exe is already between.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, self, "mcp")
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", `"$0" mcp; exit $?`, self)
	}
	cmd.Env = env
	cmd.Stderr = nil
	in, err := cmd.StdinPipe()
	if err != nil {
		return report + err.Error()
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return report + err.Error()
	}
	if err := cmd.Start(); err != nil {
		return report + err.Error()
	}
	defer func() { _ = cmd.Wait() }()
	defer in.Close()
	rd := bufio.NewReader(out)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = in.Write(append(b, '\n'))
	}
	recv := func(id int) map[string]any {
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return nil
			}
			var m map[string]any
			if json.Unmarshal(line, &m) == nil && m["id"] == float64(id) {
				return m
			}
		}
	}
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "selftest", "version": "1"}}})
	if recv(1) == nil {
		return report + "no answer to initialize"
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": "run_task", "arguments": map[string]any{"prompt": "a nested task"}}})
	res := recv(2)
	b, _ := json.Marshal(res["result"])
	return report + string(b)
}
