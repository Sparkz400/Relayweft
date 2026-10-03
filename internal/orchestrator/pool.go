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
	"sync/atomic"
	"time"

	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/sysload"
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
// It is keyed by the repository's shared git directory, so every worktree
// of a repo (the main tree, the bench workspace) shares one pool.
func repoCache(root string) string {
	key := root
	if common, err := (git{root}).out("rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil && common != "" {
		key = common
	}
	h := sha1.Sum([]byte(canonPath(key)))
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
		touch(path)
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
	if err := checkDisk(filepath.Dir(path)); err != nil {
		return err
	}
	g := git{root}
	g.removeWorktree(path)
	return g.addWorktree(path, commit)
}

// minFreeDisk is the free space below which no new pool worktree is created
// (orchestrator.min_free_disk_gb; 0 = no check).
var minFreeDisk atomic.Uint64

// errLowDisk makes the caller fall back to the main tree.
type errLowDisk struct{ free, min uint64 }

func (e errLowDisk) Error() string {
	return fmt.Sprintf("only %s free on the pool's disk (minimum %s): not creating another worktree", humanBytes(e.free), humanBytes(e.min))
}

func checkDisk(dir string) error {
	min := minFreeDisk.Load()
	if min == 0 {
		return nil
	}
	if free, ok := sysload.DiskFree(dir); ok && free < min {
		return errLowDisk{free, min}
	}
	return nil
}

// touch records a slot's last use (the lock file's mtime) for pruning.
func touch(path string) {
	now := time.Now()
	os.Chtimes(path+".lock", now, now)
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
			if checkDisk(dir) != nil {
				unlock()
				return
			}
			touch(path)
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

// PoolInfo describes one repo's worktree pool.
type PoolInfo struct {
	Dir      string    // the pool directory
	Repo     string    // the repository it belongs to ("" if unknown)
	Slots    int       // worktrees in the pool
	Bytes    uint64    // disk used
	LastUsed time.Time // most recent use of any slot
}

func worktreesBase() string {
	base, err := cacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "switchyard", "worktrees")
}

// Pools lists every repo's pool with its size. Walking big pools takes a
// moment, so callers run it off the UI thread.
func Pools() []PoolInfo {
	repos, _ := os.ReadDir(worktreesBase())
	var out []PoolInfo
	for _, r := range repos {
		pd := filepath.Join(worktreesBase(), r.Name(), "pool")
		slots, err := os.ReadDir(pd)
		if err != nil {
			continue
		}
		info := PoolInfo{Dir: pd}
		for _, s := range slots {
			if !s.IsDir() {
				continue
			}
			path := filepath.Join(pd, s.Name())
			info.Slots++
			info.Bytes += dirSize(path)
			if t := lastUse(path); t.After(info.LastUsed) {
				info.LastUsed = t
			}
			if info.Repo == "" {
				info.Repo = slotRepo(path)
			}
		}
		if info.Slots > 0 {
			out = append(out, info)
		}
	}
	return out
}

// PoolSize returns the disk used by the pool of the repo at root.
func PoolSize(root string) uint64 { return dirSize(poolDir(root)) }

func lastUse(slot string) time.Time {
	if st, err := os.Stat(slot + ".lock"); err == nil {
		return st.ModTime()
	}
	if st, err := os.Stat(slot); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func dirSize(dir string) uint64 {
	var n uint64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += uint64(fi.Size())
			}
		}
		return nil
	})
	return n
}

// slotGitDir reads a worktree's .git file ("gitdir: <repo>/.git/worktrees/x").
func slotGitDir(slot string) string {
	b, err := os.ReadFile(filepath.Join(slot, ".git"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if !strings.HasPrefix(s, "gitdir:") {
		return ""
	}
	return filepath.FromSlash(strings.TrimSpace(strings.TrimPrefix(s, "gitdir:")))
}

// slotRepo is the working directory of the repository a slot belongs to.
func slotRepo(slot string) string {
	gd := slotGitDir(slot)
	if gd == "" {
		return ""
	}
	common := filepath.Dir(filepath.Dir(gd)) // <repo>/.git
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common
}

// PrunePools removes pool worktrees not used for maxIdle (and not in use),
// across all repos, and returns how many and how much disk was freed.
func PrunePools(maxIdle time.Duration) (removed int, freed uint64) {
	if maxIdle <= 0 {
		return 0, 0
	}
	repos, _ := os.ReadDir(worktreesBase())
	for _, r := range repos {
		pd := filepath.Join(worktreesBase(), r.Name(), "pool")
		slots, err := os.ReadDir(pd)
		if err != nil {
			continue
		}
		for _, s := range slots {
			if !s.IsDir() {
				continue
			}
			path := filepath.Join(pd, s.Name())
			if time.Since(lastUse(path)) < maxIdle {
				continue
			}
			unlock, ok := proc.TryLock(path + ".lock")
			if !ok {
				continue // in use right now
			}
			size := dirSize(path)
			gd := slotGitDir(path)
			if err := os.RemoveAll(path); err == nil {
				removed++
				freed += size
				if gd != "" {
					// Drop the repo's worktree record for the deleted slot.
					git{"."}.out("--git-dir="+filepath.Dir(filepath.Dir(gd)), "worktree", "prune")
				}
			}
			unlock()
			os.Remove(path + ".lock")
		}
		os.Remove(pd)
		os.Remove(filepath.Dir(pd))
	}
	return removed, freed
}

func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}

// HumanBytes formats a byte count (exported for sy doctor).
func HumanBytes(n uint64) string { return humanBytes(n) }
