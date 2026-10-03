package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Worktree strategy (decided for v1):
//
//   - At the start of the execute phase the main working tree is captured as a
//     snapshot commit (tracked + untracked, honouring .gitignore) using a
//     temporary index, so the user's index and branch are never touched.
//   - Each parallel writing agent gets a pooled worktree (pool.go) moved to
//     the current integration commit (the snapshot plus everything merged so
//     far).
//   - When an agent finishes, its worktree is committed and merged into the
//     integration commit with `git merge-tree --write-tree` (pure object
//     operation, git >= 2.38).
//   - The difference between the old and new integration commit is applied to
//     the main working tree with `git apply`, so the user sees results as soon
//     as each agent finishes.
//   - On a conflict the agent's commit is kept on branch sy/<session>/<step>
//     and the reviewer is told; nothing is half-applied.

type git struct{ dir string }

func (g git) run(env []string, stdin []byte, args ...string) (string, error) {
	// Parallel checkout (one worker per core) for worktree creation, slot
	// resets and restores into the main tree; git ignores it elsewhere.
	args = append([]string{"-c", "checkout.workers=0"}, args...)
	if runtime.GOOS == "windows" {
		// Worktrees live under %LOCALAPPDATA%; deep repos exceed MAX_PATH.
		args = append([]string{"-c", "core.longpaths=true"}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}

func (g git) out(args ...string) (string, error) {
	s, err := g.run(nil, nil, args...)
	return strings.TrimSpace(s), err
}

// isRepo reports whether dir is inside a git work tree.
func isRepo(dir string) bool {
	s, err := git{dir}.out("rev-parse", "--is-inside-work-tree")
	return err == nil && s == "true"
}

func repoRoot(dir string) (string, error) { return git{dir}.out("rev-parse", "--show-toplevel") }

var reGitVersion = regexp.MustCompile(`(\d+)\.(\d+)`)

// GitVersion returns major, minor of the installed git.
func GitVersion() (int, int, error) {
	s, err := git{"."}.out("version")
	if err != nil {
		return 0, 0, err
	}
	m := reGitVersion.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("cannot parse %q", s)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a, b, nil
}

// SupportsMergeTree reports whether git has `merge-tree --write-tree` (2.38+).
func SupportsMergeTree() bool {
	a, b, err := GitVersion()
	return err == nil && (a > 2 || (a == 2 && b >= 38))
}

// snapshot commits the working tree (including untracked, non-ignored files)
// without touching the real index, HEAD or any branch.
func (g git) snapshot(msg string) (string, error) {
	f, err := os.CreateTemp("", "sy-index-*")
	if err != nil {
		return "", err
	}
	idx := f.Name()
	f.Close()
	os.Remove(idx) // git creates it
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	head, headErr := g.out("rev-parse", "--verify", "-q", "HEAD")
	// Seed the temporary index with a copy of the real one: its cached stat
	// data lets `add -A` re-hash only changed files. An index built by
	// read-tree has none, so every file (and every LFS object, through the
	// clean filter) would be re-hashed, which takes minutes in big repos.
	if !g.copyIndex(idx) && headErr == nil && head != "" {
		if _, err := g.run(env, nil, "read-tree", head); err != nil {
			return "", err
		}
	}
	if _, err := g.run(env, nil, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := g.run(env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", msg}
	if headErr == nil && head != "" {
		args = append(args, "-p", head)
	}
	return g.commitTree(args...)
}

// copyIndex copies the repository's index file to dst. The real index is
// only read, never written. The copy keeps the original mtime: git compares
// it with each entry's mtime to catch "racily clean" files (edited in the
// same timestamp tick the index was written), and a fresh mtime would make
// such edits invisible to `add -A`.
func (g git) copyIndex(dst string) bool {
	src, err := g.out("rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return false
	}
	fi, err := os.Stat(src)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return false
	}
	if err := os.Chtimes(dst, fi.ModTime(), fi.ModTime()); err != nil {
		os.Remove(dst)
		return false
	}
	return true
}

func (g git) commitTree(args ...string) (string, error) {
	env := []string{
		"GIT_AUTHOR_NAME=Switchyard", "GIT_AUTHOR_EMAIL=switchyard@localhost",
		"GIT_COMMITTER_NAME=Switchyard", "GIT_COMMITTER_EMAIL=switchyard@localhost",
	}
	s, err := g.run(env, nil, args...)
	return strings.TrimSpace(s), err
}

// lfsSkip leaves Git LFS files in worktrees as pointer files, so a worktree
// of a repo with gigabytes of assets is as cheap as one without. Committing a
// pointer again is a no-op, and changed LFS files reach the main tree through
// applyDiff, whose `git restore` runs the normal LFS filters there.
var lfsSkip = []string{"GIT_LFS_SKIP_SMUDGE=1"}

// addWorktree creates a detached worktree at commit.
func (g git) addWorktree(path, commit string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := g.run(lfsSkip, nil, "worktree", "add", "--detach", "--force", path, commit)
	return err
}

// usesLFS reports whether the repo tracks files with Git LFS (top-level
// .gitattributes, which is where `git lfs track` writes).
func (g git) usesLFS() bool {
	data, err := os.ReadFile(filepath.Join(g.dir, ".gitattributes"))
	return err == nil && bytes.Contains(data, []byte("filter=lfs"))
}

// trackedFiles counts the files in the index, i.e. what a worktree checks out.
func (g git) trackedFiles() int {
	s, err := g.run(nil, nil, "ls-files", "-z")
	if err != nil {
		return 0
	}
	return strings.Count(s, "\x00")
}

func (g git) removeWorktree(path string) {
	g.out("worktree", "remove", "--force", path)
	os.RemoveAll(path)
	g.out("worktree", "prune")
}

// commitAll commits everything in a worktree on top of its HEAD. changed is
// false when the agent did not modify anything.
func (g git) commitAll(msg string) (commit string, changed bool, err error) {
	if _, err := g.out("add", "-A"); err != nil {
		return "", false, err
	}
	if _, err := g.out("diff", "--cached", "--quiet"); err == nil {
		head, err := g.out("rev-parse", "HEAD")
		return head, false, err
	}
	env := []string{
		"GIT_AUTHOR_NAME=Switchyard", "GIT_AUTHOR_EMAIL=switchyard@localhost",
		"GIT_COMMITTER_NAME=Switchyard", "GIT_COMMITTER_EMAIL=switchyard@localhost",
	}
	// No signing (would prompt or fail for GPG/SSH-signing users) and no hooks:
	// these commits are internal plumbing, never pushed.
	if _, err := g.run(env, nil, "-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", msg); err != nil {
		return "", false, err
	}
	head, err := g.out("rev-parse", "HEAD")
	return head, true, err
}

// mergeTree merges theirs into ours as objects only. On conflict, clean is
// false and info lists the conflicted paths.
func (g git) mergeTree(ours, theirs string) (tree string, clean bool, info string, err error) {
	out, err := g.run(nil, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", ours, theirs)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil {
		// Exit status 1 means conflicts; the first line is still the tree.
		if ee, ok := asExit(err); ok && ee == 1 && len(lines) > 0 {
			return lines[0], false, strings.Join(lines[1:], ", "), nil
		}
		return "", false, "", err
	}
	return lines[0], true, "", nil
}

func asExit(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// applyDiff brings the working tree from commit `from` to commit `to`.
//
// Changed files are written with `git restore --source=<to> --worktree`, which
// runs git's checkout filters, so line endings follow core.autocrlf on
// Windows (a plain `git apply` of an LF patch fails on CRLF files). A file is
// only overwritten when its content still matches `from`; if the user edited
// it meanwhile, a 3-way patch is attempted for those files and anything that
// still does not fit is reported as a conflict instead of being clobbered.
func (g git) applyDiff(from, to string) error {
	out, err := g.run(nil, nil, "diff", "--name-status", "--no-renames", "-z", from, to)
	if err != nil {
		return err
	}
	fields := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var write, remove, touched []string
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := fields[i], fields[i+1]
		clean, err := g.unchangedSince(from, path)
		if err != nil {
			return err
		}
		if !clean {
			touched = append(touched, path)
			continue
		}
		if strings.HasPrefix(status, "D") {
			remove = append(remove, path)
		} else {
			write = append(write, path)
		}
	}
	// Merge the files the user touched first, in memory, so a conflict
	// leaves the working tree exactly as it was.
	var conflicts []string
	merged := map[string]string{}
	for _, p := range touched {
		out, err := g.mergeFile(from, to, p)
		if err != nil {
			conflicts = append(conflicts, p)
			continue
		}
		merged[p] = out
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("you changed %s while agents were working and the edits overlap", strings.Join(conflicts, ", "))
	}
	if len(write) > 0 {
		args := append([]string{"restore", "--source=" + to, "--worktree", "--"}, write...)
		if _, err := g.run(nil, nil, args...); err != nil {
			return err
		}
	}
	for _, p := range remove {
		if err := os.Remove(filepath.Join(g.dir, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for p, content := range merged {
		if err := os.WriteFile(filepath.Join(g.dir, filepath.FromSlash(p)), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// mergeFile 3-way merges one file the user edited while an agent changed it:
// base = the file at `from`, theirs = the file at `to`, ours = the working
// tree. Blobs are read with --filters so line endings match the checkout.
// It returns the merged content; an error means the edits conflict.
func (g git) mergeFile(from, to, path string) (string, error) {
	full := filepath.Join(g.dir, filepath.FromSlash(path))
	theirs, err := g.run(nil, nil, "cat-file", "--filters", to+":"+path)
	if err != nil {
		return "", err // deleted by the agent but edited by the user: conflict
	}
	base, _ := g.run(nil, nil, "cat-file", "--filters", from+":"+path)
	tmp, err := os.MkdirTemp("", "sy-merge-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	bp, tp := filepath.Join(tmp, "base"), filepath.Join(tmp, "theirs")
	os.WriteFile(bp, []byte(base), 0o644)
	os.WriteFile(tp, []byte(theirs), 0o644)
	if _, err := os.Stat(full); err != nil {
		return "", err
	}
	merged, err := g.run(nil, nil, "merge-file", "-p", full, bp, tp)
	if err != nil {
		return "", err // exit status > 0 means conflicts
	}
	return merged, nil
}

// unchangedSince reports whether the working-tree file at path still has the
// content it had in commit (both missing counts as unchanged).
func (g git) unchangedSince(commit, path string) (bool, error) {
	want, err := g.out("rev-parse", "-q", "--verify", commit+":"+path)
	if err != nil {
		want = ""
	}
	if _, err := os.Lstat(filepath.Join(g.dir, filepath.FromSlash(path))); os.IsNotExist(err) {
		return want == "", nil
	}
	have, err := g.out("hash-object", "--", path)
	if err != nil {
		return false, err
	}
	return have == want, nil
}

// diff returns `git diff --stat` and the (truncated) patch between a commit
// and the current working tree.
func (g git) diff(from string, max int) (stat, patch string) {
	now, err := g.snapshot("switchyard review snapshot")
	if err != nil {
		return "", ""
	}
	stat, _ = g.out("diff", "--stat", from, now)
	p, _ := g.run(nil, nil, "diff", from, now)
	if len(p) > max {
		p = p[:max] + "\n... (diff truncated) ..."
	}
	return stat, p
}
