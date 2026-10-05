package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/runner"
)

// logText joins the log lines a task showed.
func logText(rec *recorder) string {
	var b strings.Builder
	for _, e := range rec.all() {
		if e.Kind == event.Log {
			b.WriteString(e.Text + "\n")
		}
	}
	return b.String()
}

// personsGit is what sy must never change in the person's repo: HEAD, the
// current branch, the index and the list of branches (sy may only add its
// own).
func personsGit(t *testing.T, dir string) (head, branch, index string, branches []string) {
	t.Helper()
	g := git{dir}
	head = headOf(t, dir)
	branch, _ = g.out("symbolic-ref", "-q", "HEAD")
	index, _ = g.out("diff", "--cached", "--name-only")
	list, _ := g.out("for-each-ref", "--format=%(refname)", "refs/heads")
	return head, branch, index, strings.Fields(list)
}

// interruptC stops a task while step c's agent has written the first half
// of c.txt in its pool worktree.
func interruptC(t *testing.T, dir string) *TaskState {
	t.Helper()
	st, _ := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
		s.OnSession("sess-c")
	})
	if st.Running["c"].Slot == "" {
		t.Fatal("c did not run in a pool worktree")
	}
	return st
}

// checkSaved checks that the task's state records c's half-done edit on a
// new branch that holds exactly that edit.
func checkSaved(t *testing.T, dir string, st *TaskState, base string) SavedEdits {
	t.Helper()
	after, err := LoadTask(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Saved) != 1 {
		t.Fatalf("the task's state records %d saved edits, want 1: %+v", len(after.Saved), after.Saved)
	}
	sv := after.Saved[0]
	if sv.Step != "c" || sv.Task != st.ID || sv.Base != base || !(strings.HasPrefix(sv.Branch, "sy/") || strings.HasPrefix(sv.Branch, "refs/switchyard/kept/")) {
		t.Errorf("saved edits: %+v (base %s)", sv, base)
	}
	if _, ok := after.Running["c"]; ok {
		t.Error("c still counts as running in the worktree that was given up")
	}
	g := git{dir}
	if got, err := g.out("show", sv.Branch+":c.txt"); err != nil || got != "c1" {
		t.Errorf("c.txt on %s = %q (%v), want the half-done edit", sv.Branch, got, err)
	}
	if names, _ := g.out("diff", "--name-only", sv.Base, sv.Branch); names != "c.txt" {
		t.Errorf("git diff base %s names %q, want only c.txt", sv.Branch, names)
	}
	if h := sv.Hint(); !strings.Contains(h, sv.Branch) || !strings.Contains(h, " diff "+sv.Base[:12]+" "+sv.Branch) {
		t.Errorf("hint does not say where the edits are and how to get them: %s", h)
	}
	if u := after.UnfinishedSaved(); len(u) != 1 {
		t.Errorf("UnfinishedSaved = %+v", u)
	}
	return sv
}

// The person's repo is unchanged apart from sy's new branch.
func checkUntouched(t *testing.T, dir, head, branch, index string, branches []string, saved string) {
	t.Helper()
	h, b, i, bs := personsGit(t, dir)
	if h != head || b != branch || i != index {
		t.Errorf("HEAD, branch or index changed: %s %s %q -> %s %s %q", head, branch, index, h, b, i)
	}
	if len(bs) != len(branches)+1 || !strings.Contains(strings.Join(bs, " "), "refs/heads/"+saved) {
		t.Errorf("branches %v -> %v (only %s may be added)", branches, bs, saved)
	}
}

