package orchestrator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchEvidenceIncludesUntrackedBinaryAndCandidates(t *testing.T) {
	dir := gitRepo(t)
	ws, err := NewBenchWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Reset(headOf(t, dir)); err != nil {
		t.Fatal(err)
	}
	base, err := ws.StartCommit()
	if err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(ws.Path, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "new binary.bin"), []byte{0, 1, 2, 3, 4}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "new text.txt"), []byte("evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := (git{ws.Path}).snapshot("candidate")
	if err != nil {
		t.Fatal(err)
	}
	tgit(t, ws.Path, "update-ref", "refs/heads/rw/kept", snap)
	out := t.TempDir()
	if err := ws.Evidence(base, out); err != nil {
		t.Fatal(err)
	}
	indexAfter, _ := os.ReadFile(filepath.Join(ws.Path, ".git", "index"))
	if !bytes.Equal(index, indexAfter) {
		t.Fatal("evidence changed staging")
	}
	if headOf(t, ws.Path) != base {
		t.Fatal("evidence changed HEAD")
	}
	for _, name := range []string{"solution.patch", "candidate-001.patch"} {
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || !strings.Contains(string(b), "GIT binary patch") || !strings.Contains(string(b), "+evidence") {
			t.Fatalf("lost evidence %s: %s %v", name, b, err)
		}
	}
	if err := ws.Clear(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "candidates.txt")); err != nil || !strings.Contains(string(b), "refs/heads/rw/kept") {
		t.Fatal("candidate evidence lost after clear", err)
	}
}
