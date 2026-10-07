//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// writeAgentShim writes a script called name that starts the rw copy as
// the scripted agent. A name other than claude is passed on in
// RW_SELFTEST_AS.
func writeAgentShim(dir, bin, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	shim := filepath.Join(dir, name)
	as := ""
	if name != "claude" {
		as = envSelftestAs + "=" + name + "; export " + envSelftestAs + "\n"
	}
	body := "#!/bin/sh\n" + as + "exec '" + bin + "' " + selftestAgentCmd + " \"$@\"\n"
	return shim, os.WriteFile(shim, []byte(body), 0o755)
}

func (t *selftest) defender() {}

// Closing a console window mid-task is a Windows check
// (selftest_close_windows.go).
func cmdSelftestConsole([]string) { os.Exit(2) }

func (t *selftest) closeScenarios() {}
