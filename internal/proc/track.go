package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
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
	trackMu.Lock()
	defer trackMu.Unlock()
	if file := trackedFile(cmd.Dir); file != "" {
		pid := cmd.Process.Pid
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		fmt.Fprintf(f, "%d %s\n", pid, procStamp(pid))
		f.Close()
	}
}

// trackedFile is the pid file of the tracked directory dir is in ("" when
// none). trackMu must be held.
func trackedFile(dir string) string {
	if dir == "" {
		return ""
	}
	dir = filepath.Clean(dir)
	for d, file := range tracked {
		if dir == d || strings.HasPrefix(dir, d+string(filepath.Separator)) {
			return file
		}
	}
	return ""
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
		if f[0] == containerTag {
			if len(f) == 3 && !RemoveContainer(f[1], f[2]) {
				return false
			}
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

// Alive reports whether the process with this pid is still running.
func Alive(pid int) bool { return pid > 0 && alive(pid) }

// Identity identifies a running process beyond its pid (its start time),
// so a pid the system gave to another program since is not mistaken for
// it. "" when the process is gone, or where sy cannot tell (macOS: use
// Alive there).
func Identity(pid int) string { return procStamp(pid) }

// Containers: a sandboxed agent runs in a container (internal/sandbox).
// When the sy that started it dies, the container may outlive it, so it
// is recorded in the tracked directory's pid file as
// "container <runtime> <name>", and ReapOrphans removes it before the
// next sy uses the directory.

const containerTag = "container"

// NoteContainer records a container whose agent works in dir, when dir is
// tracked (TrackDir).
func NoteContainer(dir, runtime, name string) {
	if !validContainer(runtime, name) {
		return
	}
	trackMu.Lock()
	defer trackMu.Unlock()
	if file := trackedFile(dir); file != "" {
		if f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "%s %s %s\n", containerTag, runtime, name)
			f.Close()
		}
	}
}

// ForgetContainer drops what NoteContainer recorded, once the container
// is gone.
func ForgetContainer(dir, runtime, name string) {
	trackMu.Lock()
	defer trackMu.Unlock()
	file := trackedFile(dir)
	if file == "" {
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	drop := containerTag + " " + runtime + " " + name
	var keep []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && strings.TrimSpace(line) != drop {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		os.Remove(file)
		return
	}
	os.WriteFile(file, []byte(strings.Join(keep, "\n")+"\n"), 0o644)
}

// validContainer accepts only the runtimes sy runs and the names it gives
// its containers (sy-...): a pid file never makes sy run another program
// or remove another container.
func validContainer(runtime, name string) bool {
	if runtime != "docker" && runtime != "podman" || !strings.HasPrefix(name, "sy-") || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

var (
	// containerWait bounds one `docker rm -f`.
	containerWait = 30 * time.Second
	// lookRuntime finds docker or podman; tests swap in a fake.
	lookRuntime = Resolve
)

// RemoveContainer stops and removes a container sy started (docker rm -f).
// It reports false only when the runtime did not answer in time, so the
// container may still be running. A container that is already gone, and
// a runtime that is not installed or not running (its containers cannot
// run either), count as removed.
func RemoveContainer(runtime, name string) bool {
	if !validContainer(runtime, name) {
		return true // not one of sy's: never touched
	}
	bin, err := lookRuntime(runtime)
	if err != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), containerWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "rm", "-f", name)
	background(cmd)
	cmd.Env = WithoutSecrets(os.Environ())
	// "No such container" and "cannot connect to the daemon" both mean it
	// is not running.
	cmd.Run()
	return ctx.Err() == nil
}

// LiveOrphans returns the processes recorded in a pid file that are still
// running (the same process, not a program that got the pid since),
// without touching them.
func LiveOrphans(file string) []int {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var out []int
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
		if cur := procStamp(pid); cur != "" {
			if stamp == "" || cur == stamp {
				out = append(out, pid)
			}
		} else if stamp == "" && alive(pid) { // no stamps on this OS
			out = append(out, pid)
		}
	}
	return out
}
