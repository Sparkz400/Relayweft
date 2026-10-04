package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/runner"
)

// A file the agent reported (by absolute path, as Claude Code does) is not
// unreported because of its letter case: on Windows the recorded list is
// lower-cased (canon.Path), and every file with an upper-case letter used to
// count as an edit nobody reported, which stopped unattended pull requests
// and sy watch pushes.
func TestReportedFileWithUpperCaseIsNotUnreported(t *testing.T) {
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, "[SY:STEP]") {
			p := filepath.Join(s.Dir, "Notes.MD")
			os.WriteFile(p, []byte("by agent\n"), 0o644)
			return runner.Result{Final: "ok", Files: []string{p}}
		}
		return approve()
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.ReviewBeforeDone = false })
	o.Run(context.Background(), "add notes file")
	plan, err := PreviewUndo(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || len(plan.Unreported) != 0 {
		t.Fatalf("changes = %v, unreported = %v", plan.Changes, plan.Unreported)
	}
}
