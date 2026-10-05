package orchestrator

import (
	"context"
	"testing"
	"time"
)

// runBG starts run in the background and returns its result channel and a
// cancel for its context. Whatever the test does, even a failed assertion
// that returns at once, the run is stopped (cancelled, then unblock) and
// waited for before newOrc's cleanup closes the event channel: a task
// still running then would panic with a send on a closed channel and hide
// the real failure.
func runBG(t *testing.T, run func(ctx context.Context) TaskResult, unblock func()) (<-chan TaskResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan TaskResult, 1)
	stopped := make(chan struct{})
	go func() {
		out <- run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		if unblock != nil {
			unblock()
		}
		select {
		case <-stopped:
		case <-time.After(60 * time.Second):
			t.Error("the task did not stop after its test ended")
		}
	})
	return out, cancel
}

// waitFor polls cond every 10ms until it holds or d passes.
func waitFor(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); ; {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
