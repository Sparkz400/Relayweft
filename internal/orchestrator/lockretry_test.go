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

	// A lock older than the wait was left behind: no wait at all.
	lockRetryFor = 5 * time.Second
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	start = time.Now()
	if _, err := g.run(nil, nil, "restore", "--worktree", "--", "README.md"); err == nil {
		t.Fatal("restore with a lock left behind an hour ago worked")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("waited %s for a lock left behind", took)
	}
}

// A merge into the tree that waits for another git's lock checks the files
// again before it writes: a file the user saved meanwhile is not
// overwritten (the editor's own git holding index.lock right after a save
// is the likely case). Found in review of the lock wait.
func TestApplyWaitingForALockKeepsUserEdits(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	p := filepath.Join(dir, "shared.txt")
	from, err := g.snapshot("from")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("agent\n"), 0o644)
	to, err := g.snapshot("to")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("base\n"), 0o644)
	lock := filepath.Join(dir, ".git", "index.lock")
	os.WriteFile(lock, nil, 0o644)
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(p, []byte("the user's edit\n"), 0o644)
		os.Remove(lock)
	}()
	err = g.applyDiff(from, to)
	if err == nil {
		t.Fatal("the merge went on although the file changed while it waited")
	}
	if got := read(t, p); got != "the user's edit\n" {
		t.Errorf("shared.txt = %q, want the user's edit kept (%v)", got, err)
	}
}

// Found by the load test: the snapshot before an undo, or at a task's end,
// failed when another rw removed a file while git add -A read the tree
// ("unable to stat ...: No such file", "unable to index file"). The tree
// is read again then.
func TestSnapshotReadsTheTreeAgain(t *testing.T) {
	dir := gitRepo(t)
	p := filepath.Join(dir, "busy.txt")
	os.WriteFile(p, []byte("x\n"), 0o644)
	release := holdUnreadable(t, p)
	defer release()
	tries := 0
	snapshotRetried = func(try int) {
		tries = try
		release()
	}
	defer func() { snapshotRetried = nil }()
	snap, err := (git{dir}).snapshot("test")
	if err != nil {
		t.Fatalf("snapshot while a file was unreadable for a moment: %v", err)
	}
	if tries != 1 {
		t.Errorf("read the tree again %d times, want 1", tries)
	}
	if out, _ := (git{dir}).out("show", snap+":busy.txt"); out != "x" {
		t.Errorf("busy.txt in the snapshot = %q", out)
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
			name := "new.txt"
			if strings.Contains(s.Prompt, "another") {
				name = "other.txt"
			}
			os.WriteFile(filepath.Join(s.Dir, name), []byte("created\n"), 0o644)
			return runner.Result{Final: "edited", Files: []string{name}}
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
	res = o.Run(context.Background(), "create another file")
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	undone := filepath.Join(dir, ".git", filepath.FromSlash(undoPrefix(dir)+res.UndoKey), "undone.lock")
	os.WriteFile(undone, nil, 0o644)
	defer os.Remove(undone)
	if _, err := Undo(dir, res.UndoKey, false, false); err == nil || !strings.Contains(err.Error(), "before the undo") {
		t.Fatalf("undo without its pre-undo record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.txt")); err != nil {
		t.Errorf("the refused undo changed the tree: %v", err)
	}
}
