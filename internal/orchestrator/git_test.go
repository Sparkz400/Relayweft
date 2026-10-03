package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupApply makes a repo, snapshots it, and creates a second commit where
// file.txt is changed in a worktree, returning (repo, from, to).
func setupApply(t *testing.T, autocrlf bool, content, changed string) (string, string, string) {
	t.Helper()
	dir := gitRepo(t)
	g := git{dir}
	if autocrlf {
		g.out("config", "core.autocrlf", "true")
	}
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte(content), 0o644)
	from, err := g.snapshot("from")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.addWorktree(wt, from); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.removeWorktree(wt) })
	os.WriteFile(filepath.Join(wt, "file.txt"), []byte(changed), 0o644)
	os.WriteFile(filepath.Join(wt, "new.txt"), []byte("new\n"), 0o644)
	os.Remove(filepath.Join(wt, "shared.txt"))
	to, _, err := (git{wt}).commitAll("change")
	if err != nil {
		t.Fatal(err)
	}
	return dir, from, to
}

func TestApplyDiffWritesAddsAndDeletes(t *testing.T) {
	dir, from, to := setupApply(t, false, "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	if err := (git{dir}).applyDiff(from, to); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "file.txt")); got != "one\nTWO\nthree\n" {
		t.Errorf("file.txt = %q", got)
	}
	if read(t, filepath.Join(dir, "new.txt")) != "new\n" {
		t.Error("new file not written")
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.txt")); !os.IsNotExist(err) {
		t.Error("deleted file still there")
	}
}

func TestApplyDiffCRLF(t *testing.T) {
	// With autocrlf the user's checkout has CRLF while blobs are LF.
	dir, from, to := setupApply(t, true, "one\r\ntwo\r\nthree\r\n", "one\r\nTWO\r\nthree\r\n")
	if err := (git{dir}).applyDiff(from, to); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(dir, "file.txt"))
	if strings.ReplaceAll(got, "\r\n", "\n") != "one\nTWO\nthree\n" {
		t.Errorf("file.txt = %q", got)
	}
}

func TestApplyDiffUserEditElsewhereMerges(t *testing.T) {
	dir, from, to := setupApply(t, false, "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\nTWO\n3\n4\n5\n6\n7\n8\n9\n")
	// The user edits a distant line of the same file while the agent works.
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("1\n2\n3\n4\n5\n6\n7\n8\nNINE\n"), 0o644)
	if err := (git{dir}).applyDiff(from, to); err != nil {
		t.Fatalf("3-way should merge: %v", err)
	}
	if got := read(t, filepath.Join(dir, "file.txt")); got != "1\nTWO\n3\n4\n5\n6\n7\n8\nNINE\n" {
		t.Errorf("file.txt = %q", got)
	}
}

func TestApplyDiffUserConflictIsNotClobbered(t *testing.T) {
	dir, from, to := setupApply(t, false, "a\nb\nc\n", "a\nAGENT\nc\n")
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("a\nUSER\nc\n"), 0o644)
	err := (git{dir}).applyDiff(from, to)
	if err == nil || !strings.Contains(err.Error(), "file.txt") {
		t.Fatalf("want a conflict naming file.txt, got %v", err)
	}
	if got := read(t, filepath.Join(dir, "file.txt")); got != "a\nUSER\nc\n" {
		t.Errorf("user's file changed: %q", got)
	}
	// Nothing else from the conflicting change may be half-applied.
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Error("new.txt was written although the change conflicted")
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.txt")); err != nil {
		t.Error("shared.txt was deleted although the change conflicted")
	}
}

func TestSnapshotLeavesIndexAlone(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "ignored"), 0o755)
	os.WriteFile(filepath.Join(dir, "ignored", "big.bin"), []byte("x"), 0o644)
	snap, err := (git{dir}).snapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	files, _ := (git{dir}).out("ls-tree", "-r", "--name-only", snap)
	if !strings.Contains(files, "untracked.txt") || strings.Contains(files, "big.bin") {
		t.Errorf("snapshot files:\n%s", files)
	}
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, _ := cmd.Output()
	if strings.Contains(string(out), "A ") {
		t.Errorf("index changed:\n%s", out)
	}
}
