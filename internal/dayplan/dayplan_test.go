package dayplan

import (
	"strings"
	"testing"
	"time"
)

// at is a time on a fixed local day.
func at(h, m int) time.Time { return time.Date(2026, 10, 6, h, m, 0, 0, time.Local) }

func tasks(n int, tokens float64, d time.Duration) []Task {
	out := make([]Task, n)
	for i := range out {
		out[i] = Task{Text: "t", Tokens: tokens, Duration: d}
	}
	return out
}

func providers(ps []Slot) string {
	var b []string
	for _, s := range ps {
		b = append(b, s.Provider)
	}
	return strings.Join(b, ",")
}

func TestExpiringWindowFirst(t *testing.T) {
	now := at(22, 0)
	// Claude's window ends in an hour with room left; Codex has none
	// running. Claude's quota is lost at 23:00, so it goes first.
	provs := []Provider{
		{Name: "codex", Capacity: 1_000_000},
		{Name: "claude", Used: 0.5, ResetsAt: at(23, 0), Capacity: 1_000_000},
	}
	p := Make(tasks(6, 100_000, 10*time.Minute), provs, Options{Now: now})
	// 0.5 + 4×0.1 reaches the 0.9 ceiling, then Codex opens a window.
	if got, want := providers(p.Slots), "claude,claude,claude,claude,codex,codex"; got != want {
		t.Fatalf("providers = %s, want %s", got, want)
	}
	if s := p.Slots[4]; !s.Opens || !s.Strict || !s.Start.Equal(at(22, 40)) {
		t.Fatalf("codex slot = %+v, want it to open a window at 22:40, strictly", s)
	}
	if p.Slots[0].Opens || p.Slots[0].Strict {
		t.Fatalf("first slot = %+v: claude's window was running and codex had room", p.Slots[0])
	}
	if len(p.Skipped) != 0 {
		t.Fatalf("skipped %v", p.Skipped)
	}
}

func TestWaitsForTheNextReset(t *testing.T) {
	now := at(1, 0)
	provs := []Provider{
		{Name: "codex", Used: 0.95, ResetsAt: at(3, 0), Capacity: 1e6},
		{Name: "claude", LimitedUntil: at(2, 30), Used: 1, ResetsAt: at(2, 30), Capacity: 1e6},
	}
	p := Make(tasks(2, 100_000, 10*time.Minute), provs, Options{Now: now})
	if len(p.Slots) != 2 {
		t.Fatalf("slots = %+v, skipped %v", p.Slots, p.Skipped)
	}
	s := p.Slots[0]
	if s.Provider != "claude" || !s.Start.Equal(at(2, 30)) || !strings.Contains(s.Wait, "claude's limit resets at 02:30") || !s.Opens {
		t.Fatalf("first slot = %+v", s)
	}
	if _, ok := p.First(now); ok {
		t.Fatal("First says start now, but the plan starts with a wait")
	}
	if len(p.Resets) != 1 || p.Resets[0].Provider != "claude" {
		t.Fatalf("resets = %+v", p.Resets)
	}
	if p.Slots[1].Wait != "" || !p.Slots[1].Start.Equal(at(2, 40)) {
		t.Fatalf("second slot = %+v", p.Slots[1])
	}
}

