package tui

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/runner"
)

func TestPlanDependencyPicker(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	res := askPlan(ap, context.Background())
	nextApproval(t, m, ap)
	p := m.overlay.(*planOverlay)

	// c runs after b.
	m.Update(key("down"))
	m.Update(key("down"))
	m.Update(key("x"))
	if !p.deps || strings.Join(p.depIDs, ",") != "a,b" {
		t.Fatalf("picker for c: open=%v ids=%v", p.deps, p.depIDs)
	}
	checkView(t, m, 80, 24)
	if v := checkView(t, m, 160, 45); !strings.Contains(v, "c runs after") || !strings.Contains(v, "[ ]") {
		t.Fatalf("picker not rendered:\n%s", v)
	}
	if !strings.Contains(m.overlay.keys(), "space toggle") {
		t.Errorf("keys = %q", m.overlay.keys())
	}
	m.Update(key("down"))
	m.Update(key(" "))
	m.Update(key("enter"))
	if p.deps || strings.Join(p.plan.Subtasks[2].DependsOn, ",") != "b" {
		t.Fatalf("c deps = %v (picker open %v, err %q)", p.plan.Subtasks[2].DependsOn, p.deps, p.err)
	}
	if v := checkView(t, m, 160, 45); !strings.Contains(v, "after b") {
		t.Error("list does not show the new dependency")
	}

	// a after c would close the loop a -> c -> b -> a.
	m.Update(key("up"))
	m.Update(key("up"))
	m.Update(key("x"))
	m.Update(key("down")) // c
	m.Update(key(" "))
	m.Update(key("enter"))
	if !p.deps || !strings.Contains(p.err, "cycle") || len(p.plan.Subtasks[0].DependsOn) != 0 {
		t.Fatalf("cycle accepted: open=%v err=%q deps=%v", p.deps, p.err, p.plan.Subtasks[0].DependsOn)
	}
	if v := checkView(t, m, 160, 45); !strings.Contains(v, "a after c after b after a") {
		t.Errorf("cycle not explained:\n%s", v)
	}
	m.Update(key("esc")) // cancel the picker, not the task
	if p.deps || len(p.plan.Subtasks[0].DependsOn) != 0 || m.overlay == nil {
		t.Fatal("esc in the picker must only close the picker")
	}

	// Removing a dependency works too: b no longer waits for a.
	m.Update(key("down"))
	m.Update(key("x"))
	m.Update(key(" ")) // a
	m.Update(key("enter"))
	m.Update(key("enter"))
	a := waitPlan(t, res)
	if !a.ok {
		t.Fatal("plan not approved")
	}
	got := map[string]string{}
	for _, st := range a.p.Subtasks {
		got[st.ID] = strings.Join(st.DependsOn, ",")
	}
	if got["a"] != "" || got["b"] != "" || got["c"] != "b" {
		t.Fatalf("deps = %v", got)
	}
}

func TestDepCycle(t *testing.T) {
	sts := testPlan().Subtasks // b after a
	if c := depCycle(sts, "a", []string{"b"}); strings.Join(c, " ") != "a b a" {
		t.Errorf("cycle = %v", c)
	}
	if c := depCycle(sts, "c", []string{"a", "b"}); c != nil {
		t.Errorf("no cycle expected: %v", c)
	}
	if c := depCycle(sts, "c", []string{"c"}); c == nil {
		t.Error("a step waiting for itself is a cycle")
	}
}

func hunkChanges() orchestrator.ChangeSet {
	patch := "diff --git a/multi.go b/multi.go\nindex 1..2 100644\n--- a/multi.go\n+++ b/multi.go\n" +
		"@@ -1,3 +1,3 @@\n a\n-one\n+ONE\n b\n" +
		"@@ -10,3 +10,3 @@\n c\n-two\n+TWO\n d\n" +
		"@@ -20,3 +20,3 @@\n e\n-three\n+THREE\n f\n"
	return orchestrator.ChangeSet{StepID: "edit", Title: "three hunks", Files: []orchestrator.FileChange{
		{Path: "multi.go", Status: "M", Added: 3, Deleted: 3, Patch: patch},
		{Path: "single.go", Status: "M", Added: 1, Deleted: 1, Patch: "diff --git a/single.go b/single.go\n--- a/single.go\n+++ b/single.go\n@@ -1 +1 @@\n-x\n+y\n"},
		{Path: "new.go", Status: "A", Added: 1, Patch: "+new"},
	}}
}

func askHunkReview(ap *Approver) chan orchestrator.ChangeDecision {
	out := make(chan orchestrator.ChangeDecision, 1)
	go func() { out <- ap.ReviewChanges(context.Background(), hunkChanges()) }()
	return out
}

