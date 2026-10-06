//go:build !windows

package orchestrator

import (
	"os"
	"testing"
)

// holdUnreadable makes path unreadable, so git cannot read it until
// release.
func holdUnreadable(t *testing.T, path string) (release func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chmod(path, 0o644) }
}
