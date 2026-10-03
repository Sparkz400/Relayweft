package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sysload"
)

func TestCheckoutWorkersCapped(t *testing.T) {
	n := checkoutWorkers()
	if n < 1 || n > 4 || n > max(1, runtime.NumCPU()/2) {
		t.Fatalf("checkout workers = %d on %d cpus", n, runtime.NumCPU())
	}
}

// Two parallel explorers: while the machine is "busy", the second one is
// held until the load drops; the first one always starts.
func TestBusyMachineHoldsNewAgents(t *testing.T) {
	old := busyPoll
	busyPoll = 10 * time.Millisecond
	defer func() { busyPoll = old }()

	var busy atomic.Bool
	busy.Store(true)
	var mu sync.Mutex
	var startedAt []time.Time
	release := make(chan struct{})
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerPlan) {
			return runner.Result{Final: planJSON(
				map[string]any{"id": "a", "kind": "explore", "prompt": "find a"},
				map[string]any{"id": "b", "kind": "explore", "prompt": "find b"},
			)}
		}
		mu.Lock()
		startedAt = append(startedAt, time.Now())
		first := len(startedAt) == 1
		mu.Unlock()
		if first {
			<-release // keep the first agent running while the second waits
		}
		return runner.Result{Final: "found"}
	})
	o, rec := newOrc(t, "", set, func(c *config.Config) {
		c.Orchestrator.ReviewBeforePlan = false
		c.Orchestrator.MaxThreads = 2
		c.Orchestrator.BusyMaxWait = config.Duration(5 * time.Second)
	})
	o.opts.Load = func() sysload.Sample {
		if busy.Load() {
			return sysload.Sample{CPU: 0.99, CPUOK: true}
		}
		return sysload.Sample{CPU: 0.2, CPUOK: true}
	}
	done := make(chan TaskResult)
	go func() { done <- o.Run(context.Background(), longTask) }()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := len(startedAt)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d agents running while the machine is busy, want only the first", n)
	}
	busy.Store(false)
	time.Sleep(100 * time.Millisecond)
	close(release)
	if res := <-done; !res.OK {
		t.Fatalf("%+v", res)
	}
	held := false
	for _, e := range rec.all() {
		if e.Kind == event.Log && strings.Contains(e.Text, "machine busy (CPU 99%") {
			held = true
		}
	}
	if !held {
		t.Error("holding an agent was not logged")
	}
}

func TestBusyMachineGivesUpAfterMaxWait(t *testing.T) {
	old := busyPoll
	busyPoll = 5 * time.Millisecond
	defer func() { busyPoll = old }()
	release := make(chan struct{})
	var n atomic.Int32
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerPlan) {
			return runner.Result{Final: planJSON(
				map[string]any{"id": "a", "kind": "explore", "prompt": "a"},
				map[string]any{"id": "b", "kind": "explore", "prompt": "b"},
			)}
		}
		if n.Add(1) == 1 {
			<-release
		}
		return runner.Result{Final: "ok"}
	})
	o, _ := newOrc(t, "", set, func(c *config.Config) {
		c.Orchestrator.ReviewBeforePlan = false
		c.Orchestrator.BusyMaxWait = config.Duration(100 * time.Millisecond)
	})
	o.opts.Load = func() sysload.Sample { return sysload.Sample{MemFree: 10 << 20, MemOK: true} }
	done := make(chan TaskResult)
	go func() { done <- o.Run(context.Background(), longTask) }()
	deadline := time.Now().Add(3 * time.Second)
	for n.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if n.Load() < 2 {
		t.Fatal("second agent never started although busy_max_wait passed")
	}
	<-done
}

