package mcpserve

import (
	"fmt"
	"os"
	"strings"

	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/runner"
)

// Nested says why an rw mcp must refuse to start tasks ("" = it may): it
// runs under one of rw's own agents, which would hand its step back to rw
// and recurse. Two signs, because a client may not pass the environment
// on (Codex starts MCP servers with a short list of variables):
//
//   - RW_AGENT, which rw sets for every agent it starts;
//   - an rw process above this one with another program in between (the
//     agent CLI). The rw processes right above this one are skipped: a
//     Scoop shim (rw.exe) starts the real rw.exe as its child.
//
// self is this program's name (proc.ProgramName of os.Executable).
func Nested(getenv func(string) string, self string, ancestors []proc.Ancestor) string {
	if v := getenv(runner.EnvAgent); v != "" {
		return fmt.Sprintf("it was started by an agent of a running rw task (%s=%s is set)", runner.EnvAgent, oneLine(v, 20))
	}
	isRW := func(name string) bool { return name == "rw" || name == self }
	between := false
	for _, a := range ancestors {
		switch {
		case !isRW(a.Name):
			between = true
		case between:
			return fmt.Sprintf("it runs under %s (pid %d), an rw that started the agent above it", a.Name, a.PID)
		}
	}
	return ""
}

// NestedHere is Nested for this process.
func NestedHere() string {
	self, _ := os.Executable()
	return Nested(os.Getenv, proc.ProgramName(self), proc.Ancestors())
}

// callerEnv are the variables Claude Code sets for the MCP servers it
// starts: they describe the calling session (its id, pid and project, and
// a socket with a token to talk to it). rw's agents must not inherit them:
// a Claude agent would take the caller's session for its parent, and the
// token would reach every agent and hook.
var callerEnv = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_SSE_PORT",
	"CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_EMIT_STARTUP_TIMING",
	"CLAUDE_CODE_QUESTION_PREVIEW_FORMAT",
	"CLAUDE_AGENT_SDK_VERSION",
	"CLAUDE_PID",
	"CLAUDE_PROJECT_DIR",
	"MCP_CONNECTION_NONBLOCKING",
}

// DropCallerEnv removes the calling session's variables from this
// process's environment, so nothing rw starts inherits them. It returns
// the names it removed (never values).
func DropCallerEnv() []string {
	var gone []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		for _, c := range callerEnv {
			if strings.EqualFold(name, c) {
				os.Unsetenv(name)
				gone = append(gone, name)
				break
			}
		}
	}
	return gone
}
