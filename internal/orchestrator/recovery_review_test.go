package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/runner"
)

// Completed steps must not be skipped after their changes have been undone.
func TestUndoneTaskCannotResumeUntilRedone(t *testing.T) {
	dir := gitRepo(t)
	st, _ := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
		if err := os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte("c1\n"), 0600); err != nil {
			t.Error(err)
		}
	})
	if _, err := Undo(dir, st.UndoKey, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := Recovery(dir)
	if err != nil || len(items) != 1 {
		t.Fatalf("recovery: %v %+v", err, items)
	}
	if items[0].CanResume || items[0].CanUndo || items[0].Status != "undone" {
		t.Errorf("undone work exposes recovery actions: %+v", items[0])
	}
	ran := false
	o, _ := newOrc(t, dir, both(func(s runner.Spec) runner.Result {
		ran = true
		if r, ok := midstepPlan(s); ok {
			return r
		}
		finishC(s.Dir)
		return runner.Result{Final: "finished c"}
	}), nil)
	res := o.RunWith(context.Background(), st.Task, TaskOptions{Resume: st, Force: true})
	if ran || res.OK || !strings.Contains(res.Summary, "undone") {
		t.Fatalf("undone task resumed and skipped reverted steps: ran=%v %+v", ran, res)
	}
	if _, err := Undo(dir, st.UndoKey, true, false); err != nil {
		t.Fatal(err)
	}
	items, err = Recovery(dir)
	if err != nil || !items[0].CanResume {
		t.Fatalf("redo did not restore recovery: %v %+v", err, items)
	}
	res = o.RunWith(context.Background(), st.Task, TaskOptions{Resume: st, Force: true})
	if !res.OK || !ran {
		t.Fatalf("redone task could not finish: %+v", res)
	}
	for _, name := range []string{"shared.txt", "b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("completed work missing after redo/resume: %s: %v", name, err)
		}
	}
}
