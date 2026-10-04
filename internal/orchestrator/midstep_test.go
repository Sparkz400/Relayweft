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

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/runner"
)

// ctxRunner is a test runner that sees the agent's context (a hanging
// agent waits for it to be cancelled).
type ctxRunner struct {
	provider string
	fn       func(ctx context.Context, s runner.Spec) runner.Result
}

func (x ctxRunner) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Model: s.Model, Kind: event.Started}.Stamp())
	r := x.fn(ctx, s)
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Kind: event.Done, OK: r.OK()}.Stamp())
	return r
}

func bothCtx(fn func(ctx context.Context, s runner.Spec) runner.Result) runner.Set {
	return runner.Set{
		event.Codex:  ctxRunner{event.Codex, func(ctx context.Context, s runner.Spec) runner.Result { s.Provider = event.Codex; return fn(ctx, s) }},
		event.Claude: ctxRunner{event.Claude, func(ctx context.Context, s runner.Spec) runner.Result { s.Provider = event.Claude; return fn(ctx, s) }},
	}
}

// midstepPlan plans a, b and c (c after a and b): a appends to shared.txt,
// b writes b.txt, c works on c.txt. Writers run in pool worktrees.
func midstepPlan(s runner.Spec) (runner.Result, bool) {
	switch {
	case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
		return approve(), true
	case strings.Contains(s.Prompt, runner.MarkerPlan):
		return runner.Result{Final: planJSON(
			map[string]any{"id": "a", "title": "extend shared", "kind": "edit", "prompt": "append a", "files": []string{"shared.txt"}},
			map[string]any{"id": "b", "title": "write b", "kind": "edit", "prompt": "write b", "files": []string{"b.txt"}},
			map[string]any{"id": "c", "title": "write c", "kind": "edit", "prompt": "write c in two parts", "files": []string{"c.txt"}, "depends_on": []string{"a", "b"}},
		)}, true
	}
	switch s.StepID {
	case "a":
		appendFile(s.Dir, "shared.txt", "a\n")
		return runner.Result{Final: "appended a", SessionID: "sess-a"}, true
	case "b":
		os.WriteFile(filepath.Join(s.Dir, "b.txt"), []byte("b\n"), 0o644)
		return runner.Result{Final: "wrote b", SessionID: "sess-b"}, true
	}
	return runner.Result{}, false
}

func appendFile(dir, name, text string) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.WriteString(text)
		f.Close()
	}
}

// finishC is step c's work, whoever does it: the second part after the
// first, or both parts when there is no first part in its folder.
func finishC(dir string) {
	b, _ := os.ReadFile(filepath.Join(dir, "c.txt"))
	if string(b) == "c1\n" {
		appendFile(dir, "c.txt", "c2\n")
	} else {
		os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c1\nc2\n"), 0o644)
	}
}

