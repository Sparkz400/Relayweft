package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.evs...)
}

// newOrc builds an orchestrator whose events are collected.
func newOrc(t *testing.T, dir string, set runner.Set, edit func(*config.Config)) (*Orchestrator, *recorder) {
	t.Helper()
	cfg := config.Default()
	if edit != nil {
		edit(cfg)
	}
	ch := make(chan event.Event, 64)
	rec := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range ch {
			rec.mu.Lock()
			rec.evs = append(rec.evs, e)
			rec.mu.Unlock()
		}
	}()
	t.Cleanup(func() { close(ch); <-done })
	log, err := sessionlog.Open(t.TempDir(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	o := New(Options{
		Dir: dir, Store: config.NewStore(cfg, filepath.Join(t.TempDir(), "sy.yaml")),
		Runners: func(*config.Config) runner.Set { return set }, Tracker: limits.NewTracker(),
		Log: log, Events: ch, NoGit: dir == "",
	})
	return o, rec
}

func TestDemoScenarioEndToEnd(t *testing.T) {
	o, rec := newOrc(t, "", runner.NewFakeSet(0), nil)
	res := o.Run(context.Background(), "Make the parser keep trailing empty fields and add a strict flag with tests and docs please")
	if !res.OK {
		t.Fatalf("demo task failed: %+v", res)
	}
	var rules []string
	checkpoints, limitsHit := 0, 0
	for _, e := range rec.all() {
		switch e.Kind {
		case event.Route:
			rules = append(rules, e.Decision.Rule)
		case event.Checkpoint:
			checkpoints++
		case event.ProviderState:
			limitsHit++
		}
	}
	joined := strings.Join(rules, ",")
	for _, want := range []string{router.RulePlan, router.RuleReview, router.RuleReadOnly, router.RuleRepeatError, router.RuleLimit} {
		if !strings.Contains(joined, want) {
			t.Errorf("rule %s never fired: %s", want, joined)
		}
	}
	// plan review + final review (rejected) + final review again, plus an
	// error review when the repeated error is not interrupted by the limit
	// hit (agents run in parallel, so which call hits the limit varies).
	if checkpoints < 3 {
		t.Errorf("checkpoints = %d", checkpoints)
	}
	if limitsHit != 1 {
		t.Errorf("limit events = %d", limitsHit)
	}
	if !o.Tracker().Limited(event.Codex) {
		t.Error("codex should be marked limited")
	}
}

func TestSmallTaskSkipsPlanner(t *testing.T) {
	o, rec := newOrc(t, "", runner.NewFakeSet(0), func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	res := o.Run(context.Background(), "where is the parser")
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	for _, e := range rec.all() {
		if e.Kind == event.Route && e.Decision.Rule == router.RulePlan {
			t.Fatal("planner ran for a small task")
		}
		if e.Kind == event.Route && e.Decision.Role != event.RoleExplorer {
			t.Fatalf("small read-only task routed to %s", e.Decision.Role)
		}
	}
}

// scripted is a test runner driven by a function.
type scripted struct {
	provider string
	fn       func(s runner.Spec) runner.Result
}

func (x scripted) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Model: s.Model, Kind: event.Started}.Stamp())
	r := x.fn(s)
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Kind: event.Done, OK: r.OK()}.Stamp())
	return r
}

func both(fn func(s runner.Spec) runner.Result) runner.Set {
	return runner.Set{event.Codex: scripted{event.Codex, fn}, event.Claude: scripted{event.Claude, fn}}
}

func planJSON(subtasks ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"summary": "test plan", "subtasks": subtasks})
	return "```json\n" + string(b) + "\n```"
}

