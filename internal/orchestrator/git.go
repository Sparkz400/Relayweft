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
	"sync/atomic"
	"time"

	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/proc"
)

// Worktree strategy (decided for v1):
//
//   - At the start of the execute phase the main working tree is captured as a
//     snapshot commit (tracked + untracked, honouring .gitignore) using a
//     temporary index, so the user's index and branch are never touched.
//   - Each parallel writing agent gets a pooled worktree (pool.go) moved to
//     the current integration commit (the snapshot plus everything merged so
//     far).
//   - When an agent finishes, its worktree is committed with plumbing
//     (add -A on a temporary index, write-tree, commit-tree) and merged
//     into the integration commit with `git merge-tree --write-tree` (pure
//     object operation, git >= 2.38).
//   - The difference between the old and new integration commit is applied to
//     the main working tree (apply.go), so the user sees results as soon as
//     each agent finishes.
//   - On a conflict the agent's commit is kept on branch sy/<session>/<step>
//     and the reviewer is told; nothing is half-applied.

type git struct{ dir string }

// checkoutWorkers caps git's parallel checkout: half the cores, at most 4.
// One worker per core (checkout.workers=0) on several pool slots at once,
// plus antivirus scanning every new file, can saturate a whole machine.
func checkoutWorkers() int { return max(1, min(4, runtime.NumCPU()/2)) }

// gitError is a failed git command; msg is what git printed on stderr.
type gitError struct {
	args string
	err  error
	msg  string
}

func (e *gitError) Error() string { return fmt.Sprintf("git %s: %v: %s", e.args, e.err, e.msg) }
func (e *gitError) Unwrap() error { return e.err }

var (
	// gitRuns counts git processes (tests check that batch operations stay
	// batched).
	gitRuns atomic.Int64
	// maxCmdLine refuses command lines longer than this many bytes (0 = no
	// limit). Windows' limit is 32767 characters; tests set it elsewhere to
	// catch commands that would only break there.
	maxCmdLine atomic.Int64
)

func init() {
	if runtime.GOOS == "windows" {
		maxCmdLine.Store(32000)
	}
}

func (g git) run(env []string, stdin []byte, args ...string) (string, error) {
	return g.exec(true, env, stdin, args)
}

// runMagic is run with pathspec magic (":(exclude)...") allowed.
func (g git) runMagic(env []string, stdin []byte, args ...string) (string, error) {
	return g.exec(false, env, stdin, args)
}

func (g git) exec(literal bool, env []string, stdin []byte, args []string) (string, error) {
	// Parallel checkout for worktree creation, slot resets and restores into
	// the main tree; git ignores it elsewhere.
	pre := []string{"-c", "checkout.workers=" + strconv.Itoa(checkoutWorkers())}
	if literal {
		// Paths are literal: "[id].tsx" must not also match "i.tsx".
		pre = append([]string{"--literal-pathspecs"}, pre...)
	}
	args = append(pre, args...)
	if runtime.GOOS == "windows" {
		// Worktrees live under %LOCALAPPDATA%; deep repos exceed MAX_PATH.
		args = append([]string{"-c", "core.longpaths=true"}, args...)
	}
	if limit := maxCmdLine.Load(); limit > 0 {
		n := len("git")
		for _, a := range args {
			n += len(a) + 3
		}
		if int64(n) > limit {
			return "", fmt.Errorf("git %s: command line too long (%d bytes)", gitArgs(args), n)
		}
	}
	gitRuns.Add(1)
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
	proc.Background(cmd)
	start := time.Now()
	err := cmd.Start()
	if err == nil {
		proc.Started(cmd)
		err = cmd.Wait()
	}
	took := time.Since(start)
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		diag.Logf("git %s (in %s) failed after %s: %v: %s", gitArgs(args), g.dir, took.Round(time.Millisecond), err, clip(msg, 500))
		return out.String(), &gitError{args: strings.Join(args, " "), err: err, msg: msg}
	}
	if took > 300*time.Millisecond {
		diag.Logf("git %s (in %s) took %s", gitArgs(args), g.dir, took.Round(time.Millisecond))
	}
	return out.String(), nil
}

