package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
)

// The agent process itself must not see the tokens, while an MCP secret
// the config names explicitly still arrives.
func TestExecAgentEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	fix, _ := filepath.Abs(filepath.Join("testdata", "claude_stream.jsonl"))
	script := "#!/bin/sh\ncat > /dev/null\nenv > '" + envFile + "'\ncat '" + fix + "'\n"
	cli := filepath.Join(dir, "cli")
	if runtime.GOOS == "windows" {
		// A .cmd shim, as npm installs the real CLIs.
		script = "@echo off\r\nset > \"" + envFile + "\"\r\ntype \"" + fix + "\"\r\n"
		cli += ".cmd"
	}
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	t.Setenv("GITLAB_TOKEN", "glpat_secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-keep")
	cfg := config.Default()
	pc := cfg.Providers[event.Claude]
	pc.Command = cli
	r := NewClaude(pc, nil)
	var c collector
	spec := Spec{AgentID: "a1", Role: "worker", Prompt: "p", Dir: dir, MCP: &MCPRun{ChildEnv: []string{"RW_MCP_GH=mcp_explicit"}}}
	if res := r.Run(context.Background(), spec, c.emit); !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	s := string(env)
	if strings.Contains(s, "ghs_secret") || strings.Contains(s, "glpat_secret") {
		t.Errorf("forge token reached the agent:\n%s", s)
	}
	if !strings.Contains(s, "ANTHROPIC_API_KEY=sk-keep") || !strings.Contains(s, "RW_MCP_GH=mcp_explicit") {
		t.Errorf("model key or MCP env missing:\n%s", s)
	}
}