// With less free disk than min_free_disk_gb, no worktree is created and
// writers fall back to the main tree, one at a time; the task still works.
func TestLowDiskFallsBackToMainTree(t *testing.T) {
	dir := gitRepo(t)
	var dirs sync.Map
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(
				map[string]any{"id": "a", "kind": "edit", "prompt": "a", "files": []string{"a.txt"}},
				map[string]any{"id": "b", "kind": "edit", "prompt": "b", "files": []string{"b.txt"}},
			)}
		case strings.Contains(s.Prompt, "[SY:STEP]"):
			dirs.Store(s.StepID, s.Dir)
			os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte(s.StepID), 0o644)
			return runner.Result{Final: "done"}
		}
		return approve()
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		c.Orchestrator.MinFreeDiskGB = 1 << 30 // no disk is that big
	})
	defer minFreeDisk.Store(0)
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("%+v", res)
	}
	dirs.Range(func(k, v any) bool {
		if v.(string) != dir {
			t.Errorf("%v ran in %v, want the main tree", k, v)
		}
		return true
	})
	if read(t, filepath.Join(dir, "a.txt")) != "a" || read(t, filepath.Join(dir, "b.txt")) != "b" {
		t.Error("writes missing")
	}
	logged := false
	for _, e := range rec.all() {
		if e.Kind == event.Log && strings.Contains(e.Text, "free on the pool's disk") {
			logged = true
		}
	}
	if !logged {
		t.Error("low disk fallback not logged")
	}
}

func TestPrunePoolsRemovesIdleSlots(t *testing.T) {
	minFreeDisk.Store(0)
	// Pruning scans every pool: use a cache of our own.
	oldCache, cache := cacheDir, t.TempDir()
	cacheDir = func() (string, error) { return cache, nil }
	defer func() { cacheDir = oldCache }()
	dir := gitRepo(t)
	snap, err := (git{dir}).snapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	s.release()
	if pools := Pools(); len(pools) == 0 || pools[0].Slots == 0 || pools[0].Bytes == 0 {
		t.Fatalf("pools = %+v", pools)
	}
	// Used just now: not pruned.
	if n, _ := PrunePools(time.Hour); n != 0 {
		t.Fatalf("pruned a fresh slot")
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(path+".lock", old, old)
	n, freed := PrunePools(24 * time.Hour)
	if n != 1 || freed == 0 {
		t.Fatalf("pruned %d (%d bytes), want 1", n, freed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("slot directory still there")
	}
	if wl, _ := (git{dir}).out("worktree", "list"); strings.Count(wl, "\n") != 0 {
		t.Errorf("worktree record not pruned:\n%s", wl)
	}
	// A slot that is in use is never pruned.
	s2, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(s2.path+".lock", old, old)
	if n, _ := PrunePools(time.Hour); n != 0 {
		t.Error("pruned a slot in use")
	}
	s2.release()
}

// The full undo story on a real repo: task edits and creates files, the user
// edits one of them afterwards, undo restores everything else and keeps the
// user's edit, redo puts the task's changes back.
func TestUndoAndRedo(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "story.txt"), []byte("one\ntwo\nthree\nfour\nfive\nsix\n"), 0o644)
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, "[SY:STEP]") {
			os.WriteFile(filepath.Join(s.Dir, "story.txt"), []byte("ONE\ntwo\nthree\nfour\nfive\nsix\n"), 0o644)
			os.WriteFile(filepath.Join(s.Dir, "new.txt"), []byte("created\n"), 0o644)
			os.Remove(filepath.Join(s.Dir, "shared.txt"))
			return runner.Result{Final: "edited"}
		}
		return approve()
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	res := o.Run(context.Background(), "edit story")
	if !res.OK || res.UndoKey == "" {
		t.Fatalf("%+v", res)
	}
	// The user keeps working after the task.
	os.WriteFile(filepath.Join(dir, "story.txt"), []byte("ONE\ntwo\nthree\nfour\nfive\nSIX by user\n"), 0o644)

	list, err := UndoList(dir)
	if err != nil || len(list) != 1 || list[0].Key != res.UndoKey || list[0].Task != "edit story" {
		t.Fatalf("list = %+v %v", list, err)
	}
	plan, err := PreviewUndo(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 3 || len(plan.Edited) != 1 || plan.Edited[0] != "story.txt" {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := Undo(dir, "", false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "story.txt")); got != "one\ntwo\nthree\nfour\nfive\nSIX by user\n" {
		t.Errorf("story.txt after undo = %q (task change reverted, user edit kept)", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Error("file created by the task still exists")
	}
	if read(t, filepath.Join(dir, "shared.txt")) != "base\n" {
		t.Error("file deleted by the task not restored")
	}
	if l, _ := UndoList(dir); !l[0].Undone {
		t.Error("task not marked undone")
	}
	if _, _, err := findTask(dir, "", false); err == nil {
		t.Error("an undone task is offered for undo again")
	}
	if _, err := Undo(dir, "", true); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "story.txt")); got != "ONE\ntwo\nthree\nfour\nfive\nSIX by user\n" {
		t.Errorf("story.txt after redo = %q", got)
	}
	if read(t, filepath.Join(dir, "new.txt")) != "created\n" {
		t.Error("redo did not recreate new.txt")
	}
}

