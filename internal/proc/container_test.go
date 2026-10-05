package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain: with SY_FAKE_RUNTIME set the test binary is a fake docker that
// logs its arguments there (and sleeps SY_FAKE_RUNTIME_SLEEP first).
func TestMain(m *testing.M) {
	if log := os.Getenv("SY_FAKE_RUNTIME"); log != "" {
		if d, err := time.ParseDuration(os.Getenv("SY_FAKE_RUNTIME_SLEEP")); err == nil {
			time.Sleep(d)
		}
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
		f.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeDocker(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("SY_FAKE_RUNTIME", log)
	old := lookRuntime
	lookRuntime = func(string) (string, error) { return exe, nil }
	t.Cleanup(func() { lookRuntime = old })
	return log
}

// A container whose agent works in a tracked folder is recorded in its pid
// file; the next sy that takes the folder over removes it (a sy that died
// cannot), and a clean end drops the record.
func TestContainerInPidFile(t *testing.T) {
	log := fakeDocker(t)
	slot := t.TempDir()
	pidFile := slot + ".pid"
	stop := TrackDir(slot, pidFile)
	NoteContainer(filepath.Join(slot, "sub"), "docker", "sy-1-2-ab")
	NoteContainer(slot, "docker", "sy-1-2-cd")
	// Never another program or another container.
	NoteContainer(slot, "sh", "sy-1-2-ef")
	NoteContainer(slot, "docker", "sy-1-2-ef;rm")
	NoteContainer(slot, "docker", "my-db")
	ForgetContainer(slot, "docker", "sy-1-2-cd")
	stop()
	b, _ := os.ReadFile(pidFile)
	if strings.TrimSpace(string(b)) != "container docker sy-1-2-ab" {
		t.Fatalf("pid file = %q", b)
	}
	if len(LiveOrphans(pidFile)) != 0 {
		t.Error("a container line counted as a live process")
	}
	if !ReapOrphans(pidFile) {
		t.Fatal("ReapOrphans failed")
	}
	if c, _ := os.ReadFile(log); strings.TrimSpace(string(c)) != "rm -f sy-1-2-ab" {
		t.Errorf("runtime calls = %q", c)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("pid file not removed")
	}
	// The last record forgotten: the file goes.
	stop = TrackDir(slot, pidFile)
	NoteContainer(slot, "podman", "sy-3-4-ab")
	ForgetContainer(slot, "podman", "sy-3-4-ab")
	stop()
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("empty pid file left behind")
	}
}

// A runtime that does not answer in time may still run the container: the
// folder must not be reused.
func TestReapContainerTimeout(t *testing.T) {
	fakeDocker(t)
	t.Setenv("SY_FAKE_RUNTIME_SLEEP", "3s")
	old := containerWait
	containerWait = 300 * time.Millisecond
	defer func() { containerWait = old }()
	pidFile := filepath.Join(t.TempDir(), "slot.pid")
	os.WriteFile(pidFile, []byte("container docker sy-1-2-ab\n"), 0o644)
	if ReapOrphans(pidFile) {
		t.Error("a container that could not be removed counted as gone")
	}
	// No runtime at all: its containers cannot run.
	lookRuntime = func(string) (string, error) { return "", os.ErrNotExist }
	if !ReapOrphans(pidFile) {
		t.Error("a container of a runtime that is not installed blocks the folder")
	}
}
