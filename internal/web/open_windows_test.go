//go:build windows

package web

import (
	"testing"

	"golang.org/x/sys/windows"

	"github.com/sparkz400/switchyard/internal/proc"
)

// The browser sy web / sy app opens must leave sy's kill-on-close job, or
// closing sy kills the user's whole browser (found on a real desktop).
func TestStartDetachedBreaksAwayFromJob(t *testing.T) {
	if err := proc.Guard(); err != nil {
		t.Fatal(err)
	}
	cmd, err := startDetachedCmd("cmd.exe", []string{"/c", "exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB == 0 {
		t.Error("the browser was started inside sy's job: it would be killed when sy exits")
	}
}
