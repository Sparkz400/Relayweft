package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
)

// Regression tests for the adversarial review of resolve steps.

// blockingAsker holds the conflict question until its context ends, as a
// person who has not answered when the task is stopped.
type blockingAsker struct {
	fakeApprover
	asked chan struct{}
}

func (a *blockingAsker) ApproveResolve(ctx context.Context, q ConflictQuestion) bool {
	a.asked <- struct{}{}
	<-ctx.Done()
	return false
}

// Stopped while rw asks whether an agent may resolve (the step's worktree
// already reset for the merge), rw resume used to continue the writer in
// that worktree, full of conflict markers. Now the state says the step's
// work is kept, and the resume lands it and asks again.
func TestResolveCancelAtQuestionThenResume(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	c.edit = func(cfg *config.Config) { cfg.Orchestrator.Conflicts = config.ConflictsAsk }
	o, _ := newOrc(t, dir, c.runners(), c.edit)
	asker := &blockingAsker{asked: make(chan struct{}, 1)}
	withApprover(o, asker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() { done <- o.RunWith(ctx, longTask, TaskOptions{}) }()
	select {
	case <-asker.asked:
	case r := <-done:
		t.Fatalf("the task ended before the question: %+v", r)
	case <-time.After(60 * time.Second):
		t.Fatal("no conflict question")
	}
	cancel()
	<-done
	st := History(dir, 1)[0]
	second := ""
	for id, run := range st.Running {
		if run.Resolve != nil && run.Kept != "" && run.Slot == "" {
			second = id
		}
	}
	if second == "" {
		t.Fatalf("the interrupted question is not recorded as a kept resolve: %+v", st.Running)
	}

	c2 := newConflictTest(t, dir)
	c2.arrived = 2
	close(c2.both)
	c2.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("x and y\n"), 0o644)
		return runner.Result{Final: "kept both"}
	}
	o2, _ := newOrc(t, dir, c2.runners(), c.edit)
	withApprover(o2, &conflictAsker{answer: true})
	res := o2.RunWith(context.Background(), "", TaskOptions{Resume: &st, Force: true})
	if !res.OK || len(c2.writers) != 0 || len(c2.resolves) != 1 {
		t.Fatalf("resume: %+v, writers %v, %d resolve agents", res, c2.writers, len(c2.resolves))
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "x and y\n" {
		t.Errorf("shared.txt = %q", got)
	}
}

// An agent that aborts the merge leaves a clean tree; the next attempt
// gets the conflict again instead of nothing to resolve. Second review:
// one that aborts and then writes some other file must not land as a
// resolution (the step's change was silently dropped).
func TestResolveAbortedMergeStartsAgain(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		path := filepath.Join(s.Dir, "shared.txt")
		if !strings.Contains(read(t, path), "<<<<<<<") {
			t.Error("an attempt without a conflict in its worktree")
		}
		if !strings.Contains(s.Prompt, "merge was aborted") {
			tgit(t, s.Dir, "merge", "--abort")
			os.WriteFile(filepath.Join(s.Dir, "notes.txt"), []byte("gave up\n"), 0o644)
			return runner.Result{Final: "aborted"}
		}
		os.WriteFile(path, []byte("x and y\n"), 0o644)
		return runner.Result{Final: "resolved"}
	}
	res, _ := c.run(context.Background())
	if !res.OK || len(c.resolves) != 2 || read(t, filepath.Join(dir, "shared.txt")) != "x and y\n" {
		t.Fatalf("%+v, %d attempts", res, len(c.resolves))
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err == nil {
		t.Error("the aborted attempt's file landed")
	}
}

