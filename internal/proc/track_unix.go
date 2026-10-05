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

// groupAlive reports whether a process group with this id still has a
// process.
func groupAlive(pid int) bool { return syscall.Kill(-pid, 0) == nil }

// reap kills a leftover agent's process group. Agents run in their own
// process group (Setpgid), so the group id is the recorded pid. own: this
// rw recorded the pid since it took the directory (see ReapOwn).
func reap(pid int, stamp string, own bool) bool {
	leader := alive(pid)
	group := syscall.Kill(-pid, 0) == nil
	if !leader && !group {
		return true
	}
	if !stamped || stamp == "" {
		// No stamp (none on this OS, or the process had already ended
		// when it was recorded): a live pid cannot be told from a program
		// that got it since. Only a group this rw recorded itself, whose
		// leader is gone, is still known to be its agent's.
		if leader || !own {
			return false
		}
	} else if leader && procStamp(pid) != stamp {
		// The pid belongs to another program now. POSIX does not let a pid
		// that is still in use as a process group id be handed out again
		// (Linux and macOS check it), so the agent's group is gone.
		return true
	}
	if !leader && !own {
		// The leader is gone but its group still runs. Probably what the
		// agent started, but the group may also be a later program's that
		// got the pid after the agent's group ended (the hold of a killed
		// rw's worktree lasts days, and macOS reuses pids after 99999):
		// nothing proves which, so nothing is killed. The slot is skipped
		// and rw doctor lists the group.
		return false
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 60; i++ {
		if syscall.Kill(-pid, 0) != nil && !alive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
