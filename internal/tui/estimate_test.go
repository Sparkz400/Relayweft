package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// fakeEstimate estimates 20k tokens per step (40k for worker_high), with
// no history for step c, plus the final review and a budget warning.
func fakeEstimate(calls *int) func(orchestrator.Plan) orchestrator.PlanEstimate {
	return func(p orchestrator.Plan) orchestrator.PlanEstimate {
		*calls++
		var e orchestrator.PlanEstimate
		add := func(id, role string) {
			se := orchestrator.StepEstimate{StepID: id, Role: role, Route: "claude:sonnet:medium"}
			tok := 20_000.0
			if role == event.RoleWorkerHigh {
				tok = 40_000
			}
			se.Tokens = sessionlog.Spread{Low: tok / 2, Mid: tok, High: tok * 2}
			se.Seconds = sessionlog.Spread{Low: 60, Mid: 90, High: 120}
			se.USD = sessionlog.Spread{Low: 0.1, Mid: 0.2, High: 0.4}
			se.Source, se.Samples = sessionlog.SourceRepo, 5
			if id == "c" {
				se.Source, se.Samples = sessionlog.SourceNone, 0
				e.NoHistory++
			}
			e.Steps = append(e.Steps, se)
			e.Tokens, e.Seconds, e.USD = e.Tokens.Add(se.Tokens), e.Seconds.Add(se.Seconds), e.USD.Add(se.USD)
		}
		for _, st := range p.Subtasks {
			role := st.Role
			if role == "" {
				role = event.RoleWorker
			}
			add(st.ID, role)
		}
		add(orchestrator.FinalReviewID, event.RoleReviewer)
		e.Warnings = []string{"likely over this task's tokens: ~80k (40k-160k), 50k left of 50k tokens"}
		return e
	}
}

func TestPlanOverlayShowsEstimate(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 45})
	calls := 0
	res := make(chan planAnswer, 1)
	go func() {
		p, ok := ap.ApprovePlanEstimate(context.Background(), "fix the parser", testPlan(), fakeEstimate(&calls))
		res <- planAnswer{p, ok}
	}()
	nextApproval(t, m, ap)
	p := m.overlay.(*planOverlay)
	if p.est == nil || calls != 1 {
		t.Fatalf("no estimate (calls %d)", calls)
	}
	v := checkView(t, m, 160, 45)
	for _, want := range []string{"ESTIMATE", "20k · 1m30s · $0.20", "20k · 1m30s · $0.20 ?", "estimate incl. final review: ~80k tok (40k-160k)",
		"1 of 4 steps without history", "budget: likely over this task's tokens"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	checkView(t, m, 80, 24)
	checkView(t, m, 60, 20)
	// A new role is re-estimated at once.
	m.Update(key("down"))
	m.Update(key("r")) // b: planner
	m.Update(key("r")) // worker
	m.Update(key("r")) // worker_high
	if b, _ := p.est.Step("b"); b.Role != event.RoleWorkerHigh || calls < 4 {
		t.Fatalf("b = %+v after %d estimates", b, calls)
	}
	if v := checkView(t, m, 160, 45); !strings.Contains(v, "40k · 1m30s · $0.20") {
		t.Errorf("re-estimate not shown:\n%s", v)
	}
	// Typing into a prompt does not re-estimate on every key.
	m.Update(key("e"))
	before := calls
	m.Update(key("x"))
	m.Update(key("y"))
	if calls != before {
		t.Fatalf("re-estimated while typing: %d -> %d", before, calls)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.Update(key("enter"))
	if a := waitPlan(t, res); !a.ok || a.p.Subtasks[1].Role != event.RoleWorkerHigh {
		t.Fatalf("answer = %+v", a)
	}
}
