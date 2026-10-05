package sandbox

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/proc"
)

// Submodules. The container cannot write the repository's git folder, but
// a submodule's .git (a file, or missing in a pool worktree where
// submodules are not checked out) lies in the work tree. An agent that
// wrote one could point it at a folder of its own with a git config that
// runs commands, and git on this machine would act on it the next time it
// looks into that submodule. Besides proc.GitGuard on sy's own git, the
// sandbox mounts each existing submodule .git read-only, and after the run
// checks every submodule's .git against what it was before: one that
// appeared or changed is removed or put back, and the run fails.

// linkState is what a submodule's .git was before the run.
type linkState struct {
	kind string // missing, file, dir, other
	data []byte // a file's content; a folder's digest
}

// gitlinks lists the submodule paths in the index of the repository at dir
// (git ls-files -s: mode 160000), relative, with forward slashes.
func gitlinks(dir string) []string {
	args := append(append([]string(nil), proc.GitGuard...), "ls-files", "-s", "-z")
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	proc.Background(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var links []string
	for _, rec := range bytes.Split(out, []byte{0}) {
		meta, p, ok := bytes.Cut(rec, []byte{'\t'})
		if ok && bytes.HasPrefix(meta, []byte("160000 ")) {
			links = append(links, string(p))
		}
	}
	return links
}

// linkStateOf reads the .git of the submodule at rel without following
// symlinks (an agent may have left one anywhere in the work tree).
func linkStateOf(dir, rel string) linkState {
	p := filepath.Join(dir, filepath.FromSlash(rel), ".git")
	st, err := os.Lstat(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return linkState{kind: "missing"}
	case err != nil:
		return linkState{kind: "other", data: []byte(err.Error())}
	case st.Mode().IsRegular():
		b, err := os.ReadFile(p)
		if err != nil {
			return linkState{kind: "other", data: []byte(err.Error())}
		}
		return linkState{kind: "file", data: b}
	case st.IsDir():
		return linkState{kind: "dir", data: dirDigest(p)}
	}
	return linkState{kind: "other", data: []byte(st.Mode().String())}
}

// dirDigest sums the parts of a .git folder that decide what git runs and
// where it looks: config, HEAD, commondir, gitdir, hooks, info.
func dirDigest(gitDir string) []byte {
	h := sha256.New()
	for _, n := range []string{"config", "config.worktree", "HEAD", "commondir", "gitdir"} {
		b, _ := os.ReadFile(filepath.Join(gitDir, n))
		fmt.Fprintf(h, "%s %d\n", n, len(b))
		h.Write(b)
	}
	for _, sub := range []string{"hooks", "info"} {
		filepath.WalkDir(filepath.Join(gitDir, sub), func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				info, _ := d.Info()
				if info != nil {
					fmt.Fprintf(h, "%s %v %d %d\n", p, info.Mode(), info.Size(), info.ModTime().UnixNano())
				}
			}
			return nil
		})
	}
	return h.Sum(nil)
}

// guardLinks records the submodules' .git before the run and returns the
// read-only mounts for those that exist (regular files and folders only).
func (b *Box) guardLinks(readOnly bool) []Mount {
	b.links = map[string]linkState{}
	var mounts []Mount
	for _, rel := range gitlinks(b.dir) {
		if !safeRel(rel) {
			continue
		}
		st := linkStateOf(b.dir, rel)
		b.links[rel] = st
		if !readOnly && (st.kind == "file" || st.kind == "dir") {
			mounts = append(mounts, Mount{Source: filepath.Join(b.dir, filepath.FromSlash(rel), ".git"), Target: Work + "/" + rel + "/.git"})
		}
	}
	return mounts
}

