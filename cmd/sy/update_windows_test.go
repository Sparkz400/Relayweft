//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A second update while the binary moved aside by the first one still runs
// (sy web left open across two releases): the running .old can be neither
// deleted nor replaced on Windows, so the new swap must use another name.
func TestReplaceBinaryWhileOldStillRuns(t *testing.T) {
	ping, err := exec.LookPath("ping")
	if err != nil {
		t.Skip(err)
	}
	src, err := os.ReadFile(ping)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "sy.exe")
	os.WriteFile(exe, []byte("v2"), 0o755)
	// v1, moved aside by the first update, still running.
	old := exe + ".old"
	if err := os.WriteFile(old, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(old, "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	kept, err := replaceBinary(exe, []byte("v3"), true)
	if err != nil {
		t.Fatalf("second update failed while the old binary runs: %v", err)
	}
	if kept == old || readFile(t, kept) != "v2" {
		t.Errorf("previous binary reported as %s", kept)
	}
	if readFile(t, exe) != "v3" {
		t.Error("not replaced")
	}
	// v2 was moved aside under another name; the running v1 is untouched.
	matches, _ := filepath.Glob(exe + ".old*")
	var v2 bool
	for _, m := range matches {
		if b, _ := os.ReadFile(m); string(b) == "v2" {
			v2 = true
		}
	}
	if !v2 {
		t.Errorf("v2 not kept for rollback; have %v", matches)
	}

	// Once nothing runs them any more, the next start removes them all.
	cmd.Process.Kill()
	cmd.Wait()
	withTarget(t, exe)
	cleanupOldBinary()
	if left, _ := filepath.Glob(exe + ".old*"); len(left) != 0 {
		t.Errorf("cleanup left %s", strings.Join(left, ", "))
	}
}

func withTarget(t *testing.T, exe string) {
	old := updateTarget
	t.Cleanup(func() { updateTarget = old })
	updateTarget = func() (string, error) { return exe, nil }
}