func approve() runner.Result {
	return runner.Result{Final: `{"approve": true, "advice": "ok"}`}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if !SupportsMergeTree() {
		t.Skip("git < 2.38")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	// Tests compare exact bytes; CI's Windows git defaults to autocrlf=true.
	// The CRLF path has its own test that turns it on explicitly.
	run("config", "core.autocrlf", "false")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func headOf(t *testing.T, dir string) string {
	s, err := (git{dir}).out("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func read(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

const longTask = "Create files a and b in parallel then combine them into c with a summary line please"

func TestWorktreesMergeIntoWorkingTree(t *testing.T) {
	dir := gitRepo(t)
	head := headOf(t, dir)
	// The user has uncommitted work: a tracked change and an untracked file.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\nuser edit\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine\n"), 0o644)

	var mu sync.Mutex
	dirs := map[string]string{}
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(
				map[string]any{"id": "a", "title": "write a", "kind": "edit", "prompt": "write a", "files": []string{"a.txt"}},
				map[string]any{"id": "b", "title": "write b", "kind": "edit", "prompt": "write b", "files": []string{"b.txt"}},
				map[string]any{"id": "c", "title": "combine", "kind": "edit", "prompt": "combine", "files": []string{"c.txt"}, "depends_on": []string{"a", "b"}},
			)}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			if strings.Contains(s.Prompt, runner.MarkerFinalReview) && !strings.Contains(s.Prompt, "c.txt") {
				return runner.Result{Final: `{"approve": false, "advice": "diff does not show c.txt"}`}
			}
			return approve()
		}
		mu.Lock()
		dirs[s.StepID] = s.Dir
		mu.Unlock()
		// Workers must see the user's uncommitted work.
		if b, _ := os.ReadFile(filepath.Join(s.Dir, "notes.txt")); string(b) != "mine\n" {
			return runner.Result{Err: errString("worktree is missing the user's untracked file")}
		}
		switch s.StepID {
		case "a", "b":
			os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte(s.StepID+"\n"), 0o644)
		case "c":
			a, errA := os.ReadFile(filepath.Join(s.Dir, "a.txt"))
			b, errB := os.ReadFile(filepath.Join(s.Dir, "b.txt"))
			if errA != nil || errB != nil {
				return runner.Result{Err: errString("dependency results were not merged before c started")}
			}
			os.WriteFile(filepath.Join(s.Dir, "c.txt"), append(a, b...), 0o644)
		}
		return runner.Result{Final: "done " + s.StepID, Files: []string{s.StepID + ".txt"}}
	})
	o, _ := newOrc(t, dir, set, nil)
	res := o.Run(context.Background(), longTask)
	if !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if got := read(t, filepath.Join(dir, "c.txt")); got != "a\nb\n" {
		t.Errorf("c.txt = %q", got)
	}
	if read(t, filepath.Join(dir, "a.txt")) != "a\n" || read(t, filepath.Join(dir, "b.txt")) != "b\n" {
		t.Error("a/b not applied to the working tree")
	}
	if read(t, filepath.Join(dir, "README.md")) != "# test\nuser edit\n" || read(t, filepath.Join(dir, "notes.txt")) != "mine\n" {
		t.Error("user's uncommitted work was changed")
	}
	if headOf(t, dir) != head {
		t.Error("HEAD moved: switchyard must not commit on the user's branch")
	}
	slots := map[string]bool{}
	for id, d := range dirs {
		if !strings.HasPrefix(d, poolDir(dir)) {
			t.Errorf("%s ran in %s, want a pooled worktree", id, d)
		}
		slots[d] = true
	}
	// Slots stay for the next task, unlocked.
	for d := range slots {
		unlock, ok := proc.TryLock(d + ".lock")
		if !ok {
			t.Errorf("slot %s is still locked", d)
			continue
		}
		unlock()
	}
	// The planner phase prewarmed max_threads slots; the plan used some of them.
	if n, err := CleanPool(dir); err != nil || n != config.Default().Orchestrator.MaxThreads || len(slots) > n {
		t.Errorf("CleanPool = %d, %v; want max_threads, used %d", n, err, len(slots))
	}
	wl, _ := (git{dir}).out("worktree", "list")
	if n := len(strings.Split(wl, "\n")); n != 1 {
		t.Errorf("leftover worktrees after CleanPool:\n%s", wl)
	}
	if st, _ := (git{dir}).out("status", "--porcelain"); strings.Contains(st, "A ") {
		t.Errorf("index was modified:\n%s", st)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestWorktreeConflictKeepsBranch(t *testing.T) {
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(
				map[string]any{"id": "x", "title": "edit shared one way", "kind": "edit", "prompt": "change shared", "files": []string{"shared.txt"}},
				map[string]any{"id": "y", "title": "edit shared another way", "kind": "edit", "prompt": "change shared", "files": []string{"shared.txt"}},
			)}
		case strings.Contains(s.Prompt, "[SY:"+"PLAN-REVIEW]"), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return approve()
		}
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("changed by "+s.StepID+"\n"), 0o644)
		return runner.Result{Final: "changed"}
	})
	o, rec := newOrc(t, dir, set, nil)
	res := o.Run(context.Background(), longTask)
	if res.OK {
		t.Fatal("a conflict must not report success")
	}
	if len(res.Kept) != 1 || !strings.HasPrefix(res.Kept[0], "sy/") {
		t.Fatalf("kept = %v", res.Kept)
	}
	if _, err := (git{dir}).out("rev-parse", "--verify", res.Kept[0]); err != nil {
		t.Errorf("branch %s missing: %v", res.Kept[0], err)
	}
	got := read(t, filepath.Join(dir, "shared.txt"))
	if got != "changed by x\n" && got != "changed by y\n" {
		t.Errorf("shared.txt = %q (exactly one side should be applied)", got)
	}
	mergeFails := 0
	for _, e := range rec.all() {
		if e.Kind == event.Merge && !e.OK {
			mergeFails++
		}
	}
	if mergeFails != 1 {
		t.Errorf("merge failures = %d", mergeFails)
	}
}

