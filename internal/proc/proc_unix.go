//go:build !windows

package proc

import (
	"context"
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

// lower renices a process (children forked later inherit it).
func lower(pid int) { _ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10) }

// Shell runs a command line through sh.
func Shell(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	Prepare(cmd)
	return cmd
}
