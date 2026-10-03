package orchestrator

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A file that no longer exists (an agent deleted it) canonicalizes like the
// folder it was in, also when that folder is reached through a symlink
// (macOS: /var -> /private/var).
func TestCanonPathDeletedFileThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(link, "sub", "gone.txt")
	rel, err := filepath.Rel(canonPath(real), canonPath(gone))
	if err != nil || filepath.ToSlash(rel) != "sub/gone.txt" {
		t.Fatalf("rel = %q, %v", rel, err)
	}
}
