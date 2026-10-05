package orchestrator

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sparkz400/relayweft/internal/proc"
)

// BenchWorkspace is where `rw bench` runs: a standalone repository outside
// the user's working tree, rebuilt for every run from one commit's files.
// It holds a single commit with that tree and nothing else: no history, no
// remote, no objects or refs shared with the user's repo. So an agent
// cannot look up a history task's solution with git (`git log --all`,
// `git show <sha>`), nor the work of another mode's run. Ignored files
// (node_modules, build caches) survive between runs so setup commands get
// faster after the first run.
type BenchWorkspace struct {
	root string
	Path string
}

// NewBenchWorkspace prepares the bench workspace of the repo containing dir.
func NewBenchWorkspace(dir string) (*BenchWorkspace, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("rw bench needs a git repository: %s", dir)
	}
	return &BenchWorkspace{root: root, Path: filepath.Join(repoCache(root), "bench", "work")}, nil
}

// Lock makes sure only one rw bench uses the workspace.
func (b *BenchWorkspace) Lock() (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(b.Path), 0o755); err != nil {
		return nil, fmt.Errorf("bench workspace: %w", err)
	}
	unlock, ok := proc.TryLock(b.Path + ".lock")
	if !ok {
		return nil, fmt.Errorf("another rw bench is running for this repo")
	}
	return unlock, nil
}

// Head returns the commit the runs start from.
func (b *BenchWorkspace) Head() (string, error) { return git{b.root}.out("rev-parse", "HEAD") }

// Dirty reports whether the user's tree has changes that HEAD does not.
func (b *BenchWorkspace) Dirty() bool {
	s, _ := git{b.root}.out("status", "--porcelain", "--ignore-submodules=all")
	return s != ""
}

// BenchCommitMessage is the message of the workspace's only commit.
const BenchCommitMessage = "rw bench: the task's starting point (the repository's history is left out)"

// Reset makes the workspace a fresh repository whose only commit has the
// files of commit (a new commit, not the original one).
func (b *BenchWorkspace) Reset(commit string) error {
	gd := filepath.Join(b.Path, ".git")
	st, err := os.Lstat(gd)
	own := err == nil && st.IsDir()
	_ = os.MkdirAll(filepath.Dir(b.Path), 0o755) // so the disk check can measure it
	if err := checkDisk(filepath.Dir(b.Path)); err != nil && !own {
		return err
	}
	tree, err := git{b.root}.out("rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if err != nil {
		return err
	}
	if own {
		// The pool worktrees of the previous run's repository hold its work.
		_, _ = CleanPool(b.Path)
	} else if _, err := os.Lstat(b.Path); err == nil {
		// The workspace of an older rw (a worktree sharing the repo's
		// history) or a broken one: remove it and its worktree record.
		if err := removeSlot(git{b.root}.commonDir(), b.Path); err != nil {
			return err
		}
	}
	// The old index lets the checkout below keep the files that did not
	// change (their stat data is still right) instead of writing them all.
	oldIndex := b.Path + ".index"
	os.Remove(oldIndex)
	_ = os.Rename(filepath.Join(gd, "index"), oldIndex)
	if err := os.RemoveAll(gd); err != nil {
		return err
	}
	if err := os.MkdirAll(b.Path, 0o755); err != nil {
		return err
	}
	g := git{b.Path}
	// No templates: the user's could bring hooks.
	if _, err := g.run(nil, nil, "init", "-q", "--template=", "--initial-branch=main", "."); err != nil {
		return err
	}
	if err := b.copySettings(gd); err != nil {
		return err
	}
	if err := b.loadTree(tree); err != nil {
		return err
	}
	c, err := g.commitTree(tree, nil, BenchCommitMessage)
	if err != nil {
		return err
	}
	if _, err := g.run(nil, nil, "update-ref", "HEAD", c); err != nil {
		return err
	}
	reset := func() error {
		_, err := g.run(lfsSkip, nil, append(noHooks(), "reset", "-q", "--hard", c)...)
		return err
	}
	if os.Rename(oldIndex, filepath.Join(gd, "index")) != nil || reset() != nil {
		os.Remove(filepath.Join(gd, "index")) // no usable index: write every file
		if err := reset(); err != nil {
			return err
		}
	}
	// -ff: also nested repositories an agent created. Ignored files stay.
	_, err = g.out("clean", "-ffdq")
	return err
}

// benchSettings are the repo-local settings the workspace takes over: how
// files are checked out (line endings, filters like Git LFS's) and who
// commits, not where the repo came from (remotes, branches).
var benchSettings = regexp.MustCompile(`^(core\.(autocrlf|eol|safecrlf|longpaths)|filter\..+|user\.(name|email))$`)

