package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

func TestReportCommand(t *testing.T) {
	home := isolate(t)
	dir := t.TempDir()
	chdir(t, dir)
	if err := cmdReport(nil); err == nil || !strings.Contains(err.Error(), "no task") {
		t.Fatalf("no task: %v", err)
	}
	cfgDir, _ := os.UserConfigDir()
	st := orchestrator.TaskState{ID: "20260101-120000-abcd-task-1", Task: "make <it> work", Dir: dir, Mode: "routed",
		Status: "done", Created: time.Now().Add(-time.Minute), Updated: time.Now(), CostLine: "codex 1.2k fresh tokens"}
	data, _ := json.Marshal(st)
	os.MkdirAll(filepath.Join(cfgDir, "switchyard", "tasks"), 0o755)
	if err := os.WriteFile(filepath.Join(cfgDir, "switchyard", "tasks", st.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Default: the last task here, into the reports folder.
	if err := cmdReport(nil); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(cfgDir, "switchyard", "reports", st.ID+".html")
	page, err := os.ReadFile(def)
	if err != nil || !strings.Contains(string(page), "make &lt;it&gt; work") || !strings.Contains(string(page), "codex 1.2k") {
		t.Fatalf("default report %s: %v\n%s", def, err, page)
	}
	if !strings.HasPrefix(def, home) {
		t.Fatalf("report outside the isolated config dir: %s", def)
	}
	// By id, Markdown, flags after the id.
	out := filepath.Join(t.TempDir(), "my report.md")
	if err := cmdReport([]string{st.ID, "--md", "--out", out}); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(out)
	if !strings.HasPrefix(string(md), "# Switchyard task report") || !strings.Contains(string(md), "make <it> work") {
		t.Fatalf("markdown:\n%s", md)
	}
	if err := cmdReport([]string{"nope"}); err == nil {
		t.Fatal("unknown task accepted")
	}
	if p, _ := reportPath(`a/b\c:d`, true); filepath.Base(p) != "a_b_c_d.md" {
		t.Errorf("unsafe id path: %s", p)
	}
}

func TestDoctorListsMCP(t *testing.T) {
	isolate(t)
	t.Setenv("SY_DOCTOR_SECRET", "hunter2")
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	cfg := config.Default()
	cfg.MCP.Servers = map[string]config.MCPServer{
		"gitsrv":  {Command: "git", Env: map[string]string{"TOKEN": "${SY_DOCTOR_SECRET}", "LIT": "plain-secret"}},
		"missing": {Command: "definitely-not-a-real-mcp-xyz"},
		"remote":  {URL: "https://mcp.example.com/v1?token=${SY_DOCTOR_SECRET}", Headers: map[string]string{"Authorization": "Bearer abc"}},
		"unset":   {URL: "http://localhost:1/mcp", Headers: map[string]string{"X": "${SY_DOCTOR_UNSET}"}},
	}
	ok := func(b bool) string { return map[bool]string{true: "ok  ", false: "FAIL"}[b] }
	var buf bytes.Buffer
	problems := doctorMCP(&buf, cfg, dir, ok, "warn")
	out := buf.String()
	if problems != 1 {
		t.Errorf("problems = %d\n%s", problems, out)
	}
	for _, want := range []string{"4 server(s) for worker, worker_high, explorer, researcher", "ok     gitsrv", "FAIL   missing", `"definitely-not-a-real-mcp-xyz" not found`,
		"http https://mcp.example.com", "warn   unset", "${SY_DOCTOR_UNSET}"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"hunter2", "plain-secret", "Bearer", "token="} {
		if strings.Contains(out, secret) {
			t.Errorf("doctor leaks %q:\n%s", secret, out)
		}
	}
	// An untrusted repo file's servers are named as ignored.
	os.WriteFile(filepath.Join(dir, config.RepoFileName), []byte("mcp: {servers: {repo: {command: evil}}}\n"), 0o644)
	buf.Reset()
	doctorMCP(&buf, config.Default(), dir, ok, "warn")
	if !strings.Contains(buf.String(), "sy trust") || strings.Contains(buf.String(), "evil") {
		t.Errorf("untrusted repo mcp:\n%s", buf.String())
	}
	// No servers: nothing printed.
	buf.Reset()
	if doctorMCP(&buf, config.Default(), t.TempDir(), ok, "warn"); buf.Len() != 0 {
		t.Errorf("no servers printed %q", buf.String())
	}
}
