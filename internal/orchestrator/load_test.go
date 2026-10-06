package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// Heavy-load test (ROADMAP "Not verified yet": a big repo, three agents in
// parallel, hours of use). Three writers, each an orchestrator as a rw
// window is, run tasks on one big repo at the same time, sharing its
// worktree pool. Their agents are real subprocesses (this test binary as a
// scripted Claude CLI) that read and edit existing files and add new ones,
// three steps at a time. Every 7th task hangs and is cancelled; every
// other cancelled task is resumed (--force). Every 5th task is undone
// right after it ended, every other undo is redone.
//
// All writers stop at a quiet point every RW_LOAD_QUIET (default 10m; 1m
// for runs under 15m). There the test checks that every edit that must be
// in the tree is there and every undone one is not, and that goroutines,
// leftover processes and open handles are back to their baseline; it
// records the heap, the pool's slots and disk use, refs, and rw's CPU
// time. In between a monitor records the peaks every 5s.
//
//	RW_LOAD_DURATION  how long (required; e.g. 3m for a local smoke run)
//	RW_LOAD_REPO      a git repo to copy (a local clone; default: a
//	                  generated one with RW_LOAD_FILES files, 1500)
//	RW_LOAD_WORK      folder for the copy, the pool and the state (default:
//	                  a temp folder)
//	RW_LOAD_OUT       folder for samples.csv and summary.txt (optional)
//
// With git-lfs installed, the copy gets a commit with Git LFS files.
//
//	RW_LOAD_DURATION=3m go test -run '^TestLoad$' -v -timeout 0 ./internal/orchestrator

const (
	loadCLIFlag  = "--rw-load-fake-cli"
	loadStateArg = "--rw-load-state="
	loadWriters  = 3
)

func init() {
	for _, a := range os.Args[1:] {
		if a == loadCLIFlag {
			os.Exit(loadAgentCLI())
		}
	}
}

var (
	reLoadSpec  = regexp.MustCompile(`\[load ([^\]]*)\]`)
	reLoadTask  = regexp.MustCompile(`load task (w\d+-r\d+)`)
	reLoadEdit  = regexp.MustCompile(`append <<([^>]+)>> to <<([^>]+)>>`)
	reLoadNew   = regexp.MustCompile(`create <<([^>]+)>> with <<([^>]+)>>`)
	reLoadReads = regexp.MustCompile(`read <<([^>]+)>>`)
)

