//go:build !windows

package proc

import (
	"syscall"
	"time"
)

func alive(pid int) bool {
	if stamped {
		return procStamp(pid) != "" // a zombie is not alive
	}
	return syscall.Kill(pid, 0) == nil
}

// reap kills a leftover agent's process group. Agents run in their own
// process group (Setpgid), so the group id is the recorded pid.
func reap(pid int, stamp string) bool {
	leader := alive(pid)
	group := syscall.Kill(-pid, 0) == nil
	if !leader && !group {
		return true
	}
	if !stamped || stamp == "" {
		return false // cannot tell a leftover agent from a program that got the pid since
	}
	if leader && procStamp(pid) != stamp {
		// The pid belongs to another program now. POSIX does not let a pid
		// that is still in use as a process group id be handed out again
		// (Linux and macOS check it), so the agent's group is gone.
		return true
	}
	syscall.Kill(-pid, syscall.SIGKILL)
	syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 60; i++ {
		if syscall.Kill(-pid, 0) != nil && !alive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
