package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// The selftest finds a child sy's task states where that sy keeps them:
// profileConfigDir must be what os.UserConfigDir says under profileEnv.
func TestProfileConfigDir(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "Test User (äö)")
	for _, kv := range profileEnv(profile) {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "APPDATA", "XDG_CONFIG_HOME", "HOME":
			t.Setenv(k, v)
		}
	}
	got, err := os.UserConfigDir()
	if err != nil || got != profileConfigDir(profile) {
		t.Errorf("UserConfigDir = %q (%v), profileConfigDir = %q", got, err, profileConfigDir(profile))
	}
}

// The kill waits until the task state records the agent's session, in
// the format sy writes it.
func TestWaitSessionSaved(t *testing.T) {
	cfg := t.TempDir()
	tasks := filepath.Join(cfg, "switchyard", "tasks")
	os.MkdirAll(tasks, 0o755)
	write := func(session string) {
		st := orchestrator.TaskState{ID: "t1", Status: "running", Running: map[string]orchestrator.StepRun{"c": {Provider: "claude", Session: session}}}
		b, _ := json.Marshal(st)
		os.WriteFile(filepath.Join(tasks, "t1.json"), b, 0o644)
	}
	write("")
	if _, ok := waitSessionSaved(cfg, "selftest-1", 100*time.Millisecond); ok {
		t.Fatal("found a session that is not saved")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		write("selftest-1")
	}()
	waited, ok := waitSessionSaved(cfg, "selftest-1", 10*time.Second)
	<-done
	if !ok || waited < 150*time.Millisecond {
		t.Errorf("waited %s, ok %v", waited, ok)
	}
}

// TestSelftest builds sy and runs `sy selftest` end to end: a real sy run
// killed while an agent works, sy history, sy resume, sy undo and redo, in
// a profile and project with spaces and non-ASCII letters (on Windows with
// the agent behind a .cmd shim, and a task inside a stand-in OneDrive).
func TestSelftest(t *testing.T) {
	if testing.Short() {
		t.Skip("builds sy and runs whole tasks")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "sy")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Built before isolate: a fresh build cache would rebuild everything.
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	isolate(t)
	args := []string{"selftest", "--files", "50", "--in", dir}
	oneDrive := filepath.Join(dir, "OneDrive - Test")
	if runtime.GOOS == "windows" {
		os.MkdirAll(oneDrive, 0o755)
		t.Setenv("OneDrive", oneDrive)
		t.Setenv("OneDriveCommercial", "")
		t.Setenv("OneDriveConsumer", "")
		args = append(args, "--onedrive")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil || strings.Contains(got, "FAIL") {
		t.Fatalf("sy selftest: %v\n%s", err, got)
	}
	// Every OS: the killed sy's agent is stopped (Windows: with sy, by the
	// job object; Linux and macOS: by the next sy, after checking it is
	// the same process), and the step continues in its own worktree.
	for _, want := range []string{"ok   kill", "ok   history", "ok   resume", "ok   undo", "ok   redo", "Still to do by hand",
		"ok   orphans", "continued its agent's session in the folder it ran in",
		"ok   first run 1 sy setup --yes: 0 question(s)", "ok   first run 2 sy run", "ok   first run 3 sy setup outside a repo"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	// The onboarding timings (Phase 2 exit criterion), shown with -v.
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "first run") {
			t.Log(strings.TrimSpace(l))
		}
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(got, "a repo inside OneDrive") {
			t.Errorf("output lacks %q:\n%s", "a repo inside OneDrive", got)
		}
		if es, _ := os.ReadDir(oneDrive); len(es) != 0 {
			t.Errorf("the OneDrive test repo was not removed: %v", es)
		}
	}
	// Passed: the work folder is removed.
	if ms, _ := filepath.Glob(filepath.Join(dir, "sy selftest *")); len(ms) != 0 {
		t.Errorf("work folder left behind: %v", ms)
	}
}

// TestOnboarding times the guided first run from fresh profiles (Phase 2
// exit criterion: from install to the first task in under 5 minutes):
// `sy setup --yes`, `sy run "task"` with no config (Enter twice), and
// `sy setup` outside a repo (the sample project), with scripted Claude and
// Codex CLIs. CI runs it with -v to print the timings.
func TestOnboarding(t *testing.T) {
	if testing.Short() {
		t.Skip("builds sy and runs whole tasks")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "sy")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	isolate(t)
	cmd := exec.Command(bin, "selftest", "--first-run", "--in", dir)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil || strings.Contains(got, "FAIL") {
		t.Fatalf("sy selftest --first-run: %v\n%s", err, got)
	}
	for i, c := range firstRunCases {
		want := fmt.Sprintf("ok   first run %d %s: %d question(s)", i+1, c.typed, c.questions)
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "first run") {
			t.Log(strings.TrimSpace(l))
		}
	}
}

func TestInOneDrive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("OneDrive detection is Windows only")
	}
	root := t.TempDir()
	od := filepath.Join(root, "OneDrive - Contoso")
	os.MkdirAll(filepath.Join(od, "repo"), 0o755)
	t.Setenv("OneDrive", "")
	t.Setenv("OneDriveConsumer", "")
	t.Setenv("OneDriveCommercial", od)
	if r, in := inOneDrive(filepath.Join(od, "repo")); !in || r != od {
		t.Errorf("repo in OneDrive: got %q %v", r, in)
	}
	if _, in := inOneDrive(strings.ToUpper(filepath.Join(od, "repo"))); !in {
		t.Error("case differences must not matter on Windows")
	}
	// A sibling whose name starts like the root is not inside it.
	os.MkdirAll(od+" old", 0o755)
	if _, in := inOneDrive(od + " old"); in {
		t.Error("sibling folder reported as inside OneDrive")
	}
	if _, in := inOneDrive(root); in {
		t.Error("parent reported as inside OneDrive")
	}
}