// loadAgentCLI is the scripted agent: a plan from the task's [load ...]
// spec, approving reviews, and steps that read files, append a marker
// line to existing files and create new ones in their working directory.
func loadAgentCLI() int {
	state := ""
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, loadStateArg) {
			state = strings.TrimPrefix(a, loadStateArg)
		}
	}
	in, _ := io.ReadAll(os.Stdin)
	prompt := string(in)
	enc := json.NewEncoder(os.Stdout)
	sid := fmt.Sprintf("load-%d-%d", os.Getpid(), time.Now().UnixNano())
	for i, a := range os.Args {
		if a == "--resume" && i+1 < len(os.Args) {
			sid = os.Args[i+1]
		}
	}
	enc.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "load"})
	result := func(text string) int {
		enc.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text,
			"session_id": sid, "usage": map[string]any{"input_tokens": 3000, "output_tokens": 200}})
		return 0
	}
	fail := func(msg string) int {
		enc.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": msg, "session_id": sid})
		return 1
	}
	tool := func(name, path string) {
		enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "t", "name": name, "input": map[string]any{"file_path": path}}}}})
	}
	key := "x"
	if m := reLoadTask.FindStringSubmatch(prompt); m != nil {
		key = m[1]
	}
	switch {
	case strings.Contains(prompt, runner.MarkerPlanReview), strings.Contains(prompt, runner.MarkerFinalReview),
		strings.Contains(prompt, runner.MarkerErrorReview):
		return result(`{"approve": true, "advice": "ok", "issues": []}`)
	case strings.Contains(prompt, runner.MarkerPlan):
		return result(loadPlan(prompt))
	}
	// A step (or a resumed one): its own line of the prompt.
	own := prompt
	if i := strings.Index(prompt, "YOUR SUBTASK"); i >= 0 {
		own = prompt[i:]
		if j := strings.Index(own, "\n"); j >= 0 {
			own = own[j+1:]
		}
		if j := strings.Index(own, "\n"); j >= 0 {
			own = own[:j]
		}
	} else if strings.Contains(prompt, runner.MarkerResume) && state != "" {
		// A resume names no subtask: continue the step this session hung in.
		b, err := os.ReadFile(filepath.Join(state, "hang-"+key))
		if err != nil {
			return fail("load agent: resumed without a hung step")
		}
		own = string(b)
	}
	wd, _ := os.Getwd()
	for _, m := range reLoadReads.FindAllStringSubmatch(own, -1) {
		data, err := os.ReadFile(filepath.Join(wd, filepath.FromSlash(m[1])))
		if err != nil {
			return fail("read " + m[1] + ": " + err.Error())
		}
		_ = len(data)
		tool("Read", filepath.Join(wd, filepath.FromSlash(m[1])))
	}
	if strings.Contains(own, "{hang}") && state != "" {
		marker := filepath.Join(state, "hang-"+key)
		if _, err := os.Stat(marker); err != nil {
			os.WriteFile(marker+".tmp", []byte(own), 0o644)
			os.Rename(marker+".tmp", marker)
			time.Sleep(10 * time.Minute)
			return fail("load agent: was not cancelled within 10 minutes")
		}
	}
	for _, m := range reLoadEdit.FindAllStringSubmatch(own, -1) {
		p := filepath.Join(wd, filepath.FromSlash(m[2]))
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return fail("edit " + m[2] + ": " + err.Error())
		}
		_, err = f.WriteString("\n" + m[1] + "\n")
		f.Close()
		if err != nil {
			return fail("edit " + m[2] + ": " + err.Error())
		}
		tool("Edit", p)
	}
	for _, m := range reLoadNew.FindAllStringSubmatch(own, -1) {
		p := filepath.Join(wd, filepath.FromSlash(m[1]))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(m[2]+"\n"), 0o644); err != nil {
			return fail("create " + m[1] + ": " + err.Error())
		}
		tool("Write", p)
	}
	return result("done: " + own)
}

// loadPlan builds the plan of a task from its spec: "look" (a read-only
// step) and edit steps separated by ";", each "id: action, action ...".
func loadPlan(prompt string) string {
	m := reLoadSpec.FindStringSubmatch(prompt)
	if m == nil {
		return "```json\n{\"summary\": \"nothing\", \"subtasks\": []}\n```"
	}
	var subs []map[string]any
	for _, part := range strings.Split(m[1], ";") {
		id, body, _ := strings.Cut(strings.TrimSpace(part), ":")
		id, body = strings.TrimSpace(id), strings.TrimSpace(body)
		var files []string
		for _, e := range reLoadEdit.FindAllStringSubmatch(body, -1) {
			files = append(files, e[2])
		}
		for _, e := range reLoadNew.FindAllStringSubmatch(body, -1) {
			files = append(files, e[1])
		}
		kind := "edit"
		if len(files) == 0 {
			kind = "explore"
		}
		subs = append(subs, map[string]any{"id": id, "title": "step " + id, "kind": kind, "prompt": body, "files": files})
	}
	b, _ := json.Marshal(map[string]any{"summary": "Edit the files.", "subtasks": subs})
	return "```json\n" + string(b) + "\n```"
}

// loadMark is one marker line a task appends to a file (or the content of
// a file it creates).
type loadMark struct {
	file, mark string
	created    bool
}

// loadWriter is one writer's view of the tree: the marks that must be
// there and those that must not (undone).
type loadWriter struct {
	id                                 int
	o                                  *Orchestrator
	files                              []string // the existing files it may edit
	rng                                *rand.Rand
	live, gone                         map[loadMark]bool
	tasks, ok, cancels, resumes, undos int
	redos, failed                      int
	taskTime                           time.Duration
}

