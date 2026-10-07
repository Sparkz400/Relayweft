package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/notify"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
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

// b turns best of N on or off for a writing step; a read-only step refuses.
func TestPlanApprovalBestOfToggle(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	res := askPlan(ap, context.Background())
	nextApproval(t, m, ap)
	m.Update(key("b")) // a is read-only
	if v := checkView(t, m, 160, 45); !strings.Contains(v, "best of N is for steps that change files") {
		t.Errorf("no refusal for a read-only step:\n%s", v)
	}
	m.Update(key("down"))
	m.Update(key("b")) // b: on
	if v := checkView(t, m, 160, 45); !strings.Contains(v, "best of N") {
		t.Errorf("the plan does not show best of N:\n%s", v)
	}
	m.Update(key("down"))
	m.Update(key("b"))
	m.Update(key("b")) // c: on, then off
	m.Update(key("enter"))
	a := waitPlan(t, res)
	if !a.ok || a.p.Subtasks[0].BestOf != "" || a.p.Subtasks[1].BestOf != orchestrator.BestOfOn || a.p.Subtasks[2].BestOf != orchestrator.BestOfOff {
		t.Fatalf("answer = %+v", a.p.Subtasks)
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
	if d := waitDecision(t, res); d.Feedback != "handle the empty case" || len(d.Apply) != 0 { // feedback never applies
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
	m.command("/conflicts maybe") // usage, nothing changes
	m.command("/conflicts ASK")
	oc := orc.Store().Get().Orchestrator
	if oc.ApprovePlan || !oc.ReviewChanges || oc.ReviewBeforeDone || oc.Conflicts != config.ConflictsAsk {
		t.Errorf("orchestrator cfg = approve %v review %v reviewer %v conflicts %q", oc.ApprovePlan, oc.ReviewChanges, oc.ReviewBeforeDone, oc.Conflicts)
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
		{"@types/node bump to v22", "", "", false},
		{"@Component rename props", "", "", false},
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

func TestExplainCommand(t *testing.T) {
	m, orc, ch := newModel(t, false)
	if cmd := m.command("/explain"); cmd != nil {
		m.Update(cmd())
	}
	if got := logTexts(m); !strings.Contains(got[len(got)-1], "no finished task") {
		t.Errorf("/explain without tasks: %q", got[len(got)-1])
	}
	orc.Run(context.Background(), "Make the parser keep trailing empty fields and add a strict flag with tests and docs please")
	drain(m, ch)
	if !strings.Contains(strings.Join(logTexts(m), "\n"), "/explain says why it ran this way") {
		t.Error("the result does not point at /explain")
	}
	// Demo tasks keep no state: record a finished one here.
	cd, _ := os.UserConfigDir()
	os.MkdirAll(filepath.Join(cd, "relayweft", "tasks"), 0o700)
	data, _ := json.Marshal(orchestrator.TaskState{ID: "explain-here", Dir: m.opt.Dir, Task: "Make the parser keep trailing empty fields", Status: "done",
		Created: time.Now(), Plan: &orchestrator.Plan{Subtasks: []orchestrator.Subtask{{ID: "one"}}}})
	if err := os.WriteFile(filepath.Join(cd, "relayweft", "tasks", "explain-here.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := m.command("/explain")
	if cmd == nil {
		t.Fatal("/explain runs nothing")
	}
	m.Update(cmd())
	all := strings.Join(logTexts(m), "\n")
	for _, want := range []string{"trailing empty fields", "Why ", "Escalations", "--open shows the same reasons"} {
		if !strings.Contains(all, want) {
			t.Errorf("/explain lacks %q:\n%s", want, all)
		}
	}
	if cmd := m.command("/explain nope"); cmd != nil {
		m.Update(cmd())
	}
	if got := logTexts(m); !strings.HasPrefix(got[len(got)-1], "explain: ") {
		t.Errorf("unknown id: %q", got[len(got)-1])
	}
	if m.command("/explain a b") != nil || !strings.Contains(strings.Join(logTexts(m), "\n"), "usage: /explain") {
		t.Error("two ids accepted")
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
	if !strings.Contains(all, "Relayweft: done: all good") || !strings.Contains(all, "limit") || strings.Contains(all, "short") {
		t.Errorf("notifications = %q", all)
	}
}

// Webhooks get the same news, with desktop notifications off too.
func TestWebhookNotifications(t *testing.T) {
	m, orc, _ := newModel(t, false)
	posted := make(chan notify.Message, 8)
	old := sendWebhooks
	sendWebhooks = func(_ context.Context, hooks []notify.Webhook, msg notify.Message) error {
		if len(hooks) != 1 {
			t.Errorf("hooks = %v", hooks)
		}
		posted <- msg
		return nil
	}
	t.Cleanup(func() { sendWebhooks = old })
	orc.Store().Update(func(c *config.Config) error {
		c.Notify.Enabled = false
		c.Notify.MinTask = config.Duration(time.Minute)
		c.Notify.Webhooks = []notify.Webhook{{URL: "https://ntfy.sh/t", Events: []string{"done", "limit"}}}
		return nil
	})
	m.running = true
	m.taskStart = time.Now().Add(-2 * time.Minute)
	m.handleEvent(event.Event{Kind: event.TaskDone, OK: true, Text: "all good"}.Stamp())
	m.alert(notify.EventWaiting, "Relayweft needs you", "not in this webhook's events")
	m.handleEvent(event.Event{Kind: event.ProviderState, Provider: event.Codex, Until: time.Now().Add(time.Hour), Text: "codex limit"}.Stamp())
	var got []notify.Message
	for len(got) < 2 {
		select {
		case msg := <-posted:
			got = append(got, msg)
		case <-time.After(5 * time.Second):
			t.Fatalf("webhooks posted %v", got)
		}
	}
	select {
	case msg := <-posted:
		t.Fatalf("unwanted post: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
	events := map[string]notify.Message{}
	for _, msg := range got {
		events[msg.Event] = msg
	}
	if d := events[notify.EventDone]; !strings.HasPrefix(d.Body, "all good\n2m") || d.Source == "" {
		t.Errorf("done = %+v", d)
	}
	if l := events[notify.EventLimit]; l.Body != "codex limit" {
		t.Errorf("limit = %+v", l)
	}
}

func TestAgentTabCompletion(t *testing.T) {
	m, _, _ := newModel(t, false)
	m.input.SetValue("@ed")
	if !m.completeAgent() || !strings.Contains(m.notice, "no running or finished agent") {
		t.Errorf("completion without sessions: %q", m.notice)
	}
	m.input.SetValue("plain task")
	if m.completeAgent() {
		t.Error("completion outside @")
	}
}

func init() {
	// Tests press keys right after an overlay opens; the grace period that
	// protects a typing user has its own test.
	overlayGrace = 0
}

// Keys typed while an approval pops up must not answer it.
func TestOverlayGraceIgnoresTyping(t *testing.T) {
	isolateState(t)
	overlayGrace = 300 * time.Millisecond
	defer func() { overlayGrace = 0 }()
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	res := askPlan(ap, context.Background())
	nextApproval(t, m, ap)
	for _, k := range []string{"d", "d", "enter"} {
		m.Update(key(k))
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case r := <-res:
		t.Fatalf("typing answered the plan: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	time.Sleep(350 * time.Millisecond) // the user pauses
	m.Update(key("enter"))
	r := waitPlan(t, res)
	if !r.ok || len(r.p.Subtasks) != 3 {
		t.Fatalf("after the pause: %+v", r)
	}
}
