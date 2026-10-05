//go:build !windows

package web

import (
	"syscall"
	"testing"
)

// The browser rw web / rw app starts must not stay in rw's process group:
// Ctrl+C in the terminal (or closing it) signals the whole foreground
// group, and Chrome quits on SIGINT and SIGHUP, taking the user's other
// windows with it (found on a real Linux desktop).
func TestStartDetachedLeavesProcessGroup(t *testing.T) {
	cmd, err := startDetachedCmd("sleep", []string{"30"})
	if err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid == syscall.Getpgrp() {
		t.Error("the browser was started in rw's process group: Ctrl+C on rw would kill it")
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("browser process group = %d, want its own (%d)", pgid, cmd.Process.Pid)
	}
}
