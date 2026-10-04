package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/proc"
)

// Regression tests for an adversarial review of the worktree pool, the
// commit of an agent's worktree and applying changes to the user's tree.

// tgit runs git in dir as a user would and fails the test on error.
func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := tgitTry(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return out
}

func tgitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// chattr sets or clears the immutable flag, skipping the test where that
// is not possible (not Linux, not root, file system without support).
func chattr(t *testing.T, on bool, path string) {
	t.Helper()
	flag := "-i"
	if on {
		flag = "+i"
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on Linux (chattr +i)")
	}
	if out, err := exec.Command("chattr", flag, path).CombinedOutput(); err != nil {
		if on {
			t.Skipf("chattr: %v %s", err, out)
		}
		return
	}
	if on {
		t.Cleanup(func() { exec.Command("chattr", "-i", path).Run() })
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// agentChange snapshots dir, lets change edit a worktree of the snapshot
// and commits the result, returning (from, to).
func agentChange(t *testing.T, dir string, change func(wt string)) (string, string) {
	t.Helper()
	g := git{dir}
	from, err := g.snapshot("from")
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.addWorktree(wt, from); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.removeWorktree(wt) })
	change(wt)
	sc, err := (git{wt}).commitWork(from, "change")
	if err != nil || !sc.Changed {
		t.Fatalf("commitWork: %+v %v", sc, err)
	}
	return from, sc.Commit
}

// Finding 1: a crash during `git worktree add` leaves the record locked
// ("initializing"); the slot must still be recreated.
func TestPoolRecreatesLockedInitializingSlot(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	s.release()
	gd := slotGitDir(path)
	os.WriteFile(filepath.Join(gd, "locked"), []byte("initializing"), 0o644)
	os.Remove(filepath.Join(path, ".git"))
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatalf("acquire after a crashed worktree add: %v", err)
	}
	defer s.release()
	if s.path != path || !isWorktreeOf(dir, path) {
		t.Errorf("slot %s not recreated at %s", s.path, path)
	}
}

// Finding 1: a slot that cannot be prepared at all is skipped; the next
// index is used instead of failing (and sending writers to the main tree).
func TestPoolSkipsUnusableSlot(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	s.release()
	os.Remove(filepath.Join(path, ".git"))
	chattr(t, true, path) // cannot be deleted or renamed
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatalf("acquire with slot 0 unusable: %v", err)
	}
	defer s.release()
	if s.path == path {
		t.Fatal("got the unusable slot")
	}
}

// Finding 1: a slot directory that cannot be deleted (a file held open on
// Windows) is moved aside, and the slot path is used again.
func TestPoolMovesUndeletableSlotAside(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	s.release()
	write(t, filepath.Join(path, "ignored", "server.log"), "x")
	chattr(t, true, filepath.Join(path, "ignored"))
	os.Remove(filepath.Join(path, ".git"))
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	s.release()
	if s.path != path || !isWorktreeOf(dir, path) {
		t.Errorf("slot %s, want %s recreated", s.path, path)
	}
	trash, _ := filepath.Glob(path + ".trash-*")
	if len(trash) != 1 {
		t.Fatalf("trash = %v, want the old slot moved aside", trash)
	}
	chattr(t, false, filepath.Join(trash[0], "ignored"))
	if _, err := CleanPool(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trash[0]); !os.IsNotExist(err) {
		t.Error("sy clean left the moved-aside slot")
	}
}

