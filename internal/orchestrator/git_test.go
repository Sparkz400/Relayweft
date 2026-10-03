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

// In an LFS repo a worktree holds pointer files, and committing it after a
// code-only change does not touch the LFS file.
func TestWorktreeKeepsLFSPointers(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	if _, err := g.out("lfs", "install", "--local"); err != nil {
		t.Skip("git lfs not installed")
	}
	g.out("lfs", "track", "*.bin")
	big := strings.Repeat("binary asset ", 1000)
	os.WriteFile(filepath.Join(dir, "asset.bin"), []byte(big), 0o644)
	if _, err := g.out("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.commitTree("commit", "-q", "-m", "lfs"); err != nil {
		t.Fatal(err)
	}
	if !g.usesLFS() {
		t.Fatal("usesLFS = false")
	}
	if n := g.trackedFiles(); n != 5 { // .gitattributes .gitignore README.md asset.bin shared.txt
		t.Errorf("trackedFiles = %d, want 5", n)
	}
	snap, err := g.snapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.addWorktree(wt, snap); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.removeWorktree(wt) })
	data, _ := os.ReadFile(filepath.Join(wt, "asset.bin"))
	if !strings.HasPrefix(string(data), "version https://git-lfs") {
		t.Fatalf("worktree asset.bin is not a pointer: %.60q", data)
	}
	os.WriteFile(filepath.Join(wt, "shared.txt"), []byte("changed\n"), 0o644)
	commit, changed, err := (git{wt}).commitAll("w")
	if err != nil || !changed {
		t.Fatalf("commitAll = %v, %v", changed, err)
	}
	files, _ := g.out("diff", "--name-only", snap, commit)
	if files != "shared.txt" {
		t.Errorf("changed files = %q, want only shared.txt", files)
	}
}

// The snapshot is seeded from the real index, but must still equal the
// working tree: staged files that were deleted afterwards are gone, staged
// content that was edited afterwards has the edited version.
func TestSnapshotFromStagedIndexMatchesWorkingTree(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	os.WriteFile(filepath.Join(dir, "staged-then-deleted.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("staged\n"), 0o644)
	g.out("add", "-A")
	os.Remove(filepath.Join(dir, "staged-then-deleted.txt"))
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("edited\n"), 0o644)
	os.Remove(filepath.Join(dir, "README.md"))
	before, _ := g.out("ls-files", "--stage")

	snap, err := g.snapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	files, _ := g.out("ls-tree", "-r", "--name-only", snap)
	if files != ".gitignore\nshared.txt" {
		t.Errorf("snapshot files:\n%s", files)
	}
	if got, _ := g.out("show", snap+":shared.txt"); got != "edited" {
		t.Errorf("shared.txt = %q, want the working tree version", got)
	}
	if after, _ := g.out("ls-files", "--stage"); after != before {
		t.Errorf("real index changed:\n%s\nwant\n%s", after, before)
	}
}