func TestReviewHunkSelection(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	res := askHunkReview(ap)
	nextApproval(t, m, ap)
	v := m.overlay.(*reviewOverlay)
	if s := checkView(t, m, 160, 45); !strings.Contains(s, "0/3 hunks") && !strings.Contains(s, "3/3 hunks") {
		t.Fatalf("hunk count not shown:\n%s", s)
	}

	// Into the hunks of multi.go, turn the middle one off.
	m.Update(key("tab"))
	if !v.inDiff {
		t.Fatal("tab did not focus the hunks")
	}
	m.Update(key("down"))
	m.Update(key(" "))
	if v.hunk != 1 || v.hunks[0][1] || !v.partial(0) {
		t.Fatalf("hunk=%d hunks=%v", v.hunk, v.hunks[0])
	}
	if !strings.Contains(m.overlay.keys(), "toggle hunk") {
		t.Errorf("keys = %q", m.overlay.keys())
	}
	checkView(t, m, 80, 24)
	s := checkView(t, m, 160, 45)
	for _, want := range []string{"[~]", "2/3 hunks", "hunk 2 of 3", "(1 in part)"} {
		if !strings.Contains(s, want) {
			t.Errorf("view lacks %q:\n%s", want, s)
		}
	}
	// Back to the files: a one-hunk file and a new file can't be split.
	m.Update(key("esc"))
	if v.inDiff || m.overlay == nil {
		t.Fatal("esc in the hunks must go back to the file list")
	}
	m.Update(key("down"))
	m.Update(key("tab"))
	if v.inDiff || !strings.Contains(checkView(t, m, 160, 45), "only be taken whole (only one hunk)") {
		t.Error("one-hunk file entered hunk mode")
	}
	m.Update(key("down"))
	m.Update(key("]"))
	if v.inDiff || !strings.Contains(v.note, "new file") {
		t.Errorf("new file: inDiff=%v note=%q", v.inDiff, v.note)
	}
	m.Update(key(" ")) // drop new.go entirely
	m.Update(key("enter"))
	d := waitDecision(t, res)
	if strings.Join(d.Apply, ",") != "multi.go,single.go" {
		t.Fatalf("apply = %v", d.Apply)
	}
	if len(d.Hunks) != 1 || len(d.Hunks["multi.go"]) != 2 || d.Hunks["multi.go"][0] != 0 || d.Hunks["multi.go"][1] != 2 {
		t.Fatalf("hunks = %v", d.Hunks)
	}
	if !strings.Contains(m.notice, "1 of them in part") {
		t.Errorf("notice = %q", m.notice)
	}

	// Every hunk off leaves the file out; space on a partial file selects
	// it whole again.
	res = askHunkReview(ap)
	nextApproval(t, m, ap)
	v = m.overlay.(*reviewOverlay)
	m.Update(key("]"))
	for i := 0; i < 3; i++ {
		m.Update(key(" "))
		m.Update(key("]"))
	}
	if v.include[0] || v.partial(0) {
		t.Fatalf("all hunks off: include=%v", v.include[0])
	}
	if s := checkView(t, m, 120, 40); !strings.Contains(s, "0/3 hunks") {
		t.Error("count not updated")
	}
	m.Update(key("["))
	m.Update(key(" ")) // hunk 3 back on: partial
	m.Update(key("tab"))
	if !v.partial(0) {
		t.Fatal("expected a partial file")
	}
	m.Update(key(" ")) // whole file again
	if v.partial(0) || !v.include[0] {
		t.Fatal("space on a partial file must select it whole")
	}
	m.Update(key("enter"))
	if d := waitDecision(t, res); len(d.Apply) != 3 || d.Hunks != nil {
		t.Fatalf("decision = %+v", d)
	}
}

// gate is a runner whose first worker run blocks until released; every
// run is recorded.
type gate struct {
	mu      sync.Mutex
	specs   []runner.Spec
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gate) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	emit(event.Event{AgentID: s.AgentID, Provider: s.Provider, Model: s.Model, Kind: event.Started}.Stamp())
	g.mu.Lock()
	g.specs = append(g.specs, s)
	g.mu.Unlock()
	if s.AgentID == "work" {
		first := false
		g.once.Do(func() { first = true })
		if first {
			close(g.started)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		}
	}
	emit(event.Event{AgentID: s.AgentID, Provider: s.Provider, Kind: event.Done, OK: true}.Stamp())
	return runner.Result{Final: "ok", SessionID: "sess-" + s.AgentID}
}

