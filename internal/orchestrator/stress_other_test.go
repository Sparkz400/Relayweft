//go:build !windows

package orchestrator

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// handleSlack is how far open fds may rise above the post-warm-up count.
const handleSlack = 10

// openHandles counts this process's open file descriptors (Linux /proc);
// -1 where /proc is not available (macOS).
func openHandles() int {
	es, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(es)
}

// strayProcesses counts processes the stress test must not leave behind:
// children of this process (zombies included: an un-waited child is a
// leak too) and any other process running this test binary (the fake agent
// CLI and its grandchild, even after they were reparented). -1 where /proc
// is not available.
func strayProcesses() int {
	es, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	self := os.Getpid()
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	n := 0
	for _, e := range es {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		if stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); err == nil {
			// "pid (comm) state ppid ...": comm may hold spaces and parens.
			s := string(stat)
			if i := strings.LastIndexByte(s, ')'); i > 0 {
				if f := strings.Fields(s[i+1:]); len(f) > 1 && f[1] == strconv.Itoa(self) {
					n++
					continue
				}
			}
		}
		if p, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); err == nil && exe != "" && p == exe {
			n++
		}
	}
	return n
}
