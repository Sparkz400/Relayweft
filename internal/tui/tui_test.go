package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/runner"
)

func newModel(t *testing.T, ascii bool) (*Model, *orchestrator.Orchestrator, chan event.Event) {
	t.Helper()
	return newModelWith(t, ascii, nil)
}

// newModelWith builds a model on fake runners; ap (may be nil) is wired
// into both the orchestrator and the TUI, as cmd/sy does.
func newModelWith(t *testing.T, ascii bool, ap *Approver) (*Model, *orchestrator.Orchestrator, chan event.Event) {
	t.Helper()
	isolateState(t)
	ch := make(chan event.Event, 4096)
	store := config.NewStore(config.Default(), filepath.Join(t.TempDir(), "switchyard.yaml"))
	fake := runner.NewFakeSet(0)
	opts := orchestrator.Options{
		Dir: t.TempDir(), Store: store, Runners: func(*config.Config) runner.Set { return fake },
		Tracker: limits.NewTracker(), Events: ch, NoGit: true, Mode: "demo",
	}
	if ap != nil {
		opts.Approver = ap
	}
	orc := orchestrator.New(opts)
	mode := "unicode"
	if ascii {
		mode = "ascii"
	}
	m := New(Options{Orc: orc, Events: ch, Dir: "/work/project", Theme: NewTheme(mode), Version: "test", Approver: ap})
	return m, orc, ch
}

func drain(m *Model, ch chan event.Event) {
	for {
		select {
		case e := <-ch:
			m.handleEvent(e)
		default:
			return
		}
	}
}

func checkView(t *testing.T, m *Model, w, h int) string {
	t.Helper()
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.animate()
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) > h {
		t.Errorf("%dx%d: view has %d lines", w, h, len(lines))
	}
	for i, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("%dx%d: line %d is %d cells wide: %q", w, h, i, lw, l)
			break
		}
	}
	return v
}

func TestRenderFullDemoRunAtManySizes(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		m, orc, ch := newModel(t, ascii)
		res := orc.Run(context.Background(), "Make the parser keep trailing empty fields and add a strict flag with tests and docs please")
		if !res.OK {
			t.Fatalf("demo run failed: %+v", res)
		}
		drain(m, ch)
		if len(m.order) < 4 {
			t.Fatalf("agents in tree = %v", m.order)
		}
		for _, size := range [][2]int{{160, 50}, {120, 40}, {100, 30}, {80, 24}, {60, 20}, {40, 12}} {
			v := checkView(t, m, size[0], size[1])
			if size[0] >= 100 && !strings.Contains(v, "REVIEWER") {
				t.Errorf("%v: reviewer panel missing", size)
			}
		}
		// Focus an agent: the log filters to it.
		m.focus = focusTree
		m.selected = 1
		v := checkView(t, m, 140, 44)
		if !strings.Contains(v, "LOG · ") {
			t.Error("selected agent log not shown")
		}
		m.fullLog = true
		checkView(t, m, 140, 44)
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestPickerChangesAndSavesModels(t *testing.T) {
	m, orc, _ := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 44})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.picker == nil {
		t.Fatal("ctrl+o did not open the picker")
	}
	// Row 1 = worker; column 3 = claude model.
	m.Update(key("down"))
	for i := 0; i < 3; i++ {
		m.Update(key("right"))
	}
	m.Update(key("enter"))
	if !m.picker.choosing {
		t.Fatal("options not opened")
	}
	// Pick "custom…" and type a model id.
	for i, o := range m.picker.options {
		if o.custom {
			m.picker.optIdx = i
		}
	}
	m.Update(key("enter"))
	for _, r := range "claude-sonnet-5-5" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(key("enter"))
	got := orc.Store().Get().Roles[event.RoleWorker].Claude.Model
	if got != "claude-sonnet-5-5" {
		t.Fatalf("worker claude model = %q", got)
	}
	// Change the worker's prefer to claude via the list.
	m.Update(key("right"))
	m.Update(key("right")) // wraps to column 0 (prefer)
	m.Update(key("enter"))
	for i, o := range m.picker.options {
		if o.value == config.PreferClaude {
			m.picker.optIdx = i
		}
	}
	m.Update(key("enter"))
	if p := orc.Store().Get().Roles[event.RoleWorker].Prefer; p != config.PreferClaude {
		t.Fatalf("prefer = %s", p)
	}
	if d := orc.Router().Preview(event.RoleWorker, ""); d.Provider != event.Claude || d.Model != "claude-sonnet-5-5" {
		t.Errorf("router does not see the change: %+v", d)
	}
	checkView(t, m, 140, 44)
	m.Update(key("s"))
	if m.dirty {
		t.Error("save did not clear dirty flag")
	}
	saved, _, err := config.Load(orc.Store().Path())
	if err != nil || saved.Roles[event.RoleWorker].Claude.Model != "claude-sonnet-5-5" {
		t.Fatalf("saved config = %+v %v", saved.Roles[event.RoleWorker], err)
	}
	m.Update(key("esc"))
	if m.picker != nil {
		t.Error("esc did not close the picker")
	}
}

