package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/router"
)

// isolateState points task state (resume/history) at a temp dir and stubs
// desktop notifications.
func isolateState(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	for _, k := range []string{"XDG_CONFIG_HOME", "APPDATA", "HOME"} {
		t.Setenv(k, d)
	}
	old := sendNotify
	sendNotify = func(string, string) error { return nil }
	t.Cleanup(func() { sendNotify = old })
}

// nextApproval waits for the bridge's next request and hands it to m.
func nextApproval(t *testing.T, m *Model, ap *Approver) {
	t.Helper()
	got := make(chan tea.Msg, 1)
	go func() { got <- waitApproval(ap)() }()
	select {
	case msg := <-got:
		m.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("no approval request arrived")
	}
	if m.overlay == nil {
		t.Fatal("approval did not open an overlay")
	}
}

func testPlan() orchestrator.Plan {
	return orchestrator.Plan{Summary: "three steps", Subtasks: []orchestrator.Subtask{
		{ID: "a", Title: "look around", Kind: router.KindExplore, Prompt: "find the parser"},
		{ID: "b", Title: "change it", Kind: router.KindEdit, Prompt: "edit the parser", DependsOn: []string{"a"}},
		{ID: "c", Title: "docs", Kind: router.KindEdit, Prompt: "update docs"},
	}}
}

type planAnswer struct {
	p  orchestrator.Plan
	ok bool
}

func askPlan(ap *Approver, ctx context.Context) chan planAnswer {
	out := make(chan planAnswer, 1)
	go func() {
		p, ok := ap.ApprovePlan(ctx, "fix the parser", testPlan())
		out <- planAnswer{p, ok}
	}()
	return out
}

func waitPlan(t *testing.T, ch chan planAnswer) planAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("ApprovePlan did not return")
	}
	return planAnswer{}
}

func TestPlanApprovalEditDeleteMoveApprove(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	res := askPlan(ap, context.Background())
	nextApproval(t, m, ap)
	checkView(t, m, 80, 24)
	if v := checkView(t, m, 160, 45); !strings.Contains(v, "APPROVE THE PLAN") || !strings.Contains(v, "after a") {
		t.Fatalf("plan overlay not rendered:\n%s", v)
	}
	m.Update(key("d")) // delete a: b no longer waits for it
	m.Update(key("k")) // b: edit -> explore
	m.Update(key("r")) // b: auto -> planner
	m.Update(key("r")) // -> worker
	m.Update(key("e"))
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("rewrite the parser")})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m.Update(key("J")) // b below c
	m.Update(key("enter"))
	a := waitPlan(t, res)
	if !a.ok || len(a.p.Subtasks) != 2 {
		t.Fatalf("answer = %+v", a)
	}
	c, b := a.p.Subtasks[0], a.p.Subtasks[1]
	if c.ID != "c" || b.ID != "b" {
		t.Fatalf("order = %s, %s", c.ID, b.ID)
	}
	if b.Kind != router.KindExplore || b.Role != event.RoleWorker || b.Prompt != "rewrite the parser" || len(b.DependsOn) != 0 {
		t.Errorf("b = %+v", b)
	}
	if m.overlay != nil || len(m.approvals) != 0 {
		t.Error("overlay still open after approving")
	}
}

func TestPlanApprovalEmptyPlanAndCancel(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, true, ap)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	res := askPlan(ap, context.Background())
	nextApproval(t, m, ap)
	for i := 0; i < 3; i++ {
		m.Update(key("d"))
	}
	m.Update(key("enter"))
	p := m.overlay.(*planOverlay)
	if p.err == "" || !strings.Contains(m.View(), "no subtasks") {
		t.Fatalf("empty plan accepted (err %q)", p.err)
	}
	m.Update(key("e")) // nothing to edit: must not panic
	m.Update(key("esc"))
	if a := waitPlan(t, res); a.ok {
		t.Fatal("esc must cancel the task")
	}
}

