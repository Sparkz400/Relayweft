package orchestrator

import (
	"context"
	"testing"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
)

// rw morning tells unattended tasks from watched ones by their state.
func TestStateRecordsUnattended(t *testing.T) {
	dir := gitRepo(t)
	set := runner.Set{event.Codex: &runner.Fake{Provider: event.Codex}, event.Claude: &runner.Fake{Provider: event.Claude}}
	o, _ := newOrc(t, dir, set, nil)
	o.RunWith(context.Background(), "where is the readme", TaskOptions{Unattended: true})
	o.RunWith(context.Background(), "where is the license", TaskOptions{})
	hist := History(dir, 2)
	if len(hist) != 2 {
		t.Fatalf("history = %+v", hist)
	}
	byTask := map[string]bool{}
	for _, s := range hist {
		byTask[s.Task] = s.Unattended
	}
	if !byTask["where is the readme"] || byTask["where is the license"] {
		t.Fatalf("unattended = %v", byTask)
	}
}
