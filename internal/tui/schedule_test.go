package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// stubAwake counts keep-awake holds instead of touching the OS.
func stubAwake(t *testing.T) *atomic.Int32 {
	t.Helper()
	var held atomic.Int32
	old := keepAwake
	keepAwake = func() func() {
		held.Add(1)
		return func() { held.Add(-1) }
	}
	t.Cleanup(func() { keepAwake = old })
	return &held
}

func TestScheduledTaskStartsWhenDue(t *testing.T) {
	held := stubAwake(t)
	m, _, ch := newModel(t, false)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.command("/schedule in 2h fix the parser")
	// A clock time 3h ahead stays after "in 2h" at any time of day (a fixed
	// 23:59 came first from 21:59 on, and the test failed every evening).
	m.command("/schedule " + time.Now().Add(3*time.Hour).Format("15:04") + " second one")
	if len(m.queue) != 2 || !m.queue[0].unattended || time.Until(m.queue[0].at) < 119*time.Minute {
		t.Fatalf("queue = %+v", m.queue)
	}
	m.tickSchedule()
	if m.running {
		t.Fatal("a task scheduled in 2h started now")
	}
	if held.Load() != 1 {
		t.Errorf("keep-awake holds = %d while a scheduled task waits", held.Load())
	}
	if v := checkView(t, m, 140, 40); !strings.Contains(v, "next ") {
		t.Error("header does not show the next scheduled start")
	}
	m.command("/schedule rm 2")
	if len(m.queue) != 1 || m.queue[0].text != "fix the parser" {
		t.Fatalf("after rm: %+v", m.queue)
	}

	// Due while another task runs: it waits for that task.
	m.queue[0].at = time.Now().Add(-time.Second)
	m.running = true
	m.tickSchedule()
	if len(m.queue) != 1 {
		t.Fatal("a due task started while another one ran")
	}
	m.running = false
	m.tickSchedule()
	if !m.running || m.taskText != "fix the parser" || len(m.queue) != 0 || !m.current.unattended {
		t.Fatalf("running=%v task=%q queue=%d", m.running, m.taskText, len(m.queue))
	}
	if held.Load() != 1 {
		t.Errorf("keep-awake holds = %d while the scheduled task runs", held.Load())
	}
	<-m.taskDone
	drain(m, ch)
	m.tickSchedule()
	if m.running || held.Load() != 0 {
		t.Errorf("after the run: running=%v holds=%d", m.running, held.Load())
	}
	m.Shutdown()
}

func TestScheduleCommandErrorsAndReset(t *testing.T) {
	stubAwake(t)
	m, orc, _ := newModel(t, false)
	for _, bad := range []string{"/schedule tomorrowish fix", "/schedule in 2h", "/schedule reset bard fix", "/schedule rm 9"} {
		m.command(bad)
		if len(m.queue) != 0 {
			t.Fatalf("%q queued something", bad)
		}
	}
	resets := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	orc.Tracker().SetQuota(event.Claude, event.QuotaInfo{Utilization: 0.99, Window: "five_hour", ResetsAt: resets})
	m.command("/schedule reset claude big refactor")
	if len(m.queue) != 1 || !m.queue[0].at.Equal(resets) || m.queue[0].text != "big refactor" {
		t.Fatalf("queue = %+v", m.queue)
	}
	// Unknown reset (codex never reported one): it runs as soon as possible.
	m.command("/schedule reset codex small fix")
	if len(m.queue) != 1 || !m.running {
		t.Fatalf("unknown reset did not start now: queue=%d running=%v", len(m.queue), m.running)
	}
	m.command("/schedule")
	found := false
	for _, l := range m.logs {
		if strings.Contains(l.text, "big refactor") && strings.Contains(l.text, "in 3h") {
			found = true
		}
	}
	if !found {
		t.Error("/schedule does not list the scheduled task with its time")
	}
	m.Shutdown()
}

func TestBudgetOverlay(t *testing.T) {
	for _, k := range []string{"y", "n"} {
		ap := NewApprover()
		m, orc, _ := newModelWith(t, false, ap)
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		got := make(chan bool, 1)
		go func() {
			got <- ap.ApproveBudget(context.Background(), orchestrator.BudgetRequest{Limit: orchestrator.LimitTaskUSD, Used: 2.04, Max: 2, Task: "fix the parser", Next: "start b (docs)"})
		}()
		nextApproval(t, m, ap)
		if _, ok := m.overlay.(*budgetOverlay); !ok {
			t.Fatalf("overlay = %T", m.overlay)
		}
		v := checkView(t, m, 120, 40)
		for _, want := range []string{"BUDGET REACHED", "this task's cost $2.04 reached the budget of $2.00", "Continue? y/n", "--budget-task-usd"} {
			if !strings.Contains(v, want) {
				t.Errorf("view misses %q", want)
			}
		}
		m.Update(key("x")) // other keys do nothing
		m.Update(key(k))
		select {
		case ok := <-got:
			if ok != (k == "y") {
				t.Errorf("key %s answered %v", k, ok)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no answer")
		}
		if m.overlay != nil {
			t.Error("overlay still open")
		}
		_ = orc
		m.Shutdown()
	}
}

func TestHeaderShowsBudget(t *testing.T) {
	m, orc, _ := newModel(t, false)
	if v := checkView(t, m, 160, 30); strings.Contains(v, "today") {
		t.Error("budget shown without a budget")
	}
	orc.Store().Update(func(c *config.Config) error { c.Budget.DayUSD = 2; return nil })
	if v := checkView(t, m, 160, 30); !strings.Contains(v, "$0.00/$2 today") {
		t.Errorf("header misses the budget:\n%s", strings.Split(v, "\n")[0])
	}
}