// Check compares every submodule's .git with what it was before the run.
// One that appeared or changed, or a symlink in its path, is removed (a
// file is put back), and the error says so: the run must count as failed.
// nil when nothing changed.
func (b *Box) Check() error {
	if b == nil || len(b.links) == 0 {
		return nil
	}
	var bad []string
	for rel, before := range b.links {
		if link := symlinkIn(b.dir, rel); link != "" {
			os.Remove(link) // the link itself, never what it points to
			bad = append(bad, filepath.ToSlash(strings.TrimPrefix(link, b.dir+string(filepath.Separator)))+" (a symlink)")
			continue
		}
		now := linkStateOf(b.dir, rel)
		if now.kind == before.kind && bytes.Equal(now.data, before.data) {
			continue
		}
		p := filepath.Join(b.dir, filepath.FromSlash(rel), ".git")
		switch before.kind {
		case "missing":
			os.RemoveAll(p)
		case "file":
			os.RemoveAll(p)
			os.WriteFile(p, before.data, 0o644)
		}
		bad = append(bad, rel+"/.git")
	}
	if len(bad) == 0 {
		return nil
	}
	slices.Sort(bad)
	diag.Logf("sandbox %s: submodule .git changed in the container: %s", b.Name, strings.Join(bad, ", "))
	diag.Health("sandbox-escape-attempt", "container", b.Name, "paths", strings.Join(bad, " "))
	return fmt.Errorf("sandbox: the agent changed %s, which git on this machine would follow (a way out of the sandbox); sy removed or restored it, and the run counts as failed", strings.Join(bad, ", "))
}

// symlinkIn returns the first part of rel's path in dir, then its .git,
// that is a symlink, or "".
func symlinkIn(dir, rel string) string {
	return symlinkOnPath(dir, append(strings.Split(rel, "/"), ".git"))
}

// symlinkOnPath walks parts below base and returns the first that is a
// symlink ("" when none is, or the path ends early).
func symlinkOnPath(base string, parts []string) string {
	p := base
	for _, part := range parts {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err != nil {
			return ""
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return p
		}
	}
	return ""
}

// safeRel accepts a relative path inside the work tree, with forward
// slashes and no backslashes (Windows would read those as separators).
func safeRel(rel string) bool {
	if rel == "" || strings.Contains(rel, `\`) || strings.Contains(rel, ":") || path.IsAbs(rel) {
		return false
	}
	c := path.Clean(rel)
	return c == rel && c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

// homeResets are files in a sandbox home that make a CLI or shell run
// commands, and that sy never puts there: an agent may have left them for
// the next agent's CLI. They are removed before every run (unless a mount
// of yours goes there).
var homeResets = []string{
	".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshrc", ".zshenv", ".zprofile",
	".gitconfig", ".config/git", ".npmrc",
	".claude/settings.json", ".claude/settings.local.json", ".claude/hooks", ".claude/commands", ".claude/agents",
	".codex/config.toml", ".gemini/settings.json", ".qwen/settings.json",
}

// resetHome removes homeResets from home, without following symlinks
// (os.RemoveAll removes a link, not what it points to), except where one
// of mounts goes.
func resetHome(home string, mounts []Mount) {
	for _, rel := range homeResets {
		target := Home + "/" + rel
		if slices.ContainsFunc(mounts, func(m Mount) bool {
			return m.Target == target || strings.HasPrefix(m.Target, target+"/") || strings.HasPrefix(target, m.Target+"/")
		}) {
			continue
		}
		if link := symlinkOnPath(home, strings.Split(rel, "/")); link != "" {
			os.Remove(link) // the link, not what it points to
			continue
		}
		os.RemoveAll(filepath.Join(home, filepath.FromSlash(rel)))
	}
}

// runtimeSockets are where docker's and podman's sockets usually are; a
// mount that holds one gives the agent the runtime, and with it this
// machine.
func runtimeSockets() []string {
	home, _ := os.UserHomeDir()
	s := []string{"/var/run/docker.sock", "/run/docker.sock", "/run/podman/podman.sock", "/var/run/podman/podman.sock"}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		s = append(s, filepath.Join(x, "docker.sock"), filepath.Join(x, "podman", "podman.sock"))
	}
	if home != "" {
		for _, p := range []string{".docker/run/docker.sock", ".docker/desktop/docker.sock", ".colima/default/docker.sock",
			".rd/docker.sock", ".local/share/containers/podman/machine/podman.sock"} {
			s = append(s, filepath.Join(home, filepath.FromSlash(p)))
		}
	}
	return s
}

// holdsSocket returns the runtime socket inside src (or src itself), or "".
func holdsSocket(src string) string {
	for _, s := range runtimeSockets() {
		if _, err := os.Lstat(s); err != nil {
			continue
		}
		if rel, err := filepath.Rel(src, s); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return s
		}
	}
	return ""
}