// Finding 2: an agent that commits itself, switches to a user branch or
// trips a prepare-commit-msg hook does not lose its work, and no user
// branch is moved.
func TestCommitWorkAgentUsesGit(t *testing.T) {
	dir := gitRepo(t)
	tgit(t, dir, "branch", "feature")
	os.WriteFile(filepath.Join(dir, ".git", "hooks", "prepare-commit-msg"),
		[]byte("#!/bin/sh\ngit symbolic-ref -q HEAD >/dev/null || { echo 'no branch' >&2; exit 1; }\n"), 0o755)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	// The agent commits on its own.
	write(t, filepath.Join(s.path, "shared.txt"), "agent\n")
	tgit(t, s.path, "-c", "core.hooksPath=/nonexistent", "commit", "-qam", "agent commit")
	sc, err := (git{s.path}).commitWork(snap, "switchyard: x")
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Changed || sc.Head == "" {
		t.Fatalf("agent's own commit lost: %+v", sc)
	}
	if got := tgit(t, dir, "show", sc.Commit+":shared.txt"); got != "agent" {
		t.Errorf("commit has shared.txt = %q", got)
	}
	if parent := tgit(t, dir, "rev-parse", sc.Commit+"^"); parent != snap {
		t.Errorf("commit parent = %s, want the base %s", parent, snap)
	}
	// It switches to the user's branch and commits there.
	tgit(t, s.path, "checkout", "-q", "-f", "feature")
	write(t, filepath.Join(s.path, "f.txt"), "f\n")
	tgit(t, s.path, "add", "f.txt")
	tgit(t, s.path, "-c", "core.hooksPath=/nonexistent", "commit", "-qm", "on feature")
	tip := tgit(t, dir, "rev-parse", "feature")
	write(t, filepath.Join(s.path, "uncommitted.txt"), "u\n")
	sc, err = (git{s.path}).commitWork(snap, "switchyard: y")
	if err != nil {
		t.Fatalf("commit with a failing prepare-commit-msg hook: %v", err)
	}
	if now := tgit(t, dir, "rev-parse", "feature"); now != tip {
		t.Errorf("user branch feature moved from %s to %s", tip, now)
	}
	if sc.Head != tip {
		t.Errorf("Head = %s, want the agent's commit %s", sc.Head, tip)
	}
	if files := tgit(t, dir, "diff", "--name-only", snap, sc.Commit); !strings.Contains(files, "uncommitted.txt") || !strings.Contains(files, "f.txt") {
		t.Errorf("changed files = %q", files)
	}
}

// Finding 3: a file that cannot be written stops the apply before anything
// is written.
func TestApplyDiffRefusesUnwritableFileUpFront(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "z.txt"), "z\n")
	from, to := agentChange(t, dir, func(wt string) {
		write(t, filepath.Join(wt, "shared.txt"), "agent\n")
		write(t, filepath.Join(wt, "z.txt"), "agent z\n")
	})
	chattr(t, true, filepath.Join(dir, "z.txt"))
	if err := (git{dir}).applyDiff(from, to); err == nil {
		t.Fatal("apply into an immutable file succeeded")
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "base\n" {
		t.Errorf("shared.txt = %q: half-applied", got)
	}
}

// Finding 3: a failure after some files were written puts them back.
func TestApplyDiffRollsBack(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "d", "x.txt"), "x\n")
	from, to := agentChange(t, dir, func(wt string) {
		write(t, filepath.Join(wt, "shared.txt"), "agent\n")
		write(t, filepath.Join(wt, "new", "n.txt"), "n\n")
		os.Remove(filepath.Join(wt, "d", "x.txt"))
	})
	chattr(t, true, filepath.Join(dir, "d")) // the removal fails after the writes
	err := (git{dir}).applyDiff(from, to)
	if err == nil {
		t.Fatal("apply succeeded although d/x.txt cannot be removed")
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "base\n" {
		t.Errorf("shared.txt = %q, want it put back", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Error("new/n.txt (and its directory) not removed again")
	}
	if got := read(t, filepath.Join(dir, "d", "x.txt")); got != "x\n" {
		t.Errorf("d/x.txt = %q", got)
	}
}

// Finding 3: rollback never undoes an edit the user made meanwhile.
func TestApplyDiffRollbackKeepsUserEdits(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "d", "x.txt"), "x\n")
	from, to := agentChange(t, dir, func(wt string) {
		write(t, filepath.Join(wt, "shared.txt"), "agent\n")
		os.Remove(filepath.Join(wt, "d", "x.txt"))
	})
	chattr(t, true, filepath.Join(dir, "d"))
	applyHook = func(stage string) {
		if stage == "wrote" {
			write(t, filepath.Join(dir, "shared.txt"), "user\n")
		}
	}
	defer func() { applyHook = nil }()
	if err := (git{dir}).applyDiff(from, to); err == nil {
		t.Fatal("apply succeeded")
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "user\n" {
		t.Errorf("shared.txt = %q, the user's edit was rolled back", got)
	}
}

