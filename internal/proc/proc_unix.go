//go:build !windows

package proc

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid = the whole process group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func guard() error { return nil }

func background(cmd *exec.Cmd) {}

// breakaway starts cmd in a session of its own: a terminal's Ctrl+C, and
// the SIGHUP when it closes, go to sy's whole process group, and a browser
// sy started (Chrome quits on both) must not die with sy.
func breakaway(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// lower renices a process (children forked later inherit it).
func lower(pid int) { _ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10) }

// Shell runs a command line through sh, without sy's tokens (WithoutSecrets).
func Shell(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	Prepare(cmd)
	cmd.Env = WithoutSecrets(os.Environ())
	return cmd
}
