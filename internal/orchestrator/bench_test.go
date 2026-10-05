package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bench run starts in a repository that holds only the base commit's
// files: the later commits (the solution of a history task), branches,
// tags, stashes, notes, remotes and the previous run's work cannot be
// reached with git from inside it.
func TestBenchWorkspaceHidesLaterHistory(t *testing.T) {
	dir := gitRepo(t)
	write := func(name, body string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := headOf(t, dir)
	write("calc.go", "package calc\n\nfunc Mul(a, b int) int { return a * b } // SOLUTION\n")
	write("calc_test.go", "package calc\n\n// TEST\n")
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-q", "-m", "Add Mul")
	solution := headOf(t, dir)
	solutionBlob := tgit(t, dir, "rev-parse", "HEAD:calc.go")
	tgit(t, dir, "tag", "v1")
	tgit(t, dir, "notes", "add", "-m", "note", "HEAD")
	tgit(t, dir, "remote", "add", "origin", dir)
	tgit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	write("later.txt", "later\n")
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-q", "-m", "Later")
	write("README.md", "# stashed\n")
	tgit(t, dir, "stash")

	ws, err := NewBenchWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Reset(base); err != nil {
		t.Fatal(err)
	}
	in := func(args ...string) (string, error) { return tgitTry(ws.Path, args...) }
	if out, _ := in("rev-list", "--all", "--reflog"); len(strings.Fields(out)) != 1 {
		t.Errorf("commits reachable in the workspace: %q", out)
	}
	if out, _ := in("log", "--all", "--format=%s"); out != BenchCommitMessage {
		t.Errorf("git log --all: %q", out)
	}
	for _, id := range []string{solution, solutionBlob} {
		if _, err := in("cat-file", "-e", id); err == nil {
			t.Errorf("%s can be read in the workspace", id)
		}
	}
	if out, _ := in("cat-file", "--batch-all-objects", "--batch-check"); strings.Contains(out, solutionBlob) {
		t.Error("the solution's blob is in the object store")
	}
	if out, _ := in("for-each-ref"); strings.Count(out, "\n") != 0 || !strings.HasSuffix(out, "refs/heads/main") {
		t.Errorf("refs: %q", out)
	}
	if out, _ := in("remote"); out != "" {
		t.Errorf("remotes: %q", out)
	}
	if out, _ := in("rev-parse", "--path-format=absolute", "--git-common-dir"); !samePath(out, filepath.Join(ws.Path, ".git")) {
		t.Errorf("common dir %q", out)
	}
	for _, f := range []string{"commondir", "objects/info/alternates", "shallow", "packed-refs"} {
		if _, err := os.Stat(filepath.Join(ws.Path, ".git", filepath.FromSlash(f))); err == nil {
			t.Errorf(".git/%s exists", f)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(ws.Path, "README.md")); string(data) != "# test\n" {
		t.Errorf("README.md = %q", data)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "calc.go")); err == nil {
		t.Error("calc.go is there at the base")
	}
	if out, _ := in("status", "--porcelain"); out != "" {
		t.Errorf("status: %q", out)
	}

	// A run's work does not reach the next run; ignored files stay.
	os.WriteFile(filepath.Join(ws.Path, "work.txt"), []byte("mine\n"), 0o644)
	os.MkdirAll(filepath.Join(ws.Path, "ignored"), 0o755)
	os.WriteFile(filepath.Join(ws.Path, "ignored", "cache"), []byte("x"), 0o644)
	in("add", "-A")
	in("-c", "user.name=a", "-c", "user.email=a@a", "commit", "-q", "-m", "agent work")
	work, _ := in("rev-parse", "HEAD")
	if err := ws.Reset(base); err != nil {
		t.Fatal(err)
	}
	if _, err := in("cat-file", "-e", work); err == nil {
		t.Error("the previous run's commit is still there")
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "work.txt")); err == nil {
		t.Error("the previous run's file is still there")
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "ignored", "cache")); err != nil {
		t.Errorf("ignored file gone: %v", err)
	}

	// The solution's run, then the base again (its pack comes back from
	// the cache): nothing of the solution stays.
	if err := ws.Reset(solution); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws.Path, "calc.go")); !strings.Contains(string(data), "SOLUTION") {
		t.Errorf("calc.go at the solution = %q", data)
	}
	if err := ws.Reset(base); err != nil {
		t.Fatal(err)
	}
	if _, err := in("cat-file", "-e", solutionBlob); err == nil {
		t.Error("the solution's blob is there after switching back")
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "calc.go")); err == nil {
		t.Error("calc.go is still there")
	}
}

// RestoreFiles copies only the named files of the reference commit in,
// with the repo's line-ending settings, and removes the ones it lacks.
func TestBenchRestoreFilesCopiesOnlyThose(t *testing.T) {
	dir := gitRepo(t)
	base := headOf(t, dir)
	os.WriteFile(filepath.Join(dir, "calc.go"), []byte("package calc // SOLUTION\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "calc_test.go"), []byte("package calc\n\n// TEST\n"), 0o644)
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-q", "-m", "Add calc")
	ref := headOf(t, dir)
	codeBlob := tgit(t, dir, "rev-parse", "HEAD:calc.go")
	tgit(t, dir, "config", "core.autocrlf", "true") // checkouts write CRLF
	tgit(t, dir, "remote", "add", "origin", "https://example.com/x.git")

	ws, err := NewBenchWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Reset(base); err != nil {
		t.Fatal(err)
	}
	if out, _ := tgitTry(ws.Path, "config", "core.autocrlf"); out != "true" {
		t.Errorf("core.autocrlf in the workspace: %q", out)
	}
	if out, _ := tgitTry(ws.Path, "config", "--get-regexp", "remote"); out != "" {
		t.Errorf("remote settings copied: %q", out)
	}
	os.WriteFile(filepath.Join(ws.Path, "gone_test.go"), []byte("x"), 0o644)
	if err := ws.RestoreFiles(ref, []string{"calc_test.go", "gone_test.go"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws.Path, "calc_test.go")); string(data) != "package calc\r\n\r\n// TEST\r\n" {
		t.Errorf("calc_test.go = %q", data)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "gone_test.go")); err == nil {
		t.Error("gone_test.go was not removed")
	}
	if _, err := tgitTry(ws.Path, "cat-file", "-e", codeBlob); err == nil {
		t.Error("the solution's code came along")
	}
	if out, _ := tgitTry(ws.Path, "status", "--porcelain"); out != "?? calc_test.go" {
		t.Errorf("status: %q", out)
	}
	files, err := ws.FilesAt(ref, []string{"calc_test.go", "gone_test.go", "README.md"})
	if err != nil || strings.Join(files, ",") != "calc_test.go,README.md" {
		t.Errorf("FilesAt = %v, %v", files, err)
	}
}

// The workspace of an older rw, a worktree of the repo, is replaced by a
// standalone repository and its worktree record removed.
func TestBenchWorkspaceReplacesOldWorktree(t *testing.T) {
	dir := gitRepo(t)
	ws, err := NewBenchWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := (git{dir}).addWorktree(ws.Path, "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := ws.Reset(headOf(t, dir)); err != nil {
		t.Fatal(err)
	}
	if isWorktreeOf(dir, ws.Path) {
		t.Error("still a worktree of the repo")
	}
	if out := tgit(t, dir, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("worktrees:\n%s", out)
	}
	if data, _ := os.ReadFile(filepath.Join(ws.Path, "README.md")); string(data) != "# test\n" {
		t.Errorf("README.md = %q", data)
	}
}
