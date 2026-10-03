//go:build !windows

package notify

import "os/exec"

// hideWindow is a no-op: only Windows pops up console windows.
func hideWindow(*exec.Cmd) {}