func TestUndoConflictChangesNothing(t *testing.T) {
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, "[SY:STEP]") {
			os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("agent\n"), 0o644)
			os.WriteFile(filepath.Join(s.Dir, "other.txt"), []byte("agent other\n"), 0o644)
			return runner.Result{Final: "edited"}
		}
		return approve()
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	o.Run(context.Background(), "edit shared")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("user rewrote the same line\n"), 0o644)
	if _, err := Undo(dir, "", false); err == nil || !strings.Contains(err.Error(), "shared.txt") {
		t.Fatalf("want a conflict on shared.txt, got %v", err)
	}
	if read(t, filepath.Join(dir, "shared.txt")) != "user rewrote the same line\n" || read(t, filepath.Join(dir, "other.txt")) != "agent other\n" {
		t.Error("a conflicting undo changed files")
	}
	if l, _ := UndoList(dir); l[0].Undone {
		t.Error("failed undo left the task marked undone")
	}
}

func TestUndoKeepsNewest30(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	snap, _ := g.snapshot("s")
	for i := 0; i < 33; i++ {
		key := "k" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		g.recordSnapshot(key, "before", snap)
		g.recordSnapshot(key, "after", snap)
	}
	trimUndo(dir)
	if l, _ := UndoList(dir); len(l) != undoKeep {
		t.Fatalf("kept %d tasks", len(l))
	}
}

func TestTaskCostPerProvider(t *testing.T) {
	set := runner.Set{
		event.Codex: scripted{event.Codex, func(s runner.Spec) runner.Result {
			if strings.Contains(s.Prompt, runner.MarkerPlan) {
				return runner.Result{Final: planJSON(map[string]any{"id": "w", "kind": "edit", "prompt": "fix it"}), Tokens: event.TokenUsage{Input: 1000, Output: 100}}
			}
			return runner.Result{Final: "ok", Tokens: event.TokenUsage{Input: 3000, Cached: 1000, Output: 200}}
		}},
		event.Claude: scripted{event.Claude, func(s runner.Spec) runner.Result {
			return runner.Result{Final: `{"approve": true}`, Tokens: event.TokenUsage{Input: 500, Output: 50, CostUSD: 0.12}}
		}},
	}
	o, rec := newOrc(t, "", set, nil)
	o.Tracker().SetQuota(event.Claude, event.QuotaInfo{Utilization: 0.40, ResetsAt: time.Now().Add(time.Hour)})
	res := o.Run(context.Background(), longTask)
	c := res.Cost
	if c.PerProvider[event.Codex].Total() != 1100+2200 || c.PerProvider[event.Claude].Total() != 2*550 {
		t.Errorf("per provider = %+v", c.PerProvider)
	}
	if c.CostUSD < 0.23 || c.CostUSD > 0.25 {
		t.Errorf("cost usd = %v", c.CostUSD)
	}
	if c.QuotaBefore[event.Claude] != 0.40 {
		t.Errorf("quota before = %v", c.QuotaBefore)
	}
	sum := c.Summary()
	for _, want := range []string{"codex 3.3k", "claude 1.1k", "≈$0.24", "claude limit 40%→40%"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary %q misses %q", sum, want)
		}
	}
	found := false
	for _, e := range rec.all() {
		if e.Kind == event.TaskDone && e.Cost != nil && e.Cost.CostUSD == c.CostUSD {
			found = true
		}
	}
	if !found {
		t.Error("TaskDone has no cost")
	}
}