// Finding 4: hundreds of paths must not end up on one command line
// (Windows: 32K characters), neither for writing nor with a file filter.
func TestApplyDiffManyPaths(t *testing.T) {
	dir := gitRepo(t)
	old := maxCmdLine.Load()
	maxCmdLine.Store(32000)
	defer maxCmdLine.Store(old)
	long := strings.Repeat("deeply-nested-directory-name/", 2)
	var names []string
	for i := 0; i < 600; i++ {
		names = append(names, long+"file-with-a-rather-long-name-"+strings.Repeat("x", 10)+"-"+strconv.Itoa(i)+".txt")
	}
	from, to := agentChange(t, dir, func(wt string) {
		for _, n := range names {
			write(t, filepath.Join(wt, filepath.FromSlash(n)), "new\n")
		}
	})
	g := git{dir}
	if err := g.applyDiff(from, to, names...); err != nil {
		t.Fatalf("apply with a file filter: %v", err)
	}
	if read(t, filepath.Join(dir, filepath.FromSlash(names[599]))) != "new\n" {
		t.Error("file not written")
	}
	if err := g.applyDiff(to, from); err != nil {
		t.Fatalf("apply back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(names[0]))); !os.IsNotExist(err) {
		t.Error("file not removed")
	}
}

// Finding 5: a file the user edits after the check but before the write is
// not overwritten; and checking many files takes a handful of git
// processes, not several per file (finding 16).
func TestApplyDiffRechecksBeforeWriting(t *testing.T) {
	dir := gitRepo(t)
	from, to := agentChange(t, dir, func(wt string) {
		write(t, filepath.Join(wt, "shared.txt"), "agent\n")
		write(t, filepath.Join(wt, "README.md"), "# agent\n")
	})
	applyHook = func(stage string) {
		if stage == "checked" {
			time.Sleep(10 * time.Millisecond) // a different mtime tick
			write(t, filepath.Join(dir, "shared.txt"), "user, just now\n")
		}
	}
	defer func() { applyHook = nil }()
	err := (git{dir}).applyDiff(from, to)
	if err == nil || !strings.Contains(err.Error(), "shared.txt") {
		t.Fatalf("want an error naming shared.txt, got %v", err)
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "user, just now\n" {
		t.Errorf("shared.txt = %q: the user's edit was overwritten", got)
	}
	if got := read(t, filepath.Join(dir, "README.md")); got != "# test\n" {
		t.Errorf("README.md = %q: half-applied", got)
	}
}

func TestApplyDiffBatchesHashing(t *testing.T) {
	dir := gitRepo(t)
	for i := 0; i < 100; i++ {
		write(t, filepath.Join(dir, "f", strconv.Itoa(i)+".txt"), "x\n")
	}
	from, to := agentChange(t, dir, func(wt string) {
		for i := 0; i < 100; i++ {
			write(t, filepath.Join(wt, "f", strconv.Itoa(i)+".txt"), "agent\n")
		}
	})
	before := gitRuns.Load()
	if err := (git{dir}).applyDiff(from, to); err != nil {
		t.Fatal(err)
	}
	if n := gitRuns.Load() - before; n > 10 {
		t.Errorf("applying 100 files ran git %d times", n)
	}
}

// Finding 6: nested repositories an agent creates are left out of its
// commit (with or without a commit of their own) and removed on reset.
func TestPoolNestedRepos(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	withCommit := filepath.Join(s.path, "vendor", "lib")
	write(t, filepath.Join(withCommit, "x.go"), "package x\n")
	tgit(t, withCommit, "init", "-q")
	tgit(t, withCommit, "add", "-A")
	tgit(t, withCommit, "commit", "-qm", "c")
	empty := filepath.Join(s.path, "scratch")
	write(t, filepath.Join(empty, "y.txt"), "y\n")
	tgit(t, empty, "init", "-q")
	write(t, filepath.Join(s.path, "shared.txt"), "agent\n")
	sc, err := (git{s.path}).commitWork(snap, "x")
	if err != nil {
		t.Fatalf("commitWork with nested repositories: %v", err)
	}
	if !sc.Changed || len(sc.Nested) != 2 {
		t.Errorf("commit = %+v, want changed with 2 nested repos reported", sc)
	}
	if tree := tgit(t, dir, "ls-tree", "-r", sc.Commit); strings.Contains(tree, "160000") || !strings.Contains(tree, "shared.txt") {
		t.Errorf("tree:\n%s", tree)
	}
	s.release()
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if _, err := os.Stat(withCommit); !os.IsNotExist(err) {
		t.Error("nested repository survived the slot reset")
	}
}

// Finding 7: a submodule change does not break apply/undo; it is skipped
// and reported.
func TestApplyDiffSkipsSubmodules(t *testing.T) {
	sub := gitRepo(t)
	super := gitRepo(t)
	tgit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "mod")
	tgit(t, super, "commit", "-qm", "sub")
	g := git{super}
	before, _ := g.snapshot("before")
	write(t, filepath.Join(super, "mod", "new.txt"), "n\n")
	tgit(t, filepath.Join(super, "mod"), "add", "-A")
	tgit(t, filepath.Join(super, "mod"), "commit", "-qm", "bump")
	write(t, filepath.Join(super, "shared.txt"), "task edit\n")
	after, _ := g.snapshot("after")
	skipped, err := g.applyDiffReport(after, before)
	if err != nil {
		t.Fatalf("undo with a submodule bump: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "mod" {
		t.Errorf("skipped = %v, want [mod]", skipped)
	}
	if got := read(t, filepath.Join(super, "shared.txt")); got != "base\n" {
		t.Errorf("shared.txt = %q", got)
	}
}

// Finding 8: the user's hooks do not run in pool worktrees.
func TestPoolIgnoresUserHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks")
	}
	dir := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "hooklog")
	for _, h := range []string{"post-checkout", "post-commit", "pre-commit", "prepare-commit-msg", "commit-msg"} {
		os.WriteFile(filepath.Join(dir, ".git", "hooks", h), []byte("#!/bin/sh\necho "+h+" >> "+marker+"\n"), 0o755)
	}
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.path, "shared.txt"), "agent\n")
	if _, err := (git{s.path}).commitWork(snap, "x"); err != nil {
		t.Fatal(err)
	}
	s.release()
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	s.release()
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("hooks ran:\n%s", b)
	}
}

