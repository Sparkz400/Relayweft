package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func TestConflictOverlay(t *testing.T) {
	q := orchestrator.ConflictQuestion{Task: "fix the parser", StepID: "b", Title: "say b", With: "step a (say a)", Files: []string{"shared.txt"}}
	for _, k := range []string{"y", "n"} {
		ap := NewApprover()
		m, _, _ := newModelWith(t, false, ap)
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		got := make(chan bool, 1)
		go func() { got <- ap.ApproveResolve(context.Background(), q) }()
		nextApproval(t, m, ap)
		if _, ok := m.overlay.(*conflictOverlay); !ok {
			t.Fatalf("overlay = %T", m.overlay)
		}
		v := checkView(t, m, 120, 40)
		for _, want := range []string{"MERGE CONFLICT", "b conflicts with step a (say a) in shared.txt", "Let an agent resolve it? y/n", "kept on a branch"} {
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
		m.Shutdown()
	}
}

// A conflict resolution under review says so.
func TestReviewShowsConflictResolution(t *testing.T) {
	ap := NewApprover()
	m, _, _ := newModelWith(t, false, ap)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	cs := testChanges()
	cs.Conflict = "edit conflicts with step a (say a) in parser.go"
	out := make(chan orchestrator.ChangeDecision, 1)
	go func() { out <- ap.ReviewChanges(context.Background(), cs) }()
	nextApproval(t, m, ap)
	v := checkView(t, m, 140, 40)
	for _, want := range []string{"REVIEW CONFLICT RESOLUTION", "conflict: edit conflicts with step a (say a) in parser.go"} {
		if !strings.Contains(v, want) {
			t.Errorf("view misses %q", want)
		}
	}
	m.Shutdown()
}
