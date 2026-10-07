package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemorySourcesStayInsideProject(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "private.txt")
	if err := os.WriteFile(outside, []byte("private content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local.txt"), []byte("project content"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newSources(root)
	if s.sum("local.txt") == "" {
		t.Fatal("local source was not fingerprinted")
	}
	for _, p := range []string{"../private.txt", outside, `..\private.txt`} {
		if got := s.sum(p); got != "" {
			t.Errorf("fingerprinted outside project: %q -> %q", p, got)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "external.txt")); err != nil {
		t.Logf("symlink creation unavailable: %v", err)
	} else if got := newSources(root).sum("external.txt"); got != "" {
		t.Errorf("followed a source symlink outside project: %q", got)
	}
}