func TestApproverUnblocksOnCancelAndClose(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	ctx, cancel := context.WithCancel(context.Background())
	res := askPlan(ap, ctx)
	nextApproval(t, m, ap)
	cancel()
	if a := waitPlan(t, res); a.ok {
		t.Fatal("cancelled approval returned ok")
	}
	m.Update(tickMsg(time.Now()))
	if m.overlay != nil || len(m.approvals) != 0 {
		t.Error("stale approval still on screen after the task was cancelled")
	}
	// Nobody listening: cancel still unblocks.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if d := ap.ReviewChanges(ctx2, orchestrator.ChangeSet{}); len(d.Apply) != 0 {
		t.Error("timed-out review applied something")
	}
	res = askPlan(ap, context.Background())
	m.Shutdown() // closes the approver
	if a := waitPlan(t, res); a.ok {
		t.Fatal("approval after shutdown returned ok")
	}
}

// TestApprovalInRealTask runs the demo pipeline with approvals on: the
// orchestrator waits for the TUI and runs the edited plan.
func TestApprovalInRealTask(t *testing.T) {
	ap := NewApprover()
	m, orc, ch := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	done := make(chan orchestrator.TaskResult, 1)
	go func() {
		done <- orc.Run(context.Background(), "Make the parser keep trailing empty fields and add a strict flag with tests and docs please")
	}()
	nextApproval(t, m, ap)
	p := m.overlay.(*planOverlay)
	gone := p.plan.Subtasks[0].ID
	m.Update(key("d"))
	m.Update(key("enter"))
	res := <-done
	drain(m, ch)
	if !strings.Contains(strings.Join(logTexts(m), "\n"), "plan approved") {
		t.Errorf("log does not show the approval; result %+v", res)
	}
	for _, id := range m.order {
		if id == gone {
			t.Errorf("agents %v: the deleted subtask %s still ran", m.order, gone)
		}
	}
}

func logTexts(m *Model) []string {
	var out []string
	for _, l := range m.logs {
		out = append(out, l.text)
	}
	return out
}

func testChanges() orchestrator.ChangeSet {
	return orchestrator.ChangeSet{StepID: "edit", Title: "change the parser", Summary: "done", Round: 2, Files: []orchestrator.FileChange{
		{Path: "parser.go", Status: "M", Added: 3, Deleted: 1, Patch: "diff --git a/parser.go b/parser.go\n--- a/parser.go\n+++ b/parser.go\n@@ -1,2 +1,4 @@\n-old\n+new\n+more\n+lines\n ctx"},
		{Path: "parser_test.go", Status: "A", Added: 10, Patch: "+test"},
		{Path: "logo.png", Status: "D", Binary: true},
	}}
}

func askReview(ap *Approver) chan orchestrator.ChangeDecision {
	out := make(chan orchestrator.ChangeDecision, 1)
	go func() { out <- ap.ReviewChanges(context.Background(), testChanges()) }()
	return out
}

func waitDecision(t *testing.T, ch chan orchestrator.ChangeDecision) orchestrator.ChangeDecision {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("ReviewChanges did not return")
	}
	return orchestrator.ChangeDecision{}
}

func TestReviewOverlayDecisions(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// Deselect the first file, apply the rest.
	res := askReview(ap)
	nextApproval(t, m, ap)
	checkView(t, m, 80, 24)
	v := checkView(t, m, 160, 45)
	for _, want := range []string{"REVIEW CHANGES", "round 2", "parser_test.go", "+more", "binary"} {
		if !strings.Contains(v, want) {
			t.Errorf("review view lacks %q", want)
		}
	}
	m.Update(key(" "))
	m.Update(key("j"))
	m.Update(key("enter"))
	d := waitDecision(t, res)
	if strings.Join(d.Apply, ",") != "parser_test.go,logo.png" || d.Feedback != "" {
		t.Fatalf("decision = %+v", d)
	}

	// None selected: enter asks once more, then rejects.
	res = askReview(ap)
	nextApproval(t, m, ap)
	m.Update(key("n"))
	m.Update(key("enter"))
	select {
	case d := <-res:
		t.Fatalf("rejected without confirmation: %+v", d)
	case <-time.After(20 * time.Millisecond):
	}
	m.Update(key("enter"))
	if d := waitDecision(t, res); len(d.Apply) != 0 {
		t.Fatalf("decision = %+v", d)
	}

	// Feedback.
	res = askReview(ap)
	nextApproval(t, m, ap)
	m.Update(key("f"))
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("handle the empty case")})
	m.Update(key("enter"))
	if d := waitDecision(t, res); d.Feedback != "handle the empty case" || len(d.Apply) != 3 {
		t.Fatalf("decision = %+v", d)
	}

	// esc rejects all.
	res = askReview(ap)
	nextApproval(t, m, ap)
	m.Update(key("esc"))
	if d := waitDecision(t, res); len(d.Apply) != 0 || d.Feedback != "" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestParallelReviewsAreShownOneAtATime(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	r1, r2 := askReview(ap), askReview(ap)
	nextApproval(t, m, ap)
	nextApproval(t, m, ap)
	if len(m.approvals) != 2 || !strings.Contains(m.View(), "1 more waiting") {
		t.Fatalf("approvals = %d", len(m.approvals))
	}
	m.Update(key("enter"))
	m.Update(key("esc"))
	a, b := waitDecision(t, r1), waitDecision(t, r2)
	if len(a.Apply)+len(b.Apply) != 3 {
		t.Errorf("decisions %+v %+v", a, b)
	}
}