// interruptStep runs task until step's agent works (start does the first
// half of its work and may report a session), then stops it the way a dead
// sy does: the task state on disk is put back to what it was while the
// agent worked (status running, the agent recorded), and every lock is
// free. It returns that state and the agent's spec.
func interruptStep(t *testing.T, dir string, edit func(*config.Config), task, step string, plan func(runner.Spec) (runner.Result, bool), start func(s runner.Spec)) (*TaskState, runner.Spec) {
	t.Helper()
	started := make(chan runner.Spec, 1)
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := plan(s); ok {
			return r
		}
		if s.StepID == step {
			start(s)
			started <- s
			<-ctx.Done()
			return runner.Result{Err: errors.New("killed"), Killed: true}
		}
		return runner.Result{Final: "ok"}
	})
	o, _ := newOrc(t, dir, set, edit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() { done <- o.RunWith(ctx, task, TaskOptions{}) }()
	var spec runner.Spec
	select {
	case spec = <-started:
	case r := <-done:
		t.Fatalf("the task ended before %s started: %+v", step, r)
	case <-time.After(60 * time.Second):
		t.Fatalf("%s never started", step)
	}
	hist := History(dir, 1)
	if len(hist) != 1 || hist[0].Status != "running" {
		t.Fatalf("no running task state: %+v", hist)
	}
	id := hist[0].ID
	saved, err := os.ReadFile(statePath(id))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	// sy died: nothing after this point was saved.
	if err := os.WriteFile(statePath(id), saved, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Interrupted() {
		t.Fatal("the stopped task is not interrupted")
	}
	return st, spec
}

// A task killed while a step's agent works in a pool worktree is resumed
// by continuing that agent's session in the same worktree, which still
// holds its half-done edit; every edit lands exactly once, even though the
// tree changed while sy was down.
func TestResumeContinuesInterruptedSessionInPoolWorktree(t *testing.T) {
	dir := gitRepo(t)
	st, first := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
		s.OnSession("sess-c") // the CLI's first line
	})
	// The session and the agent's folder were saved while it worked.
	run, ok := st.Running["c"]
	if !ok || run.Session != "sess-c" || run.Slot == "" || !samePath(run.Dir, first.Dir) || run.Base == "" || run.Attempt != 1 || run.Provider == "" {
		t.Fatalf("running step not saved: %+v", st.Running)
	}
	if !within(first.Dir, poolDir(dir)) {
		t.Fatalf("c did not run in a pool worktree: %s", first.Dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); err == nil {
		t.Fatal("c's half-done edit reached the tree")
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "base\na\n" {
		t.Fatalf("a was not merged before the interruption: %q", got)
	}
	// While sy was down, the person changed the line a added. (Merged
	// against git's own merge base, c's work would conflict with it.)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\nA\n"), 0o644)

	var mu sync.Mutex
	var specs []runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerPlanReview) || strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return approve()
		}
		mu.Lock()
		specs = append(specs, s)
		mu.Unlock()
		if s.StepID == "c" && s.Resume == "sess-c" {
			finishC(s.Dir)
			return runner.Result{Final: "finished c", SessionID: "sess-c"}
		}
		return runner.Result{Err: errors.New("unexpected agent")}
	})
	o, _ := newOrc(t, dir, set, nil)
	res := o.RunWith(context.Background(), "", TaskOptions{Resume: st})
	if !res.OK {
		t.Fatalf("resume failed: %+v", res)
	}
	if len(specs) != 1 {
		t.Fatalf("resume ran %d agents, want only c's session: %+v", len(specs), specs)
	}
	s := specs[0]
	if s.Resume != "sess-c" || !samePath(s.Dir, first.Dir) || s.Provider != run.Provider || !strings.Contains(s.Prompt, runner.MarkerResume) ||
		!strings.Contains(s.Prompt, "do not redo edits") {
		t.Fatalf("c was not continued in its session and folder: resume %q dir %s (was %s) provider %s\n%s", s.Resume, s.Dir, first.Dir, s.Provider, s.Prompt)
	}
	for f, want := range map[string]string{"shared.txt": "base\nA\n", "b.txt": "b\n", "c.txt": "c1\nc2\n"} {
		if got := read(t, filepath.Join(dir, f)); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	saved, _ := LoadTask(st.ID)
	if saved.Status != "done" || len(saved.Running) != 0 || !saved.Results["c"].OK {
		t.Errorf("state after the resume: %+v", saved)
	}
}