func TestLoad(t *testing.T) {
	s := os.Getenv("RW_LOAD_DURATION")
	if s == "" {
		t.Skip("set RW_LOAD_DURATION (e.g. 3m) to run the heavy-load test")
	}
	total, err := time.ParseDuration(s)
	if err != nil || total <= 0 {
		t.Fatalf("RW_LOAD_DURATION=%q: want a duration like 2h", s)
	}
	quiet := 10 * time.Minute
	if total < 15*time.Minute {
		quiet = time.Minute
	}
	if v := os.Getenv("RW_LOAD_QUIET"); v != "" {
		if quiet, err = time.ParseDuration(v); err != nil {
			t.Fatal(err)
		}
	}
	work := os.Getenv("RW_LOAD_WORK")
	if work == "" {
		work = t.TempDir()
	}
	out := os.Getenv("RW_LOAD_OUT")
	if out != "" {
		os.MkdirAll(out, 0o755)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	// The repo, the pool and the task state live in work.
	began := time.Now()
	dir := filepath.Join(work, "repo")
	if src := os.Getenv("RW_LOAD_REPO"); src != "" {
		loadGit(t, work, "clone", "-q", src, dir)
	} else {
		n, _ := strconv.Atoi(os.Getenv("RW_LOAD_FILES"))
		loadSynthRepo(t, dir, max(n, 1500))
	}
	loadGit(t, dir, "config", "core.autocrlf", "false")
	loadGit(t, dir, "config", "user.name", "rw load")
	loadGit(t, dir, "config", "user.email", "load@localhost")
	lfs := loadAddLFS(t, dir)
	oldCache := cacheDir
	cacheDir = func() (string, error) { return filepath.Join(work, "cache"), nil }
	t.Cleanup(func() { cacheDir = oldCache })
	files := loadEditable(t, dir)
	t.Logf("repo %s: %d tracked files, %d editable, LFS %v, ready in %s", dir, loadCount(t, dir), len(files), lfs, time.Since(began).Round(time.Second))

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(work, "agent-state")
	os.MkdirAll(state, 0o755)
	cfg := config.Default()
	cfg.Orchestrator.MinFreeDiskGB = 1
	cfg.Notify.Enabled = false
	pc := cfg.Providers[event.Claude]
	pc.Command = self
	pc.ExtraArgs = []string{loadCLIFlag, loadStateArg + state}
	cfg.Providers[event.Claude] = pc
	set := runner.Set{event.Claude: runner.NewClaude(pc, limits.NewDetector(cfg.LimitPatterns))}
	set[event.Codex] = set[event.Claude]
	logs := filepath.Join(work, "sessions")
	sampler := sysload.NewSampler(2 * time.Second) // as rw has one
	sampler.Get()

	var events atomic.Int64
	writers := make([]*loadWriter, loadWriters)
	for i := range writers {
		ch := make(chan event.Event, 4096)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range ch {
				events.Add(1)
			}
		}()
		t.Cleanup(func() { close(ch); <-done })
		log, err := sessionlog.Open(logs, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { log.Close() })
		o := New(Options{Dir: dir, Store: config.NewStore(cfg.Clone(), filepath.Join(work, fmt.Sprintf("rw%d.yaml", i))),
			Runners: func(*config.Config) runner.Set { return set }, Tracker: limits.NewTracker(), Log: log, Events: ch,
			Mode: "routed", Load: sampler.Get})
		var mine []string
		for j, f := range files {
			if j%loadWriters == i {
				mine = append(mine, f)
			}
		}
		seed := int64(i) + time.Now().UnixNano()
		t.Logf("writer %d: seed %d", i+1, seed)
		writers[i] = &loadWriter{id: i + 1, o: o, files: mine, rng: rand.New(rand.NewSource(seed)),
			live: map[loadMark]bool{}, gone: map[loadMark]bool{}}
	}

	m := newLoadMonitor(t, dir, out)
	m.logs = logs
	m.base = sampleResources()
	m.cpu0 = loadCPU()
	m.wall0 = time.Now()
	stopMon := m.watch(5 * time.Second)
	defer stopMon()

	var gate sync.RWMutex // writers hold it shared per round; a quiet point takes it
	var stop atomic.Bool
	time.AfterFunc(total, func() { stop.Store(true) })
	var wg sync.WaitGroup
	errc := make(chan error, loadWriters)
	for _, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 1; !stop.Load(); round++ {
				gate.RLock()
				err := w.round(t, dir, state, round)
				gate.RUnlock()
				if err != nil {
					errc <- fmt.Errorf("writer %d round %d: %w", w.id, round, err)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for q := 1; ; q++ {
		select {
		case <-done:
		case err := <-errc:
			t.Error(err)
			// The others stop at the deadline; do not wait for it.
			stop.Store(true)
			<-done
		case <-time.After(quiet):
			gate.Lock()
			m.quietPoint(fmt.Sprintf("quiet %d", q), writers)
			gate.Unlock()
			if t.Failed() {
				stop.Store(true)
				<-done
				m.report(writers, events.Load())
				return
			}
			continue
		}
		break
	}
	for len(errc) > 0 { // the other writers' errors, if any
		t.Error(<-errc)
	}
	m.quietPoint("end", writers)
	m.report(writers, events.Load())
}

// round is one task of a writer, with its cancel, resume, undo and redo.
func (w *loadWriter) round(t *testing.T, dir, state string, round int) error {
	key := fmt.Sprintf("w%d-r%d", w.id, round)
	hang := round%7 == 0
	// Three edit steps of 2-4 files each, a new file in one of them, and a
	// read-only step that reads a few files.
	var steps []string
	var marks []loadMark
	reads := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		reads = append(reads, "read <<"+w.files[w.rng.Intn(len(w.files))]+">>")
	}
	steps = append(steps, "look: "+strings.Join(reads, " "))
	picked := map[string]bool{}
	for s, id := range []string{"a", "b", "c"} {
		var acts []string
		for n := 2 + w.rng.Intn(3); len(acts) < n; {
			f := w.files[w.rng.Intn(len(w.files))]
			if picked[f] {
				continue
			}
			picked[f] = true
			mk := loadMark{file: f, mark: fmt.Sprintf("rw-load-mark(%s.%s.%d)", key, id, len(acts))}
			marks = append(marks, mk)
			acts = append(acts, fmt.Sprintf("append <<%s>> to <<%s>>", mk.mark, f))
		}
		if s == 0 {
			mk := loadMark{file: fmt.Sprintf("rw-load/w%d/%s.txt", w.id, key), mark: "created by " + key, created: true}
			marks = append(marks, mk)
			acts = append(acts, fmt.Sprintf("create <<%s>> with <<%s>>", mk.file, mk.mark))
		}
		line := strings.Join(acts, " ")
		if hang && id == "c" {
			line += " {hang}"
		}
		steps = append(steps, id+": "+line)
	}
	text := fmt.Sprintf("Relayweft load task %s: change the files as the plan says [load %s]", key, strings.Join(steps, "; "))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if hang {
		marker := filepath.Join(state, "hang-"+key)
		go func() {
			for ctx.Err() == nil {
				if _, err := os.Stat(marker); err == nil {
					cancel()
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	start := time.Now()
	res := w.o.Run(ctx, text)
	w.taskTime += time.Since(start)
	w.tasks++
	if hang {
		if res.OK || !strings.HasPrefix(res.Summary, "cancelled") {
			return fmt.Errorf("the hanging task %s was not cancelled: %+v", key, res)
		}
		w.cancels++
		if w.cancels%2 == 1 {
			st, err := LoadTask(res.UndoKey)
			if err != nil {
				return fmt.Errorf("resume %s: %w", key, err)
			}
			res = w.o.RunWith(context.Background(), "", TaskOptions{Resume: st, Force: true})
			w.resumes++
			if !res.OK {
				return fmt.Errorf("resumed task %s failed: %s", key, res.Summary)
			}
		} else {
			// Not resumed: what landed stays, the hung step's edits do not
			// (they stay in its worktree or on a branch).
			for _, mk := range marks {
				if loadHas(dir, mk) {
					w.live[mk] = true
				} else {
					w.gone[mk] = true
				}
			}
			return nil
		}
	} else if !res.OK {
		// A failure under load is a finding: say why, with the log.
		w.failed++
		return fmt.Errorf("task %s failed: %s", key, res.Summary)
	}
	w.ok++
	for _, mk := range marks {
		if !loadHas(dir, mk) {
			return fmt.Errorf("task %s: %s has no %q", key, mk.file, mk.mark)
		}
		w.live[mk] = true
	}
	if w.ok%5 != 0 {
		return nil
	}
	// Undo this task (only the agents' files: the other writers' edits of
	// the same time are in its snapshots too), and every other time redo.
	if _, err := Undo(dir, res.UndoKey, false, true); err != nil {
		return fmt.Errorf("undo %s: %w", key, err)
	}
	w.undos++
	for _, mk := range marks {
		if loadHas(dir, mk) {
			return fmt.Errorf("undo %s: %s still has %q", key, mk.file, mk.mark)
		}
		delete(w.live, mk)
		w.gone[mk] = true
	}
	if w.undos%2 == 0 {
		if _, err := Undo(dir, res.UndoKey, true, true); err != nil {
			return fmt.Errorf("redo %s: %w", key, err)
		}
		w.redos++
		for _, mk := range marks {
			if !loadHas(dir, mk) {
				return fmt.Errorf("redo %s: %s has no %q", key, mk.file, mk.mark)
			}
			delete(w.gone, mk)
			w.live[mk] = true
		}
	}
	return nil
}

func loadHas(dir string, mk loadMark) bool {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(mk.file)))
	if err != nil {
		return false
	}
	if mk.created {
		return strings.TrimSpace(string(b)) == mk.mark
	}
	return strings.Contains(string(b), "\n"+mk.mark+"\n")
}

// loadMonitor records samples and checks the quiet points.
type loadMonitor struct {
	t     *testing.T
	dir   string
	csv   *os.File
	logs  string // the writers' session logs
	base  resources
	warm  resources // at the first quiet point
	cpu0  loadCPUTimes
	wall0 time.Time

	mu                               sync.Mutex
	peakGor, peakHandles, peakProcs  int
	peakHeap                         uint64
	quiet                            []resources
	slots, worktrees, refs, maxSlots int
	poolBytes, maxPoolBytes          int64
	refsAt                           []int
}

func newLoadMonitor(t *testing.T, dir, out string) *loadMonitor {
	m := &loadMonitor{t: t, dir: dir}
	if out != "" {
		if f, err := os.Create(filepath.Join(out, "samples.csv")); err == nil {
			m.csv = f
			fmt.Fprintln(f, "seconds,kind,goroutines,heap_mb,handles,processes,pool_slots,pool_mb,refs,rw_cpu_s")
			t.Cleanup(func() { f.Close() })
		}
	}
	return m
}

// watch samples every period without a GC (the peaks while tasks run).
func (m *loadMonitor) watch(period time.Duration) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tk := time.NewTicker(period)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
			}
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			r := resources{goroutines: runtime.NumGoroutine(), heap: ms.HeapAlloc, handles: openHandles(), stray: strayProcesses()}
			m.mu.Lock()
			m.peakGor = max(m.peakGor, r.goroutines)
			m.peakHandles = max(m.peakHandles, r.handles)
			m.peakProcs = max(m.peakProcs, r.stray)
			m.peakHeap = max(m.peakHeap, r.heap)
			m.mu.Unlock()
			m.row("run", r)
		}
	}()
	return func() { close(done); <-finished }
}

