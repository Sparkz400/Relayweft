package orchestrator

import (
	"fmt"
	"path/filepath"
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

// Head returns the commit the runs start from.
func (b *BenchWorkspace) Head() (string, error) { return git{b.root}.out("rev-parse", "HEAD") }

// Dirty reports whether the user's tree has changes that HEAD does not.
func (b *BenchWorkspace) Dirty() bool {
	s, _ := git{b.root}.out("status", "--porcelain")
	return s != ""
}

// Reset makes the workspace a clean checkout of commit.
func (b *BenchWorkspace) Reset(commit string) error {
	if err := checkDisk(filepath.Dir(b.Path)); err != nil && !isWorktreeOf(b.root, b.Path) {
		return err
	}
	return prepareSlot(b.root, b.Path, commit)
}