// When the interrupted session cannot be continued, a fresh agent takes the
// step over, with the note about half-done edits, in the folder that holds
// them when it still can.
func TestResumeFallsBackToFreshAgent(t *testing.T) {
	cases := []struct {
		name string
		// noSession: the agent died before its CLI reported a session.
		noSession bool
		// before changes things while sy is down; it returns the
		// provider the fresh agent must not use ("" = any).
		before func(t *testing.T, dir string, st *TaskState) (edit func(*config.Config))
		// sessionFails: the CLI does not know the session any more.
		sessionFails bool
		// sameSlot: the fresh agent works where the half-done edit is.
		sameSlot bool
	}{
		{name: "session unknown to the CLI", sessionFails: true, sameSlot: true},
		{name: "no session saved", noSession: true, sameSlot: true},
		{name: "provider disabled", sameSlot: true, before: func(t *testing.T, dir string, st *TaskState) func(*config.Config) {
			prov := st.Running["c"].Provider
			return func(c *config.Config) {
				pc := c.Providers[prov]
				pc.Disabled = true
				c.Providers[prov] = pc
			}
		}},
		{name: "worktree reused since", before: func(t *testing.T, dir string, st *TaskState) func(*config.Config) {
			// Another task took the slot: it is at another commit now and
			// the half-done edit is gone.
			if err := resetSlot(st.Running["c"].Slot, headOf(t, dir)); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := gitRepo(t)
			st, first := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
				os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
				if !c.noSession {
					s.OnSession("sess-c")
				}
			})
			prev := st.Running["c"]
			var edit func(*config.Config)
			if c.before != nil {
				edit = c.before(t, dir, st)
			}
			var mu sync.Mutex
			var specs []runner.Spec
			set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
				if strings.Contains(s.Prompt, runner.MarkerPlanReview) || strings.Contains(s.Prompt, runner.MarkerFinalReview) {
					return approve()
				}
				mu.Lock()
				specs = append(specs, s)
				mu.Unlock()
				if s.Resume != "" && c.sessionFails {
					return runner.Result{Err: errors.New("No conversation found with session ID: " + s.Resume)}
				}
				finishC(s.Dir)
				return runner.Result{Final: "finished c", SessionID: "sess-new"}
			})
			o, _ := newOrc(t, dir, set, edit)
			res := o.RunWith(context.Background(), "", TaskOptions{Resume: st})
			if !res.OK {
				t.Fatalf("resume failed: %+v", res)
			}
			fresh := specs[len(specs)-1]
			if fresh.Resume != "" || !strings.Contains(fresh.Prompt, "NOTE: an earlier attempt at this subtask was interrupted") {
				t.Fatalf("the last agent is not a fresh one with the note: resume %q\n%s", fresh.Resume, fresh.Prompt)
			}
			wantRuns := 1
			if c.sessionFails {
				wantRuns = 2
				if specs[0].Resume != "sess-c" {
					t.Errorf("the session was not tried first: %+v", specs[0])
				}
			}
			if len(specs) != wantRuns {
				t.Errorf("%d agents ran, want %d", len(specs), wantRuns)
			}
			// Where the half-done edit is gone (the worktree was reused),
			// the fresh agent may get the same path, reset.
			if c.sameSlot && !samePath(fresh.Dir, first.Dir) {
				t.Errorf("fresh agent in %s, not where the half-done edit is (%s)", fresh.Dir, first.Dir)
			}
			if c.before != nil && c.sameSlot && fresh.Provider == prev.Provider {
				t.Errorf("a disabled provider (%s) ran the step", prev.Provider)
			}
			if got := read(t, filepath.Join(dir, "c.txt")); got != "c1\nc2\n" {
				t.Errorf("c.txt = %q", got)
			}
			if got := read(t, filepath.Join(dir, "shared.txt")); got != "base\na\n" {
				t.Errorf("shared.txt = %q", got)
			}
		})
	}
}

// A step that ran in the main tree (no worktrees) continues its session
// there; a read-only step continues its session too.
func TestResumeContinuesMainTreeAndReadOnlySteps(t *testing.T) {
	for _, c := range []struct{ task, step string }{
		{"add a w file", "work"},           // small task: one writer in the main tree
		{"where is the parser", "explore"}, // small read-only task
	} {
		t.Run(c.step, func(t *testing.T) {
			dir := gitRepo(t)
			plan := func(s runner.Spec) (runner.Result, bool) {
				if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
					return approve(), true
				}
				return runner.Result{}, false
			}
			st, first := interruptStep(t, dir, nil, c.task, c.step, plan, func(s runner.Spec) {
				if !s.ReadOnly {
					os.WriteFile(filepath.Join(s.Dir, "w.txt"), []byte("w1\n"), 0o644)
				}
				s.OnSession("sess-" + c.step)
			})
			if !samePath(first.Dir, dir) || st.Running[c.step].Slot != "" {
				t.Fatalf("%s did not run in the main tree: %s %+v", c.step, first.Dir, st.Running)
			}
			var got []runner.Spec
			set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
				if r, ok := plan(s); ok {
					return r
				}
				got = append(got, s)
				if !s.ReadOnly {
					appendFile(s.Dir, "w.txt", "w2\n")
				}
				return runner.Result{Final: "finished", SessionID: s.Resume}
			})
			o, _ := newOrc(t, dir, set, nil)
			if res := o.RunWith(context.Background(), "", TaskOptions{Resume: st}); !res.OK {
				t.Fatalf("resume failed: %+v", res)
			}
			if len(got) != 1 || got[0].Resume != "sess-"+c.step || !samePath(got[0].Dir, dir) || got[0].ReadOnly != first.ReadOnly {
				t.Fatalf("the step's session was not continued in the main tree: %+v", got)
			}
			if first.ReadOnly != strings.Contains(got[0].Prompt, "read-only") {
				t.Errorf("resume prompt:\n%s", got[0].Prompt)
			}
			if !first.ReadOnly {
				if w := read(t, filepath.Join(dir, "w.txt")); w != "w1\nw2\n" {
					t.Errorf("w.txt = %q", w)
				}
			}
		})
	}
}