// Finding 9: creating, cleaning and pruning pool worktrees never drops the
// user's own worktree whose directory is missing for a moment.
func TestPoolKeepsUsersMissingWorktree(t *testing.T) {
	dir := gitRepo(t)
	ext := filepath.Join(t.TempDir(), "usb", "wt")
	tgit(t, dir, "worktree", "add", "-q", "-b", "feature", ext)
	os.Rename(filepath.Dir(ext), filepath.Dir(ext)+".unplugged")
	defer os.Rename(filepath.Dir(ext)+".unplugged", filepath.Dir(ext))
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	s.release()
	os.Remove(filepath.Join(s.path, ".git")) // force a recreate
	if s, err = acquireSlot(dir, snap); err != nil {
		t.Fatal(err)
	}
	s.release()
	os.Chtimes(s.path+".lock", time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	PrunePools(time.Minute)
	CleanPool(dir)
	if list := tgit(t, dir, "worktree", "list", "--porcelain"); !strings.Contains(list, "usb") {
		t.Errorf("the user's worktree record was pruned:\n%s", list)
	}
}

// Finding 10: big untracked files stay out of snapshots and worktrees.
func TestSnapshotSkipsBigUntrackedFiles(t *testing.T) {
	dir := gitRepo(t)
	old := snapshotMaxFile.Load()
	snapshotMaxFile.Store(1 << 20)
	defer snapshotMaxFile.Store(old)
	f, _ := os.Create(filepath.Join(dir, "dump.sql"))
	f.Truncate(2 << 20)
	f.Close()
	write(t, filepath.Join(dir, "notes.txt"), "small\n")
	snap, skipped, err := (git{dir}).snapshotSkipping("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "dump.sql" {
		t.Errorf("skipped = %v", skipped)
	}
	tree := tgit(t, dir, "ls-tree", "-r", "--name-only", snap)
	if strings.Contains(tree, "dump.sql") || !strings.Contains(tree, "notes.txt") {
		t.Errorf("snapshot tree:\n%s", tree)
	}
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if _, err := os.Stat(filepath.Join(s.path, "dump.sql")); !os.IsNotExist(err) {
		t.Error("big file copied into the worktree")
	}
}

// Finding 11: an untouched symlink is not mistaken for an edit, and an
// agent's retargeted link is applied (undo too).
func TestApplyDiffSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "a.txt"), "A\n")
	write(t, filepath.Join(dir, "b.txt"), "B\n")
	os.Symlink("a.txt", filepath.Join(dir, "link"))
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-qm", "link")
	from, to := agentChange(t, dir, func(wt string) {
		os.Remove(filepath.Join(wt, "link"))
		os.Symlink("b.txt", filepath.Join(wt, "link"))
	})
	if err := (git{dir}).applyDiff(from, to); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "link")); target != "b.txt" {
		t.Errorf("link -> %q", target)
	}
	if read(t, filepath.Join(dir, "a.txt")) != "A\n" {
		t.Error("a.txt changed through the link")
	}
}

