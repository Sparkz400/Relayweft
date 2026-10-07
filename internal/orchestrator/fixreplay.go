package orchestrator

import "github.com/sparkz400/relayweft/internal/router"

// A replayed task (rw bench --replay-fix) measures what happens after the
// work: its one step puts a saved run's change in place, and the verify,
// review and fix rounds run as in any task, with independent tests written
// earlier. A replay of the tests alone (--replay-tests) shows which saved
// runs they catch; this shows whether the fix round they start turns a
// catch into a pass, and what it does to correct work they fail.

// ReplayWork is the saved change a replayed task verifies and fixes.
type ReplayWork struct {
	// Apply puts the change in the task's tree. It runs after the task's
	// start snapshot, so reviews and fix agents see it as the task's work.
	Apply func() error
	// Tests are the independent tests (nil: none).
	Tests *ReqTests
	// Summary is what the step reports as its result.
	Summary string
}

func replayPlan() Plan {
	return Plan{Summary: "a saved change, replayed", Subtasks: []Subtask{{ID: "replay", Title: "The saved change", Kind: router.KindEdit}}}
}

// replayStep stands in for execute: it puts the change in place.
func (o *Orchestrator) replayStep(t *task, plan Plan) (map[string]stepResult, *ReqTests) {
	res := stepResult{ok: true, final: t.replay.Summary}
	if err := t.replay.Apply(); err != nil {
		res = stepResult{err: "replay: " + err.Error()}
		o.logf("replay: could not put the change in place: %v", err)
	}
	return map[string]stepResult{plan.Subtasks[0].ID: res}, t.replay.Tests
}
