package proc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A lock file deleted while held must not let a later TryLock on the old
// (unlinked) file count as holding the path.
func TestTryLockRejectsDeletedLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot.lock")
	f, err := openLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Remove(path); err != nil {
		t.Skipf("cannot delete an open file here: %v", err)
	}
	if tryLock(f) && stillAt(f, path) {
		t.Fatal("a lock on a deleted lock file counts as holding the path")
	}
	unlock, ok := TryLock(path)
	if !ok {
		t.Fatal("TryLock on the recreated path failed")
	}
	unlock()
}

// The holder can delete its lock file (rw clean); the next TryLock gets a
// fresh file.
func TestTryLockDeleteWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot.lock")
	unlock, ok := TryLock(path)
	if !ok {
		t.Fatal("lock failed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("deleting a held lock file: %v", err)
	}
	unlock2, ok := TryLock(path)
	if !ok {
		t.Fatal("lock after delete failed")
	}
	unlock2()
	unlock()
}

// A process instance's stamp: the same while it runs, "" once it ended,
// also while it is a zombie its parent has not waited for yet.
func TestProcStamp(t *testing.T) {
	if !stamped {
		t.Skip("no process stamps on " + runtime.GOOS)
	}
	self := procStamp(os.Getpid())
	if self == "" || procStamp(os.Getpid()) != self {
		t.Fatalf("own stamp %q is empty or changes", self)
	}
	// The child runs until its stdin is closed.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWaitForStdin$")
	cmd.Env = append(os.Environ(), "RW_PROC_HELPER=stdin")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer in.Close()
	first := procStamp(cmd.Process.Pid)
	if first == "" || first == self {
		t.Errorf("a running child has stamp %q (parent %q)", first, self)
	}
	if again := procStamp(cmd.Process.Pid); again != first {
		t.Errorf("a running child's stamp changed: %q, %q", first, again)
	}
	if !Alive(cmd.Process.Pid) {
		t.Error("a running child counts as gone")
	}
	in.Close()
	// Not waited for: on Unix it stays a zombie.
	deadline := time.Now().Add(20 * time.Second)
	for procStamp(cmd.Process.Pid) != "" {
		if time.Now().After(deadline) {
			t.Fatalf("an ended child (a zombie on Unix) still has stamp %q", procStamp(cmd.Process.Pid))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if Alive(cmd.Process.Pid) {
		t.Error("an ended child counts as alive")
	}
}

// TestHelperWaitForStdin is a child process for other tests: it runs until
// its stdin is closed.
func TestHelperWaitForStdin(t *testing.T) {
	if os.Getenv("RW_PROC_HELPER") != "stdin" {
		t.Skip("helper process")
	}
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// A recorded agent that ended and left a process running in its group: a
// later rw cannot prove the group is still the agent's (the pid may have
// been reused for another group meanwhile), so it does not kill it and
// lists it; the rw that recorded it kills it when it releases the folder.
func TestReapLeavesUnverifiedGroup(t *testing.T) {
	if runtime.GOOS == "windows" || !stamped {
		t.Skip("process groups with stamps: Linux, macOS")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "slot.pid")
	stop := TrackDir(dir, pidFile)
	defer stop()
	// The leader runs until its stdin closes, so it is recorded with its
	// stamp while it lives.
	cmd := Shell(context.Background(), "sleep 300 & read x")
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Started(cmd)
	pid := cmd.Process.Pid
	defer syscallKillGroup(pid)
	if data, _ := os.ReadFile(pidFile); len(strings.Fields(string(data))) != 2 {
		t.Fatalf("pid file %q lacks the leader's stamp", data)
	}
	in.Close()
	cmd.Wait() // the leader ended; the sleep still runs in its group
	if !groupAlive(pid) {
		t.Fatal("the group ended with its leader")
	}
	if ReapOrphans(pidFile) {
		t.Fatal("a later rw killed a group whose leader it cannot verify")
	}
	if !groupAlive(pid) {
		t.Fatal("the unverified group was killed")
	}
	if got := LiveOrphans(pidFile); len(got) != 1 || got[0] != pid {
		t.Errorf("LiveOrphans = %v, want the group %d listed", got, pid)
	}
	if !ReapOwn(pidFile) {
		t.Fatal("the rw that recorded the agent could not stop what it left")
	}
	if groupAlive(pid) {
		t.Error("ReapOwn left the group running")
	}
}

func TestTrackDirAndReapOrphans(t *testing.T) {
	if runtime.GOOS == "windows" || !stamped {
		t.Skip("killing leftover agents needs process stamps (Linux, macOS)")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "slot.pid")
	stop := TrackDir(dir, pidFile)
	cmd := Shell(context.Background(), "sleep 300")
	cmd.Dir = filepath.Join(dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Started(cmd)
	stop()
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	data, _ := os.ReadFile(pidFile)
	if !strings.HasPrefix(string(data), strconv.Itoa(cmd.Process.Pid)+" ") {
		t.Fatalf("pid file = %q, want pid %d", data, cmd.Process.Pid)
	}
	if !ReapOrphans(pidFile) {
		t.Fatal("ReapOrphans could not kill the leftover process")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("leftover process still running")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("pid file not removed")
	}
	// A stale entry whose pid now belongs to another process is left alone.
	os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getppid())+" 1\n"), 0o644)
	if !ReapOrphans(pidFile) {
		t.Error("a reused pid blocked the directory")
	}
}
