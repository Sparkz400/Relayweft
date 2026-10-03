package orchestrator

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/sparkz400/switchyard/internal/proc"
)

// Worktree pool:
//
//   - Writing agents run in pooled worktrees that persist between tasks
//     (<user cache dir>/switchyard/worktrees/<repo hash>/pool/<n>) instead of
//     a fresh `git worktree add` per agent and task.
//   - Acquiring a slot moves it to the task's integration commit with
//     `git checkout --force --detach` plus `git clean -fd`: git rewrites only
//     the files that differ, so after the first use a slot is ready almost
//     instantly however big the repo is.
//   - Ignored files (node_modules, build caches) survive in the slots, so
//     agents can run builds there once those exist.
//   - Each slot has a lock file held while an agent uses it; the OS releases
//     it if sy crashes, and two sy instances never share a slot.
//   - The first slots are created in the background while the planner runs.
//   - `sy clean` removes the pool.

const maxPoolSlots = 16

// cacheDir is where pools live; tests point it at a temporary directory.
var cacheDir = os.UserCacheDir

// repoCache is Switchyard's cache directory for a repo: outside the repo, so
// `git status` in the user's tree stays clean.
func repoCache(root string) string {
	h := sha1.Sum([]byte(canonPath(root)))
	base, err := cacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "switchyard", "worktrees", hex.EncodeToString(h[:])[:12])
}

func poolDir(root string) string { return filepath.Join(repoCache(root), "pool") }

type slot struct {
	path   string
	unlock func()
}

func (s *slot) release() { s.unlock() }

// acquireSlot locks a free pool worktree and moves it to commit, creating it
// when it does not exist yet.
func acquireSlot(root, commit string) (*slot, error) {
	dir := poolDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for i := 0; i < maxPoolSlots; i++ {
		path := filepath.Join(dir, strconv.Itoa(i))
		unlock, ok := proc.TryLock(path + ".lock")
		if !ok {
			continue
		}
		if err := prepareSlot(root, path, commit); err != nil {
			unlock()
			return nil, err
		}
		return &slot{path: path, unlock: unlock}, nil
	}
	return nil, fmt.Errorf("all %d pool worktrees are in use", maxPoolSlots)
}

// prepareSlot makes path a clean worktree at commit, reusing it when it is
// still a worktree of root and recreating it otherwise.
func prepareSlot(root, path, commit string) error {
	if isWorktreeOf(root, path) {
		wg := git{path}
		if _, err := wg.run(lfsSkip, nil, "checkout", "--force", "--detach", commit); err == nil {
			if _, err := wg.out("clean", "-fd"); err == nil {
				return nil
			}
		}
	}
	g := git{root}
	g.removeWorktree(path)
	return g.addWorktree(path, commit)
}

// prewarmPool creates the first n slots at commit if they are missing, so
// the expensive first checkout overlaps with planning. Slots in use by
// another sy are skipped.
func prewarmPool(root, commit string, n int) {
	dir := poolDir(root)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	for i := 0; i < n && i < maxPoolSlots; i++ {
		path := filepath.Join(dir, strconv.Itoa(i))
		unlock, ok := proc.TryLock(path + ".lock")
		if !ok {
			continue
		}
		if !isWorktreeOf(root, path) {
			g := git{root}
			g.removeWorktree(path)
			if g.addWorktree(path, commit) == nil {
				// The first status check of a fresh checkout re-reads every
				// file (racily clean entries); pay for it here, in the
				// background, instead of when an agent needs the slot.
				git{path}.out("update-index", "-q", "--refresh")
			}
		}
		unlock()
	}
}

// isWorktreeOf reports whether path is the top of a git worktree belonging to
// root's repository.
func isWorktreeOf(root, path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	s, err := git{path}.out("rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	lines := strings.Split(s, "\n")
	if err != nil || len(lines) != 2 || !samePath(lines[0], path) {
		return false
	}
	common, err := git{root}.out("rev-parse", "--path-format=absolute", "--git-common-dir")
	return err == nil && samePath(strings.TrimSpace(lines[1]), common)
}

func samePath(a, b string) bool { return canonPath(a) == canonPath(b) }

// canonPath normalizes a path for comparison: git prints C:/x where Go uses
// C:\x, git resolves symlinks (macOS /var -> /private/var), and Windows paths
// are case-insensitive.
func canonPath(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// CleanPool removes the pooled worktrees of the repo containing dir that are
// not in use and returns how many were removed.
func CleanPool(dir string) (int, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("not a git repository: %s", dir)
	}
	pd := poolDir(root)
	entries, err := os.ReadDir(pd)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(pd, e.Name())
		unlock, ok := proc.TryLock(path + ".lock")
		if !ok {
			continue // in use by a running sy
		}
		git{root}.removeWorktree(path)
		unlock()
		os.Remove(path + ".lock")
		n++
	}
	os.Remove(pd)               // only succeeds when empty
	os.Remove(filepath.Dir(pd)) // likewise
	return n, nil
}

// bigRepoFiles is where PerfTips starts suggesting git settings.
const bigRepoFiles = 5000

// PerfTips suggests repo-local git settings that make snapshots fast in big
// repos: the built-in file system monitor (Windows and macOS) and the
// untracked cache. It returns the tracked file count and the commands to run.
func PerfTips(dir string) (files int, tips []string) {
	if !isRepo(dir) {
		return 0, nil
	}
	g := git{dir}
	files = g.trackedFiles()
	if files < bigRepoFiles {
		return files, nil
	}
	set := func(key string) bool {
		v, _ := g.out("config", key)
		return v != "" && !strings.EqualFold(v, "false")
	}
	if (runtime.GOOS == "windows" || runtime.GOOS == "darwin") && !set("core.fsmonitor") {
		tips = append(tips, "git config core.fsmonitor true")
	}
	if !set("core.untrackedCache") {
		tips = append(tips, "git config core.untrackedCache true")
	}
	return files, tips
}