// newModelRunner builds a model whose orchestrator runs r for every
// provider in dir (sessions persist per dir, as in real use).
func newModelRunner(t *testing.T, dir string, r runner.Runner) (*Model, *orchestrator.Orchestrator, chan event.Event) {
	t.Helper()
	ch := make(chan event.Event, 4096)
	store := config.NewStore(config.Default(), filepath.Join(t.TempDir(), "relayweft.yaml"))
	set := runner.Set{event.Codex: r, event.Claude: r}
	orc := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: store, Runners: func(*config.Config) runner.Set { return set },
		Tracker: limits.NewTracker(), Events: ch, NoGit: true,
	})
	m := New(Options{Orc: orc, Events: ch, Dir: dir, Theme: NewTheme("unicode"), Version: "test"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m, orc, ch
}

func TestTellRunningAgentFromPrompt(t *testing.T) {
	isolateState(t)
	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	m, orc, _ := newModelRunner(t, t.TempDir(), g)
	done := make(chan orchestrator.TaskResult, 1)
	go func() { done <- orc.Run(context.Background(), "add a test") }()
	select {
	case <-g.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never started")
	}

	// Tab completes the running agent; /agents lists it.
	m.input.SetValue("@wo")
	if !m.completeAgent() || m.input.Value() != "@work " {
		t.Fatalf("completion = %q (%q)", m.input.Value(), m.notice)
	}
	m.command("/agents")
	if all := strings.Join(logTexts(m), "\n"); !strings.Contains(all, "running now") || !strings.Contains(all, "work") {
		t.Errorf("/agents does not list the running agent:\n%s", all)
	}

	m.running = true // as if the TUI had started the task
	m.submit("@work use table tests")
	if len(m.queue) != 0 || !strings.Contains(m.notice, "will be delivered when work's turn ends") {
		t.Fatalf("queue=%d notice=%q", len(m.queue), m.notice)
	}
	close(g.release)
	select {
	case res := <-done:
		if !res.OK {
			t.Fatalf("task: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("task did not finish")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delivered := false
	for _, s := range g.specs {
		if s.AgentID == "work" && s.Resume == "sess-work" && strings.Contains(s.Prompt, "use table tests") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("message not delivered to the running agent: %d runs", len(g.specs))
	}
}

func TestAgentsListSurvivesRestart(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	close(g.release)
	_, orc, _ := newModelRunner(t, dir, g)
	if res := orc.Run(context.Background(), "add a test"); !res.OK {
		t.Fatalf("%+v", res)
	}
	// Age the saved sessions by two days, as if rw ran the day before
	// yesterday.
	var files []string
	filepath.WalkDir(os.Getenv("HOME"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(filepath.Dir(p)) == "sessions" && strings.HasSuffix(p, ".json") {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 {
		t.Fatalf("session files = %v", files)
	}
	data, _ := os.ReadFile(files[0])
	var ss map[string]orchestrator.AgentSession
	if err := json.Unmarshal(data, &ss); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 2, 9, 30, 0, 0, time.Local)
	for k, s := range ss {
		s.Ended = old
		ss[k] = s
	}
	data, _ = json.Marshal(ss)
	os.WriteFile(files[0], data, 0o644)

	// A new rw in the same folder.
	m2, _, _ := newModelRunner(t, dir, g)
	m2.command("/agents")
	all := strings.Join(logTexts(m2), "\n")
	for _, want := range []string{"work", "task: add a test", old.Format("Jan 2")} {
		if !strings.Contains(all, want) {
			t.Errorf("/agents lacks %q:\n%s", want, all)
		}
	}
	m2.input.SetValue("@wo")
	if !m2.completeAgent() || m2.input.Value() != "@work " {
		t.Errorf("completion after restart = %q", m2.input.Value())
	}
}

func TestWhen(t *testing.T) {
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.Local)
	cases := map[time.Time]string{
		time.Date(2026, 10, 3, 9, 5, 0, 0, time.Local):  "09:05",
		time.Date(2026, 10, 2, 23, 1, 0, 0, time.Local): "Oct 2 23:01",
		time.Date(2025, 12, 31, 8, 0, 0, 0, time.Local): "Dec 31 2025 08:00",
		{}: "?",
	}
	for in, want := range cases {
		if got := when(in, now); got != want {
			t.Errorf("when(%v) = %q, want %q", in, got, want)
		}
	}
	if firstLine("fix it\n\nFollow-up: more") != "fix it" {
		t.Error("firstLine")
	}
}

func TestHelpMentionsNewKeys(t *testing.T) {
	m, _, _ := newModel(t, false)
	m.command("/help")
	help := strings.Join(logTexts(m), "\n")
	for _, want := range []string{"x dependencies", "hunks", "[~]", "turn ends", "kept across restarts", "truncated"} {
		if !strings.Contains(help, want) {
			t.Errorf("/help lacks %q", want)
		}
	}
	p := &planOverlay{}
	if !strings.Contains(p.keys(), "x deps") {
		t.Errorf("plan keys = %q", p.keys())
	}
	v := &reviewOverlay{}
	if !strings.Contains(v.keys(), "tab hunks") {
		t.Errorf("review keys = %q", v.keys())
	}
}