func TestSlashCommands(t *testing.T) {
	m, orc, _ := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	run := func(s string) {
		m.focus = focusPrompt
		m.input.SetValue(s)
		pressEnter(m)
	}
	run("/route reviewer codex:gpt-6-astra:max")
	if r := orc.Store().Get().Roles[event.RoleReviewer].Codex; r.Model != "gpt-6-astra" || r.Effort != "max" {
		t.Errorf("route = %+v", r)
	}
	run("/prefer all claude")
	cfg := orc.Store().Get()
	if cfg.Roles[event.RolePlanner].Prefer != "claude" || cfg.Roles[event.RoleReviewer].Prefer != "other" {
		t.Errorf("prefer all: planner=%s reviewer=%s", cfg.Roles[event.RolePlanner].Prefer, cfg.Roles[event.RoleReviewer].Prefer)
	}
	run("/threads 7")
	if orc.Store().Get().Orchestrator.MaxThreads != 7 {
		t.Error("/threads ignored")
	}
	run("/limit codex set")
	if !orc.Tracker().Limited(event.Codex) {
		t.Error("/limit set ignored")
	}
	run("/limit codex reset")
	if orc.Tracker().Limited(event.Codex) {
		t.Error("/limit reset ignored")
	}
	run("/route nobody codex:x")
	if !strings.Contains(m.logs[len(m.logs)-1].text, "not changed") {
		t.Errorf("bad route not reported: %q", m.logs[len(m.logs)-1].text)
	}
	run("/help")
	run("/bogus")
	if !strings.Contains(m.logs[len(m.logs)-1].text, "unknown command") {
		t.Error("unknown command not reported")
	}
}

func TestTabCyclesAndKillNeedsSelection(t *testing.T) {
	m, _, _ := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.Update(key("tab")) // prompt -> tree, nothing selected
	if m.focus != focusTree || m.selectedAgent() != "" {
		t.Fatalf("focus=%v selected=%q", m.focus, m.selectedAgent())
	}
	m.Update(key("k"))
	if !strings.Contains(m.notice, "select an agent") {
		t.Errorf("notice = %q", m.notice)
	}
	m.Update(key("tab"))
	if m.selectedAgent() != orchestrator.AgentMain {
		t.Errorf("selected = %q", m.selectedAgent())
	}
	m.Update(key("k"))
	if !strings.Contains(m.notice, "press k again") {
		t.Errorf("first k must only arm the kill: %q", m.notice)
	}
	m.Update(key("k"))
	if !strings.Contains(m.notice, "nothing running") {
		t.Errorf("second k should try to kill: %q", m.notice)
	}
	m.Update(key("enter"))
	if m.focus != focusPrompt {
		t.Error("enter did not return to the prompt")
	}
}

// pressEnter is a human Enter: a pause before it, then the submit check.
func pressEnter(m *Model) {
	m.input.lastKey = time.Time{}
	_, cmd := m.Update(key("enter"))
	if cmd != nil {
		m.Update(cmd())
	}
}

// fakeClock drives the prompt's paste detection deterministically.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time       { return c.t }
func (c *fakeClock) step(d time.Duration) { c.t = c.t.Add(d) }

