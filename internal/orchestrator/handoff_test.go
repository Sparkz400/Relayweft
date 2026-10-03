package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoMap(t *testing.T) {
	dir := gitRepo(t)
	for _, f := range []string{"cmd/sy/main.go", "internal/a/a.go", "internal/b/b.go", "internal/b/b_test.go", "docs/x.md"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	g := git{dir}
	g.out("add", "-A")
	g.out("commit", "-qm", "files")
	m := repoMap(dir)
	for _, want := range []string{"internal/ 3 files (.go)", "cmd/ 1 files (.go)", "docs/ 1 files (.md)", "Root files:"} {
		if !strings.Contains(m, want) {
			t.Errorf("map lacks %q:\n%s", want, m)
		}
	}
	if strings.Index(m, "internal/") > strings.Index(m, "cmd/") {
		t.Errorf("biggest directory not first:\n%s", m)
	}
}

func TestRepoNotes(t *testing.T) {
	root := t.TempDir()
	if repoNotes(root) != "" {
		t.Fatal("notes before any task")
	}
	for i := 0; i < notesKeep+3; i++ {
		addRepoNote(root, fmt.Sprintf("task %d\nwith a second line", i), "ok", []string{"a.go"})
	}
	notes := repoNotes(root)
	if strings.Count(notes, "- ") != notesInclude || !strings.Contains(notes, fmt.Sprintf("task %d with a second line", notesKeep+2)) {
		t.Fatalf("notes:\n%s", notes)
	}
	data, _ := os.ReadFile(notesPath(root))
	if n := len(splitNotes(string(data))); n != notesKeep {
		t.Fatalf("kept %d entries", n)
	}
	if repoNotes(t.TempDir()) != "" {
		t.Fatal("notes leaked to another repo")
	}
}
