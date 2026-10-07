//go:build !windows

package proc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testWatch turns the wrapper on for this test with a pipe of its own;
// closing the returned write end is what rw's death does.
func testWatch(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := watch.Swap(&watchPipe{r: r, w: w})
	t.Cleanup(func() {
		watch.Store(old)
		r.Close()
		w.Close()
	})
	return w
}

func waitGroupGone(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); groupAlive(pid); {
		if time.Now().After(end) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// The wrapper is invisible to the command: input, output, errors and the
// exit code pass through, Wait does not wait for the watcher, and the
// watcher ends with the command. Run a few times: bash (macOS's sh) only
// sometimes reported the killed watcher on stderr.
func TestWrapperPassesThrough(t *testing.T) {
	testWatch(t)
	for i := 0; i < 20 && !t.Failed(); i++ {
		passThrough(t)
	}
}

func passThrough(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `cat; printf ' "%s"' "$@"; echo oops >&2; exit 7`, "x", "a b", `c"d`)
	Prepare(cmd)
	if cmd.Path != shPath || len(cmd.Args) < 3 || cmd.Args[2] != wrapper {
		t.Fatalf("not wrapped: %s %q", cmd.Path, cmd.Args)
	}
	cmd.Stdin = strings.NewReader("hello")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	began := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Started(cmd)
	err := cmd.Wait()
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("Wait took %s: the watcher holds the command's output", took)
	}
	if code := cmd.ProcessState.ExitCode(); code != 7 || err == nil {
		t.Errorf("exit code %d (%v), want 7", code, err)
	}
	if got, want := out.String(), `hello "a b" "c"d"`; got != want {
		t.Errorf("stdout %q, want %q", got, want)
	}
	if errOut.String() != "oops\n" {
		t.Errorf("stderr %q", errOut.String())
	}
	if !waitGroupGone(cmd.Process.Pid, 2*time.Second) {
		syscallKillGroup(cmd.Process.Pid)
		t.Error("the watcher outlived its command")
	}
}

// When rw's end of the pipe closes, the wrapper kills the command and what
// it started in its group, and rw's pid file entry stays the group's id.
func TestWrapperKillsGroupWhenPipeCloses(t *testing.T) {
	w := testWatch(t)
	cmd := Shell(context.Background(), "sleep 300 & echo $!; sleep 300")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Started(cmd)
	pid := cmd.Process.Pid
	defer syscallKillGroup(pid)
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(line))
	if pg, err := syscall.Getpgid(child); err != nil || pg != pid {
		t.Fatalf("the command's child is in group %d (%v), want %d", pg, err, pid)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("the command ended on its own")
	case <-time.After(300 * time.Millisecond):
	}
	w.Close() // rw died
	if !waitGroupGone(pid, 5*time.Second) {
		t.Fatal("the command's group still runs 5s after the pipe closed")
	}
	if Alive(child) {
		t.Error("the command's child still runs")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return")
	}
}

// Without Guard nothing is wrapped (tests and commands that never call it).
func TestNoWrapperWithoutGuard(t *testing.T) {
	old := watch.Swap(nil)
	defer watch.Store(old)
	cmd := exec.Command("/bin/sh", "-c", "true")
	Prepare(cmd)
	if cmd.Path != "/bin/sh" || len(cmd.Args) != 3 || cmd.ExtraFiles != nil {
		t.Errorf("wrapped without Guard: %s %q", cmd.Path, cmd.Args)
	}
}

// rw mcp finds the rw above it by walking up the process tree. The wrapper
// is one more process between rw and its agent: the walk goes through it
// and still reaches rw.
func TestAncestorsThroughWrapper(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Ancestors is not implemented here")
	}
	testWatch(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), exe, "-test.run=^TestHelperAncestors$")
	Prepare(cmd)
	cmd.Env = append(os.Environ(), "RW_PROC_HELPER=ancestors")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var pids []int
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var pid int
		var name string
		if _, err := fmt.Sscan(line, &pid, &name); err == nil {
			pids, names = append(pids, pid), append(names, name)
		}
	}
	if len(pids) < 2 || pids[0] != cmd.Process.Pid || pids[1] != os.Getpid() {
		t.Fatalf("ancestors %v %v; want the wrapper %d, then this process %d", pids, names, cmd.Process.Pid, os.Getpid())
	}
	if self := ProgramName(exe); names[0] == self || names[1] != self {
		t.Errorf("names %v: want a shell, then %s", names, self)
	}
}

// TestHelperAncestors prints this process's ancestors, one "pid name" per
// line.
func TestHelperAncestors(t *testing.T) {
	if os.Getenv("RW_PROC_HELPER") != "ancestors" {
		t.Skip("helper process")
	}
	for _, a := range Ancestors() {
		fmt.Println(a.PID, a.Name)
	}
	os.Exit(0)
}
