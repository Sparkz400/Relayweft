//go:build windows

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func isProcessInJob(p, j windows.Handle, in *bool) error {
	var r int32
	if ok, _, err := procIsProcessInJob.Call(uintptr(p), uintptr(j), uintptr(unsafe.Pointer(&r))); ok == 0 {
		return err
	}
	*in = r != 0
	return nil
}

// A browser rw opens must not join rw's kill-on-close job: found on a real
// desktop, where `rw app` started Edge, the user opened another Edge window
// (it lives in the same process) and it was killed when rw exited. Agents
// (no Breakaway) must stay in the job.
func TestBreakawayLeavesGuardJob(t *testing.T) {
	if job == 0 {
		// An outer job (CI runner, terminal) that forbids breakaway makes
		// CREATE_BREAKAWAY_FROM_JOB fail whatever rw's own job allows.
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		var inJob bool
		isProcessInJob(windows.CurrentProcess(), 0, &inJob)
		if inJob {
			err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
				uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
			if err == nil && info.BasicLimitInformation.LimitFlags&(windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK|windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK) == 0 {
				t.Skip("this test runs in a job that forbids breakaway")
			}
		}
	}
	if err := Guard(); err != nil {
		t.Fatal(err)
	}
	start := func(away bool) *exec.Cmd {
		cmd := exec.Command("cmd.exe", "/c", "ping -n 30 127.0.0.1 >nul")
		background(cmd)
		if away {
			Breakaway(cmd)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start (breakaway=%v): %v", away, err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	inGuardJob := func(cmd *exec.Cmd) bool {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(h)
		var in bool
		if err := isProcessInJob(h, job, &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	if inGuardJob(start(true)) {
		t.Error("a Breakaway child is in rw's kill-on-close job; it would die with rw")
	}
	if !inGuardJob(start(false)) {
		t.Error("an ordinary child left rw's job; it would outlive rw")
	}
}

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
