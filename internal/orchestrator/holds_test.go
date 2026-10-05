package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
)

// A cancelled task keeps c's Running entry (so `rw resume --force`
// can continue it), and its worktree stays held: the next task in the same
// repo leaves it alone, and rw resume --force continues c there.
func TestCancelledTaskKeepsItsWorktree(t *testing.T) {
	dir := gitRepo(t)
	started := make(chan struct{})
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := midstepPlan(s); ok {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
		s.OnSession("sess-c")
		close(started)
		<-ctx.Done()
		return runner.Result{Err: errors.New("killed"), Killed: true}
	})
	o, _ := newOrc(t, dir, set, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan TaskResult, 1)
	go func() { done <- o.RunWith(ctx, longTask, TaskOptions{}) }()
	<-started
	cancel()
	<-done
	st := History(dir, 1)[0]
	held := st.Running["c"].Slot
	t.Logf("status %s, running c in %s", st.Status, held)
	if b, err := os.ReadFile(filepath.Join(held, "c.txt")); err != nil || string(b) != "c1\n" {
		t.Fatalf("half-done edit not in slot after cancel: %q %v", b, err)
	}

	// Another task in the same repo.
	other := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte(s.StepID+"\n"), 0o644)
		return runner.Result{Final: "wrote"}
	})
	o2, _ := newOrc(t, dir, other, nil)
	if res := o2.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("other task: %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(held, "c.txt")); err != nil || string(b) != "c1\n" {
		t.Errorf("cancelled task's half-done edit in %s was wiped by the next task (c.txt=%q err=%v)", held, b, err)
	}

	var resumed string
	set = bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := midstepPlan(s); ok {
			return r
		}
		resumed = s.Resume
		finishC(s.Dir)
		return runner.Result{Final: "ok"}
	})
	o3, _ := newOrc(t, dir, set, nil)
	res := o3.RunWith(context.Background(), "", TaskOptions{Resume: &st, Force: true})
	t.Logf("forced resume ok=%v resumed session=%q", res.OK, resumed)
	if resumed != "sess-c" {
		t.Errorf("forced resume of the cancelled task did not continue sess-c (the hold was not honoured)")
	}
}

// slotHeld reads the owning task's state file while the task's own
// goroutines save it (write tmp + rename). On Windows a read racing the
// rename fails; that must count as held, never free the slot (and so
// delete its edits) on a guess.
func TestSlotHeldSafeDuringSaves(t *testing.T) {
	slot := filepath.Join(t.TempDir(), "0")
	os.MkdirAll(slot, 0o755)
	st := &TaskState{ID: "review-race", Status: "running"}
	st.setRunning("c", StepRun{Provider: "claude", Dir: slot, Slot: slot, Base: "x", Started: time.Now()})
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				st.noteSession("c", "s") // no-op after the first
				st.save()
			}
		}
	}()
	falseCount := 0
	for i := 0; i < 1000; i++ {
		if !slotHeld(slot) {
			falseCount++
			holdSlot(slot, slotHold{Task: st.ID, Step: "c"}) // put the mark back to keep counting
		}
	}
	close(stop)
	if falseCount > 0 {
		t.Errorf("slotHeld said a held slot is free (and removed its mark) %d of 1000 times during concurrent saves", falseCount)
	}
}

// In a resumed task, another writer that starts together with the
// interrupted step scans the pool from slot 0 and briefly locks the held
// slot to check its hold. The interrupted step's claim waits for that
// instead of abandoning its session and edits.
func TestResumeClaimWaitsForAScan(t *testing.T) {
	startedOver := 0
	const n = 2
	for i := 0; i < n; i++ {
		dir := gitRepo(t)
		st, _ := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
			os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
			s.OnSession("sess-c")
		})
		// A step that had not started yet, with nothing to wait for.
		var ds []Subtask
		for k := 0; k < 1; k++ {
			ds = append(ds, Subtask{ID: "d" + string(rune('0'+k)), Title: "write d", Kind: "edit", Prompt: "write d", Files: []string{"d.txt"}})
		}
		st.Plan.Subtasks = append(st.Plan.Subtasks, ds...)
		st.save()
		var mu sync.Mutex
		resumed := false
		set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
			if strings.Contains(s.Prompt, runner.MarkerPlanReview) || strings.Contains(s.Prompt, runner.MarkerFinalReview) {
				return approve()
			}
			if s.StepID == "c" {
				mu.Lock()
				resumed = resumed || s.Resume == "sess-c"
				mu.Unlock()
				finishC(s.Dir)
				return runner.Result{Final: "c"}
			}
			t.Logf("%s ran in %s", s.StepID, s.Dir)
			os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte("d\n"), 0o644)
			return runner.Result{Final: "d"}
		})
		o, rec := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.MaxThreads = 8 })
		res := o.RunWith(context.Background(), "", TaskOptions{Resume: st})
		if !resumed {
			startedOver++
			t.Logf("res %s", res.Summary)
			rec.mu.Lock()
			for _, e := range rec.evs {
				if e.Text != "" && (strings.Contains(e.Text, "c:") || strings.Contains(e.Text, "interrupt") || strings.Contains(e.Text, "worktree")) {
					t.Logf("%s %s: %s", e.Kind, e.AgentID, e.Text)
				}
			}
			rec.mu.Unlock()
		}
	}
	if startedOver > 0 {
		t.Errorf("the interrupted step started over instead of continuing its session in %d of %d resumes", startedOver, n)
	}
}

