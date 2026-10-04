//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// writeAgentShim writes a claude script that starts the sy copy as the
// scripted agent.
func writeAgentShim(dir, bin string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	shim := filepath.Join(dir, "claude")
	body := "#!/bin/sh\nexec '" + bin + "' " + selftestAgentCmd + " \"$@\"\n"
	return shim, os.WriteFile(shim, []byte(body), 0o755)
}

func (t *selftest) defender() {}
