package health

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/diag"
)

// TestFatalCrashIsReported runs a process that dies of an unrecovered panic
// in a goroutine (nothing can recover that): the fatal-error file holds the
// runtime's report, and the health report counts it as a crash.
func TestFatalCrashIsReported(t *testing.T) {
	if dir := os.Getenv("RW_HEALTH_FATAL_DIR"); dir != "" {
		diag.Init(dir)
		diag.Start("run")
		go func() { panic("unrecoverable test crash") }()
		select {}
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFatalCrashIsReported$")
	cmd.Env = append(os.Environ(), "RW_HEALTH_FATAL_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("the child did not crash: %s", out)
	}
	r, err := Build(Options{Dir: dir, NoLeftovers: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Crashes) != 1 || r.Crashes[0].Kind != "fatal" || r.Crashes[0].Cmd != "run" || !strings.Contains(r.Crashes[0].Detail, "unrecoverable test crash") || len(r.Unclean) != 0 {
		t.Fatalf("crashes = %+v, unclean = %+v", r.Crashes, r.Unclean)
	}
}