// A hold older than 7 days is released when another task needs the
// worktree: the half-done edits are saved on a branch first, the task's
// state says where, and the resume says it and starts the step over.
func TestExpiredHoldSavesEdits(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	run := st.Running["c"]
	run.Started = time.Now().Add(-holdMaxAge - time.Hour)
	st.Running["c"] = run
	st.save()
	head, branch, index, branches := personsGit(t, dir)

	other := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte(s.StepID+"\n"), 0o644)
		return runner.Result{Final: "wrote"}
	})
	o, rec := newOrc(t, dir, other, nil)
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("other task: %+v", res)
	}
	sv := checkSaved(t, dir, st, run.Base)
	if _, err := os.Stat(holdPath(run.Slot)); err == nil {
		t.Error("the expired hold mark is still there")
	}
	if !strings.Contains(logText(rec), sv.Branch) {
		t.Errorf("the task that freed the worktree did not say where the edits went:\n%s", logText(rec))
	}
	idx, _ := (git{dir}).out("diff", "--cached", "--name-only")
	checkUntouched(t, dir, head, branch, idx, branches, sv.Branch)
	if idx != index {
		t.Errorf("index changed: %q -> %q", index, idx)
	}

	// The resume says where the edits are, and c starts over.
	var resumed []runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return approve()
		}
		resumed = append(resumed, s)
		finishC(s.Dir)
		return runner.Result{Final: "finished c"}
	})
	o2, rec2 := newOrc(t, dir, set, nil)
	fresh, _ := LoadTask(st.ID)
	if res := o2.RunWith(context.Background(), "", TaskOptions{Resume: fresh}); !res.OK {
		t.Fatalf("resume: %+v", res)
	}
	if len(resumed) != 1 || resumed[0].Resume != "" {
		t.Errorf("c should start over with a fresh agent: %+v", resumed)
	}
	if !strings.Contains(logText(rec2), sv.Branch) {
		t.Errorf("the resume did not say where the half-done edits are:\n%s", logText(rec2))
	}
}

// sy clean saves a held worktree's half-done edits on a branch before it
// removes the worktree. A worktree whose task a sy is running is kept.
func TestCleanSavesHeldEdits(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	run := st.Running["c"]
	head, branch, index, branches := personsGit(t, dir)
	n, saved, err := CleanPoolSaved(dir)
	if err != nil || n == 0 || len(saved) != 1 {
		t.Fatalf("clean: %d removed, saved %+v, err %v", n, saved, err)
	}
	sv := checkSaved(t, dir, st, run.Base)
	if saved[0].Branch != sv.Branch {
		t.Errorf("sy clean reported %s, the state records %s", saved[0].Branch, sv.Branch)
	}
	if _, err := os.Stat(run.Slot); err == nil {
		t.Error("the worktree was not removed")
	}
	checkUntouched(t, dir, head, branch, index, branches, sv.Branch)

	// A sy runs the next interrupted task right now: its worktree is
	// neither saved nor removed.
	dir2 := gitRepo(t)
	st2 := interruptC(t, dir2)
	unlock, ok := proc.TryLock(filepath.Join(stateDir(), st2.ID+".lock"))
	if !ok {
		t.Fatal("task lock")
	}
	_, saved, err = CleanPoolSaved(dir2)
	unlock()
	if err == nil || !strings.Contains(err.Error(), "kept") || len(saved) != 0 {
		t.Errorf("clean while the task runs: saved %+v, err %v", saved, err)
	}
	if read(t, filepath.Join(st2.Running["c"].Slot, "c.txt")) != "c1\n" {
		t.Error("the running task's worktree lost its edit")
	}
	if _, err := os.Stat(holdPath(st2.Running["c"].Slot)); err != nil {
		t.Error("the running task's hold mark was removed")
	}
}

// Edits that cannot be saved keep the worktree held, past 7 days too.
func TestExpiredHoldKeptWhenSaveFails(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	run := st.Running["c"]
	run.Started = time.Now().Add(-holdMaxAge - time.Hour)
	st.Running["c"] = run
	st.save()
	// The worktree's link to its repository is broken: git cannot read it.
	if err := os.WriteFile(filepath.Join(run.Slot, ".git"), []byte("gitdir: "+filepath.Join(t.TempDir(), "gone")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, ok := lockSlot(run.Slot)
	if !ok {
		t.Fatal("lock")
	}
	held, note := slotHeldNote(run.Slot)
	unlock()
	if !held || !strings.Contains(note, "could not be saved") {
		t.Errorf("held %v, note %q", held, note)
	}
	if _, err := os.Stat(holdPath(run.Slot)); err != nil {
		t.Error("the hold mark was removed although the edits were not saved")
	}
	if after, _ := LoadTask(st.ID); len(after.Saved) != 0 || after.Running["c"].Slot == "" {
		t.Errorf("state changed although nothing was saved: %+v", after)
	}
}