// A task cancelled while a step's agent worked keeps that agent on record:
// sy resume --force continues its session.
func TestCancelledStepKeepsItsSession(t *testing.T) {
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
	if st.Status != "cancelled" || st.Running["c"].Session != "sess-c" {
		t.Fatalf("cancelled state: %s %+v", st.Status, st.Running)
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
	o2, _ := newOrc(t, dir, set, nil)
	if res := o2.RunWith(context.Background(), "", TaskOptions{Resume: &st, Force: true}); !res.OK || resumed != "sess-c" {
		t.Fatalf("forced resume: %+v resumed %q", res, resumed)
	}
	if got := read(t, filepath.Join(dir, "c.txt")); got != "c1\nc2\n" {
		t.Errorf("c.txt = %q", got)
	}
}

// mergeTreeBase merges a step's work against the commit it started from:
// a change already in the tree is not applied again, and later changes to
// the same file are kept.
func TestMergeTreeBase(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	appendFile(dir, "shared.txt", "a\n")
	base, err := g.snapshot("base") // the run that was interrupted
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.addWorktree(wt, base); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "c.txt"), []byte("c\n"), 0o644)
	work, err := (git{wt}).commitWork(base, "c")
	if err != nil {
		t.Fatal(err)
	}
	// The person changed the line the interrupted run's other step added.
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\nA\n"), 0o644)
	now, err := g.snapshot("resumed") // built on HEAD, not on base
	if err != nil {
		t.Fatal(err)
	}
	if _, clean, _, _ := g.mergeTree(now, work.Commit); clean {
		t.Error("the plain merge (git's own merge base) did not conflict: the test does not show the problem")
	}
	tree, clean, info, err := g.mergeTreeBase(base, now, work.Commit)
	if err != nil || !clean {
		t.Fatalf("merge with base: clean=%v %s %v", clean, info, err)
	}
	for f, want := range map[string]string{"shared.txt": "base\nA\n", "c.txt": "c\n"} {
		got, _ := g.out("cat-file", "-p", tree+":"+f)
		if got+"\n" != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
}

// claimSlot takes the one pool worktree an agent used: as it is when it
// still holds that agent's run (its hold mark, at its commit), else not at
// all; or reset to a commit for a follow-up (recreated at the same path if
// it was deleted).
func TestClaimSlot(t *testing.T) {
	old := claimWait
	claimWait = 300 * time.Millisecond
	defer func() { claimWait = old }()
	dir := gitRepo(t)
	g := git{dir}
	base, err := g.snapshot("base")
	if err != nil {
		t.Fatal(err)
	}
	s, err := acquireSlot(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	run := slotHold{Task: "t1", Step: "c", Token: "tok1"}
	holdSlot(path, run)
	os.WriteFile(filepath.Join(path, "half.txt"), []byte("half\n"), 0o644)
	if _, err := claimSlot(dir, path, base, &run); err == nil {
		t.Fatal("a slot in use was claimed")
	}
	s.release() // the sy using it died

	kept, err := claimSlot(dir, path, base, &run)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if read(t, filepath.Join(path, "half.txt")) != "half\n" {
		t.Fatal("claiming with keep changed the slot")
	}
	kept.release()

	// Another run's mark, or none: the worktree was used since.
	for _, h := range []slotHold{{Task: "t1", Step: "c", Token: "tok2"}, {Task: "t2", Step: "c", Token: "tok1"}} {
		if _, err := claimSlot(dir, path, base, &h); err == nil {
			t.Errorf("claimed with a mark of another run: %+v", h)
		}
	}
	appendFile(dir, "shared.txt", "x\n")
	other, _ := g.snapshot("other")
	if _, err := claimSlot(dir, path, other, &run); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("a slot at another commit was claimed: %v", err)
	}
	if _, err := claimSlot(dir, filepath.Join(path, "sub"), base, &run); err == nil {
		t.Fatal("a folder inside a slot was claimed as a slot")
	}
	if _, err := claimSlot(dir, t.TempDir(), base, &run); err == nil {
		t.Fatal("a folder outside the pool was claimed")
	}

	// A follow-up: moved to the current state; recreated when deleted.
	// (t1 has no task state: its mark is stale.)
	for i := 0; i < 2; i++ {
		fs, err := claimSlot(dir, path, other, nil)
		if err != nil {
			t.Fatalf("claim for a follow-up (%d): %v", i, err)
		}
		if _, err := os.Stat(filepath.Join(path, "half.txt")); err == nil {
			t.Error("the slot was not reset")
		}
		if read(t, filepath.Join(path, "shared.txt")) != "base\nx\n" {
			t.Error("the slot is not at the commit")
		}
		fs.release()
		if i == 0 {
			removeSlot(g.commonDir(), path) // sy clean
		}
	}
	// Taken by a running sy: refused.
	unlock, ok := proc.TryLock(path + ".lock")
	if !ok {
		t.Fatal("lock")
	}
	defer unlock()
	if _, err := claimSlot(dir, path, other, nil); err == nil {
		t.Fatal("a locked slot was claimed")
	}
}

