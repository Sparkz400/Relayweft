package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
)

type recoveryEventRunner func(context.Context, runner.Spec, func(event.Event)) runner.Result

func (f recoveryEventRunner) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	return f(ctx, s, emit)
}

// A killed process never returns Result.Files. Persist streamed file reports so
// undo still covers the first half of a resumed step, while keeping user edits.
func TestResumeKeepsStreamedFilesBeforeHardKill(t *testing.T) {
	dir := gitRepo(t)
	started := make(chan struct{}, 1)
	rn := recoveryEventRunner(func(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
		if s.Role == "planner" {
			return runner.Result{Final: planJSON(map[string]any{"id": "write", "title": "write markers", "kind": "edit", "prompt": "write markers"})}
		}
		if s.ReadOnly {
			return runner.Result{Final: "ok"}
		}
		name := "early.txt"
		if s.Resume != "" {
			name = "late.txt"
		}
		if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(name), 0600); err != nil {
			return runner.Result{Err: err}
		}
		emit(event.Event{Kind: event.FileEdit, AgentID: s.AgentID, Text: filepath.Join(s.Dir, name)})
		if s.Resume != "" {
			return runner.Result{Final: "finished", Files: []string{name}}
		}
		s.OnSession("interrupted-session")
		started <- struct{}{}
		<-ctx.Done()
		return runner.Result{Err: errors.New("killed"), Killed: true}
	})
	o, _ := newOrc(t, dir, runner.Set{event.Codex: rn, event.Claude: rn}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() { done <- o.RunWith(ctx, "write recovery markers", TaskOptions{}) }()
	select {
	case <-started:
	case r := <-done:
		t.Fatalf("ended before interruption: %+v", r)
	case <-time.After(time.Minute):
		t.Fatal("writer did not start")
	}
	id := History(dir, 1)[0].ID
	saved, err := os.ReadFile(statePath(id))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	st, err := LoadTask(id)
	if err != nil {
		t.Fatal(err)
	}
	// No after snapshot is produced when the process is forcibly killed.
	git{dir}.deleteSnapshot(st.UndoKey, "after")
	if err := os.WriteFile(statePath(id), saved, 0600); err != nil {
		t.Fatal(err)
	}
	st, err = LoadTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user.txt"), []byte("keep user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := o.RunWith(context.Background(), st.Task, TaskOptions{Resume: st}); !r.OK {
		t.Fatalf("resume: %+v", r)
	}
	if _, err := Undo(dir, st.UndoKey, false, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"early.txt", "late.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("agent file survived undo: %s: %v", name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "user.txt")); err != nil || string(b) != "keep user edit" {
		t.Fatalf("concurrent user edit changed: %q %v", b, err)
	}
}