func TestUntilLeavesTheRest(t *testing.T) {
	now := at(5, 0)
	provs := []Provider{{Name: "codex", Capacity: 1e6}, {Name: "claude", Capacity: 1e6}}
	p := Make(tasks(5, 10_000, time.Hour), provs, Options{Now: now, Until: at(7, 30)})
	if len(p.Slots) != 3 { // 05:00, 06:00, 07:00
		t.Fatalf("slots = %d (%+v)", len(p.Slots), p.Slots)
	}
	if len(p.Skipped) != 2 || !strings.Contains(p.Skipped[0].Why, "after 07:30") {
		t.Fatalf("skipped = %+v", p.Skipped)
	}
	// A reset after the deadline is no reason to wait.
	full := []Provider{{Name: "codex", Used: 0.95, ResetsAt: at(8, 0)}, {Name: "claude", Used: 0.95, ResetsAt: at(9, 0)}}
	p = Make(tasks(1, 10_000, time.Minute), full, Options{Now: now, Until: at(7, 0)})
	if len(p.Slots) != 0 || len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0].Why, "codex's window resets at 08:00, after 07:00") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestFreshAtOpensNoLateWindow(t *testing.T) {
	provs := []Provider{{Name: "codex", Capacity: 1e6}, {Name: "claude", Used: 0.3, ResetsAt: at(4, 0), Capacity: 1e6}}
	// 03:00: a new window would run until 08:00, past 07:00. Claude's
	// running window may still be used.
	p := Make(tasks(3, 100_000, 20*time.Minute), provs, Options{Now: at(3, 0), FreshAt: at(7, 0)})
	if got := providers(p.Slots); got != "claude,claude,claude" {
		t.Fatalf("providers = %s", got)
	}
	for _, s := range p.Slots {
		if !s.Strict {
			t.Fatalf("slot %+v: codex is held back, so the task must not use it", s)
		}
	}
	// With claude full too, nothing is left to do before 07:00.
	provs[1].Used = 0.9
	p = Make(tasks(1, 100_000, time.Minute), provs, Options{Now: at(3, 0), FreshAt: at(7, 0)})
	if len(p.Slots) != 0 || !strings.Contains(p.Skipped[0].Why, "a new codex window would still run at 07:00") {
		t.Fatalf("plan = %+v", p)
	}
	// At 01:00 a new window ends at 06:00: allowed.
	p = Make(tasks(1, 100_000, time.Minute), provs, Options{Now: at(1, 0), FreshAt: at(7, 0)})
	if len(p.Slots) != 1 || p.Slots[0].Provider != "codex" || !p.Slots[0].Opens {
		t.Fatalf("plan = %+v", p)
	}
	// FreshAt is a deadline too.
	p = Make(tasks(1, 1, time.Minute), provs, Options{Now: at(7, 30), FreshAt: at(7, 0).Add(24 * time.Hour), Until: at(7, 40)})
	if len(p.Slots) != 1 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestDayBudget(t *testing.T) {
	provs := []Provider{{Name: "codex", Capacity: 1e7}}
	// 400k of 1M used today: one 500k task fits, the next waits for
	// midnight, when the budget starts over.
	p := Make(tasks(2, 500_000, 30*time.Minute), provs, Options{Now: at(22, 0), DayTokens: 1_000_000, DayTokensUsed: 400_000})
	if len(p.Slots) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	if s := p.Slots[1]; !s.Start.Equal(at(24, 0)) || !strings.Contains(s.Wait, "midnight") {
		t.Fatalf("second slot = %+v", s)
	}
	// A task over the whole budget never fits; the next one still runs.
	q := []Task{{Tokens: 2_000_000, Duration: time.Minute}, {Tokens: 100, Duration: time.Minute}}
	p = Make(q, provs, Options{Now: at(22, 0), DayTokens: 1_000_000})
	if len(p.Slots) != 1 || p.Slots[0].Task != 1 || len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0].Why, "over budget.day_tokens") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestUnknownCapacityGuesses(t *testing.T) {
	provs := []Provider{{Name: "claude"}}
	p := Make(tasks(12, 1, time.Minute), provs, Options{Now: at(20, 0)})
	// 10% each: 9 tasks reach the ceiling, the rest wait 5 hours for the
	// window the first task opened.
	if len(p.Slots) != 12 || !p.Slots[0].Guess || p.Slots[0].Share != DefaultShare {
		t.Fatalf("slots = %+v", p.Slots)
	}
	if s := p.Slots[9]; !s.Start.Equal(at(25, 0)) || s.Wait == "" {
		t.Fatalf("10th slot = %+v", s)
	}
}

func TestFullWithoutKnownReset(t *testing.T) {
	// Over the ceiling but no reset known: assume a full window from now.
	p := Make(tasks(1, 1, time.Minute), []Provider{{Name: "claude", Used: 0.95}}, Options{Now: at(20, 0)})
	if len(p.Slots) != 1 || !p.Slots[0].Start.Equal(at(25, 0)) {
		t.Fatalf("plan = %+v", p)
	}
}

func TestResetsWhileTasksRun(t *testing.T) {
	// Claude's window resets at 21:30 while tasks run back to back; it is
	// not full afterwards, and the reset shows where it happens.
	provs := []Provider{{Name: "claude", Used: 0.5, ResetsAt: at(21, 30), Capacity: 1e6}}
	p := Make(tasks(4, 100_000, 20*time.Minute), provs, Options{Now: at(20, 30)})
	if len(p.Slots) != 4 || p.Slots[3].Wait != "" || !p.Slots[3].Start.Equal(at(21, 30)) || !p.Slots[3].Opens {
		t.Fatalf("slots = %+v", p.Slots)
	}
	if len(p.Resets) != 1 || !p.Resets[0].At.Equal(at(21, 30)) {
		t.Fatalf("resets = %+v", p.Resets)
	}
}
