package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	ch := make(chan event.Event, 4096)
	store := config.NewStore(config.Default(), filepath.Join(t.TempDir(), "switchyard.yaml"))
	fake := runner.NewFakeSet(0)
	orc := orchestrator.New(orchestrator.Options{
		Dir: t.TempDir(), Store: store, Runners: func(*config.Config) runner.Set { return fake },
		Tracker: limits.NewTracker(), Events: ch, NoGit: true, Mode: "demo",
	})
	mode := "unicode"
	if ascii {
		mode = "ascii"
	}
	m := New(Options{Orc: orc, Events: ch, Dir: "/work/project", Theme: NewTheme(mode), Version: "test"})
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
		m.Update(key("enter"))
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
	m.Update(key("enter"))
	if m.focus != focusPrompt {
		t.Error("enter did not return to the prompt")
	}
}
