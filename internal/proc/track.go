package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Pid files: while a directory is tracked, every agent process started in
// it (or below) is recorded in a file, so the next sy that takes over the
// directory can deal with agents a killed sy left running there.
//
// Only context-bound commands are recorded (agents, hooks, verify commands:
// exec.CommandContext sets cmd.Cancel); git helpers are short-lived and not
// process group leaders.

var (
	trackMu sync.Mutex
	tracked = map[string]string{} // directory -> pid file
)

// TrackDir records the processes started in dir in file until stop is
// called.
func TrackDir(dir, file string) (stop func()) {
	dir = filepath.Clean(dir)
	trackMu.Lock()
	tracked[dir] = file
	trackMu.Unlock()
	return func() {
		trackMu.Lock()
		if tracked[dir] == file {
			delete(tracked, dir)
		}
		trackMu.Unlock()
	}
}

func noteStart(cmd *exec.Cmd) {
	if cmd.Process == nil || cmd.Cancel == nil || cmd.Dir == "" {
		return
	}
	dir := filepath.Clean(cmd.Dir)
	trackMu.Lock()
	defer trackMu.Unlock()
	for d, file := range tracked {
		if dir == d || strings.HasPrefix(dir, d+string(filepath.Separator)) {
			pid := cmd.Process.Pid
			f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return
			}
			fmt.Fprintf(f, "%d %s\n", pid, procStamp(pid))
			f.Close()
			return
		}
	}
}

// ReapOrphans deals with the processes recorded in a pid file: processes
// that are gone (or whose pid now belongs to another program) are ignored;
// on Unix live ones are killed with their whole process group. It returns
// false when something recorded is still running and could not be killed
// or verified (always the case for a live process on Windows), so the
// caller should not use the directory. On true the pid file is removed.
func ReapOrphans(file string) bool {
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		stamp := ""
		if len(f) > 1 {
			stamp = f[1]
		}
		if !reap(pid, stamp) {
			return false
		}
	}
	os.Remove(file)
	return true
}
