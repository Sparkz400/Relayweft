package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A finished command leaves no zombie in its group, also where the
// orphans' new parent never reaps them (rw as pid 1 in a container). A
// zombie watcher kept the group alive: the next run saw the slot's agent
// as still running.
func TestWrapperLeavesNoZombie(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperNoReaper$")
	cmd.Env = append(os.Environ(), "RW_PROC_HELPER=noreaper")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestHelperNoReaper adopts orphans (a subreaper) and never reaps them, as
// a container's pid 1 may not.
func TestHelperNoReaper(t *testing.T) {
	if os.Getenv("RW_PROC_HELPER") != "noreaper" {
		t.Skip("helper process")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Println("prctl:", err)
		os.Exit(1)
	}
	if err := Guard(); err != nil {
		fmt.Println("guard:", err)
		os.Exit(1)
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0")
	Prepare(cmd)
	if err := cmd.Run(); err != nil {
		fmt.Println("run:", err)
		os.Exit(1)
	}
	pid := cmd.Process.Pid
	for end := time.Now().Add(2 * time.Second); groupAlive(pid); {
		if time.Now().After(end) {
			fmt.Println("the wrapper's group is still alive 2s after it ended (an unreaped watcher)")
			os.Exit(1)
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.Exit(0)
}
