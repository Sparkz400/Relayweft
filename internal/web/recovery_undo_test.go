package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sysload"
)

type recoveryWriter struct{}

func (recoveryWriter) Run(_ context.Context, s runner.Spec, _ func(event.Event)) runner.Result {
	for _, name := range []string{"agent.txt", "concurrent-user.txt"} {
		if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(name), 0600); err != nil {
			return runner.Result{Err: err}
		}
	}
	return runner.Result{Final: "written", Files: []string{"agent.txt"}}
}

func TestRecoveryUndoRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	dir := e.srv.opt.Dir
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
	}
	cfg := config.Default()
	cfg.Orchestrator.Classic()
	cfg.Orchestrator.Handoff = false
	cfg.Orchestrator.Worktrees = false
	cfg.Orchestrator.ReviewBeforeDone = false
	cfg.Orchestrator.ApprovePlan = false
	orc := orchestrator.New(orchestrator.Options{Dir: dir, Store: config.NewStore(cfg, ""), Mode: "single", Load: func() sysload.Sample { return sysload.Sample{} }, Runners: func(*config.Config) runner.Set { return runner.Set{event.Codex: recoveryWriter{}} }})
	result := orc.Run(context.Background(), "edit the fixture")
	if !result.OK || result.UndoKey == "" {
		t.Fatalf("task failed: %+v", result)
	}
	items, err := orchestrator.Recovery(dir)
	if err != nil || len(items) != 1 || !items[0].CanUndo {
		t.Fatalf("%v %+v", err, items)
	}
	req := map[string]any{"id": items[0].ID}
	// Inspect the real task snapshots before and after undo, through the same
	// authenticated API used by the result screen.
	inspect := func(undone bool) {
		t.Helper()
		res, data := e.do("GET", "/api/results/"+items[0].ID, nil, nil)
		var v inspectionView
		if res.StatusCode != 200 || json.Unmarshal(data, &v) != nil || v.Report.Diff == nil {
			t.Fatalf("inspection: %d %s", res.StatusCode, data)
		}
		if len(v.Report.Diff.Files) != 2 || v.Report.Diff.Undone != undone || v.Recovery.CanUndo == undone {
			t.Fatalf("inspection snapshots: %s", data)
		}
		if undone && (v.Recovery.Status != "undone" || v.Recovery.CanResume) {
			t.Fatalf("undone task recovery: %s", data)
		}
	}
	inspect(false)
	res, data := e.do("POST", "/api/recovery/undo", req, nil)
	if res.StatusCode != 200 {
		t.Fatalf("preview %d: %s", res.StatusCode, data)
	}
	var preview struct {
		Plan orchestrator.UndoPlan `json:"plan"`
	}
	if err := json.Unmarshal(data, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Plan.TotalChanges() != 2 || len(preview.Plan.Unreported) != 1 {
		t.Fatalf("preview: %+v", preview.Plan)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.txt")); err != nil {
		t.Fatal("preview changed the tree")
	}
	// The same guarded endpoint refuses untrusted origins and missing sessions.
	req["apply"] = true
	for _, h := range []map[string]string{{SessionHeader: ""}, {"Origin": "https://attacker.invalid"}} {
		res, _ = e.do("POST", "/api/recovery/undo", req, h)
		if res.StatusCode < 400 {
			t.Fatal("unguarded undo")
		}
	}
	res, data = e.do("POST", "/api/recovery/undo", req, nil)
	if res.StatusCode != 200 {
		t.Fatalf("apply %d: %s", res.StatusCode, data)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.txt")); !os.IsNotExist(err) {
		t.Fatal("agent file remains")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "concurrent-user.txt")); err != nil || string(b) != "concurrent-user.txt" {
		t.Fatal("unreported user change lost")
	}
	res, _ = e.do("POST", "/api/recovery/undo", req, nil)
	if res.StatusCode != 409 {
		t.Fatal("repeated undo accepted")
	}
	items, err = orchestrator.Recovery(dir)
	if err != nil || items[0].CanUndo {
		t.Fatal("undone task still offers undo")
	}
	inspect(true)
}