// A slot that another task moved to a descendant of the interrupted run's
// base (the person merged a kept sy branch, and that task's agent worked on
// top of it) passes the commit check; its hold mark tells it apart.
func TestClaimSlotRejectsReuseAtDescendant(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	base, _ := g.snapshot("base")
	s, err := acquireSlot(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	holdSlot(path, slotHold{Task: "t1", Step: "c", Token: "tok1"})
	s.release()
	// Another task takes the slot at a commit built on base.
	tree, _ := g.out("rev-parse", base+"^{tree}")
	desc, err := g.commitTree("commit-tree", tree, "-p", base, "-m", "on top")
	if err != nil {
		t.Fatal(err)
	}
	if err := resetSlot(path, desc); err != nil {
		t.Fatal(err)
	}
	holdSlot(path, slotHold{Task: "t2", Step: "x", Token: "tok9"})
	if err := slotHolds(dir, path, base); err != nil {
		t.Fatalf("the commit check alone should pass here: %v", err)
	}
	if _, err := claimSlot(dir, path, base, &slotHold{Task: "t1", Step: "c", Token: "tok1"}); err == nil {
		t.Fatal("claimed a slot another task reused")
	}
}

// A follow-up to a Claude agent that ran in a pool worktree resumes its
// session in that worktree (Claude keeps sessions per folder), at the main
// tree's current state, and its changes land in the main tree. Without the
// worktree (in use elsewhere) it falls back to a fresh agent in the main
// tree; Codex sessions resume from the main tree as before.
func TestFollowUpResumesInPoolWorktree(t *testing.T) {
	dir := gitRepo(t)
	var mu sync.Mutex
	var specs []runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := midstepPlan(s); ok {
			if s.StepID == "a" {
				r.SessionID = "sess-a-" + s.Provider
			}
			return r
		}
		mu.Lock()
		specs = append(specs, s)
		mu.Unlock()
		if s.StepID == "c" {
			finishC(s.Dir)
			return runner.Result{Final: "c", SessionID: "sess-c"}
		}
		// A follow-up: write the file it was asked for where it works.
		name := strings.Fields(s.Prompt)[len(strings.Fields(s.Prompt))-1]
		os.WriteFile(filepath.Join(s.Dir, name), []byte(s.Provider+"\n"), 0o644)
		sid := s.Resume
		if sid == "" {
			sid = "sess-fresh"
		}
		return runner.Result{Final: "wrote " + name, SessionID: sid}
	})
	o, _ := newOrc(t, dir, set, nil)
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("task: %+v", res)
	}
	sess, ok := o.Session("a")
	if !ok || sess.Slot == "" || !within(sess.Dir, sess.Slot) || !within(sess.Slot, poolDir(dir)) {
		t.Fatalf("a's session does not name its pool worktree: %+v", sess)
	}
	// The person changed the tree since.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\nmine\n"), 0o644)

	last := func() runner.Spec { mu.Lock(); defer mu.Unlock(); return specs[len(specs)-1] }
	claude := sess
	claude.Provider, claude.SessionID = event.Claude, "sess-a-claude"
	res := o.FollowUpSession(context.Background(), claude, "please also write a2.txt")
	s := last()
	if !res.OK || s.Resume != "sess-a-claude" || !samePath(s.Dir, sess.Dir) {
		t.Fatalf("claude follow-up: %+v resume %q dir %s (agent ran in %s)", res, s.Resume, s.Dir, sess.Dir)
	}
	if read(t, filepath.Join(dir, "a2.txt")) != "claude\n" {
		t.Error("the follow-up's change did not land in the main tree")
	}
	for f, want := range map[string]string{"README.md": "# test\nmine\n", "shared.txt": "base\na\n", "c.txt": "c1\nc2\n"} {
		if got := read(t, filepath.Join(dir, f)); got != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	// The worktree was moved to the tree's state before the agent ran:
	// git status there shows only the follow-up's file as new.
	if st, _ := (git{sess.Slot}).out("status", "--porcelain"); strings.TrimSpace(st) != "?? a2.txt" {
		t.Errorf("worktree status before the next use:\n%s", st)
	}
	again, _ := o.Session(sess.AgentID)
	if !samePath(again.Slot, sess.Slot) || again.SessionID != "sess-a-claude" {
		t.Errorf("the follow-up's session does not name the worktree: %+v", again)
	}

	// The worktree is busy: a fresh agent in the main tree, with context.
	unlock, ok := proc.TryLock(sess.Slot + ".lock")
	if !ok {
		t.Fatal("lock")
	}
	res = o.FollowUpSession(context.Background(), claude, "please also write a3.txt")
	unlock()
	s = last()
	if !res.OK || s.Resume != "" || !samePath(s.Dir, dir) || !strings.Contains(s.Prompt, "THE USER'S FOLLOW-UP") {
		t.Fatalf("busy worktree: %+v resume %q dir %s", res, s.Resume, s.Dir)
	}
	if read(t, filepath.Join(dir, "a3.txt")) != "claude\n" {
		t.Error("a3.txt missing")
	}

	// Codex keeps sessions by id, not by folder: resumed from the main tree.
	codex := sess
	codex.Provider, codex.SessionID = event.Codex, "sess-a-codex"
	res = o.FollowUpSession(context.Background(), codex, "please also write a4.txt")
	s = last()
	if !res.OK || s.Resume != "sess-a-codex" || !samePath(s.Dir, dir) {
		t.Fatalf("codex follow-up: %+v resume %q dir %s", res, s.Resume, s.Dir)
	}
}

