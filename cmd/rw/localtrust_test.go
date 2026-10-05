package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
)

// A relayweft.yaml that came with a cloned repo: rw says what it ignores,
// `rw trust` shows and trusts it, `rw trust --revoke` takes that back.
func TestTrustLocalConfig(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	write(t, dir, config.FileName, "hooks:\n  after_task: [\"echo cloned\"]\n")

	_, _, ignored, err := config.LoadInfo("")
	if err != nil || len(ignored) != 1 || ignored[0] != "hooks" {
		t.Fatalf("before trust: ignored %v, err %v", ignored, err)
	}
	var note bytes.Buffer
	noteUntrustedLocal(&note, config.FileName, ignored)
	if !strings.Contains(note.String(), "sets hooks, which run commands") || !strings.Contains(note.String(), "rw trust") {
		t.Errorf("note: %q", note.String())
	}
	// Without the agent CLIs installed (CI), doctor also reports problems;
	// only its note matters here.
	_ = runDoctor(&note, "")
	if !strings.Contains(note.String(), "ignored (yours apply)") {
		t.Errorf("rw doctor does not say the hooks are ignored:\n%s", note.String())
	}

	out, err := captureStdout(t, func() error { return cmdTrust([]string{"--yes"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "echo cloned") || !strings.Contains(out, "trusted:") {
		t.Errorf("rw trust output:\n%s", out)
	}
	c, _, ignored, _ := config.LoadInfo("")
	if len(ignored) != 0 || len(c.Hooks.AfterTask) != 1 {
		t.Fatalf("after trust: ignored %v hooks %v", ignored, c.Hooks.AfterTask)
	}

	if _, err := captureStdout(t, func() error { return cmdTrust([]string{"--revoke"}) }); err != nil {
		t.Fatal(err)
	}
	if _, _, ignored, _ := config.LoadInfo(""); len(ignored) != 1 {
		t.Fatalf("after revoke: ignored %v", ignored)
	}
}

// rw init writes a relayweft.yaml with the detected checks: you made it,
// so its verify commands apply without rw trust.
func TestInitTrustsItsFile(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	write(t, dir, "go.mod", "module example.com/x\n\ngo 1.22\n")
	if _, err := captureStdout(t, func() error { return cmdInit(nil) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.FileName); err != nil {
		t.Fatal(err)
	}
	c, _, ignored, err := config.LoadInfo("")
	if err != nil || len(ignored) != 0 {
		t.Fatalf("ignored %v, err %v", ignored, err)
	}
	if len(c.Verify.Commands) == 0 {
		t.Error("detected verify commands not applied")
	}
}
