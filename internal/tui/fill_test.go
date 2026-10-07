package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func logged(m *Model, s string) bool {
	for _, l := range m.logs {
		if strings.Contains(l.text, s) {
			return true
		}
	}
	return false
}

func TestFillWaitsForAWindowThenLeans(t *testing.T) {
	held := stubAwake(t)
	m, orc, ch := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.dayPlanner().ReadLogs = func(time.Time) []sessionlog.Record { return nil }
	now := time.Now()
	orc.Tracker().SetQuota(event.Codex, event.QuotaInfo{Utilization: 0.95, Window: "5h", ResetsAt: now.Add(time.Hour)})
	orc.Tracker().MarkLimited(event.Claude, now.Add(300*time.Millisecond))

	m.command("/fill on until " + now.Add(2*time.Hour).Format("15:04"))
	if !m.fill.on || !logged(m, "fill on: queued tasks run when a subscription's window has room") {
		t.Fatalf("fill on: %+v", m.fill)
	}
	// Typed while idle: queued, and held until Claude's limit resets.
	m.startTask("fix the parser")
	if m.running || len(m.queue) != 1 || !m.queue[0].unattended {
		t.Fatalf("running=%v queue=%+v", m.running, m.queue)
	}
	if !logged(m, "fill: nothing has room now; the next task waits for claude's limit resets") {
		t.Fatalf("no hold logged: %+v", m.logs)
	}
	if v := checkView(t, m, 140, 40); !strings.Contains(v, "fill waits to") {
		t.Error("header does not show the fill wait")
	}
	if held.Load() != 1 {
		t.Errorf("keep-awake holds = %d while the plan waits", held.Load())
	}
	m.command("/fill")
	if !logged(m, "Day plan · 1 queued task(s)") || !logged(m, "1. fix the parser") {
		t.Error("/fill does not print the plan")
	}

	time.Sleep(350 * time.Millisecond)
	m.tickSchedule()
	if !m.running || !m.current.planned || m.current.lean.Provider != event.Claude || !m.current.lean.Strict {
		t.Fatalf("running=%v current=%+v", m.running, m.current)
	}
	if !logged(m, "fill: starting a queued task only on claude") {
		t.Error("start not logged")
	}
	<-m.taskDone
	drain(m, ch)
	m.tickSchedule()
	if m.running || m.fill.run != nil || len(m.queue) != 0 {
		t.Fatalf("after the run: running=%v run=%v queue=%d", m.running, m.fill.run, len(m.queue))
	}
	if held.Load() != 0 {
		t.Errorf("keep-awake holds = %d with nothing left", held.Load())
	}

	m.command("/fill off")
	m.startTask("now")
	if !m.running || m.current.planned {
		t.Fatalf("with fill off a typed task runs at once: running=%v current=%+v", m.running, m.current)
	}
	<-m.taskDone
	drain(m, ch)
	m.Shutdown()
}

func TestFillResumesALimitStoppedTask(t *testing.T) {
	stubAwake(t)
	m, orc, _ := newModel(t, false)
	m.command("/fill on")
	old := fillLoadTask
	fillLoadTask = func(id string) (*orchestrator.TaskState, error) {
		if id != "t1" {
			return nil, errors.New("no such task")
		}
		return &orchestrator.TaskState{ID: id, Task: "big refactor"}, nil
	}
	defer func() { fillLoadTask = old }()
	done := make(chan struct{})
	close(done)
	orc.Tracker().MarkLimited(event.Claude, time.Now().Add(time.Hour))
	m.fill.run = &fillRun{job: job{text: "big refactor", planned: true}, done: done, id: "t1"}
	if !m.fillSettled() || len(m.queue) != 1 {
		t.Fatalf("queue = %+v", m.queue)
	}
	if j := m.queue[0]; j.resume == nil || !j.force || j.retries != 1 || !j.plannable() {
		t.Fatalf("requeued = %+v", j)
	}
	if !logged(m, "fill: a usage limit stopped big refactor; it resumes when a window has room") {
		t.Error("not logged")
	}
	// Once only, and not for a task that failed without a limit hit.
	m.queue = nil
	m.fill.run = &fillRun{job: job{text: "big refactor", planned: true, retries: 1}, done: done, id: "t1"}
	m.fillSettled()
	m.fill.run = &fillRun{job: job{text: "other"}, done: done, id: "t1", hits: orc.Tracker().Snapshot(event.Claude).LimitHits}
	m.fillSettled()
	if len(m.queue) != 0 {
		t.Fatalf("requeued again: %+v", m.queue)
	}
	// Still running: not settled.
	m.fill.run = &fillRun{done: make(chan struct{})}
	if m.fillSettled() {
		t.Fatal("settled before the task ended")
	}
	m.fill.run = nil
	m.Shutdown()
}

func TestFillCommandUsage(t *testing.T) {
	stubAwake(t)
	m, _, _ := newModel(t, false)
	for _, bad := range []string{"/fill on until", "/fill on until soon", "/fill on later 07:00", "/fill maybe"} {
		m.logs = nil
		m.command(bad)
		if m.fill.on || !logged(m, "/fill on [until 07:00] [fresh 09:00]") {
			t.Errorf("%q: on=%v logs=%+v", bad, m.fill.on, m.logs)
		}
	}
	m.command("/fill")
	if !logged(m, "fill is off") {
		t.Error("/fill does not say it is off")
	}
	m.command("/fill on fresh 09:00")
	if _, fresh := m.dayPlanner().Bounds(); fresh.IsZero() || fresh.Hour() != 9 {
		t.Fatalf("fresh-at = %v", fresh)
	}
	m.Shutdown()
}