// The pool worktree of a step that sy stopped in the middle of is kept for
// the resume: another task in the same repo uses other worktrees, so the
// half-done edit is still there when the task is resumed.
func TestInterruptedWorktreeIsKeptForResume(t *testing.T) {
	dir := gitRepo(t)
	st, first := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0o644)
		s.OnSession("sess-c")
	})
	held := st.Running["c"].Slot
	if _, err := os.Stat(holdPath(held)); err != nil {
		t.Fatalf("the worktree is not marked as held: %v", err)
	}
	// Another task with two parallel writers, while the first is
	// interrupted.
	var mu sync.Mutex
	var dirs []string
	other := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		mu.Lock()
		dirs = append(dirs, s.Dir)
		mu.Unlock()
		os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte(s.StepID+"\n"), 0o644)
		return runner.Result{Final: "wrote"}
	})
	o, _ := newOrc(t, dir, other, nil)
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("other task: %+v", res)
	}
	for _, d := range dirs {
		if within(d, held) {
			t.Fatalf("another task's agent used the held worktree %s", held)
		}
	}
	if len(dirs) != 2 {
		t.Fatalf("other task ran %d writers", len(dirs))
	}
	var resumed runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return approve()
		}
		resumed = s
		finishC(s.Dir)
		return runner.Result{Final: "finished c", SessionID: s.Resume}
	})
	o2, _ := newOrc(t, dir, set, nil)
	if res := o2.RunWith(context.Background(), "", TaskOptions{Resume: st}); !res.OK {
		t.Fatalf("resume: %+v", res)
	}
	if resumed.Resume != "sess-c" || !samePath(resumed.Dir, first.Dir) {
		t.Fatalf("c was not continued in its worktree: %+v", resumed)
	}
	if got := read(t, filepath.Join(dir, "c.txt")); got != "c1\nc2\n" {
		t.Errorf("c.txt = %q", got)
	}
	if _, err := os.Stat(holdPath(held)); err == nil {
		t.Error("the hold mark outlived the step")
	}
}