// Finding 12: an unfinished rebase/merge/bisect does not survive a reset.
func TestPoolClearsUnfinishedGitOperations(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	gd := slotGitDir(s.path)
	write(t, filepath.Join(gd, "rebase-merge", "head-name"), "refs/heads/main\n")
	write(t, filepath.Join(gd, "rebase-merge", "onto"), snap+"\n")
	write(t, filepath.Join(gd, "rebase-merge", "interactive"), "")
	write(t, filepath.Join(gd, "BISECT_LOG"), "x\n")
	write(t, filepath.Join(gd, "CHERRY_PICK_HEAD"), snap+"\n")
	s.release()
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	for _, n := range []string{"rebase-merge", "BISECT_LOG", "CHERRY_PICK_HEAD"} {
		if _, err := os.Stat(filepath.Join(gd, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset", n)
		}
	}
	if st := tgit(t, s.path, "status"); strings.Contains(st, "rebase") {
		t.Errorf("status:\n%s", st)
	}
}

// Finding 13: an agent left running by a sy that was killed is stopped
// before the slot is used again.
func TestPoolKillsOrphanAgents(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("killing leftover agents is implemented for Linux")
	}
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	cmd := proc.Shell(context.Background(), "sleep 300")
	cmd.Dir = s.path
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc.Started(cmd)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	defer cmd.Process.Kill()
	// sy dies: the lock is released by the OS, nothing else happens.
	s.untrack()
	s.unlock()
	s2, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the leftover agent is still running in the slot")
	}
}

