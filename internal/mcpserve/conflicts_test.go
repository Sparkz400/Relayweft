package mcpserve

import (
	"context"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/web"
)

// MCP has no tool for answering a conflict question. It must fall back
// to keeping the work on a branch instead of waiting on an invisible UI.
func TestConflictQuestionDoesNotWaitInMCP(t *testing.T) {
	a := NewApprover()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- a.ApproveResolve(ctx, orchestrator.ConflictQuestion{}) }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case ok := <-done:
			if ok || len(a.Pending()) != 0 {
				t.Fatal("MCP must decline without leaving a pending question")
			}
			return
		case <-tick.C:
			if len(a.Pending()) > 0 {
				t.Fatal("MCP waits for a conflict answer that no tool can provide")
			}
		case <-timeout.C:
			t.Fatal("MCP did not decline the conflict question")
		}
	}
}

func TestConflictResolutionReviewInMCP(t *testing.T) {
	w := waitingView(&web.Request{Type: "changes", Changes: &web.ChangeView{
		StepID: "edit--resolve", Conflict: "resolved with step x",
	}})
	if w.Changes == nil || w.Changes.Conflict != "resolved with step x" {
		t.Fatal("the MCP change review lost the conflict-resolution flag")
	}
}
