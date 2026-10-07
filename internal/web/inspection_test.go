package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func TestInspectionEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	dir, _ := os.UserConfigDir()
	dir = filepath.Join(dir, "relayweft", "tasks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, st := range []orchestrator.TaskState{
		{ID: "inspect-failed", Dir: e.srv.opt.Dir, Task: "Repair <script> parser", Status: "failed", Created: now.Add(-time.Hour), Updated: now,
			Summary: "Checks failed", CostLine: "cost was not recorded",
			Plan:       &orchestrator.Plan{Subtasks: []orchestrator.Subtask{{ID: "parser", Title: "Fix parser"}}},
			Results:    map[string]orchestrator.StepState{"parser": {OK: false, Err: "compile failed"}},
			Acceptance: &orchestrator.Acceptance{Requirements: orchestrator.Level{Status: "fail"}, Criteria: []orchestrator.Criterion{{ID: "R1", Text: "Keep empty fields", Status: "unmet"}}},
			Kept:       []string{"rw/kept"}, Branches: []string{"rw/kept", "rw/alternative"}},
		{ID: "inspect-elsewhere", Dir: t.TempDir(), Task: "Other project", Status: "failed", Created: now},
		// Created more recently, but the older task above finished last after
		// resuming. The latest result must select its outcome.
		{ID: "inspect-newer", Dir: e.srv.opt.Dir, Task: "Newer task", Status: "done", Created: now.Add(-30 * time.Minute), Updated: now.Add(-time.Minute)},
	} {
		data, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, st.ID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"inspect-failed", "last"} {
		res, data := e.do("GET", "/api/results/"+id, nil, nil)
		var v inspectionView
		if res.StatusCode != 200 || json.Unmarshal(data, &v) != nil {
			t.Fatalf("%s: %d %s", id, res.StatusCode, data)
		}
		if v.Report.ID != "inspect-failed" || v.Report.Steps[0].Err != "compile failed" || v.Report.Acceptance.Criteria[0].Status != "unmet" {
			t.Fatalf("lost result evidence: %s", data)
		}
		if !v.Here || !v.Recovery.CanResume || v.Recovery.CanUndo || len(v.Recovery.Branches) != 2 {
			t.Fatalf("wrong recovery actions: %s", data)
		}
		if v.Report.Diff != nil || len(v.Report.Checks) != 0 || len(v.Report.Notes) == 0 {
			t.Fatalf("missing evidence was not preserved: %s", data)
		}
	}
	res, data := e.do("GET", "/api/results/inspect-elsewhere", nil, nil)
	var v inspectionView
	if res.StatusCode != 200 || json.Unmarshal(data, &v) != nil || v.Here || v.Recovery.CanResume || v.Recovery.CanUndo {
		t.Fatalf("cross-project actions: %d %s", res.StatusCode, data)
	}
	// Add the task's actual log format and verify the aggregate endpoint keeps
	// routing reasons, check attempts and usage alongside the saved requirements.
	logs := e.srv.store.Get().SessionDir()
	if err := os.MkdirAll(logs, 0700); err != nil {
		t.Fatal(err)
	}
	var content []byte
	for _, record := range []sessionlog.Record{
		{Type: sessionlog.TypeDecision, Step: "parser", Provider: "codex", Model: "fixture", Rule: "default", Reason: "worker route"},
		{Type: "verify", Text: "go test ./...", OK: sessionlog.Bool(false), Kind: "full"},
		{Type: "verify", Text: "go test ./...", OK: sessionlog.Bool(true), Kind: "full"},
		{Type: sessionlog.TypeTaskEnd, Tokens: &event.TokenUsage{Input: 1234, Output: 50, CostUSD: 0.01}},
	} {
		record.Session, record.TaskID = "inspect", "failed"
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(logs, "inspect.jsonl"), content, 0600); err != nil {
		t.Fatal(err)
	}
	res, data = e.do("GET", "/api/results/inspect-failed", nil, nil)
	if res.StatusCode != 200 || json.Unmarshal(data, &v) != nil || len(v.Report.Checks) != 2 || len(v.Report.Routes) != 1 {
		t.Fatalf("missing log evidence: %d %s", res.StatusCode, data)
	}
	if v.Report.Routes[0].Reason != "worker route" || v.Report.Tokens.Input != 1234 || v.Report.Why == nil || v.Report.Acceptance == nil {
		t.Fatalf("incomplete aggregate: %s", data)
	}
	for id, code := range map[string]int{"missing": 404, "a..b": 400} {
		if res, _ := e.do("GET", "/api/results/"+id, nil, nil); res.StatusCode != code {
			t.Errorf("%s: %d, want %d", id, res.StatusCode, code)
		}
	}
	if res, _ := e.do("GET", "/api/results/last", nil, map[string]string{SessionHeader: ""}); res.StatusCode == 200 {
		t.Fatal("unauthenticated result access")
	}
}
