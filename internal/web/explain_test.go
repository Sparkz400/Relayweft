package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func TestExplainEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	d, _ := os.UserConfigDir()
	d = filepath.Join(d, "relayweft", "tasks")
	os.MkdirAll(d, 0o700)
	now := time.Now()
	for _, s := range []orchestrator.TaskState{
		{ID: "explain-done", Dir: e.srv.opt.Dir, Task: "fix the parser", Status: "done", Created: now.Add(-time.Hour), Updated: now.Add(-50 * time.Minute),
			Plan: &orchestrator.Plan{Subtasks: []orchestrator.Subtask{{ID: "one"}}}},
		{ID: "elsewhere", Dir: t.TempDir(), Task: "other project", Status: "failed", Created: now},
	} {
		data, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(d, s.ID+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var v struct {
		ID, Task, Status string
		Headline         string   `json:"headline"`
		RunLabels        []string `json:"run_labels"`
		Notes            []string
	}
	for _, id := range []string{"explain-done", "last"} {
		res, data := e.do("GET", "/api/explain/"+id, nil, nil)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", id, res.StatusCode, data)
		}
		if err := json.Unmarshal(data, &v); err != nil || v.ID != "explain-done" || v.Task != "fix the parser" || v.Headline == "" || v.RunLabels == nil || len(v.Notes) == 0 {
			t.Fatalf("%s: %v %s", id, err, data)
		}
	}
	// A task of another project is readable by id, as in History's "all projects".
	if res, data := e.do("GET", "/api/explain/elsewhere", nil, nil); res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, data)
	}
	for id, code := range map[string]int{"missing-task": 404, "a..b": 400} {
		if res, _ := e.do("GET", "/api/explain/"+id, nil, nil); res.StatusCode != code {
			t.Errorf("%s: %d, want %d", id, res.StatusCode, code)
		}
	}
	if res, _ := e.do("GET", "/api/explain/last", nil, map[string]string{SessionHeader: ""}); res.StatusCode == 200 {
		t.Fatal("unauthenticated access")
	}
}
