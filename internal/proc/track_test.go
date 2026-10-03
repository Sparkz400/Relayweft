package proc

import (
	"context"
	"os"
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

// The holder can delete its lock file (sy clean); the next TryLock gets a
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

func TestTrackDirAndReapOrphans(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("killing leftover agents is verified on Linux only")
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
