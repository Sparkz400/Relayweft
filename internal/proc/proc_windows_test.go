//go:build windows

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An npm-style .cmd shim in a directory with a space, called with quoted
// arguments and a prompt on stdin, as the runners do.
func TestCmdShimWithSpacesAndQuotedArgs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Nico Schu", "npm")
	os.MkdirAll(dir, 0o755)
	shim := filepath.Join(dir, "fakecli.cmd")
	script := "@echo off\r\n" +
		"echo %* > \"%~dp0args.txt\"\r\n" +
		"findstr \"^\" > \"%~dp0stdin.txt\"\r\n" +
		"echo {\"ok\":true}\r\n"
	if err := os.WriteFile(shim, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "my repo")
	os.MkdirAll(work, 0o755)
	path, err := exec.LookPath(filepath.Join(dir, "fakecli"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), path, "exec", "-c", "model_reasoning_effort=high",
		"--allowedTools", "Bash(go test *)", "--add-dir", work)
	Prepare(cmd)
	cmd.Dir = work
	cmd.Stdin = strings.NewReader("line one\nline \"two\" & more\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `{"ok":true}`) {
		t.Errorf("stdout = %q", out)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
	for _, want := range []string{"model_reasoning_effort=high", "Bash(go test *)", work} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	in, _ := os.ReadFile(filepath.Join(dir, "stdin.txt"))
	if !strings.Contains(string(in), `line "two" & more`) {
		t.Errorf("stdin = %q", in)
	}
}
