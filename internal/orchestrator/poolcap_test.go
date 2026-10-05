package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/runner"
)

// cancelC runs longTask and cancels it while step c's agent works; edit,
// when set, is what that agent wrote in its worktree before.
func cancelC(t *testing.T, dir string, cfg func(*config.Config), edit string) *TaskState {
	t.Helper()
	started := make(chan struct{})
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := midstepPlan(s); ok {
			return r
		}
		if edit != "" {
			os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte(edit), 0o644)
		}
		s.OnSession("sess-c")
		close(started)
		<-ctx.Done()
		return runner.Result{Err: errors.New("killed"), Killed: true}
	})
	o, _ := newOrc(t, dir, set, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan TaskResult, 1)
	go func() { done <- o.RunWith(ctx, longTask, TaskOptions{}) }()
	<-started
	cancel()
	<-done
	st := History(dir, 1)[0]
	return &st
}

// Every cancelled task kept its worktree held, and the pool grew by one
// full checkout per cancel (the 30-minute stress test failed on it). At
// its size (max_threads + 1), the pool now gives up the oldest held
// worktree no sy is using, after saving its edits on a branch; sy resume
// of that task says where they are.
func TestHeldPoolStaysAtItsSize(t *testing.T) {
	dir := gitRepo(t)
	twoThreads := func(c *config.Config) { c.Orchestrator.MaxThreads = 2 }
	size := poolSize(func() *config.Config { c := config.Default(); twoThreads(c); return c }())
	var states []*TaskState
	for k := 1; k <= 4; k++ {
		states = append(states, cancelC(t, dir, twoThreads, fmt.Sprintf("c-%d\n", k)))
		if n := countSlots(poolDir(dir)); n > size {
			t.Fatalf("after %d cancelled tasks the pool has %d worktrees, more than its size %d", k, n, size)
		}
	}
	first, err := LoadTask(states[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Saved) != 1 || first.Saved[0].Step != "c" {
		t.Fatalf("the first task's worktree was given up without saving its edits: %+v", first.Saved)
	}
	sv := first.Saved[0]
	if got := tgit(t, dir, "show", sv.Branch+":c.txt"); got != "c-1" {
		t.Errorf("c.txt on %s = %q", sv.Branch, got)
	}
	if !strings.Contains(sv.Why, "another task needed") {
		t.Errorf("why: %q", sv.Why)
	}
	// The newest cancelled task still has its worktree.
	last, _ := LoadTask(states[3].ID)
	if r := last.Running["c"]; r.Slot == "" || read(t, filepath.Join(r.Slot, "c.txt")) != "c-4\n" {
		t.Errorf("the newest cancelled task lost its worktree: %+v", last.Running)
	}

	// sy resume --force of the first task says where its edits are, and
	// its step starts over.
	var resumed []runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := midstepPlan(s); ok {
			return r
		}
		resumed = append(resumed, s)
		finishC(s.Dir)
		return runner.Result{Final: "c"}
	})
	o, rec := newOrc(t, dir, set, twoThreads)
	if res := o.RunWith(context.Background(), "", TaskOptions{Resume: first, Force: true}); !res.OK {
		t.Fatalf("resume: %+v", res)
	}
	if len(resumed) != 1 || resumed[0].Resume != "" {
		t.Errorf("c should start over: %+v", resumed)
	}
	if !strings.Contains(logText(rec), sv.Branch) {
		t.Errorf("the resume did not say where the edits are:\n%s", logText(rec))
	}
}

// A step cancelled before its agent changed anything keeps no worktree:
// there is nothing to continue there.
func TestCancelledCleanStepHoldsNothing(t *testing.T) {
	dir := gitRepo(t)
	st := cancelC(t, dir, nil, "")
	if r, ok := st.Running["c"]; ok {
		t.Errorf("a cancelled step with no edits is still running in %s", r.Slot)
	}
	marks, _ := filepath.Glob(filepath.Join(poolDir(dir), "*.hold"))
	if len(marks) != 0 {
		t.Errorf("worktrees still held: %v", marks)
	}
	// With edits, the hold stays (sy resume --force continues there).
	st = cancelC(t, dir, nil, "c1\n")
	if r := st.Running["c"]; r.Slot == "" {
		t.Error("a cancelled step with edits lost its worktree")
	} else if _, err := os.Stat(holdPath(r.Slot)); err != nil {
		t.Errorf("not held: %v", err)
	}
}