// Second review: a resolution whose landing then fails (here: you edit the
// file between rw's check and its write) is paid-for work: it is kept on a
// branch, and the message names it.
func TestResolveLandingFailsKeepsResolution(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	var mu sync.Mutex
	resolved := false
	c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("x and y\n"), 0o644)
		mu.Lock()
		resolved = true
		mu.Unlock()
		return runner.Result{Final: "kept both"}
	}
	applyHook = func(stage string) {
		mu.Lock()
		defer mu.Unlock()
		if stage == "checked" && resolved {
			os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("you, right now\n"), 0o644)
		}
	}
	defer func() { applyHook = nil }()
	res, rec := c.run(context.Background())
	if res.OK || len(res.Kept) != 1 {
		t.Fatalf("%+v", res)
	}
	second := c.second()
	if !hasBranchSuffix(branchList(t, dir), "/"+second+"-resolve-attempt") {
		t.Errorf("the resolution is not kept: %s", branchList(t, dir))
	}
	found := false
	for _, e := range rec.all() {
		found = found || (e.Kind == event.Merge && !e.OK && strings.Contains(e.Text, "its conflict resolution on "))
	}
	if !found {
		t.Error("the message does not name the kept resolution")
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "you, right now\n" {
		t.Errorf("your edit was overwritten: %q", got)
	}
}

// A path with a space whose other side is missing made `git cat-file
// --batch` print "<name> missing", which was misread and ended the marker
// check for every later file. And a Markdown heading underline ("=======")
// is not a conflict marker.
func TestMarkersLeft(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	commit := func(files map[string]string) string {
		for f, s := range files {
			if s == "" {
				os.Remove(filepath.Join(dir, f))
				continue
			}
			os.WriteFile(filepath.Join(dir, f), []byte(s), 0o644)
		}
		tgit(t, dir, "add", "-A")
		tgit(t, dir, "commit", "-q", "--allow-empty", "-m", "c")
		return headOf(t, dir)
	}
	heading := "Title\n=======\n\ntext\n"
	ours := commit(map[string]string{"a b.txt": "ours\n", "z.txt": "z\n", "doc.md": heading})
	theirs := commit(map[string]string{"a b.txt": "", "z.txt": "z2\n", "doc.md": heading + "\nMore\n=======\n"})
	result := commit(map[string]string{"a b.txt": "kept\n", "z.txt": "<<<<<<< HEAD\nz\n=======\nz2\n>>>>>>> theirs\n",
		"doc.md": heading + "\nMore\n=======\n\nOther\n=======\n"})
	left, err := markersLeft(g, []string{"a b.txt", "doc.md", "z.txt"}, ours, theirs, result)
	if err != nil || strings.Join(left, ",") != "z.txt" {
		t.Errorf("markers left = %v, %v; want only z.txt", left, err)
	}
}

// Files the resolve agent changes beyond the step's change, in a conflict
// with your edits, count as agent work: rw undo --agent-files-only covers
// them instead of calling them unreported.
func TestResolveYourEditsExtraFilesAreAgentWork(t *testing.T) {
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerResolve):
			os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("your edit\nchanged by x\n"), 0o644)
			os.WriteFile(filepath.Join(s.Dir, "extra.txt"), []byte("so both fit\n"), 0o644)
			return runner.Result{Final: "kept both"}
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(
				map[string]any{"id": "x", "title": "say x", "kind": "edit", "prompt": "make shared.txt say x"},
				map[string]any{"id": "y", "title": "write y", "kind": "edit", "prompt": "write y.txt"},
			)}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return approve()
		}
		if s.StepID == "x" {
			os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("changed by x\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("your edit\n"), 0o644)
		} else {
			os.WriteFile(filepath.Join(s.Dir, "y.txt"), []byte("y\n"), 0o644)
		}
		return runner.Result{Final: "done " + s.StepID}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.Conflicts = config.ConflictsResolve })
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if !res.OK || read(t, filepath.Join(dir, "extra.txt")) != "so both fit\n" {
		t.Fatalf("%+v", res)
	}
	plan, err := PreviewUndo(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plan.Unreported {
		if p == "extra.txt" {
			t.Errorf("the resolve agent's extra file is not counted as agent work: %v", plan.Unreported)
		}
	}
}
