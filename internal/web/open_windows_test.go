//go:build windows

package web

import (
	"testing"

	"golang.org/x/sys/windows"

	"github.com/sparkz400/relayweft/internal/proc"
)

// The browser rw web / rw app opens must leave rw's kill-on-close job, or
// closing rw kills the user's whole browser (found on a real desktop).
func TestStartDetachedBreaksAwayFromJob(t *testing.T) {
	if err := proc.Guard(); err != nil {
		t.Fatal(err)
	}
	cmd, err := startDetachedCmd("cmd.exe", []string{"/c", "exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB == 0 {
		t.Error("the browser was started inside rw's job: it would be killed when rw exits")
	}
}
