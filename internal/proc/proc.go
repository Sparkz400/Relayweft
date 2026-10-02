// Package proc starts CLI subprocesses so that killing an agent kills its
// whole process tree (npm shims spawn node, which spawns tools), on Windows
// and Unix alike.
package proc

import (
	"os/exec"
	"time"
)

// Resolve finds a command on PATH. On Windows exec.LookPath honours PATHEXT,
// so "codex" resolves to the npm shim codex.cmd.
func Resolve(name string) (string, error) { return exec.LookPath(name) }

// Prepare configures cmd so cancelling its context kills the whole tree.
func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
	cmd.WaitDelay = 5 * time.Second
}

// Guard makes sure child processes die with this process (a Windows job
// object with kill-on-close). It is a no-op elsewhere, where agents get
// their own process group and are killed explicitly.
func Guard() error { return guard() }