// gitArgs shortens an argument list for the debug log.
func gitArgs(args []string) string {
	var keep []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" && i+1 < len(args) {
			i++ // skip the -c key=value prefixes
			continue
		}
		keep = append(keep, args[i])
	}
	return clip(strings.Join(keep, " "), 300)
}

func (g git) out(args ...string) (string, error) {
	s, err := g.run(nil, nil, args...)
	return strings.TrimSpace(s), err
}

// nulList joins paths for --pathspec-from-file=- --pathspec-file-nul (and
// other -z stdin lists): any number of paths, no command line limit.
func nulList(paths []string) []byte {
	var b bytes.Buffer
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return b.Bytes()
}

// noHooks makes git ignore the user's hooks (post-checkout on every slot
// reset, prepare-commit-msg failing on a detached HEAD, ...): internal
// checkouts are plumbing. The hooks path is an empty directory.
func noHooks() []string {
	dir := filepath.Join(worktreesBase(), "no-hooks")
	os.MkdirAll(dir, 0o755)
	return []string{"-c", "core.hooksPath=" + dir}
}

// isRepo reports whether dir is inside a git work tree.
func isRepo(dir string) bool {
	s, err := git{dir}.out("rev-parse", "--is-inside-work-tree")
	return err == nil && s == "true"
}

func repoRoot(dir string) (string, error) { return git{dir}.out("rev-parse", "--show-toplevel") }

// commonDir is the repository's shared git directory (absolute).
func (g git) commonDir() string {
	s, _ := g.out("rev-parse", "--path-format=absolute", "--git-common-dir")
	return s
}

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

// snapshotMaxFile is the size above which untracked files are left out of
// snapshots (orchestrator.snapshot_max_file_mb; 0 = no limit): a database
// dump or a video would otherwise be hashed into the object store and
// copied into every pool worktree.
var snapshotMaxFile atomic.Int64

func init() { snapshotMaxFile.Store(100 << 20) }

// snapshot commits the working tree (including untracked, non-ignored files)
// without touching the real index, HEAD or any branch.
func (g git) snapshot(msg string) (string, error) {
	c, _, err := g.snapshotSkipping(msg)
	return c, err
}

// snapshotSkipping is snapshot that also returns the untracked files left
// out for being bigger than snapshotMaxFile.
func (g git) snapshotSkipping(msg string) (commit string, skipped []string, err error) {
	f, err := os.CreateTemp("", "sy-index-*")
	if err != nil {
		return "", nil, err
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
			return "", nil, err
		}
	}
	if limit := snapshotMaxFile.Load(); limit > 0 {
		skipped = g.bigUntracked(env, limit)
	}
	add := []string{"-c", "core.safecrlf=false", "add", "-A"}
	if len(skipped) > 0 {
		spec := []string{":(top)"}
		for _, p := range skipped {
			spec = append(spec, ":(top,exclude,literal)"+p)
		}
		diag.Logf("snapshot of %s leaves out %d untracked file(s) over %d MB: %s", g.dir, len(skipped), snapshotMaxFile.Load()>>20, clip(strings.Join(skipped, ", "), 500))
		_, err = g.runMagic(env, nulList(spec), append(add, "--pathspec-from-file=-", "--pathspec-file-nul")...)
	} else {
		_, err = g.run(env, nil, add...)
	}
	if err != nil {
		return "", nil, err
	}
	tree, err := g.run(env, nil, "write-tree")
	if err != nil {
		return "", nil, err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree), "-m", msg}
	if headErr == nil && head != "" {
		args = append(args, "-p", head)
	}
	commit, err = g.commitTree(args...)
	return commit, skipped, err
}

// bigUntracked lists untracked, non-ignored files larger than limit.
func (g git) bigUntracked(env []string, limit int64) []string {
	out, err := g.run(env, nil, "ls-files", "-o", "--exclude-standard", "-z")
	if err != nil {
		return nil
	}
	var big []string
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(g.dir, filepath.FromSlash(p))); err == nil && fi.Mode().IsRegular() && fi.Size() > limit {
			big = append(big, p)
		}
	}
	return big
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

