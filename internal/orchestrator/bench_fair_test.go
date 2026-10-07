package orchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFairBenchClearsIgnoredAnswers(t *testing.T) {
	dir := gitRepo(t)
	ws, err := NewBenchWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := ws.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := ws.Reset(headOf(t, dir)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(ws.Path, "ignored-answer")
	if err := os.WriteFile(p, []byte("previous agent's solution"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, ".git", "info", "exclude"), []byte("ignored-answer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ws.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := ws.Reset(headOf(t, dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("answer leaked to next run")
	}
	// Clear must refuse an accidental assignment to the user's repository.
	ws.Path = dir
	if err := ws.Clear(); err == nil {
		t.Fatal("clear accepted a path outside its cache")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal("user repository changed")
	}
}
