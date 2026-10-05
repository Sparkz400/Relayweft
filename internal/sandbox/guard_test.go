package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
)

// addGitlink puts a submodule entry (mode 160000) for path into the index,
// as a repository with submodules has, without a real submodule.
func addGitlink(t *testing.T, repo, path string) {
	t.Helper()
	head := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+head+","+path)
}

// A submodule's .git lies in the work tree, outside the read-only git
// folder. One the agent wrote (here: where an unchecked-out submodule had
// none, as in a pool worktree) is removed after the run and the run fails;
// one that existed is put back.
func TestCheckUndoesSubmoduleGitFiles(t *testing.T) {
	repo := newRepo(t)
	addGitlink(t, repo, "libs/new")
	addGitlink(t, repo, "libs/old")
	os.MkdirAll(filepath.Join(repo, "libs", "new"), 0o755)
	os.MkdirAll(filepath.Join(repo, "libs", "old"), 0o755)
	orig := []byte("gitdir: ../../.git/modules/old\n")
	os.WriteFile(filepath.Join(repo, "libs", "old", ".git"), orig, 0o644)

	b := &Box{dir: repo, Name: "sy-1-2-ab"}
	mounts := b.guardLinks(false)
	// The existing one is mounted read-only over itself.
	if len(mounts) != 1 || mounts[0].Target != "/work/libs/old/.git" || mounts[0].Writable {
		t.Fatalf("mounts = %+v", mounts)
	}
	if err := b.Check(); err != nil {
		t.Fatalf("nothing changed, but: %v", err)
	}

	// What an agent could do in /work.
	os.WriteFile(filepath.Join(repo, "libs", "new", ".git"), []byte("gitdir: ../../mine\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "libs", "old", ".git"), []byte("gitdir: ../../mine\n"), 0o644)
	err := b.Check()
	if err == nil || !strings.Contains(err.Error(), "libs/new/.git") || !strings.Contains(err.Error(), "libs/old/.git") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repo, "libs", "new", ".git")); !os.IsNotExist(err) {
		t.Error("the new .git is still there")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "libs", "old", ".git")); string(got) != string(orig) {
		t.Errorf("the old .git was not put back: %q", got)
	}

	// A .git folder in place of a file goes too.
	os.Remove(filepath.Join(repo, "libs", "new", ".git"))
	os.MkdirAll(filepath.Join(repo, "libs", "new", ".git"), 0o755)
	if err := b.Check(); err == nil {
		t.Error("a new .git folder was not noticed")
	}
	if _, err := os.Lstat(filepath.Join(repo, "libs", "new", ".git")); !os.IsNotExist(err) {
		t.Error("the new .git folder is still there")
	}
}

// A symlink where a submodule is (in the container it can point anywhere
// on this machine) is removed itself, never what it points to.
func TestCheckRemovesSymlinkedSubmodule(t *testing.T) {
	repo := newRepo(t)
	addGitlink(t, repo, "sub")
	b := &Box{dir: repo}
	b.guardLinks(false)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(repo, "sub")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if err := b.Check(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repo, "sub")); !os.IsNotExist(err) {
		t.Error("the symlink is still there")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Error("sy removed what the symlink pointed to")
	}
}

func TestSafeRel(t *testing.T) {
	for rel, want := range map[string]bool{
		"sub": true, "libs/a b": true, "": false, "../x": false, "a/../../x": false, `a\..\x`: false, "/abs": false, "C:/x": false, ".": false,
	} {
		if safeRel(rel) != want {
			t.Errorf("safeRel(%q) = %v", rel, !want)
		}
	}
}

// One home per provider: an agent of one provider cannot leave files that
// another provider's CLI reads with that provider's credentials.
func TestHomePerProvider(t *testing.T) {
	repo := newRepo(t)
	claude, _ := ProjectHome(repo, "claude")
	codex, _ := ProjectHome(repo, "codex")
	checks, _ := ProjectHome(repo, "")
	if claude == codex || claude == checks || codex == checks || filepath.Dir(claude) != filepath.Dir(codex) {
		t.Errorf("homes: %s %s %s", claude, codex, checks)
	}
	if _, err := ProjectHome(repo, "../x"); err == nil {
		t.Error("a home name with a path in it was accepted")
	}
}

