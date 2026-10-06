package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
)

// rw doctor finds rw mcp in Claude Code's and Codex's configs (project,
// user and local scope), says which rw each starts, fails for one that is
// not on PATH, and warns when rw's own agents would get it.
func TestDoctorMCPServe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	proj := t.TempDir()
	bin := t.TempDir()
	rwName := "rw"
	if runtime.GOOS == "windows" {
		rwName = "rw.exe"
	}
	os.WriteFile(filepath.Join(bin, rwName), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)

	os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte(`{"mcpServers":{"relayweft":{"command":"rw","args":["mcp"],"env":{"TOKEN":"s3cret"}},"other":{"command":"node","args":["x.js"]}}}`), 0o644)
	claude := `{"mcpServers":{"rw-user":{"command":"cmd","args":["/c","rw","mcp"]}},
	  "projects":{` + jsonString(proj) + `:{"mcpServers":{"rw-local":{"command":"missing-dir/rw","args":["mcp","--dir","x"]}}},
	              "/elsewhere":{"mcpServers":{"rw-else":{"command":"rw","args":["mcp"]}}}}}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claude), 0o644)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(`model = "x"

[mcp_servers.relayweft]
command = "rw"
args = [
  "mcp",
]

[mcp_servers.relayweft.env]
SECRET = "s3cret"

[mcp_servers.docs]
command = 'node'
args = ["docs.js", "mcp"]
`), 0o644)

	cfg := config.Default()
	cfg.MCP.Servers = map[string]config.MCPServer{"loop": {Command: "rw", Args: []string{"mcp"}}}
	var buf bytes.Buffer
	ok := func(b bool) string { return map[bool]string{true: "ok", false: "FAIL"}[b] }
	problems := doctorMCPServe(&buf, cfg, proj, ok, "warn")
	out := buf.String()
	for _, want := range []string{
		`Claude Code (project .mcp.json, "relayweft")`,
		`(user scope, ` + filepath.Join(home, ".claude.json") + `, "rw-user")`,
		`(local scope, ` + filepath.Join(home, ".claude.json") + `, "rw-local"): "missing-dir/rw" not found`,
		`Codex (` + filepath.Join(home, ".codex", "config.toml") + `, "relayweft")`,
		"rw's own agents rw mcp (loop)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"rw-else", "other", "docs", "s3cret"} {
		if strings.Contains(out, not) {
			t.Errorf("%q in:\n%s", not, out)
		}
	}
	if problems != 1 {
		t.Errorf("%d problems, want 1 (the missing rw)", problems)
	}

	// Nothing set up: one info line, no problem.
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty)
	buf.Reset()
	if doctorMCPServe(&buf, config.Default(), t.TempDir(), ok, "warn"); !strings.Contains(buf.String(), "not set up") {
		t.Errorf("without a set-up:\n%s", buf.String())
	}
}

func TestTOMLStrings(t *testing.T) {
	for in, want := range map[string]string{
		`["mcp", "--dir", 'C:\x']`: "mcp|--dir|C:\\x",
		`["a\"b", "c\\d"]`:         `a"b|c\d`,
		`[ ]`:                      "",
		`["mcp"] # a comment`:      "mcp",
		`["one",`:                  "one",
	} {
		if got := strings.Join(tomlStrings(in), "|"); got != want {
			t.Errorf("tomlStrings(%s) = %q, want %q", in, got, want)
		}
	}
	if got := tomlString(`"C:\\Tools\\rw.exe"`); got != `C:\Tools\rw.exe` {
		t.Errorf("tomlString: %q", got)
	}
	if got := tomlString(`'C:\Tools\rw.exe'`); got != `C:\Tools\rw.exe` {
		t.Errorf("tomlString literal: %q", got)
	}
}