func (m *loadMonitor) row(kind string, r resources) {
	if m.csv == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := loadCPU()
	fmt.Fprintf(m.csv, "%.0f,%s,%d,%.1f,%d,%d,%d,%.0f,%d,%.1f\n", time.Since(m.wall0).Seconds(), kind, r.goroutines, float64(r.heap)/(1<<20),
		r.handles, r.stray, m.slots, float64(m.poolBytes)/(1<<20), m.refs, (c.self - m.cpu0.self).Seconds())
}

const (
	loadGoroutineSlack = 10
	loadHandleFactor   = 2 // three orchestrators: twice the stress test's slack
)

// quietPoint runs while no writer works: the tree, then the resources.
func (m *loadMonitor) quietPoint(name string, writers []*loadWriter) {
	t := m.t
	// Every edit that must be there is, and no undone one is back.
	for _, w := range writers {
		for mk := range w.live {
			if !loadHas(m.dir, mk) {
				t.Errorf("%s: writer %d: %s lost %q", name, w.id, mk.file, mk.mark)
			}
		}
		for mk := range w.gone {
			if loadHas(m.dir, mk) {
				t.Errorf("%s: writer %d: undone %q is back in %s", name, w.id, mk.mark, mk.file)
			}
		}
	}
	// Goroutines and processes settle back to the baseline.
	deadline := time.Now().Add(30 * time.Second)
	var r resources
	for {
		r = sampleResources()
		if (r.goroutines <= m.base.goroutines+loadGoroutineSlack && r.stray <= max(m.base.stray, 0)) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r.goroutines > m.base.goroutines+loadGoroutineSlack {
		var b strings.Builder
		pprof.Lookup("goroutine").WriteTo(&b, 1)
		t.Errorf("%s: %d goroutines, baseline %d: leak?\n%s", name, r.goroutines, m.base.goroutines, b.String())
	}
	if r.stray > max(m.base.stray, 0) {
		t.Errorf("%s: %d leftover processes (baseline %d)", name, r.stray, m.base.stray)
	}
	if len(m.quiet) == 0 {
		m.warm = r
	} else if r.handles >= 0 && m.warm.handles >= 0 && r.handles > m.warm.handles+loadHandleFactor*handleSlack {
		t.Errorf("%s: %d open handles, %d at the first quiet point: leak?", name, r.handles, m.warm.handles)
	}
	m.quiet = append(m.quiet, r)

	// The pool: slots, registered worktrees, disk use; refs.
	pool := poolDir(m.dir)
	slots := countSlots(pool)
	wl, _ := (git{m.dir}).out("worktree", "list", "--porcelain")
	worktrees := strings.Count(wl, "worktree ") - 1
	refs, _ := (git{m.dir}).out("for-each-ref", "--format=%(refname)")
	var bytes int64
	filepath.WalkDir(pool, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				bytes += fi.Size()
			}
		}
		return nil
	})
	m.mu.Lock()
	m.slots, m.worktrees, m.refs, m.poolBytes = slots, worktrees, len(strings.Fields(refs)), bytes
	m.maxSlots, m.maxPoolBytes = max(m.maxSlots, slots), max(m.maxPoolBytes, bytes)
	m.refsAt = append(m.refsAt, m.refs)
	m.mu.Unlock()
	// Each writer is a rw with its own bound (threads + 1); they share slots.
	if want := loadWriters * poolSize(config.Default()); slots > want || worktrees > slots {
		t.Errorf("%s: %d pool slots, %d registered worktrees (want at most %d)\n%s", name, slots, worktrees, want, wl)
	}
	tasks := 0
	for _, w := range writers {
		tasks += w.tasks
	}
	c := loadCPU()
	line := fmt.Sprintf("%s at %s: %d tasks; goroutines %d (base %d), heap %s, handles %d, processes %d; pool %d slots (%d registered), %s; %d refs; rw CPU %s",
		name, time.Since(m.wall0).Round(time.Second), tasks, r.goroutines, m.base.goroutines, mb(r.heap), r.handles, r.stray,
		slots, worktrees, mb(uint64(bytes)), m.refs, (c.self - m.cpu0.self).Round(time.Second))
	t.Log(line)
	// Kept as it goes: a run stopped by the job's time limit still has it.
	if out := os.Getenv("RW_LOAD_OUT"); out != "" {
		if f, err := os.OpenFile(filepath.Join(out, "quiet-points.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
	m.row(name, r)
}

// report sums up the run and checks the heap trend over the quiet points.
func (m *loadMonitor) report(writers []*loadWriter, events int64) {
	t := m.t
	wall := time.Since(m.wall0)
	c := loadCPU()
	self := c.self - m.cpu0.self
	var b strings.Builder
	fmt.Fprintf(&b, "load test: %s on %s/%s, %d CPUs, %d writers\n", wall.Round(time.Second), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), len(writers))
	var tasks, ok, cancels, resumes, undos, redos int
	for _, w := range writers {
		avg := time.Duration(0)
		if w.tasks > 0 {
			avg = w.taskTime / time.Duration(w.tasks)
		}
		fmt.Fprintf(&b, "  writer %d: %d tasks (%d ok, %d cancelled, %d resumed, %d undone, %d redone), avg %s per task, %d marks in the tree\n",
			w.id, w.tasks, w.ok, w.cancels, w.resumes, w.undos, w.redos, avg.Round(10*time.Millisecond), len(w.live))
		tasks, ok, cancels, resumes, undos, redos = tasks+w.tasks, ok+w.ok, cancels+w.cancels, resumes+w.resumes, undos+w.undos, redos+w.redos
	}
	fmt.Fprintf(&b, "  total: %d tasks, %d ok, %d cancelled, %d resumed, %d undone, %d redone, %d events\n", tasks, ok, cancels, resumes, undos, redos, events)
	m.mu.Lock()
	fmt.Fprintf(&b, "  goroutines: base %d, at quiet points max %d, peak while running %d\n", m.base.goroutines, maxOf(m.quiet, func(r resources) int { return r.goroutines }), m.peakGor)
	fmt.Fprintf(&b, "  open handles: base %d, first quiet point %d, max at quiet points %d, peak %d (-1: not counted on %s)\n",
		m.base.handles, m.warm.handles, maxOf(m.quiet, func(r resources) int { return r.handles }), m.peakHandles, runtime.GOOS)
	fmt.Fprintf(&b, "  processes (agents, git): peak %d while running, %d left at quiet points\n", m.peakProcs, maxOf(m.quiet, func(r resources) int { return r.stray }))
	fmt.Fprintf(&b, "  pool: max %d slots, max %s on disk; refs at quiet points %v\n", m.maxSlots, mb(uint64(m.maxPoolBytes)), m.refsAt)
	m.mu.Unlock()
	share := self.Seconds() / (wall.Seconds() * float64(runtime.NumCPU()))
	fmt.Fprintf(&b, "  rw CPU (this process: three orchestrators, no agents): %s in %s = %.1f%% of one core, %.2f%% of the machine",
		self.Round(time.Second), wall.Round(time.Second), 100*self.Seconds()/wall.Seconds(), 100*share)
	if c.children >= 0 {
		kids := c.children - m.cpu0.children
		fmt.Fprintf(&b, "; agents and git %s, so rw is %.1f%% of the CPU the work used", kids.Round(time.Second), 100*self.Seconds()/(self+kids).Seconds())
	}
	b.WriteString("\n")
	if len(m.quiet) >= 6 {
		post := m.quiet[1:]
		third := len(post) / 3
		early, late := medianHeap(post[:third]), medianHeap(post[len(post)-third:])
		fmt.Fprintf(&b, "  live heap at quiet points: base %s, first %s, median early %s, median late %s, peak while running %s\n",
			mb(m.base.heap), mb(m.warm.heap), mb(early), mb(late), mb(m.peakHeap))
		if late > early+early/2+8<<20 {
			t.Errorf("live heap grew from %s to %s over the quiet points: leak?", mb(early), mb(late))
		}
	} else {
		fmt.Fprintf(&b, "  live heap: base %s, peak while running %s (too few quiet points for a trend)\n", mb(m.base.heap), mb(m.peakHeap))
	}
	// Refs: undo keeps 30 tasks per tree, so they level off.
	if n := len(m.refsAt); n >= 6 && m.refsAt[n-1] > m.refsAt[n/2]+40 {
		t.Errorf("refs keep growing: %v", m.refsAt)
	}
	t.Log("\n" + b.String())
	if out := os.Getenv("RW_LOAD_OUT"); out != "" {
		os.WriteFile(filepath.Join(out, "summary.txt"), []byte(b.String()), 0o644)
		if t.Failed() { // the writers' session logs, for a look at what failed
			os.CopyFS(filepath.Join(out, "sessions"), os.DirFS(m.logs))
		}
	}
}

func maxOf(rs []resources, f func(resources) int) int {
	n := 0
	for i, r := range rs {
		if i == 0 || f(r) > n {
			n = f(r)
		}
	}
	return n
}

func loadGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=rw load", "GIT_AUTHOR_EMAIL=load@localhost", "GIT_COMMITTER_NAME=rw load", "GIT_COMMITTER_EMAIL=load@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func loadCount(t *testing.T, dir string) int {
	return len(strings.Split(strings.TrimSpace(loadGit(t, dir, "ls-files")), "\n"))
}

// loadSynthRepo makes a repo of n source files in nested folders.
func loadSynthRepo(t *testing.T, dir string, n int) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	loadGit(t, dir, "init", "-q", "-b", "main")
	body := strings.Repeat("// line of a source file that agents read and edit under load\n", 40)
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, "src", fmt.Sprintf("pkg%02d", i%37), fmt.Sprintf("sub%d", i%5), fmt.Sprintf("file%04d.go", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(fmt.Sprintf("package pkg%02d\n\n%s", i%37, body)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# load test repo\n"), 0o644)
	loadGit(t, dir, "add", "-A")
	loadGit(t, dir, "commit", "-q", "-m", "load test repo")
}

// loadAddLFS commits a few Git LFS files when git-lfs is installed.
func loadAddLFS(t *testing.T, dir string) bool {
	t.Helper()
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Log("git-lfs not installed: no LFS files")
		return false
	}
	loadGit(t, dir, "lfs", "install", "--local")
	loadGit(t, dir, "lfs", "track", "rw-load-assets/*.bin")
	os.MkdirAll(filepath.Join(dir, "rw-load-assets"), 0o755)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 8; i++ {
		b := make([]byte, 512<<10)
		rng.Read(b)
		os.WriteFile(filepath.Join(dir, "rw-load-assets", fmt.Sprintf("asset%d.bin", i)), b, 0o644)
	}
	loadGit(t, dir, "add", ".gitattributes", "rw-load-assets")
	loadGit(t, dir, "commit", "-q", "-m", "rw load: LFS assets")
	return true
}

// loadEditable lists tracked text files the agents may edit: small source
// and doc files, not generated, vendored or test data.
func loadEditable(t *testing.T, dir string) []string {
	t.Helper()
	ext := map[string]bool{".go": true, ".c": true, ".h": true, ".py": true, ".js": true, ".ts": true, ".java": true, ".rs": true, ".md": true, ".txt": true, ".cs": true}
	var out []string
	for _, f := range strings.Split(strings.TrimSpace(loadGit(t, dir, "ls-files", "-z")), "\x00") {
		if !ext[strings.ToLower(filepath.Ext(f))] || strings.Contains(f, "testdata/") || strings.Contains(f, "vendor/") || strings.Contains(f, " ") {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > 200<<10 {
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	if len(out) < 60*loadWriters {
		t.Fatalf("only %d editable files in the repo", len(out))
	}
	return out
}
