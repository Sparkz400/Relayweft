package mcpserve

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/runner"
)

func TestNested(t *testing.T) {
	noEnv := func(string) string { return "" }
	chain := func(names ...string) []proc.Ancestor {
		var out []proc.Ancestor
		for i, n := range names {
			out = append(out, proc.Ancestor{PID: 100 + i, Name: n})
		}
		return out
	}
	for _, tc := range []struct {
		name      string
		env       func(string) string
		self      string
		ancestors []proc.Ancestor
		want      string // "" = may start tasks
	}{
		{"a terminal", noEnv, "rw", chain("claude", "node", "bash", "systemd"), ""},
		{"no ancestors known", noEnv, "rw", nil, ""},
		// A Scoop shim (rw.exe) starts the real rw.exe as its child.
		{"behind a Scoop shim", noEnv, "rw", chain("rw", "node", "cmd", "explorer"), ""},
		{"RW_AGENT", func(k string) string {
			if k == runner.EnvAgent {
				return "77"
			}
			return ""
		}, "rw", nil, "RW_AGENT=77"},
		// rw 78 is gone: a program an agent started kept the variable.
		{"stale RW_AGENT", func(k string) string {
			if k == runner.EnvAgent {
				return "78"
			}
			return ""
		}, "rw", chain("claude", "code", "explorer"), ""},
		{"RW_AGENT that is no pid", func(k string) string {
			if k == runner.EnvAgent {
				return "yes"
			}
			return ""
		}, "rw", nil, "RW_AGENT=yes"},
		{"rw above the agent", noEnv, "rw", chain("node", "cmd", "rw", "bash"), "pid 102"},
		{"behind a shim, under rw's agent", noEnv, "rw", chain("rw", "codex", "node", "cmd", "rw", "pwsh"), "pid 104"},
		// rw renamed: its own name counts too.
		{"renamed rw above the agent", noEnv, "relayweft", chain("claude", "relayweft"), "pid 101"},
	} {
		got := Nested(tc.env, tc.self, tc.ancestors, func(pid int) bool { return pid == 77 })
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The calling Claude Code session's variables (a messaging token among
// them) are gone from rw's environment, and with it from its agents'.
func TestDropCallerEnv(t *testing.T) {
	for _, name := range callerEnv { // restored after the test
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "secret")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1") // the user's own setting stays
	gone := DropCallerEnv()
	if os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN") != "" || os.Getenv("CLAUDECODE") != "" {
		t.Error("the caller's session variables are still set")
	}
	if os.Getenv("CLAUDE_CODE_USE_BEDROCK") != "1" {
		t.Error("a user setting was dropped")
	}
	if len(gone) != 2 {
		t.Errorf("dropped %v", gone)
	}
	for _, g := range gone {
		if strings.Contains(g, "secret") {
			t.Error("a value in the list of names")
		}
	}
}

func TestLower(t *testing.T) {
	for _, tc := range []struct{ conf, req, want float64 }{
		{0, 0, 0}, {5, 0, 5}, {0, 2, 2}, {5, 2, 2}, {2, 5, 2},
	} {
		if got := lower(tc.conf, tc.req); got != tc.want {
			t.Errorf("lower(%v, %v) = %v, want %v", tc.conf, tc.req, got, tc.want)
		}
	}
}

func TestClipBytes(t *testing.T) {
	s := strings.Repeat("ä", 10) // 2 bytes each
	got := clipBytes(s, 5)
	if !utf8.ValidString(got) || !strings.HasPrefix(got, "ää") || strings.HasPrefix(got, "äää") {
		t.Errorf("%q", got)
	}
	if clipBytes("short", 10) != "short" {
		t.Error("clipped a short string")
	}
}

func TestValidID(t *testing.T) {
	for id, ok := range map[string]bool{
		"CON":                         false, // a Windows device
		"nul.json":                    false,
		"com1":                        false,
		"console-task":                true,
		"20261005-232707-4ef3-task-1": true,
		"../tasks/x":                  false,
		`..\x`:                        false,
		"a/b":                         false,
		"":                            false,
		"x..y":                        false,
		strings.Repeat("a", 201):      false,
	} {
		if validID(id) != ok {
			t.Errorf("validID(%q) = %v", id, !ok)
		}
	}
}
