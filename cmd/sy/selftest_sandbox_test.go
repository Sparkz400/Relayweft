package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/sandbox"
)

// TestSandboxSelftest builds sy and runs `sy selftest --sandbox only`: real
// sy runs with the agent (a scripted stand-in in a small test image) in a
// docker container, with pool worktrees, sy's verify command in the
// container, a step stopped by its timeout and an sy killed hard. It needs
// docker (or podman) with Linux containers and is skipped without, unless
// SY_TEST_SANDBOX=require (CI on Linux).
func TestSandboxSelftest(t *testing.T) {
	if testing.Short() {
		t.Skip("builds sy and an image, runs whole tasks")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	_, whyDocker := sandbox.Usable("docker")
	_, whyPodman := sandbox.Usable("podman")
	if whyDocker != "" && whyPodman != "" {
		if os.Getenv("SY_TEST_SANDBOX") == "require" {
			t.Fatalf("SY_TEST_SANDBOX=require, but: %s; %s", whyDocker, whyPodman)
		}
		t.Skipf("%s; %s", whyDocker, whyPodman)
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
	cmd := exec.Command(bin, "selftest", "--files", "20", "--in", dir, "--sandbox", "only")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil || strings.Contains(got, "FAIL") || strings.Contains(got, "warn ") {
		t.Fatalf("sy selftest --sandbox only: %v\n%s", err, got)
	}
	for _, want := range []string{"finished the task with every agent and the verify command in a container",
		"git worked read-only in pool worktrees", "timeout stopped its container", "sy killed hard"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
