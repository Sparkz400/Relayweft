package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
)

func TestSingleUndoKeepsUserEdits(t *testing.T) {
	dir := gitRepo(t)
	o, _ := newOrc(t, dir, both(func(s runner.Spec) runner.Result {
		for _, name := range []string{"agent.txt", "user.txt"} {
			if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(name), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return runner.Result{Final: "done", Files: []string{"agent.txt"}}
	}), nil)
	r := o.RunSingle(context.Background(), "edit the fixture", event.Codex, config.Route{Model: "fixture"})
	if !r.OK || r.UndoKey == "" {
		t.Fatalf("%+v", r)
	}
	if _, err := Undo(dir, r.UndoKey, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.txt")); !os.IsNotExist(err) {
		t.Fatal("single-agent undo did not undo the agent's file")
	}
	if _, err := os.Stat(filepath.Join(dir, "user.txt")); err != nil {
		t.Fatal("single-agent undo removed an unreported user file")
	}
}

func TestSingleCancellationCannotReportSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, _ := newOrc(t, "", both(func(runner.Spec) runner.Result { cancel(); return runner.Result{Final: "late success"} }), nil)
	if r := o.RunSingle(ctx, "task", event.Codex, config.Route{Model: "fixture"}); r.OK {
		t.Fatal("cancelled single-agent run reported success")
	}
}

func TestSingleUnavailableProvider(t *testing.T) {
	o, _ := newOrc(t, "", runner.Set{}, nil)
	if r := o.RunSingle(context.Background(), "task", "missing", config.Route{Model: "fixture"}); r.OK {
		t.Fatal("missing provider succeeded")
	}
}