// When Claude reports it is nearly out, work moves to Codex before the
// limit hits.
func TestQuotaPreemptInOrchestrator(t *testing.T) {
	var used sync.Map
	mk := func(p string) runner.Runner {
		return scripted{p, func(s runner.Spec) runner.Result {
			used.Store(s.Role+"@"+p, true)
			return runner.Result{Final: "ok"}
		}}
	}
	set := runner.Set{event.Codex: mk(event.Codex), event.Claude: mk(event.Claude)}
	o, rec := newOrc(t, "", set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	o.Tracker().SetQuota(event.Claude, event.QuotaInfo{Utilization: 0.95, Window: "five_hour", ResetsAt: time.Now().Add(time.Hour)})
	o.Run(context.Background(), "where is the parser") // explorer prefers claude
	if _, ok := used.Load("explorer@codex"); !ok {
		t.Fatal("explorer stayed on claude at 95% of its limit")
	}
	preempted := false
	for _, e := range rec.all() {
		if e.Kind == event.Route && e.Decision.Rule == router.RuleQuota {
			preempted = true
		}
	}
	if !preempted {
		t.Error("no quota-preempt decision recorded")
	}
}

// A reviewer whose provider is unavailable is retried on the other one
// instead of skipping the checkpoint.
func TestReviewerRetriesOnOtherProvider(t *testing.T) {
	var reviews sync.Map
	set := runner.Set{
		event.Codex: scripted{event.Codex, func(s runner.Spec) runner.Result {
			if s.Role == event.RoleReviewer {
				return runner.Result{Err: errString(`codex CLI "codex" not found on PATH`)}
			}
			if strings.Contains(s.Prompt, runner.MarkerPlan) {
				return runner.Result{Final: planJSON(map[string]any{"id": "w", "kind": "edit", "prompt": "fix"})}
			}
			return runner.Result{Final: "ok"}
		}},
		event.Claude: scripted{event.Claude, func(s runner.Spec) runner.Result {
			if s.Role == event.RoleReviewer {
				reviews.Store(s.StepID, true)
			}
			if strings.Contains(s.Prompt, runner.MarkerPlan) {
				return runner.Result{Final: planJSON(map[string]any{"id": "w", "kind": "edit", "prompt": "fix"})}
			}
			return runner.Result{Final: `{"approve": true, "advice": "fine"}`}
		}},
	}
	o, _ := newOrc(t, "", set, func(c *config.Config) {
		r := c.Roles[event.RoleReviewer]
		r.Prefer = config.PreferCodex
		c.Roles[event.RoleReviewer] = r
	})
	if res := o.Run(context.Background(), longTask); !res.OK || !strings.Contains(res.Summary, "reviewer approved") {
		t.Fatalf("%+v", res)
	}
	if _, ok := reviews.Load("review-plan"); !ok {
		t.Error("plan review skipped instead of moving to claude")
	}
}

func TestWorktreesOfOneRepoShareAPool(t *testing.T) {
	dir := gitRepo(t)
	ws, err := NewBenchWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := ws.Head()
	if err := ws.Reset(head); err != nil {
		t.Fatal(err)
	}
	if poolDir(ws.Path) != poolDir(dir) {
		t.Fatalf("bench workspace has its own pool:\n%s\n%s", poolDir(ws.Path), poolDir(dir))
	}
}
