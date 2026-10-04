package orchestrator

import (
	"crypto/rand"
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

	"github.com/sparkz400/switchyard/internal/canon"
	"github.com/sparkz400/switchyard/internal/diag"
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
	path    string
	unlock  func()
	untrack func()
}

// release stops tracking the slot's processes, kills what the agents left
// running there (Unix), and unlocks it.
func (s *slot) release() {
	s.untrack()
	proc.ReapOrphans(pidFile(s.path))
	s.unlock()
}

// pidFile lists the agent processes running in a slot (see proc.TrackDir):
// a sy killed with SIGKILL cannot stop its agents, and the next user of
// the slot must not share it with them.
func pidFile(slot string) string { return slot + ".pid" }

// lockSlot locks a slot and makes sure no leftover agent of a crashed sy is
// still running in it.
func lockSlot(path string) (unlock func(), ok bool) {
	unlock, ok = proc.TryLock(path + ".lock")
	if !ok {
		return nil, false
	}
	if !proc.ReapOrphans(pidFile(path)) {
		diag.Logf("pool: %s skipped, an agent of an earlier sy is still running in it", path)
		diag.Health("leftover", "what", "orphan-agent", "path", path)
		unlock()
		return nil, false
	}
	return unlock, true
}

// acquireSlot locks a free pool worktree and moves it to commit, creating it
// when it does not exist yet. A slot that cannot be prepared is skipped.
func acquireSlot(root, commit string) (*slot, error) {
	dir := poolDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var firstErr error
	for i := 0; i < maxPoolSlots; i++ {
		path := filepath.Join(dir, strconv.Itoa(i))
		unlock, ok := lockSlot(path)
		if !ok {
			continue
		}
		if err := prepareSlot(root, path, commit); err != nil {
			unlock()
			diag.Logf("pool: slot %s unusable: %v", path, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		touch(path)
		return &slot{path: path, unlock: unlock, untrack: proc.TrackDir(path, pidFile(path))}, nil
	}
	if firstErr != nil {
		return nil, fmt.Errorf("no usable pool worktree: %w", firstErr)
	}
	return nil, fmt.Errorf("all %d pool worktrees are in use", maxPoolSlots)
}

// prepareSlot makes path a clean worktree at commit, reusing it when it is
// still a worktree of root and recreating it otherwise.
func prepareSlot(root, path, commit string) error {
	if isWorktreeOf(root, path) {
		err := resetSlot(path, commit)
		if err == nil {
			return nil
		}
		diag.Logf("pool: resetting %s failed, recreating it: %v", path, err)
	}
	if err := checkDisk(filepath.Dir(path)); err != nil {
		return err
	}
	g := git{root}
	if err := removeSlot(g.commonDir(), path); err != nil {
		return err
	}
	return g.addWorktree(path, commit)
}

// staleLockAge is when a slot's index.lock counts as left behind by a git
// the agent ran and that was killed.
const staleLockAge = time.Minute

// resetSlot moves an existing slot to commit and removes everything the
// previous agent left (files, nested repositories, an unfinished rebase or
// merge). Ignored files stay.
func resetSlot(path, commit string) error {
	if gd := slotGitDir(path); gd != "" {
		clearSlotState(gd)
	}
	wg := git{path}
	if _, err := wg.run(lfsSkip, nil, append(noHooks(), "checkout", "--force", "--detach", commit)...); err != nil {
		return err
	}
	// -ff: also nested repositories an agent created.
	_, err := wg.out("clean", "-ffd")
	return err
}

// clearSlotState removes in-progress operations from a slot's git dir; a
// checkout does not end them and the next agent would find e.g. "rebase in
// progress".
func clearSlotState(gd string) {
	for _, n := range []string{"rebase-merge", "rebase-apply", "sequencer", "MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "MERGE_RR",
		"AUTO_MERGE", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", filepath.Join("refs", "bisect")} {
		os.RemoveAll(filepath.Join(gd, n))
	}
	if m, _ := filepath.Glob(filepath.Join(gd, "BISECT_*")); len(m) > 0 {
		for _, f := range m {
			os.RemoveAll(f)
		}
	}
	// The slot lock is ours and leftover agents are gone: an old index.lock
	// was left by a git that was killed.
	lock := filepath.Join(gd, "index.lock")
	if st, err := os.Stat(lock); err == nil && time.Since(st.ModTime()) > staleLockAge {
		os.Remove(lock)
	}
}

// removeSlot deletes a pool worktree and the repository's record of it, and
// nothing else: `git worktree prune` would also drop the user's own
// worktrees whose directory is missing for a moment (an unplugged drive).
// A directory that cannot be deleted (Windows: a program still has a file
// open there) is moved aside as <slot>.trash-<random> for PrunePools and
// CleanPool to delete later, so the slot path is free again.
func removeSlot(common, path string) error {
	recs := slotRecords(common, path)
	if gd := slotGitDir(path); gd != "" && isRecordOf(gd, path) {
		recs = append(recs, gd)
	}
	if err := os.RemoveAll(path); err != nil {
		if _, serr := os.Lstat(path); serr == nil {
			if rerr := os.Rename(path, trashName(path)); rerr != nil {
				return fmt.Errorf("cannot delete or move %s: %v", path, err)
			}
			diag.Logf("pool: could not delete %s (%v); moved it aside", path, err)
			diag.Health("leftover", "what", "undeletable-slot", "path", path)
		}
	}
	for _, r := range recs {
		os.RemoveAll(r)
	}
	return nil
}

func trashName(path string) string {
	var b [4]byte
	rand.Read(b[:])
	return path + ".trash-" + hex.EncodeToString(b[:])
}

func isTrash(name string) bool { return strings.Contains(name, ".trash-") }

// slotRecords finds the worktree records (<common>/worktrees/<id>) that
// point at path.
func slotRecords(common, path string) []string {
	if common == "" {
		return nil
	}
	base := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if rec := filepath.Join(base, e.Name()); e.IsDir() && isRecordOf(rec, path) {
			out = append(out, rec)
		}
	}
	return out
}

// isRecordOf reports whether the worktree record rec belongs to path (its
// gitdir file names <path>/.git).
func isRecordOf(rec, path string) bool {
	b, err := os.ReadFile(filepath.Join(rec, "gitdir"))
	if err != nil {
		return false
	}
	p := filepath.FromSlash(strings.TrimSpace(string(b)))
	if !filepath.IsAbs(p) {
		p = filepath.Join(rec, p)
	}
	return looseSamePath(filepath.Dir(p), path)
}

// looseSamePath is samePath for paths that may no longer exist (symlinks in
// their parents are still resolved).
func looseSamePath(a, b string) bool {
	if samePath(a, b) {
		return true
	}
	return filepath.Base(a) == filepath.Base(b) && samePath(filepath.Dir(a), filepath.Dir(b))
}

// removeTrash deletes slot directories moved aside earlier; it returns the
// ones that still cannot be deleted.
func removeTrash(pd string) (left []string) {
	entries, _ := os.ReadDir(pd)
	for _, e := range entries {
		if isTrash(e.Name()) {
			p := filepath.Join(pd, e.Name())
			if os.RemoveAll(p) != nil {
				left = append(left, p)
			}
		}
	}
	return left
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
// another sy are skipped. Closing stop ends it before the next slot.
func prewarmPool(root, commit string, n int, stop <-chan struct{}) {
	dir := poolDir(root)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	for i := 0; i < n && i < maxPoolSlots; i++ {
		select {
		case <-stop:
			return
		default:
		}
		path := filepath.Join(dir, strconv.Itoa(i))
		unlock, ok := lockSlot(path)
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
			if removeSlot(g.commonDir(), path) == nil && g.addWorktree(path, commit) == nil {
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

// SamePath reports whether two paths name the same file or folder (see
// canonPath).
func SamePath(a, b string) bool { return samePath(a, b) }

// canonPath normalizes a path for comparison (see canon.Path: git's C:/x,
// macOS /private/var, Windows case and 8.3 names).
func canonPath(p string) string { return canon.Path(p) }

// CleanPool removes the pooled worktrees of the repo containing dir that are
// not in use and returns how many were removed. Slots that could not be
// deleted are not counted and are named in the error.
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
	common := git{root}.commonDir()
	n := 0
	var failed []string
	for _, e := range entries {
		if !e.IsDir() || isTrash(e.Name()) {
			continue
		}
		path := filepath.Join(pd, e.Name())
		unlock, ok := lockSlot(path)
		if !ok {
			continue // in use by a running sy
		}
		if err := removeSlot(common, path); err != nil {
			failed = append(failed, path)
			unlock()
			continue
		}
		n++
		// sy clean is explicit and leaves nothing behind. The lock file is
		// deleted while it is held: proc.TryLock refuses a lock on a file
		// that is no longer at its path, so no two sy can end up holding
		// different files for one slot.
		os.Remove(pidFile(path))
		os.Remove(path + ".lock")
		unlock()
	}
	failed = append(failed, removeTrash(pd)...)
	bench := filepath.Join(filepath.Dir(pd), "bench", "work")
	if unlock, ok := proc.TryLock(bench + ".lock"); ok {
		if _, err := os.Stat(bench); err == nil {
			if removeSlot(common, bench) == nil {
				n++
			} else {
				failed = append(failed, bench)
			}
		}
		unlock()
	}
	os.Remove(pd)               // only succeeds when empty
	os.Remove(filepath.Dir(pd)) // likewise
	if len(failed) > 0 {
		return n, fmt.Errorf("removed %d pooled worktree(s); could not delete %s (a program may still have files open there)", n, strings.Join(failed, ", "))
	}
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
			if isTrash(s.Name()) {
				info.Bytes += dirSize(path)
				continue
			}
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
	gd := filepath.FromSlash(strings.TrimSpace(strings.TrimPrefix(s, "gitdir:")))
	if !filepath.IsAbs(gd) { // worktree.useRelativePaths
		gd = filepath.Join(slot, gd)
	}
	return gd
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
			if !s.IsDir() || isTrash(s.Name()) {
				continue
			}
			path := filepath.Join(pd, s.Name())
			if time.Since(lastUse(path)) < maxIdle {
				continue
			}
			unlock, ok := lockSlot(path)
			if !ok {
				continue // in use right now
			}
			size := dirSize(path)
			common := ""
			if gd := slotGitDir(path); gd != "" {
				common = filepath.Dir(filepath.Dir(gd)) // <common>/worktrees/<id>
			}
			if removeSlot(common, path) == nil {
				removed++
				freed += size
			}
			unlock()
			// The lock file stays: deleting it could let two sy instances lock
			// different files for the same slot.
		}
		for _, s := range slots {
			if isTrash(s.Name()) {
				path := filepath.Join(pd, s.Name())
				size := dirSize(path)
				if os.RemoveAll(path) == nil {
					freed += size
				}
			}
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

// Leftover is something a sy that ended badly left behind.
type Leftover struct {
	Kind   string `json:"kind"` // orphan-agent, trash, temp
	Path   string `json:"path"`
	Detail string `json:"detail"`
	Bytes  uint64 `json:"bytes,omitempty"`
}

// tempPrefixes are the temporary files sy removes when it ends normally.
var tempPrefixes = []string{"sy-merge-", "sy-index-", "sy-pr-index-", "sy-pr-body-"}

// Leftovers lists what ended sy processes left behind: agents still
// running in a pool slot no sy holds, slot directories that could not be
// deleted, and temporary files older than a day. It changes nothing.
func Leftovers() []Leftover {
	var out []Leftover
	repos, _ := os.ReadDir(worktreesBase())
	for _, r := range repos {
		pd := filepath.Join(worktreesBase(), r.Name(), "pool")
		ents, _ := os.ReadDir(pd)
		for _, e := range ents {
			p := filepath.Join(pd, e.Name())
			switch {
			case e.IsDir() && isTrash(e.Name()):
				out = append(out, Leftover{Kind: "trash", Path: p, Bytes: dirSize(p),
					Detail: "pool worktree that could not be deleted (a program had a file open); `sy clean` retries"})
			case !e.IsDir() && strings.HasSuffix(e.Name(), ".pid"):
				slot := strings.TrimSuffix(p, ".pid")
				if _, err := os.Stat(slot + ".lock"); err == nil {
					unlock, free := proc.TryLock(slot + ".lock")
					if !free {
						continue // a running sy uses the slot
					}
					unlock()
				}
				if pids := proc.LiveOrphans(p); len(pids) > 0 {
					out = append(out, Leftover{Kind: "orphan-agent", Path: slot,
						Detail: fmt.Sprintf("agent process %v of an ended sy still runs in this pool worktree", pids)})
				}
			}
		}
	}
	tmp := os.TempDir()
	ents, _ := os.ReadDir(tmp)
	var n int
	var bytes uint64
	for _, e := range ents {
		name := e.Name()
		match := false
		for _, pre := range tempPrefixes {
			match = match || strings.HasPrefix(name, pre)
		}
		if !match {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			n++
			if info.IsDir() {
				bytes += dirSize(filepath.Join(tmp, name))
			} else {
				bytes += uint64(info.Size())
			}
		}
	}
	if n > 0 {
		out = append(out, Leftover{Kind: "temp", Path: tmp, Bytes: bytes,
			Detail: fmt.Sprintf("%d temporary sy-* file(s) older than a day", n)})
	}
	return out
}
