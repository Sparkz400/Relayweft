package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
		"ok   orphans", "continued its agent's session in the folder it ran in"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
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