// copySettings gives the new repository at gd the user's repo-local
// checkout settings, info/exclude and info/attributes.
func (b *BenchWorkspace) copySettings(gd string) error {
	src := git{b.root}
	out, _ := src.run(nil, nil, "config", "--local", "-z", "--list")
	g := git{b.Path}
	for _, kv := range strings.Split(out, "\x00") {
		k, v, _ := strings.Cut(kv, "\n")
		if benchSettings.MatchString(k) {
			if _, err := g.run(nil, nil, "config", "--add", k, v); err != nil {
				return err
			}
		}
	}
	common := src.commonDir()
	for _, f := range []string{"exclude", "attributes"} {
		if data, err := os.ReadFile(filepath.Join(common, "info", f)); err == nil {
			_ = os.MkdirAll(filepath.Join(gd, "info"), 0o755)
			if err := os.WriteFile(filepath.Join(gd, "info", f), data, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadTree copies tree and everything in it into the workspace's object
// store. The pack of the last tree is kept next to the workspace (hard
// links where the disk allows), so the next run from the same commit (the
// other modes of a task) does not pack it again.
func (b *BenchWorkspace) loadTree(tree string) error {
	packs := filepath.Join(b.Path, ".git", "objects", "pack")
	cache := filepath.Join(filepath.Dir(b.Path), "packs", tree)
	if linkFiles(cache, packs) == nil {
		return nil
	}
	list, err := git{b.root}.run(nil, nil, "rev-list", "--objects", tree)
	if err != nil {
		return err
	}
	if err := copyObjects(b.root, b.Path, []byte(list)); err != nil {
		return err
	}
	// Complete or not at all: a half-written cache would be used next time.
	os.RemoveAll(filepath.Dir(cache))
	tmp := cache + ".tmp"
	if linkFiles(packs, tmp) != nil || os.Rename(tmp, cache) != nil {
		os.RemoveAll(filepath.Dir(cache))
	}
	return nil
}

// linkFiles hard-links (or else copies) the files in from into to.
func linkFiles(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("%s is empty", from)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		src, dst := filepath.Join(from, e.Name()), filepath.Join(to, e.Name())
		if os.Link(src, dst) == nil {
			continue
		}
		data, err := os.ReadFile(src)
		if err == nil {
			err = os.WriteFile(dst, data, 0o644)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// copyObjects copies the objects in list (one id per line, optionally
// followed by a path) from repository src into dst as one pack. dst gets
// copies and no link to src (no alternates, no remote).
func copyObjects(src, dst string, list []byte) error {
	pack := exec.Command("git", proc.GitArgs("pack-objects", "--stdout", "-q")...)
	pack.Dir, pack.Stdin = src, bytes.NewReader(list)
	index := exec.Command("git", proc.GitArgs("index-pack", "--stdin")...)
	index.Dir = dst
	pipe, err := pack.StdoutPipe()
	if err != nil {
		return err
	}
	index.Stdin = pipe
	var perr, ierr bytes.Buffer
	pack.Stderr, index.Stderr = &perr, &ierr
	proc.Background(pack)
	proc.Background(index)
	gitRuns.Add(2)
	if err := pack.Start(); err != nil {
		return err
	}
	proc.Started(pack)
	if err := index.Start(); err != nil {
		_ = pack.Process.Kill()
		_ = pack.Wait()
		return err
	}
	proc.Started(index)
	ierrRun := index.Wait()
	perrRun := pack.Wait()
	switch {
	case perrRun != nil:
		return fmt.Errorf("git pack-objects: %v: %s", perrRun, strings.TrimSpace(perr.String()))
	case ierrRun != nil:
		return fmt.Errorf("git index-pack: %v: %s", ierrRun, strings.TrimSpace(ierr.String()))
	}
	return nil
}

// Resolve returns the full commit id of rev.
func (b *BenchWorkspace) Resolve(rev string) (string, error) {
	return git{b.root}.out("rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

// treeEntry is one `git ls-tree` entry.
type treeEntry struct{ mode, kind, id string }

// entriesAt looks paths up in commit of the user's repo; the paths commit
// does not have are missing from the map.
func (b *BenchWorkspace) entriesAt(commit string, paths []string) (map[string]treeEntry, error) {
	found := map[string]treeEntry{}
	for len(paths) > 0 {
		chunk := paths[:min(100, len(paths))]
		paths = paths[len(chunk):]
		for _, p := range chunk {
			if strings.ContainsAny(p, "\n\r") {
				return nil, fmt.Errorf("unsupported path %q", p)
			}
		}
		out, err := git{b.root}.run(nil, nil, append([]string{"ls-tree", "-z", "--full-tree", commit, "--"}, chunk...)...)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(out, "\x00") {
			meta, p, ok := strings.Cut(line, "\t")
			if f := strings.Fields(meta); ok && len(f) == 3 {
				found[p] = treeEntry{f[0], f[1], f[2]}
			}
		}
	}
	return found, nil
}

// FilesAt returns the paths that are files in commit, in order.
func (b *BenchWorkspace) FilesAt(commit string, paths []string) ([]string, error) {
	found, err := b.entriesAt(commit, paths)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range paths {
		if found[p].kind == "blob" {
			out = append(out, p)
		}
	}
	return out, nil
}

// RestoreFiles sets paths in the workspace to their content at commit and
// removes the ones commit does not have (a history task's tests: the agent
// cannot pass the check by editing or deleting them). Only these files'
// contents are copied into the workspace's repository.
func (b *BenchWorkspace) RestoreFiles(commit string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	found, err := b.entriesAt(commit, paths)
	if err != nil {
		return err
	}
	g := git{b.Path}
	var ids, info bytes.Buffer
	for _, p := range paths {
		e := found[p]
		if e.kind != "blob" {
			if err := os.Remove(g.full(p)); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		ids.WriteString(e.id + "\n")
		fmt.Fprintf(&info, "%s %s\t%s\x00", e.mode, e.id, p)
	}
	if ids.Len() == 0 {
		return nil
	}
	if err := copyObjects(b.root, b.Path, ids.Bytes()); err != nil {
		return err
	}
	// A temporary index with just these files: checkout-index writes them
	// with the workspace's line endings and filters, like a checkout.
	idx := filepath.Join(b.Path, ".git", "rw-restore-index")
	os.Remove(idx)
	defer os.Remove(idx)
	env := append([]string{"GIT_INDEX_FILE=" + idx}, lfsSkip...)
	if _, err := g.run(env, info.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	_, err = g.run(env, nil, "checkout-index", "-a", "-f")
	return err
}

// HistoryCommit is a past commit `rw bench --from-history` can turn into a
// task.
type HistoryCommit struct {
	SHA, Parent string
	Message     string
	Files       []HistoryFile
	// Uncounted: only the file names were read; Lines and Binary are
	// unknown.
	Uncounted bool
}

// HistoryFile is one file a commit changed; Lines is added plus deleted
// lines (0 for binary files).
type HistoryFile struct {
	Path   string
	Lines  int
	Binary bool
}

// historyGit keeps the history scan cheap on big repos (a pack of many GB
// full of large binaries made `git log --numstat` grow to ~5 GB): a small
// window into the packs instead of mapping all of them, a small delta
// cache, and blobs over 1 MB count as binary without being read. With
// renames off a name list compares trees only, never file content.
var historyGit = []string{"-c", "core.quotePath=false", "-c", "core.bigFileThreshold=1m",
	"-c", "core.packedGitLimit=256m", "-c", "core.packedGitWindowSize=16m", "-c", "core.deltaBaseCacheLimit=32m",
	"-c", "diff.renames=false"}

// History returns up to limit non-merge commits reachable from HEAD, newest
// first. Commits with a path git had to quote (tabs, newlines, quotes) are
// left out. Only file names are read for all of them; the line counts only
// for the commits count accepts (the rest stay Uncounted), so git never
// diffs the content of commits that cannot become tasks.
func (b *BenchWorkspace) History(limit int, count func(HistoryCommit) bool) ([]HistoryCommit, error) {
	g := git{b.root}
	out, err := g.run(nil, nil, append(slices.Clone(historyGit), "log", "--no-merges", "--no-renames",
		"-n", strconv.Itoa(limit), "--format=%x1e%H%x1f%P%x1f%B%x1f", "--name-only", "HEAD", "--")...)
	if err != nil {
		return nil, err
	}
	list := parseHistory(out)
	var want strings.Builder
	for i := range list {
		list[i].Uncounted = true
		if count(list[i]) {
			want.WriteString(list[i].SHA + "\n")
		}
	}
	if want.Len() == 0 {
		return list, nil
	}
	out, err = g.run(nil, []byte(want.String()), append(slices.Clone(historyGit), "log", "--no-walk=unsorted", "--stdin",
		"--no-renames", "--no-textconv", "--no-ext-diff", "--format=%x1e%H%x1f%P%x1f%x1f", "--numstat", "--")...)
	if err != nil {
		return nil, err
	}
	counted := map[string][]HistoryFile{}
	for _, c := range parseHistory(out) {
		counted[c.SHA] = c.Files
	}
	for i, c := range list {
		if files, ok := counted[c.SHA]; ok {
			list[i].Files, list[i].Uncounted = files, false
		}
	}
	return list, nil
}

func parseHistory(out string) []HistoryCommit {
	var list []HistoryCommit
records:
	for _, rec := range strings.Split(out, "\x1e") {
		parts := strings.SplitN(rec, "\x1f", 4)
		if len(parts) != 4 {
			continue
		}
		parents := strings.Fields(parts[1])
		if len(parents) != 1 {
			continue // the root commit (or a merge)
		}
		c := HistoryCommit{SHA: strings.TrimSpace(parts[0]), Parent: parents[0], Message: strings.TrimSpace(parts[2])}
		for _, line := range strings.Split(parts[3], "\n") {
			f := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
			if len(f) == 1 && f[0] != "" {
				f = []string{"0", "0", f[0]} // --name-only
			}
			if len(f) != 3 {
				continue
			}
			if strings.HasPrefix(f[2], `"`) {
				continue records
			}
			hf := HistoryFile{Path: f[2], Binary: f[0] == "-"}
			if !hf.Binary {
				a, _ := strconv.Atoi(f[0])
				d, _ := strconv.Atoi(f[1])
				hf.Lines = a + d
			}
			c.Files = append(c.Files, hf)
		}
		if len(c.Files) > 0 {
			list = append(list, c)
		}
	}
	return list
}
