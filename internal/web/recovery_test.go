package web

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func TestMemoryWebRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	cmd := exec.Command("git", "init", "-q", e.srv.opt.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	res, data := e.do("GET", "/api/memory", nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%s", data)
	}
	var v orchestrator.MemoryView
	json.Unmarshal(data, &v)
	res, data = e.do("POST", "/api/memory", map[string]any{"revision": v.Revision, "text": "Remember <script> is text."}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%s", data)
	}
	res, _ = e.do("POST", "/api/memory", map[string]any{"revision": v.Revision, "text": "stale"}, nil)
	if res.StatusCode != 409 {
		t.Fatal("stale update accepted")
	}
	json.Unmarshal(data, &v)
	res, data = e.do("POST", "/api/memory", map[string]any{"revision": v.Revision, "id": v.Entries[0].ID, "delete": true}, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%s", data)
	}
	for _, path := range []string{"/api/memory", "/api/recovery"} {
		res, _ = e.do("GET", path, nil, map[string]string{SessionHeader: ""})
		if res.StatusCode == 200 {
			t.Fatal("unauthenticated access", path)
		}
	}
}

func TestResumeCannotCrossProjects(t *testing.T) {
	e := newEnv(t, nil)
	d, _ := os.UserConfigDir()
	d = filepath.Join(d, "relayweft", "tasks")
	os.MkdirAll(d, 0700)
	s := orchestrator.TaskState{ID: "other-project", Dir: t.TempDir(), Task: "must not run here", Status: "failed"}
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(d, s.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/resume", "/api/recovery/undo"} {
		res, data := e.do("POST", path, map[string]string{"id": s.ID}, nil)
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("%s: %d %s", path, res.StatusCode, data)
		}
	}
	if e.srv.snapshot().Running {
		t.Fatal("started a task from a different project")
	}
}
