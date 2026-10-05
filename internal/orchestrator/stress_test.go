package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
	"github.com/sparkz400/relayweft/internal/sysload"
)

// Long-run stress test (ROADMAP 1.7). It runs whole tasks in a loop and
// checks that goroutines, heap, open files/handles, leftover processes and
// pool worktrees stay flat. It only runs when RW_STRESS_DURATION is set:
//
//	RW_STRESS_DURATION=3m go test -run '^TestStress$' -v -timeout 0 ./internal/orchestrator
//
// The time is split between two modes:
//   - demo: the demo scenario (runner.NewFakeSet) in a no-git folder, as
//     `rw --demo` runs it; every 4th task is cancelled midway.
//   - git: a real repository with worktrees and merges. The agents are real
//     subprocesses (this test binary acting as a fake Claude CLI through
//     the real runner), so process spawning, stream parsing and tree kills
//     are exercised; every 5th task is cancelled while an agent (and a
//     grandchild it started) is running.

const (
	fakeCLIFlag  = "--rw-stress-fake-cli"
	sleeperFlag  = "--rw-stress-sleeper"
	stressDirArg = "--rw-stress-dir="
)

// init turns this test binary into the fake agent CLI when the stress test
// starts it as one. It runs before TestMain, so no test flags are parsed.
func init() {
	for _, a := range os.Args[1:] {
		switch a {
		case fakeCLIFlag:
			fakeClaudeCLI()
			os.Exit(0)
		case sleeperFlag:
			time.Sleep(2 * time.Minute)
			os.Exit(0)
		}
	}
}

var (
	reIteration = regexp.MustCompile(`iteration (\d+)`)
	reWrite     = regexp.MustCompile(`write (\S+) with (\S+)`)
)

// fakeClaudeCLI speaks just enough of `claude -p --output-format
// stream-json` for the orchestrator: plans, reviews, and steps that write
// files in the agent's working directory.
func fakeClaudeCLI() {
	in, _ := io.ReadAll(os.Stdin)
	prompt := string(in)
	stressDir := ""
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, stressDirArg) {
			stressDir = strings.TrimPrefix(a, stressDirArg)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	sid := fmt.Sprintf("stress-%d", os.Getpid())
	enc.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "stress"})
	result := func(text string) {
		enc.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text,
			"session_id": sid, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10}})
	}
	edit := func(path string) {
		enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Write", "input": map[string]any{"file_path": path}},
		}}})
	}
	iter := "0"
	if m := reIteration.FindStringSubmatch(prompt); m != nil {
		iter = m[1]
	}
	switch {
	case strings.Contains(prompt, runner.MarkerPlanReview), strings.Contains(prompt, runner.MarkerFinalReview),
		strings.Contains(prompt, runner.MarkerErrorReview):
		result(`{"approve": true, "advice": "ok"}`)
	case strings.Contains(prompt, runner.MarkerPlan):
		hang := ""
		if strings.Contains(prompt, "and hang") {
			hang = " then hang"
		}
		result(planJSON(
			map[string]any{"id": "look", "title": "look around", "kind": "explore", "prompt": "find the files"},
			map[string]any{"id": "a", "title": "write a", "kind": "edit", "prompt": "write a.txt with iter-" + iter + "-a", "files": []string{"a.txt"}},
			map[string]any{"id": "b", "title": "write b", "kind": "edit", "prompt": "write b.txt with iter-" + iter + "-b", "files": []string{"b.txt"}},
			map[string]any{"id": "c", "title": "combine", "kind": "edit", "prompt": "combine a.txt and b.txt into c.txt" + hang, "files": []string{"c.txt"}, "depends_on": []string{"a", "b"}},
		))
	case strings.Contains(prompt, "then hang"):
		// A grandchild in the same process tree: a cancel must kill both.
		self, _ := os.Executable()
		gc := exec.Command(self, sleeperFlag)
		gc.Start()
		if stressDir != "" {
			os.WriteFile(filepath.Join(stressDir, "hang-"+iter), []byte("x"), 0o644)
		}
		time.Sleep(2 * time.Minute)
		result("woke up")
	case strings.Contains(prompt, "combine a.txt and b.txt"):
		a, _ := os.ReadFile("a.txt")
		b, _ := os.ReadFile("b.txt")
		os.WriteFile("c.txt", append(a, b...), 0o644)
		edit("c.txt")
		result("combined")
	default:
		if m := reWrite.FindStringSubmatch(prompt); m != nil {
			os.WriteFile(m[1], []byte(m[2]+"\n"), 0o644)
			edit(m[1])
			result("wrote " + m[1])
			return
		}
		result("Found the files.")
	}
}