// Files that make a CLI or shell run commands are removed from a home
// before each run, except where your own mount goes; a symlink is removed
// itself, never followed.
func TestResetHome(t *testing.T) {
	home := t.TempDir()
	for _, f := range []string{".bashrc", ".gitconfig", ".claude/settings.json", ".claude/hooks/x.sh", ".codex/config.toml", ".codex/sessions/s.jsonl", ".claude/projects/-work/s.jsonl"} {
		p := filepath.Join(home, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("x"), 0o600)
	}
	resetHome(home, []Mount{{Source: "/mine", Target: "/sy/home/.gitconfig"}})
	for f, want := range map[string]bool{
		".bashrc": false, ".claude/settings.json": false, ".claude/hooks/x.sh": false, ".codex/config.toml": false,
		".gitconfig": true, ".codex/sessions/s.jsonl": true, ".claude/projects/-work/s.jsonl": true,
	} {
		_, err := os.Stat(filepath.Join(home, filepath.FromSlash(f)))
		if (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", f, err == nil, want)
		}
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "settings.json"), []byte("x"), 0o600)
	os.RemoveAll(filepath.Join(home, ".claude"))
	if err := os.Symlink(outside, filepath.Join(home, ".claude")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	resetHome(home, nil)
	if _, err := os.Stat(filepath.Join(outside, "settings.json")); err != nil {
		t.Error("resetHome followed a symlink out of the home")
	}
	if _, err := os.Lstat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Error("the symlink is still there")
	}
}

// A folder that holds the container runtime's socket is refused as a
// mount: the agent would get the runtime, and with it this machine.
func TestUserMountsRefuseRuntimeSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sock := filepath.Join(home, ".docker", "run", "docker.sock")
	os.MkdirAll(filepath.Dir(sock), 0o700)
	os.WriteFile(sock, nil, 0o600)
	_, err := userMounts(config.SandboxCfg{Mounts: []config.SandboxMount{{Path: "~", Target: "/h"}}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "docker.sock") {
		t.Errorf("err = %v", err)
	}
	if _, err := userMounts(config.SandboxCfg{Mounts: []config.SandboxMount{{Path: filepath.Join(home, "elsewhere"), Target: "/h"}}}, t.TempDir()); err == nil || strings.Contains(err.Error(), "docker.sock") {
		t.Errorf("a folder without the socket: %v", err) // missing: a plain error
	}
}

// Submodule configs and config.worktree in the git folder are readable in
// the container: they get safe copies over them, without remote URLs.
func TestGitLayoutSanitizesSubmoduleConfigs(t *testing.T) {
	repo := newRepo(t)
	mod := filepath.Join(repo, ".git", "modules", "lib")
	os.MkdirAll(mod, 0o755)
	os.WriteFile(filepath.Join(mod, "config"), []byte("[core]\n\trepositoryformatversion = 0\n\tworktree = ../../../lib\n[remote \"origin\"]\n\turl = https://x:ghs_secret@example.com/lib.git\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".git", "config.worktree"), []byte("[credential]\n\thelper = store\n"), 0o644)
	mounts, _, err := gitLayout(repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, m := range mounts {
		if b, err := os.ReadFile(m.Source); err == nil {
			seen[m.Target] = string(b)
		}
	}
	lib, ok := seen["/work/.git/modules/lib/config"]
	if !ok || strings.Contains(lib, "ghs_secret") || strings.Contains(lib, "origin") || !strings.Contains(lib, "worktree = ../../../lib") {
		t.Errorf("submodule config in the container: %v %q", ok, lib)
	}
	if wt, ok := seen["/work/.git/config.worktree"]; !ok || strings.Contains(wt, "helper") {
		t.Errorf("config.worktree in the container: %v %q", ok, wt)
	}
}