// Finding 14: kept branches are never overwritten, and step ids that are
// not valid in ref names still give a branch.
func TestSaveBranchNeverOverwrites(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	c1, _ := g.snapshot("one")
	write(t, filepath.Join(dir, "shared.txt"), "two\n")
	c2, _ := g.snapshot("two")
	o := &Orchestrator{}
	tk := &task{id: "task-1", root: dir}
	b1 := o.saveBranch(tk, "fix: a..b ~^", c1)
	b2 := o.saveBranch(tk, "fix: a..b ~^", c2)
	if b1 == b2 {
		t.Fatalf("both commits on %s", b1)
	}
	for b, want := range map[string]string{b1: c1, b2: c2} {
		if got, err := tgitTry(dir, "rev-parse", "refs/heads/"+b); err != nil || got != want {
			t.Errorf("branch %q = %q (%v), want %s", b, got, err, want)
		}
	}
	if b := o.saveBranch(tk, "../..", c1); !strings.HasPrefix(b, "sy/") {
		t.Errorf("odd step id gave %q", b)
	}
}

// Finding 15: sy clean counts only slots it actually removed.
func TestCleanPoolCountsOnlyRemovedSlots(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	prewarmPool(dir, snap, 2, nil)
	stuck := filepath.Join(poolDir(dir), "1")
	chattr(t, true, stuck)
	n, err := CleanPool(dir)
	if n != 1 || err == nil {
		t.Errorf("CleanPool = %d, %v; want 1 and an error naming the stuck slot", n, err)
	}
	if _, err := os.Stat(filepath.Join(poolDir(dir), "0.lock")); !os.IsNotExist(err) {
		t.Error("lock file of the removed slot left behind")
	}
	chattr(t, false, stuck)
	CleanPool(dir)
}

// Finding 17: a stale index.lock of a killed git does not make the slot be
// recreated (losing its build caches).
func TestPoolRemovesStaleIndexLock(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(slotGitDir(s.path), "index.lock")
	write(t, lock, "")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	write(t, filepath.Join(s.path, "ignored", "cache"), "keep")
	s.release()
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if _, err := os.Stat(filepath.Join(s.path, "ignored", "cache")); err != nil {
		t.Error("slot was recreated: the ignored cache is gone")
	}
}

// Finding 17: removing the last file of a directory removes the directory.
func TestApplyDiffRemovesEmptyDirs(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "d", "e", "x.txt"), "x\n")
	from, to := agentChange(t, dir, func(wt string) {
		os.RemoveAll(filepath.Join(wt, "d"))
	})
	if err := (git{dir}).applyDiff(from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d")); !os.IsNotExist(err) {
		t.Error("empty directory d left behind")
	}
}

// Finding 17: files an agent writes outside a sparse checkout are reported.
func TestCommitWorkReportsSparseCheckoutWrites(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "a", "x.txt"), "a\n")
	write(t, filepath.Join(dir, "b", "y.txt"), "b\n")
	tgit(t, dir, "add", "-A")
	tgit(t, dir, "commit", "-qm", "ab")
	tgit(t, dir, "sparse-checkout", "set", "a")
	snap, _ := git{dir}.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	write(t, filepath.Join(s.path, "b", "y.txt"), "agent\n")
	write(t, filepath.Join(s.path, "a", "x.txt"), "agent a\n")
	sc, err := (git{s.path}).commitWork(snap, "x")
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Changed || len(sc.Sparse) == 0 {
		t.Errorf("commit = %+v, want the write outside the cone reported", sc)
	}
}

// git 2.5x adds an "unable to index file" line for a nested repository
// without a commit; it must not turn into a failed commit.
func TestAddWarningsNewGit(t *testing.T) {
	msg := "error: 'scratch/' does not have a commit checked out\nerror: unable to index file 'scratch/'\nwarning: adding embedded git repository: vendor/lib\nhint: You've added another git repository\nfatal: adding files failed"
	nested, _, ok := addWarnings(&gitError{msg: msg})
	if !ok || len(nested) != 1 || nested[0] != "scratch" {
		t.Fatalf("nested %v ok %v", nested, ok)
	}
	if _, _, ok := addWarnings(&gitError{msg: "error: unable to index file 'real.txt'"}); ok {
		t.Fatal("a file that cannot be indexed was ignored")
	}
}