// save replaces the state file even while a reader has it open: Windows
// refuses the rename then, and the write must not be lost.
func TestSaveWhileStateIsOpen(t *testing.T) {
	st := &TaskState{ID: "save-open", Status: "running", Summary: "first"}
	st.save()
	f, err := os.Open(statePath(st.ID))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		st.Summary = "second"
		st.save()
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	f.Close()
	<-done
	got, err := LoadTask(st.ID)
	if err != nil || got.Summary != "second" {
		t.Fatalf("the save was lost: %+v %v", got, err)
	}
}

// A hold mark that cannot be read for sure, or whose task state cannot be
// read for sure, keeps the slot; only a missing state or a state that no
// longer records the step frees it.
func TestHoldStateFailsSafe(t *testing.T) {
	slot := filepath.Join(t.TempDir(), "0")
	os.MkdirAll(slot, 0o755)
	os.WriteFile(holdPath(slot), []byte("{not json"), 0o644)
	if !slotHeld(slot) {
		t.Error("an unreadable mark freed the slot")
	}
	holdSlot(slot, slotHold{Task: "hold-garbled", Step: "c"})
	os.MkdirAll(stateDir(), 0o755)
	os.WriteFile(statePath("hold-garbled"), []byte("{half a sta"), 0o644)
	if !slotHeld(slot) {
		t.Error("an unreadable task state freed the slot")
	}
	if _, err := os.Stat(holdPath(slot)); err != nil {
		t.Error("the mark was removed")
	}
	os.Remove(statePath("hold-garbled"))
	if slotHeld(slot) {
		t.Error("a mark whose task is gone still holds the slot")
	}
	if _, err := os.Stat(holdPath(slot)); err == nil {
		t.Error("a stale mark was kept")
	}
	st := &TaskState{ID: "hold-done", Status: "done"}
	st.save()
	holdSlot(slot, slotHold{Task: st.ID, Step: "c"})
	if slotHeld(slot) {
		t.Error("a task that no longer records the step holds the slot")
	}
}

// rw undo of an interrupted task frees the worktrees it held.
func TestUndoReleasesHolds(t *testing.T) {
	dir := gitRepo(t)
	st, _ := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
		s.OnSession("sess-c")
	})
	held := st.Running["c"].Slot
	if !slotHeld(held) {
		t.Fatal("the interrupted step's worktree is not held")
	}
	if _, err := Undo(dir, st.UndoKey, false, false); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if slotHeld(held) {
		t.Error("the worktree is still held after rw undo")
	}
	if after, _ := LoadTask(st.ID); len(after.Running) != 0 {
		t.Errorf("the undone task still records running steps: %+v", after.Running)
	}
}

// A saved session id that is not plain is never passed to a CLI.
func TestResumeRejectsUnsafeSessionID(t *testing.T) {
	dir := gitRepo(t)
	st, _ := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
		s.OnSession("sess-c")
	})
	r := st.Running["c"]
	r.Session = "--dangerously-skip-permissions"
	st.Running["c"] = r
	st.save()
	var specs []runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return approve()
		}
		specs = append(specs, s)
		finishC(s.Dir)
		return runner.Result{Final: "ok"}
	})
	o, _ := newOrc(t, dir, set, nil)
	if res := o.RunWith(context.Background(), "", TaskOptions{Resume: st}); !res.OK {
		t.Fatalf("resume: %+v", res)
	}
	if len(specs) != 1 || specs[0].Resume != "" {
		t.Fatalf("an unsafe session id was used: %+v", specs)
	}
}

// The resume prompt repeats the subtask, for a CLI whose conversation does
// not have it.
func TestResumePromptHasTheSubtask(t *testing.T) {
	p := resumePrompt(Subtask{ID: "c", Title: "write c", Kind: "edit", Prompt: "write c in two parts", Files: []string{"c.txt"}}, nil)
	for _, want := range []string{runner.MarkerResume, "write c in two parts", "c.txt", "do not redo edits"} {
		if !strings.Contains(p, want) {
			t.Errorf("resume prompt lacks %q:\n%s", want, p)
		}
	}
}
