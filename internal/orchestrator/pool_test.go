package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pools go to a temporary cache dir, not the real user cache.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sy-cache-*")
	if err != nil {
		panic(err)
	}
	cacheDir = func() (string, error) { return dir, nil }
	stateDir = func() string { return filepath.Join(dir, "tasks") }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// A slot is reused for the next task: same directory, moved to the new
// commit, leftovers of the previous agent removed, ignored files kept.
func TestPoolSlotReuse(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	snap1, err := g.snapshot("1")
	if err != nil {
		t.Fatal(err)
	}
	s1, err := acquireSlot(dir, snap1)
	if err != nil {
		t.Fatal(err)
	}
	// The previous agent left an edit, an untracked file and an ignored cache.
	os.WriteFile(filepath.Join(s1.path, "shared.txt"), []byte("agent edit\n"), 0o644)
	os.WriteFile(filepath.Join(s1.path, "leftover.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(s1.path, "ignored"), 0o755)
	os.WriteFile(filepath.Join(s1.path, "ignored", "cache.bin"), []byte("keep"), 0o644)

	// While s1 is held, a second acquire gets a different slot.
	s2, err := acquireSlot(dir, snap1)
	if err != nil {
		t.Fatal(err)
	}
	if s2.path == s1.path {
		t.Fatal("two holders got the same slot")
	}
	s2.release()
	s1.release()

	// The user's tree moves on; the next task gets slot 0 again at the new commit.
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("user v2\n"), 0o644)
	snap2, err := g.snapshot("2")
	if err != nil {
		t.Fatal(err)
	}
	s3, err := acquireSlot(dir, snap2)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.release()
	if s3.path != s1.path {
		t.Errorf("slot not reused: %s, want %s", s3.path, s1.path)
	}
	if got := read(t, filepath.Join(s3.path, "shared.txt")); got != "user v2\n" {
		t.Errorf("shared.txt = %q, want the new commit's content", got)
	}
	if _, err := os.Stat(filepath.Join(s3.path, "leftover.txt")); !os.IsNotExist(err) {
		t.Error("untracked leftover of the previous agent survived")
	}
	if got := read(t, filepath.Join(s3.path, "ignored", "cache.bin")); got != "keep" {
		t.Errorf("ignored cache = %q, want it kept", got)
	}
	if head, _ := (git{s3.path}).out("rev-parse", "HEAD"); head != snap2 {
		t.Errorf("slot HEAD = %s, want %s", head, snap2)
	}
}

// A slot directory that is no longer a valid worktree (deleted .git, pruned
// by the user) is recreated instead of failing.
func TestPoolRecreatesBrokenSlot(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	snap, _ := g.snapshot("s")
	s, err := acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	s.release()
	os.Remove(filepath.Join(path, ".git"))
	s, err = acquireSlot(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if s.path != path || !isWorktreeOf(dir, path) {
		t.Errorf("slot %s not recreated as a worktree", s.path)
	}
}

func TestPrewarmAndPerfTips(t *testing.T) {
	dir := gitRepo(t)
	snap, _ := (git{dir}).snapshot("s")
	prewarmPool(dir, snap, 2)
	for _, i := range []string{"0", "1"} {
		if !isWorktreeOf(dir, filepath.Join(poolDir(dir), i)) {
			t.Errorf("slot %s not prewarmed", i)
		}
	}
	if files, tips := PerfTips(dir); files != 3 || tips != nil {
		t.Errorf("PerfTips on a small repo = %d, %v", files, tips)
	}
	if n, _ := CleanPool(dir); n != 2 {
		t.Errorf("CleanPool removed %d, want 2", n)
	}
	if _, err := os.Stat(poolDir(dir)); !os.IsNotExist(err) {
		t.Error("pool dir left behind")
	}
	wl, _ := (git{dir}).out("worktree", "list")
	if strings.Count(wl, "\n") != 0 {
		t.Errorf("worktrees left:\n%s", wl)
	}
}
