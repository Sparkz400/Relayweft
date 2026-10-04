package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sparkz400/switchyard/internal/proc"
)

// BenchWorkspace is a reusable worktree for `sy bench`: every run starts
// from a clean checkout of a commit, outside the user's working tree.
// Ignored files (node_modules, build caches) survive between runs so setup
// commands get faster after the first run.
type BenchWorkspace struct {
	root string
	Path string
}

// NewBenchWorkspace prepares the bench worktree of the repo containing dir.
func NewBenchWorkspace(dir string) (*BenchWorkspace, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("sy bench needs a git repository: %s", dir)
	}
	return &BenchWorkspace{root: root, Path: filepath.Join(repoCache(root), "bench", "work")}, nil
}

// Lock makes sure only one sy bench uses the workspace.
func (b *BenchWorkspace) Lock() (unlock func(), err error) {
	os.MkdirAll(filepath.Dir(b.Path), 0o755)
	unlock, ok := proc.TryLock(b.Path + ".lock")
	if !ok {
		return nil, fmt.Errorf("another sy bench is running for this repo")
	}
	return unlock, nil
}

// Head returns the commit the runs start from.
func (b *BenchWorkspace) Head() (string, error) { return git{b.root}.out("rev-parse", "HEAD") }

// Dirty reports whether the user's tree has changes that HEAD does not.
func (b *BenchWorkspace) Dirty() bool {
	s, _ := git{b.root}.out("status", "--porcelain")
	return s != ""
}

// Reset makes the workspace a clean checkout of commit.
func (b *BenchWorkspace) Reset(commit string) error {
	os.MkdirAll(filepath.Dir(b.Path), 0o755) // so the disk check can measure it
	if err := checkDisk(filepath.Dir(b.Path)); err != nil && !isWorktreeOf(b.root, b.Path) {
		return err
	}
	return prepareSlot(b.root, b.Path, commit)
}

// Resolve returns the full commit id of rev.
func (b *BenchWorkspace) Resolve(rev string) (string, error) {
	return git{b.root}.out("rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

// RestoreFiles sets paths in the workspace to their content at commit and
// removes the ones commit does not have (a history task's tests: the agent
// cannot pass the check by editing or deleting them).
func (b *BenchWorkspace) RestoreFiles(commit string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	g := git{b.Path}
	var in strings.Builder
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\r") {
			return fmt.Errorf("unsupported path %q", p)
		}
		in.WriteString(commit + ":" + p + "\n")
	}
	out, err := g.run(nil, []byte(in.String()), "cat-file", "--batch-check=%(objecttype)")
	if err != nil {
		return err
	}
	types := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(types) != len(paths) {
		return fmt.Errorf("git cat-file: %d answers for %d paths", len(types), len(paths))
	}
	var keep []string
	for i, p := range paths {
		if strings.TrimSpace(types[i]) == "blob" {
			keep = append(keep, p)
			continue
		}
		if err := os.Remove(g.full(p)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if len(keep) == 0 {
		return nil
	}
	_, err = g.run(lfsSkip, nulList(keep), "restore", "--source="+commit, "--worktree", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

// HistoryCommit is a past commit `sy bench --from-history` can turn into a
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
