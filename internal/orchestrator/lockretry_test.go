package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
)

// Found by the load test (three rw on one repo): a step's changes did not
// land ("index.lock: File exists"; the work was kept on a branch and the
// task failed) when another git held the repo's index lock for a moment.
// git takes its locks before it changes anything, so rw runs it again.
func TestGitWaitsForAnotherGitsLock(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o644)
	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Remove(lock)
	}()
	g := git{dir}
	if _, err := g.run(nil, nil, "restore", "--worktree", "--", "README.md"); err != nil {
		t.Fatalf("restore while another git held the index for 300ms: %v", err)
	}
	if got := read(t, filepath.Join(dir, "README.md")); got != "# test\n" {
		t.Errorf("README.md = %q", got)
	}

	// A lock that stays (a git that crashed) still fails, after the wait.
	defer func(d time.Duration) { lockRetryFor = d }(lockRetryFor)
	lockRetryFor = 300 * time.Millisecond
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o644)
	os.WriteFile(lock, nil, 0o644)
	defer os.Remove(lock)
	start := time.Now()
	_, err := g.run(nil, nil, "restore", "--worktree", "--", "README.md")
	if err == nil || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("restore with a stale lock: %v", err)
	}
	if took := time.Since(start); took < lockRetryFor {
		t.Errorf("gave up after %s, before the %s wait", took, lockRetryFor)
	}
}

// A task whose end state cannot be recorded says so: rw undo cannot undo
// it. It used to fail silently and `rw undo <task>` then said "no recorded
// task".
func TestSnapshotRecordFailureIsReported(t *testing.T) {
	defer func(d time.Duration) { lockRetryFor = d }(lockRetryFor)
	lockRetryFor = 200 * time.Millisecond
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, "[RW:STEP]") {
			os.WriteFile(filepath.Join(s.Dir, "new.txt"), []byte("created\n"), 0o644)
			return runner.Result{Final: "edited", Files: []string{"new.txt"}}
		}
		return approve()
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	key := o.opts.Log.Session() + "-task-1"
	ref := filepath.Join(dir, ".git", filepath.FromSlash(undoPrefix(dir)+key), "after.lock")
	os.MkdirAll(filepath.Dir(ref), 0o755)
	if err := os.WriteFile(ref, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := o.Run(context.Background(), "create a new file")
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	warned := false
	for _, e := range rec.all() {
		warned = warned || strings.Contains(e.Text, "could not record the end state of this task")
	}
	if !warned {
		t.Error("no warning that the task cannot be undone")
	}
	os.Remove(ref)

	// Undo refuses to start when it cannot keep the state before it (a
	// redo needs it), and changes nothing.
	res = o.Run(context.Background(), "create a new file")
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	undone := filepath.Join(dir, ".git", filepath.FromSlash(undoPrefix(dir)+res.UndoKey), "undone.lock")
	os.WriteFile(undone, nil, 0o644)
	defer os.Remove(undone)
	if _, err := Undo(dir, res.UndoKey, false, false); err == nil || !strings.Contains(err.Error(), "before the undo") {
		t.Fatalf("undo without its pre-undo record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Errorf("the refused undo changed the tree: %v", err)
	}
}