func TestQueueRunsTasksOneAfterAnother(t *testing.T) {
	m, _, ch := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.running = true // pretend a task runs
	m.cancelTask = func() {}
	m.submit("first queued")
	m.submit("second queued")
	m.submit("third queued")
	if len(m.queue) != 3 || !m.queue[0].unattended {
		t.Fatalf("queue = %+v", m.queue)
	}
	if v := checkView(t, m, 120, 40); !strings.Contains(v, "queued (3)") {
		t.Error("status bar does not show the queue")
	}
	m.command("/queue rm 2")
	m.command("/queue")
	if len(m.queue) != 2 || m.queue[1].text != "third queued" {
		t.Fatalf("queue after rm = %+v", m.queue)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	if len(m.queue) != 2 || !strings.Contains(m.notice, "queued task(s) still run") {
		t.Errorf("cancel touched the queue or did not mention it: %q", m.notice)
	}
	// The running task ends: the next queued one starts.
	m.handleEvent(event.Event{Kind: event.TaskDone, Text: "cancelled"}.Stamp())
	if !m.running || m.taskText != "first queued" || len(m.queue) != 1 {
		t.Fatalf("running=%v task=%q queue=%d", m.running, m.taskText, len(m.queue))
	}
	<-m.taskDone
	drain(m, ch) // its TaskDone starts the last one
	if m.taskText != "third queued" || len(m.queue) != 0 {
		t.Fatalf("task=%q queue=%d", m.taskText, len(m.queue))
	}
	m.command("/queue clear")
	m.Shutdown()
}

func TestVerifyApproveReviewCommands(t *testing.T) {
	m, orc, _ := newModel(t, false)
	m.command("/verify go test ./...")
	m.command("/verify   go vet   ./...")
	cfg := orc.Store().Get()
	if got := strings.Join(cfg.Verify.Commands, "|"); got != "go test ./...|go vet   ./..." {
		t.Fatalf("verify = %q", got)
	}
	m.command("/verify")
	if !strings.Contains(m.logs[len(m.logs)-1].text, "go vet") {
		t.Error("/verify does not list the commands")
	}
	m.command("/verify clear")
	if n := len(orc.Store().Get().Verify.Commands); n != 0 {
		t.Errorf("verify not cleared: %d", n)
	}
	m.command("/approve off")
	m.command("/review-changes on")
	m.command("/review off")
	oc := orc.Store().Get().Orchestrator
	if oc.ApprovePlan || !oc.ReviewChanges || oc.ReviewBeforeDone {
		t.Errorf("orchestrator cfg = approve %v review %v reviewer %v", oc.ApprovePlan, oc.ReviewChanges, oc.ReviewBeforeDone)
	}
	if !m.dirty || !strings.Contains(m.logs[len(m.logs)-1].text, "/save") {
		t.Error("changes must be marked unsaved")
	}
	m.command("/help")
	help := strings.Join(logTexts(m), "\n")
	for _, c := range []string{"/approve", "/review-changes", "/verify", "/queue", "/resume", "/history", "/agents", "@<agent>"} {
		if !strings.Contains(help, c) {
			t.Errorf("/help lacks %s", c)
		}
	}
}

func TestParseFollowUp(t *testing.T) {
	cases := []struct {
		in, agent, msg string
		ok             bool
	}{
		{"@edit also handle empty input", "edit", "also handle empty input", true},
		{"@ add tests", "", "add tests", true},
		{"@last add tests", "", "add tests", true},
		{"@explore\nline two", "explore", "line two", true},
		{"@edit", "edit", "", true},
		{"@", "", "", true},
		{"fix @ the parser", "", "", false},
	}
	for _, c := range cases {
		a, msg, ok := parseFollowUp(c.in)
		if a != c.agent || msg != c.msg || ok != c.ok {
			t.Errorf("%q: got (%q, %q, %v)", c.in, a, msg, ok)
		}
	}
	m, _, _ := newModel(t, false)
	m.submit("@nobody do it")
	if m.running || !strings.Contains(m.notice, "no finished agent") {
		t.Errorf("unknown agent: running=%v notice=%q", m.running, m.notice)
	}
	m.submit("@edit")
	if !strings.Contains(m.notice, "usage") {
		t.Errorf("empty follow-up: %q", m.notice)
	}
	m.command("/agents")
	if !strings.Contains(m.logs[len(m.logs)-1].text, "no finished agents") {
		t.Error("/agents with no sessions")
	}
}

func TestResumeAndHistoryWithoutTasks(t *testing.T) {
	m, _, _ := newModel(t, false)
	m.command("/history")
	m.command("/resume")
	if !strings.Contains(m.logs[len(m.logs)-1].text, "nothing to resume") {
		t.Errorf("got %q", m.logs[len(m.logs)-1].text)
	}
	m.command("/resume nope")
	if !strings.Contains(m.logs[len(m.logs)-1].text, "no task") {
		t.Errorf("got %q", m.logs[len(m.logs)-1].text)
	}
	m.orc.SetPaused(true)
	m.command("/resume")
	if m.orc.Paused() {
		t.Error("/resume while paused must unpause")
	}
}

func TestHistoryListsFinishedTask(t *testing.T) {
	m, orc, ch := newModel(t, false)
	orc.Run(context.Background(), "Make the parser keep trailing empty fields and add a strict flag with tests and docs please")
	drain(m, ch)
	m.opt.Dir = "" // the orchestrator's temp dir differs from the model's label
	m.command("/history")
	if !strings.Contains(strings.Join(logTexts(m), "\n"), "trailing empty fields") {
		t.Error("/history does not list the task")
	}
}

func TestNotifications(t *testing.T) {
	m, orc, _ := newModel(t, false)
	var mu sync.Mutex
	var got []string
	sent := make(chan struct{}, 8)
	sendNotify = func(title, body string) error {
		mu.Lock()
		got = append(got, title+": "+body)
		mu.Unlock()
		sent <- struct{}{}
		return nil
	}
	orc.Store().Update(func(c *config.Config) error {
		c.Notify.Enabled = true
		c.Notify.MinTask = config.Duration(time.Minute)
		return nil
	})
	m.running = true
	m.taskStart = time.Now().Add(-2 * time.Minute)
	m.handleEvent(event.Event{Kind: event.TaskDone, OK: true, Text: "all good"}.Stamp())
	m.running = true
	m.taskStart = time.Now() // short task: no notification
	m.handleEvent(event.Event{Kind: event.TaskDone, OK: false, Text: "short"}.Stamp())
	m.handleEvent(event.Event{Kind: event.ProviderState, Provider: event.Codex, Until: time.Now().Add(time.Hour), Text: "codex limit"}.Stamp())
	for i := 0; i < 2; i++ {
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatal("notification not sent")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "Switchyard: done: all good") || !strings.Contains(all, "limit") || strings.Contains(all, "short") {
		t.Errorf("notifications = %q", all)
	}
}

func TestAgentTabCompletion(t *testing.T) {
	m, _, _ := newModel(t, false)
	m.input.SetValue("@ed")
	if !m.completeAgent() || !strings.Contains(m.notice, "no finished agent") {
		t.Errorf("completion without sessions: %q", m.notice)
	}
	m.input.SetValue("plain task")
	if m.completeAgent() {
		t.Error("completion outside @")
	}
}
