package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/runner"
)

// Review fixes of the pool size (#40).

// resumeC resumes an interrupted task whose step c the agent finishes, and
// returns c's runs.
func resumeC(t *testing.T, dir string, st *TaskState) (TaskResult, []runner.Spec, *recorder) {
	t.Helper()
	var specs []runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return approve()
		}
		specs = append(specs, s)
		finishC(s.Dir)
		return runner.Result{Final: "c", SessionID: s.Resume}
	})
	o, rec := newOrc(t, dir, set, nil)
	res := o.RunWith(context.Background(), "", TaskOptions{Resume: st})
	return res, specs, rec
}

// Another rw that gives up a full pool's held worktree keeps it locked
// while it saves the edits, which can take longer than a resume's usual
// wait. The resume of that task keeps waiting while the worktree is still
// its own (the other rw lets go once it sees the task running), and
// continues there instead of starting the step over.
func TestResumeWaitsForAnEvictorThatLetsGo(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	held := st.Running["c"].Slot
	old := claimWait
	claimWait = 200 * time.Millisecond
	defer func() { claimWait = old }()
	unlock, ok := proc.TryLock(held + ".lock") // the evictor, saving
	if !ok {
		t.Fatal("lock")
	}
	go func() { time.Sleep(1500 * time.Millisecond); unlock() }()
	res, specs, _ := resumeC(t, dir, st)
	if !res.OK {
		t.Fatalf("resume: %+v", res)
	}
	if len(specs) != 1 || specs[0].Resume != "sess-c" || !samePath(specs[0].Dir, st.Running["c"].Dir) {
		t.Fatalf("c did not continue its session in its worktree: %+v", specs)
	}
	if got := read(t, filepath.Join(dir, "c.txt")); got != "c1\nc2\n" {
		t.Errorf("c.txt = %q", got)
	}
}

// When a resume starts the step over anyway (the worktree stayed busy),
// the edits left in the old worktree are saved on a branch before that
// worktree is reused, not reset.
func TestAbandonedWorktreeEditsAreSaved(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	held := st.Running["c"].Slot
	old, oldHeld := claimWait, claimHeldWait
	claimWait, claimHeldWait = 100*time.Millisecond, 100*time.Millisecond
	defer func() { claimWait, claimHeldWait = old, oldHeld }()
	unlock, ok := proc.TryLock(held + ".lock")
	if !ok {
		t.Fatal("lock")
	}
	res, specs, _ := resumeC(t, dir, st)
	unlock()
	if !res.OK || len(specs) != 1 || specs[0].Resume != "" {
		t.Fatalf("the step should have started over elsewhere: %+v %+v", res, specs)
	}
	got, note := giveUp(t, held)
	if got || !strings.Contains(note, "-unfinished") {
		t.Fatalf("held %v, note %q: the abandoned edits were not saved", got, note)
	}
	after, _ := LoadTask(st.ID)
	if len(after.Saved) != 1 || after.Saved[0].Step != "c" {
		t.Fatalf("saved: %+v", after.Saved)
	}
	if c := tgit(t, dir, "show", after.Saved[0].Branch+":c.txt"); c != "c1" {
		t.Errorf("c.txt on the branch = %q", c)
	}
}

// A free worktree above a gap in the pool is used before a held one is
// given up.
func TestFreeSlotAboveAGapBeforeEviction(t *testing.T) {
	dir := gitRepo(t)
	base, _ := git{dir}.snapshot("base")
	var paths []string
	var slots []*slot
	for i := 0; i < 4; i++ {
		s, err := acquireSlot(dir, base)
		if err != nil {
			t.Fatal(err)
		}
		paths, slots = append(paths, s.path), append(slots, s)
	}
	for _, s := range slots {
		s.release()
	}
	removeSlot(git{dir}.commonDir(), paths[1]) // pruned: {0, 2, 3}
	st := &TaskState{ID: "gap-task", Status: "running"}
	for _, i := range []int{0, 2} {
		st.setRunning(string(rune('x'+i)), StepRun{Provider: "claude", Dir: paths[i], Slot: paths[i], Base: base, Started: time.Now()})
	}
	oldCap := poolCap.Load()
	poolCap.Store(3)
	defer poolCap.Store(oldCap)
	s, err := acquireSlot(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if !samePath(s.path, paths[3]) {
		t.Errorf("got %s, want the free slot %s", s.path, paths[3])
	}
	for _, i := range []int{0, 2} {
		if _, err := os.Stat(holdPath(paths[i])); err != nil {
			t.Errorf("held slot %s was given up although slot 3 was free", paths[i])
		}
	}
}

// The pool size and disk minimum apply to a follow-up in a rw that has not
// run a task yet.
func TestFollowUpSetsPoolLimits(t *testing.T) {
	dir := gitRepo(t)
	o, _ := newOrc(t, dir, bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		return runner.Result{Final: "ok"}
	}), nil)
	oldCap := poolCap.Load()
	poolCap.Store(0)
	defer poolCap.Store(oldCap)
	o.FollowUpSession(context.Background(), AgentSession{AgentID: "w", Provider: "claude", Dir: dir, Task: "t"}, "more")
	if got, want := poolCap.Load(), int64(poolSize(o.opts.Store.Get())); got != want {
		t.Errorf("pool size after a follow-up = %d, want %d", got, want)
	}
}

// A best-of winner whose worktree a full pool gives up stays the step's
// running agent: the resume lands its kept commit, and what the feedback
// rerun changed after it is on the -unfinished branch and in the hint.
func TestEvictedBestOfWinnerLandsKeptWork(t *testing.T) {
	dir := gitRepo(t)
	rs := stopInReview(t, dir, 1)
	run, ok := rs.st.Running["work"]
	if !ok || run.Kept == "" || run.Slot == "" {
		t.Fatalf("no kept winner: %+v", rs.st.Running)
	}
	inSlot := read(t, filepath.Join(run.Slot, "greet.txt"))
	var notes []string
	s, _ := evictHeld(dir, poolDir(dir), run.Base, &notes)
	if s == nil {
		t.Fatal("the winner's worktree was not given up")
	}
	s.release()
	after, _ := LoadTask(rs.st.ID)
	if r, ok := after.Running["work"]; !ok || r.Kept != run.Kept {
		t.Fatalf("the winner is no longer the step's running agent: %+v", after.Running)
	}
	if len(after.Saved) != 1 {
		t.Fatalf("saved: %+v", after.Saved)
	}
	sv := after.Saved[0]
	if got := tgit(t, dir, "show", sv.Branch+":greet.txt"); got+"\n" != inSlot {
		t.Errorf("greet.txt on %s = %q, want the worktree's %q", sv.Branch, got, inSlot)
	}
	if !strings.Contains(strings.Join(notes, "\n"), sv.Branch) {
		t.Errorf("the hint does not name %s: %q", sv.Branch, notes)
	}
	kept := tgit(t, dir, "show", run.Kept+":greet.txt")

	res, agents := rs.resume(t, dir)
	if !res.OK {
		t.Fatalf("resume: %+v", res)
	}
	for _, a := range agents {
		if strings.Contains(a.Prompt, runner.MarkerStep) || strings.Contains(a.Prompt, runner.MarkerResume) {
			t.Errorf("an agent redid the step: %s (%s)", a.AgentID, a.Provider)
		}
	}
	if got := read(t, filepath.Join(dir, "greet.txt")); got != kept+"\n" {
		t.Errorf("greet.txt = %q, want the kept winner's %q", got, kept)
	}
}