// commitTree runs `git commit-tree` as Switchyard, never signing (commit-tree
// honours commit.gpgSign, which would prompt or fail): these commits are
// internal plumbing, never pushed.
func (g git) commitTree(args ...string) (string, error) {
	env := []string{
		"GIT_AUTHOR_NAME=Switchyard", "GIT_AUTHOR_EMAIL=switchyard@localhost",
		"GIT_COMMITTER_NAME=Switchyard", "GIT_COMMITTER_EMAIL=switchyard@localhost",
	}
	s, err := g.run(env, nil, append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	return strings.TrimSpace(s), err
}

// lfsSkip leaves Git LFS files in worktrees as pointer files, so a worktree
// of a repo with gigabytes of assets is as cheap as one without. Committing a
// pointer again is a no-op, and changed LFS files reach the main tree through
// applyDiff, whose `git restore` runs the normal LFS filters there.
var lfsSkip = []string{"GIT_LFS_SKIP_SMUDGE=1"}

// addWorktree creates a detached worktree at commit. -f -f also takes over
// a path whose old worktree record is missing-but-locked (a crash during
// `worktree add` leaves it locked "initializing").
func (g git) addWorktree(path, commit string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	args := append(noHooks(), "worktree", "add", "--detach", "-f", "-f", path, commit)
	_, err := g.run(lfsSkip, nil, args...)
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

// removeWorktree deletes a worktree of g's repository and only its record.
func (g git) removeWorktree(path string) error {
	return removeSlot(g.commonDir(), path)
}

// slotCommit is what an agent left in its worktree, committed.
type slotCommit struct {
	Commit  string // base itself when nothing changed
	Changed bool
	// Head is the worktree's HEAD when the agent moved it away from base
	// (committed, switched branches); its work is in Commit either way.
	Head string
	// Nested are nested repositories (or submodules) the agent created;
	// they cannot be merged as files and are left out.
	Nested []string
	// Sparse are files written outside the sparse-checkout cone; git does
	// not record them.
	Sparse []string
}

// commitAll commits everything in a worktree on top of its HEAD. changed is
// false when nothing was modified.
func (g git) commitAll(msg string) (commit string, changed bool, err error) {
	head, err := g.out("rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", false, err
	}
	c, err := g.commitWork(head, msg)
	return c.Commit, c.Changed, err
}

// commitWork commits the worktree's files on top of base with plumbing:
// `add -A` on a temporary index, write-tree, commit-tree. No hooks, no
// signing, no commit templates, and no branch or HEAD moves; whatever the
// agent did with git itself (committing, switching branches) does not
// matter, its files are what counts.
func (g git) commitWork(base, msg string) (slotCommit, error) {
	var res slotCommit
	f, err := os.CreateTemp("", "sy-index-*")
	if err != nil {
		return res, err
	}
	idx := f.Name()
	f.Close()
	os.Remove(idx)
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	// The worktree's own index has the stat data to skip unchanged files.
	if !g.copyIndex(idx) {
		if _, err := g.run(env, nil, "read-tree", base); err != nil {
			return res, err
		}
	}
	// --ignore-errors: a nested repository without a commit, or a file
	// outside a sparse checkout, must not lose the rest of the work.
	// LC_ALL=C: the warnings are parsed.
	addEnv := append(env, "LC_ALL=C", "LANGUAGE=")
	if _, err := g.run(addEnv, nil, "-c", "core.safecrlf=false", "-c", keepSparse, "add", "-A", "--ignore-errors"); err != nil {
		nested, sparse, ok := addWarnings(err)
		if !ok {
			return res, err
		}
		res.Nested, res.Sparse = nested, sparse
	}
	if g.sparseCheckout() {
		res.Sparse = append(res.Sparse, g.skippedOnDisk(env)...)
	}
	tree, err := g.writeTree(env)
	if err != nil {
		return res, err
	}
	baseTree, err := g.out("rev-parse", base+"^{tree}")
	if err != nil {
		return res, err
	}
	// A nested repository with a commit becomes a gitlink to a commit that
	// exists only inside the slot: the main tree would get an empty
	// directory. Leave it out.
	diff, err := g.run(nil, nil, "diff-tree", "-r", "-z", "--no-renames", baseTree, tree)
	if err != nil {
		return res, err
	}
	var links []rawEntry
	for _, e := range parseRaw(diff) {
		if e.newMode == modeGitlink && e.oldMode != modeGitlink {
			links = append(links, e)
		}
	}
	if len(links) > 0 {
		for _, e := range links {
			args := []string{"update-index", "--force-remove", "--", e.path}
			if e.oldMode != modeNone {
				args = []string{"update-index", "--add", "--cacheinfo", e.oldMode + "," + e.oldID + "," + e.path}
			}
			if _, err := g.run(env, nil, args...); err != nil {
				return res, err
			}
			res.Nested = append(res.Nested, e.path)
		}
		if tree, err = g.writeTree(env); err != nil {
			return res, err
		}
	}
	res.Changed = tree != baseTree
	res.Commit = base
	if res.Changed {
		if res.Commit, err = g.commitTree("commit-tree", tree, "-p", base, "-m", msg); err != nil {
			return res, err
		}
	}
	if head, err := g.out("rev-parse", "-q", "--verify", "HEAD"); err == nil && head != base {
		res.Head = head
	}
	return res, nil
}

func (g git) writeTree(env []string) (string, error) {
	s, err := g.run(env, nil, "write-tree")
	return strings.TrimSpace(s), err
}

var reNoCommit = regexp.MustCompile(`'([^']+)' does not have a commit checked out`)

// addWarnings sorts the errors of `git add -A --ignore-errors` into nested
// repositories without a commit and paths outside the sparse-checkout cone.
// ok is false when anything else went wrong.
func addWarnings(err error) (nested, sparse []string, ok bool) {
	var ge *gitError
	if !errors.As(err, &ge) {
		return nil, nil, false
	}
	inSparse := false
	for _, l := range strings.Split(ge.msg, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "", strings.HasPrefix(l, "warning:"), l == "fatal: adding files failed":
		case strings.HasPrefix(l, "hint:"):
			inSparse = false
		case reNoCommit.MatchString(l):
			nested = append(nested, strings.TrimSuffix(reNoCommit.FindStringSubmatch(l)[1], "/"))
		case strings.HasPrefix(l, "The following paths and/or pathspecs matched paths that exist"):
			inSparse = true
		case strings.HasPrefix(l, "outside of your sparse-checkout definition"), strings.HasPrefix(l, "updated in the index"):
		case inSparse:
			sparse = append(sparse, l)
		default:
			return nil, nil, false
		}
	}
	return nested, sparse, true
}

// sparseCheckout reports whether the worktree uses sparse checkout.
func (g git) sparseCheckout() bool {
	s, _ := g.out("config", "--bool", "core.sparseCheckout")
	return s == "true"
}

// keepSparse stops git from quietly turning a file written outside the
// sparse-checkout cone into a tracked change (git >= 2.37 clears the
// skip-worktree bit of files present on disk), so such writes stay out of
// the commit consistently and skippedOnDisk can report them.
const keepSparse = "sparse.expectFilesOutsideOfPatterns=true"

// skippedOnDisk lists skip-worktree entries (outside the sparse cone) whose
// file exists on disk anyway: an agent wrote it, and git ignores it.
func (g git) skippedOnDisk(env []string) []string {
	out, err := g.run(env, nil, "-c", keepSparse, "ls-files", "-t", "-z")
	if err != nil {
		return nil
	}
	var on []string
	for _, rec := range strings.Split(out, "\x00") {
		if p, ok := strings.CutPrefix(rec, "S "); ok {
			if _, err := os.Lstat(filepath.Join(g.dir, filepath.FromSlash(p))); err == nil {
				on = append(on, p)
			}
		}
	}
	return on
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
