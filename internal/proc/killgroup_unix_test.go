//go:build !windows

package proc

import "syscall"

// syscallKillGroup kills a test's process group.
func syscallKillGroup(pid int) { syscall.Kill(-pid, syscall.SIGKILL) }
