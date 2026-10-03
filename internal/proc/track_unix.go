//go:build !windows

package proc

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// procStamp identifies a process instance beyond its pid (Linux: its start
// time), so a recorded pid that was reused by another program is not
// mistaken for a leftover agent. "" when the process is gone or a zombie,
// or on systems without /proc.
func procStamp(pid int) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state); start time is field 22
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return ""
	}
	return f[19]
}

func alive(pid int) bool {
	if runtime.GOOS == "linux" {
		return procStamp(pid) != ""
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
	if runtime.GOOS != "linux" || stamp == "" {
		return false // cannot tell a leftover agent from a program that got the pid since
	}
	if leader && procStamp(pid) != stamp {
		// The pid belongs to another program now. Linux does not hand out
		// a pid that is still in use as a process group id, so the agent's
		// group is gone.
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