func TestRepeatErrorEscalatesAndReviews(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	var roles []string
	set := both(func(s runner.Spec) runner.Result {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(map[string]any{"id": "w", "title": "fix bug", "kind": "edit", "prompt": "fix the bug"})}
		case strings.Contains(s.Prompt, runner.MarkerErrorReview):
			return runner.Result{Final: `{"approve": false, "advice": "use the other index"}`}
		case strings.Contains(s.Prompt, "[SY:"):
			if strings.Contains(s.Prompt, runner.MarkerStep) {
				break
			}
			return approve()
		}
		calls[s.StepID]++
		roles = append(roles, s.Role)
		if calls[s.StepID] <= 2 {
			return runner.Result{Err: errString("TestX failed at line 1" + strings.Repeat("0", calls[s.StepID]))}
		}
		if !strings.Contains(s.Prompt, "use the other index") {
			return runner.Result{Err: errString("advice not passed on")}
		}
		return runner.Result{Final: "fixed"}
	})
	o, _ := newOrc(t, "", set, nil)
	res := o.Run(context.Background(), longTask)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if strings.Join(roles, ",") != "worker,worker,worker_high" {
		t.Errorf("roles = %v", roles)
	}
}

func TestBothProvidersLimited(t *testing.T) {
	set := both(func(s runner.Spec) runner.Result {
		return runner.Result{Err: errString("usage limit"), LimitHit: true}
	})
	o, _ := newOrc(t, "", set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	done := make(chan TaskResult)
	go func() { done <- o.Run(context.Background(), "fix it") }()
	select {
	case res := <-done:
		if res.OK {
			t.Fatal("want failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("orchestrator looped forever with both providers limited")
	}
}

func TestKillAgent(t *testing.T) {
	started := make(chan struct{}, 1)
	set := both(func(s runner.Spec) runner.Result { return runner.Result{} })
	slow := scriptedCtx{started: started}
	set[event.Codex], set[event.Claude] = slow, slow
	o, _ := newOrc(t, "", set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	done := make(chan TaskResult)
	go func() { done <- o.Run(context.Background(), "fix it") }()
	<-started
	if !o.Kill("work") {
		t.Fatal("kill returned false")
	}
	select {
	case res := <-done:
		if res.OK {
			t.Fatal("killed task reported OK")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kill did not finish the step")
	}
}

type scriptedCtx struct{ started chan struct{} }

func (s scriptedCtx) Run(ctx context.Context, sp runner.Spec, emit func(event.Event)) runner.Result {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return runner.Result{Err: ctx.Err(), Killed: true}
}

func TestPauseHoldsDispatch(t *testing.T) {
	var mu sync.Mutex
	n := 0
	set := both(func(s runner.Spec) runner.Result {
		mu.Lock()
		n++
		mu.Unlock()
		return runner.Result{Final: "ok"}
	})
	o, _ := newOrc(t, "", set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	o.SetPaused(true)
	done := make(chan TaskResult)
	go func() { done <- o.Run(context.Background(), "fix it") }()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if n != 0 {
		t.Fatalf("%d agents ran while paused", n)
	}
	mu.Unlock()
	o.SetPaused(false)
	if res := <-done; !res.OK {
		t.Fatalf("%+v", res)
	}
}

func TestParsePlan(t *testing.T) {
	p, err := ParsePlan("Here you go:\n```json\n{\"summary\":\"s\",\"subtasks\":[{\"id\":\"A b\",\"kind\":\"Explorer\",\"prompt\":\"find x\"},{\"id\":\"a-b\",\"kind\":\"weird\",\"prompt\":\"do y\",\"depends_on\":[\"A b\",\"ghost\"]}]}\n```\nthanks")
	if err != nil {
		t.Fatal(err)
	}
	if p.Subtasks[0].ID != "a-b" || p.Subtasks[0].Kind != router.KindExplore {
		t.Errorf("first = %+v", p.Subtasks[0])
	}
	if p.Subtasks[1].ID != "t2" || p.Subtasks[1].Kind != router.KindEdit {
		t.Errorf("duplicate id / unknown kind not normalized: %+v", p.Subtasks[1])
	}
	if strings.Join(p.Subtasks[1].DependsOn, ",") != "a-b" {
		t.Errorf("deps = %v", p.Subtasks[1].DependsOn)
	}
	// Unfenced JSON and cycles.
	p, err = ParsePlan(`{"subtasks":[{"id":"x","prompt":"1","depends_on":["y"]},{"id":"y","prompt":"2","depends_on":["x"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if hasCycle(p.Subtasks) {
		t.Error("cycle not broken")
	}
	if _, err := ParsePlan("no json here"); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := ParsePlan(`{"subtasks": []}`); err == nil {
		t.Error("empty plan accepted")
	}
}

func TestParseVerdict(t *testing.T) {
	v := ParseVerdict("```json\n{\"approve\": false, \"advice\": \"fix x\", \"issues\": [\"a\"]}\n```")
	if v.Approve || v.Advice != "fix x" || len(v.Issues) != 1 {
		t.Errorf("%+v", v)
	}
	v = ParseVerdict("Looks fine to me.")
	if !v.Approve || v.Advice != "Looks fine to me." {
		t.Errorf("chatty reviewer should approve: %+v", v)
	}
}

func TestErrorSignature(t *testing.T) {
	if errorSignature("TestX failed at line 12 after 3.2s") != errorSignature("TestX failed at line 14 after 1.0s") {
		t.Error("numbers should not change the signature")
	}
	if errorSignature("TestX failed") == errorSignature("TestY failed") {
		t.Error("different errors share a signature")
	}
}

func TestLoggedOutProviderIsRoutedAround(t *testing.T) {
	var mu sync.Mutex
	var used []string
	mk := func(p string) runner.Runner {
		return scripted{p, func(s runner.Spec) runner.Result {
			mu.Lock()
			used = append(used, p)
			mu.Unlock()
			if p == event.Codex {
				return runner.Result{Err: errString("codex exited: Not logged in. Run codex login")}
			}
			return runner.Result{Final: "ok"}
		}}
	}
	set := runner.Set{event.Codex: mk(event.Codex), event.Claude: mk(event.Claude)}
	o, _ := newOrc(t, "", set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	res := o.Run(context.Background(), "fix it")
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if strings.Join(used, ",") != "codex,claude" {
		t.Errorf("used = %v (codex should be tried once, then avoided)", used)
	}
}

func TestParsePlanUniqueAndReservedIDs(t *testing.T) {
	p, err := ParsePlan(`{"subtasks":[{"id":"t2","prompt":"a"},{"id":"t2","prompt":"b"},{"id":"main","prompt":"c"},{"id":"reviewer","prompt":"d"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, st := range p.Subtasks {
		if seen[st.ID] || st.ID == AgentMain || st.ID == AgentReviewer || st.ID == AgentJudge {
			t.Fatalf("bad id %q in %+v", st.ID, p.Subtasks)
		}
		seen[st.ID] = true
	}
}

func TestCommitAllIgnoresSigningConfig(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	g.out("config", "commit.gpgsign", "true")
	g.out("config", "gpg.program", "definitely-not-gpg")
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644)
	if _, changed, err := g.commitAll("test"); err != nil || !changed {
		t.Fatalf("commit with gpgsign=true: changed=%v err=%v", changed, err)
	}
}