// resources is one sample of what must stay flat.
type resources struct {
	goroutines int
	heap       uint64 // live heap after GC
	handles    int    // open fds (Linux) or handles (Windows); -1 if unknown
	stray      int    // leftover child/agent processes; -1 if unknown
}

func sampleResources() resources {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return resources{goroutines: runtime.NumGoroutine(), heap: ms.HeapAlloc, handles: openHandles(), stray: strayProcesses()}
}

// stressMonitor tracks samples taken after every task.
type stressMonitor struct {
	t         *testing.T
	base      resources // before the first task
	warm      resources // after the warm-up tasks
	samples   []resources
	maxGor    int
	maxHandle int
	maxHeap   uint64
}

const (
	warmupTasks    = 5
	goroutineSlack = 5
)

// settle waits for the task's stragglers (prewarm, event delivery,
// process reaping) and then records a sample. It fails the test when
// goroutines or processes stay above the baseline.
func (m *stressMonitor) settle(iter int) resources {
	m.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var r resources
	for {
		r = sampleResources()
		ok := r.goroutines <= m.base.goroutines+goroutineSlack && r.stray <= max(m.base.stray, 0)
		if ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r.goroutines > m.base.goroutines+goroutineSlack {
		var b strings.Builder
		pprof.Lookup("goroutine").WriteTo(&b, 1)
		m.t.Fatalf("task %d: %d goroutines, baseline %d (+%d slack): leak?\n%s", iter, r.goroutines, m.base.goroutines, goroutineSlack, b.String())
	}
	if r.stray > max(m.base.stray, 0) {
		m.t.Fatalf("task %d: %d leftover processes (baseline %d)", iter, r.stray, m.base.stray)
	}
	m.samples = append(m.samples, r)
	if iter == warmupTasks {
		m.warm = r
	}
	if iter > warmupTasks {
		if r.handles >= 0 && m.warm.handles >= 0 && r.handles > m.warm.handles+handleSlack {
			m.t.Fatalf("task %d: %d open handles, %d after warm-up: leak?", iter, r.handles, m.warm.handles)
		}
	}
	m.maxGor = max(m.maxGor, r.goroutines)
	m.maxHandle = max(m.maxHandle, r.handles)
	if r.heap > m.maxHeap {
		m.maxHeap = r.heap
	}
	return r
}

// finish checks the heap trend: after warm-up the live heap must not keep
// growing. The median of the last third is compared with the first third.
func (m *stressMonitor) finish(name string, iters int, elapsed time.Duration) {
	m.t.Helper()
	m.t.Logf("%s: %d tasks in %s (%.1f/s)", name, iters, elapsed.Round(time.Second), float64(iters)/elapsed.Seconds())
	m.t.Logf("%s: goroutines base %d, max after a task %d", name, m.base.goroutines, m.maxGor)
	m.t.Logf("%s: open handles base %d, after warm-up %d, max %d", name, m.base.handles, m.warm.handles, m.maxHandle)
	m.t.Logf("%s: leftover processes base %d (-1 = not checked on %s)", name, m.base.stray, runtime.GOOS)
	post := m.samples
	if len(post) > warmupTasks {
		post = post[warmupTasks:]
	}
	if len(post) < 6 {
		m.t.Logf("%s: heap max %s (too few tasks for a trend)", name, mb(m.maxHeap))
		return
	}
	third := len(post) / 3
	early, late := medianHeap(post[:third]), medianHeap(post[len(post)-third:])
	m.t.Logf("%s: live heap base %s, after warm-up %s, median early %s, median late %s, max %s",
		name, mb(m.base.heap), mb(m.warm.heap), mb(early), mb(late), mb(m.maxHeap))
	// Generous bounds: catch leaks that grow with the number of tasks, not
	// noise from GC timing.
	if late > early+early/2+4<<20 {
		m.t.Errorf("%s: live heap grew from %s to %s over %d tasks: leak?", name, mb(early), mb(late), len(post))
	}
	if m.maxHeap > 4*m.warm.heap+32<<20 {
		m.t.Errorf("%s: live heap peaked at %s (%s after warm-up)", name, mb(m.maxHeap), mb(m.warm.heap))
	}
}

func medianHeap(rs []resources) uint64 {
	h := make([]uint64, len(rs))
	for i, r := range rs {
		h[i] = r.heap
	}
	sort.Slice(h, func(i, j int) bool { return h[i] < h[j] })
	return h[len(h)/2]
}

func mb(n uint64) string { return fmt.Sprintf("%.1fMB", float64(n)/(1<<20)) }

// stressOrc builds an orchestrator whose events are drained for the whole
// run, as the TUI does.
func stressOrc(t *testing.T, dir, mode string, runners func(*config.Config) runner.Set, edit func(*config.Config)) (*Orchestrator, *atomic.Int64) {
	cfg := config.Default()
	cfg.Orchestrator.MinFreeDiskGB = 0 // CI disks are small; worktrees must still be used
	if edit != nil {
		edit(cfg)
	}
	ch := make(chan event.Event, 4096)
	var n atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
			n.Add(1)
		}
	}()
	t.Cleanup(func() { close(ch); <-done })
	log, err := sessionlog.Open(t.TempDir(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	o := New(Options{
		Dir: dir, Store: config.NewStore(cfg, filepath.Join(t.TempDir(), "rw.yaml")),
		Runners: runners, Tracker: limits.NewTracker(), Log: log, Events: ch,
		NoGit: dir == "", Mode: mode,
		Load: func() sysload.Sample { return sysload.Sample{} },
	})
	return o, &n
}

func stressDuration(t *testing.T) time.Duration {
	s := os.Getenv("RW_STRESS_DURATION")
	if s == "" {
		t.Skip("set RW_STRESS_DURATION (e.g. 3m) to run the long-run stress test")
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		t.Fatalf("RW_STRESS_DURATION=%q: want a duration like 30m", s)
	}
	return d
}

func TestStress(t *testing.T) {
	total := stressDuration(t)
	t.Run("demo", func(t *testing.T) { stressDemo(t, total/2) })
	t.Run("git", func(t *testing.T) { stressGit(t, total-total/2) })
}

func stressDemo(t *testing.T, d time.Duration) {
	o, events := stressOrc(t, "", "demo", nil, nil)
	var iter int
	o.opts.Runners = func(*config.Config) runner.Set {
		if iter%4 == 0 {
			return runner.NewFakeSet(40) // slow enough to be cancelled midway
		}
		return runner.NewFakeSet(0)
	}
	m := &stressMonitor{t: t}
	m.base = sampleResources()
	start := time.Now()
	for iter = 1; time.Since(start) < d || iter <= warmupTasks+6; iter++ {
		ctx, cancel := context.WithCancel(context.Background())
		if iter%4 == 0 {
			time.AfterFunc(time.Duration(50+iter%7*40)*time.Millisecond, cancel)
		}
		res := o.Run(ctx, "Make the parser keep trailing empty fields and add a strict flag with tests and docs please")
		if iter%4 != 0 && !res.OK {
			t.Fatalf("demo task %d failed: %+v", iter, res)
		}
		cancel()
		// Every task starts with both providers available again.
		o.Tracker().Clear(event.Codex)
		o.Tracker().Clear(event.Claude)
		m.settle(iter)
	}
	m.finish("demo", iter-1, time.Since(start))
	t.Logf("demo: %d events", events.Load())
}

func stressGit(t *testing.T, d time.Duration) {
	dir := gitRepo(t)
	stressDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	pc := cfg.Providers[event.Claude]
	pc.Command = self
	pc.ExtraArgs = []string{fakeCLIFlag, stressDirArg + stressDir}
	set := runner.Set{event.Claude: runner.NewClaude(pc, limits.NewDetector(cfg.LimitPatterns))}
	set[event.Codex] = set[event.Claude]
	o, events := stressOrc(t, dir, "routed", func(*config.Config) runner.Set { return set }, func(c *config.Config) {
		c.Providers[event.Claude] = pc
	})

	maxThreads := config.Default().Orchestrator.MaxThreads
	m := &stressMonitor{t: t}
	m.base = sampleResources()
	start := time.Now()
	var iter, cancelled int
	var maxSlots, maxWorktrees, maxRefs, refsAt60 int
	for iter = 1; time.Since(start) < d || iter <= warmupTasks+6; iter++ {
		hang := iter%5 == 0
		task := fmt.Sprintf("Create files a and b in parallel then combine them into c with a summary line, iteration %d", iter)
		if hang {
			task += " and hang"
		}
		ctx, cancel := context.WithCancel(context.Background())
		resc := make(chan TaskResult, 1)
		go func() { resc <- o.Run(ctx, task) }()
		var res TaskResult
		if hang {
			marker := filepath.Join(stressDir, fmt.Sprintf("hang-%d", iter))
			deadline := time.Now().Add(2 * time.Minute)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("task %d: the hanging agent never started", iter)
				}
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
			cancelled++
		}
		select {
		case res = <-resc:
		case <-time.After(3 * time.Minute):
			t.Fatalf("task %d did not finish (hang=%v)", iter, hang)
		}
		cancel()
		if hang {
			if res.OK {
				t.Fatalf("cancelled task %d reported OK", iter)
			}
		} else {
			if !res.OK {
				t.Fatalf("git task %d failed: %+v", iter, res)
			}
			want := fmt.Sprintf("iter-%d-a\niter-%d-b\n", iter, iter)
			if got := read(t, filepath.Join(dir, "c.txt")); got != want {
				t.Fatalf("task %d: c.txt = %q, want %q", iter, got, want)
			}
		}
		m.settle(iter)

		// Pool and repository bookkeeping stays bounded.
		slots := 0
		if es, err := os.ReadDir(poolDir(dir)); err == nil {
			for _, e := range es {
				if e.IsDir() {
					slots++
				}
			}
		}
		wl, _ := (git{dir}).out("worktree", "list", "--porcelain")
		worktrees := strings.Count(wl, "worktree ") - 1
		refs, _ := (git{dir}).out("for-each-ref", "--format=%(refname)")
		nrefs := len(strings.Fields(refs))
		if slots > maxThreads+1 || worktrees > slots {
			t.Fatalf("task %d: %d pool slots, %d registered worktrees (max_threads %d)\n%s", iter, slots, worktrees, maxThreads, wl)
		}
		maxSlots, maxWorktrees, maxRefs = max(maxSlots, slots), max(maxWorktrees, worktrees), max(maxRefs, nrefs)
		// Undo keeps the last 30 tasks, so after 60 tasks the number of
		// refs must stop growing.
		if iter == 60 {
			refsAt60 = nrefs
		}
		if refsAt60 > 0 && nrefs > refsAt60+5 {
			t.Fatalf("task %d: %d refs, %d after 60 tasks: still growing", iter, nrefs, refsAt60)
		}
	}
	m.finish("git", iter-1, time.Since(start))
	t.Logf("git: %d tasks cancelled midway, %d events", cancelled, events.Load())
	t.Logf("git: pool slots max %d, registered worktrees max %d, refs max %d", maxSlots, maxWorktrees, maxRefs)
}
