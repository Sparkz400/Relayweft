//go:build !windows

package proc

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	wrap(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid = the whole process group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Unix has no kill-on-close job. Instead Guard makes a pipe whose write
// end only rw holds, and never closes. Each command Prepare sets up then
// runs in a small sh wrapper that gets the read end: it runs the command
// and, in the background, waits for the pipe to close. When rw dies, for
// any reason (kill -9, a crash, a closed terminal), the system closes the
// write end and the wrapper kills its process group: the agent and all it
// started that stayed in its group. When the command ends, the wrapper
// ends its watcher and exits with the command's status.
var (
	guardMu sync.Mutex
	// watchW is the write end. It is never closed, and kept here so the
	// garbage collector never closes it either.
	watchW *os.File
	// watch is the read end the wrappers get (nil before Guard).
	watch atomic.Pointer[os.File]
)

// shPath runs the wrapper: /bin/sh, not sh from PATH, which the user and
// the agents' environment may change.
const shPath = "/bin/sh"

// wrapper runs the command ("$@") with the watch pipe on fd 3. The
// watcher reads fd 3 until rw closes it (nothing is ever written), then
// kills the whole process group (kill 0). It must not hold the command's
// output pipes, or rw's Wait would wait for it. The command does not get
// fd 3. A command killed by a signal exits with 128+n here.
const wrapper = `{ read -r x <&3; kill -KILL 0; } </dev/null >/dev/null 2>&1 &
w=$!
"$@" 3<&-
s=$?
kill -KILL $w 2>/dev/null
exit $s`

// wrap starts cmd in the wrapper once Guard has run. It leaves alone a
// command whose path did not resolve (Start reports that) and one that
// passes files of its own (the wrapper's pipe must be fd 3).
func wrap(cmd *exec.Cmd) {
	r := watch.Load()
	if r == nil || cmd.Err != nil || cmd.Path == "" || len(cmd.ExtraFiles) > 0 {
		return
	}
	path := cmd.Path
	if !strings.Contains(path, "/") {
		path = "./" + path // exec runs a bare name from Dir; sh would search PATH
	}
	args := []string{shPath, "-c", wrapper, "rw-agent", path}
	if len(cmd.Args) > 1 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Path, cmd.Args = shPath, args
	cmd.ExtraFiles = []*os.File{r}
}

func guard() error {
	guardMu.Lock()
	defer guardMu.Unlock()
	if watch.Load() != nil {
		return nil
	}
	if _, err := os.Stat(shPath); err != nil {
		return err
	}
	// os.Pipe sets close-on-exec: no other child holds the write end, so
	// it closes exactly when rw ends.
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	watchW = w
	watch.Store(r)
	return nil
}

func background(cmd *exec.Cmd) {}

// breakaway starts cmd in a session of its own: a terminal's Ctrl+C, and
// the SIGHUP when it closes, go to rw's whole process group, and a browser
// rw started (Chrome quits on both) must not die with rw. It does not go
// through Prepare, so it gets no wrapper either.
func breakaway(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// lower renices a process (children forked later inherit it). A process
// group leader from Prepare is reniced with its whole group: its wrapper
// may have started the agent already.
func lower(cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	if a := cmd.SysProcAttr; a != nil && a.Setpgid && a.Pgid == 0 {
		_ = syscall.Setpriority(syscall.PRIO_PGRP, pid, 10)
		return
	}
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10)
}

// Shell runs a command line through sh, without rw's tokens (WithoutSecrets).
func Shell(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	Prepare(cmd)
	cmd.Env = WithoutSecrets(os.Environ())
	return cmd
}