// typeKeys sends keys as a Windows console paste does: one by one, 1ms apart.
func typeKeys(m *Model, c *fakeClock, s string) []tea.Cmd {
	var cmds []tea.Cmd
	for _, r := range s {
		c.step(time.Millisecond)
		var msg tea.KeyMsg
		switch r {
		case '\n':
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case '\t':
			msg = tea.KeyMsg{Type: tea.KeyTab}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		}
		_, cmd := m.Update(msg)
		cmds = append(cmds, cmd)
	}
	return cmds
}

func newPromptModel(t *testing.T) (*Model, *fakeClock) {
	m, _, _ := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	c := &fakeClock{t: time.Unix(1000, 0)}
	m.input.now = c.now
	return m, c
}

// fire delivers every pending submit check, like the 40ms timers would.
func fire(m *Model, cmds []tea.Cmd) {
	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		if msg, ok := cmd().(submitCheckMsg); ok {
			m.Update(msg)
		}
	}
}

func TestWindowsStylePasteKeepsAllLines(t *testing.T) {
	m, c := newPromptModel(t)
	cmds := typeKeys(m, c, "first line\nsecond line\n\tindented\nlast line")
	fire(m, cmds)
	if m.running {
		t.Fatal("a paste must never submit")
	}
	want := "first line\nsecond line\n    indented\nlast line"
	if got := m.input.Value(); got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
	if m.focus != focusPrompt {
		t.Error("a tab inside a paste moved the focus")
	}
	// Now the user presses Enter: the whole text is submitted as one task.
	c.step(2 * time.Second)
	_, cmd := m.Update(key("enter"))
	m.Update(cmd())
	if !m.running || m.taskText != want {
		t.Fatalf("submitted %q running=%v", m.taskText, m.running)
	}
	if m.input.Value() != "" {
		t.Error("prompt not cleared after submit")
	}
	m.Shutdown()
}

func TestPasteStartingOrEndingWithNewline(t *testing.T) {
	m, c := newPromptModel(t)
	c.step(time.Second)
	cmds := typeKeys(m, c, "\nabc\n")
	fire(m, cmds) // the first Enter's timer arrives after the paste
	if m.running {
		t.Fatal("submitted a paste")
	}
	if got := m.input.Value(); got != "\nabc\n" {
		t.Fatalf("value = %q", got)
	}
}

func TestBracketedPaste(t *testing.T) {
	m, _ := newPromptModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one\ntwo\nthree"), Paste: true})
	if got := m.input.Value(); got != "one\ntwo\nthree" || m.running {
		t.Fatalf("value = %q running=%v", got, m.running)
	}
	if !strings.Contains(m.View(), "three") {
		t.Error("multi-line prompt not rendered")
	}
}

func TestTypedEnterSubmitsAndAltEnterIsNewline(t *testing.T) {
	m, c := newPromptModel(t)
	for _, r := range "where is x" {
		c.step(150 * time.Millisecond) // human typing speed
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	c.step(150 * time.Millisecond)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if !strings.Contains(m.input.Value(), "\n") {
		t.Fatalf("alt+enter did not add a line: %q", m.input.Value())
	}
	c.step(150 * time.Millisecond)
	_, cmd := m.Update(key("enter"))
	if m.running {
		t.Fatal("submitted before the paste window passed")
	}
	m.Update(cmd())
	if !m.running {
		t.Fatal("typed enter did not submit")
	}
	m.Shutdown()
}

func TestCancelMarksEverythingStopped(t *testing.T) {
	m, _, ch := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	// A node that never got to run, and one that was running.
	m.running = true
	m.cancelTask = func() {}
	m.node("queued-one")
	m.node("busy").status = stRunning
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	if !m.cancelling || m.phase != "cancelling" {
		t.Fatalf("ctrl+x did not start cancelling (phase %q)", m.phase)
	}
	m.handleEvent(event.Event{Kind: event.TaskDone, Text: "cancelled: execution"}.Stamp())
	for _, id := range []string{"queued-one", "busy", orchestrator.AgentMain} {
		if st := m.nodes[id].status; st != stKilled {
			t.Errorf("%s status = %v, want killed", id, st)
		}
	}
	if m.running || m.cancelling {
		t.Error("still running after TaskDone")
	}
	_ = ch
}
