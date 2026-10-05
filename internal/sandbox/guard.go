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
	"time"

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
	cmd := exec.Command("git", proc.GitArgs("ls-files", "-s", "-z")...)
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
	var bad []string // what changed, and what sy did about it
	for rel, before := range b.links {
		if link := symlinkIn(b.dir, rel); link != "" {
			name := filepath.ToSlash(strings.TrimPrefix(link, b.dir+string(filepath.Separator)))
			if err := os.Remove(link); err != nil { // the link itself, never what it points to
				bad = append(bad, name+" is a symlink, which sy could not remove ("+err.Error()+"): remove it before you run git there")
			} else {
				bad = append(bad, name+" was a symlink: removed")
			}
			continue
		}
		now := linkStateOf(b.dir, rel)
		if now.kind == before.kind && bytes.Equal(now.data, before.data) {
			continue
		}
		bad = append(bad, rel+"/.git "+undoLink(filepath.Join(b.dir, filepath.FromSlash(rel), ".git"), before, now))
	}
	if len(bad) == 0 {
		return nil
	}
	slices.Sort(bad)
	diag.Logf("sandbox %s: submodule .git changed in the container: %s", b.Name, strings.Join(bad, "; "))
	diag.Health("sandbox-escape-attempt", "container", b.Name, "paths", strings.Join(bad, "; "))
	return fmt.Errorf("sandbox: the agent changed a submodule's .git, which git on this machine would follow (a way out of the sandbox), and the run counts as failed: %s", strings.Join(bad, "; "))
}

// undoLink undoes a change of a submodule's .git at p (never following a
// symlink) and says what it did. One that appeared is removed and a file
// is put back. A .git folder (a submodule from before git kept them in
// .git/modules) may hold the submodule's only history, so it is moved
// aside, not deleted: without a .git, git takes the submodule as not
// checked out and does not look into it.
func undoLink(p string, before, now linkState) string {
	if now.kind == "missing" {
		if before.kind == "file" {
			if err := os.WriteFile(p, before.data, 0o644); err == nil {
				return "was removed: the original was put back"
			}
		}
		return "was removed (sy cannot restore a .git folder; git takes the submodule as not checked out)"
	}
	if before.kind == "dir" || before.kind == "other" {
		aside := fmt.Sprintf("%s.sy-quarantine-%d", p, time.Now().UnixNano())
		if err := os.Rename(p, aside); err != nil {
			return "changed; sy could not move it aside (" + err.Error() + "): look at it before you run git there"
		}
		return "changed: moved to " + filepath.Base(aside) + " (it may hold the submodule's history; look at it before you put it back)"
	}
	if err := os.RemoveAll(p); err != nil {
		return "changed; sy could not remove it (" + err.Error() + "): remove it before you run git there"
	}
	if before.kind == "file" {
		if err := os.WriteFile(p, before.data, 0o644); err != nil {
			return "changed: removed, but sy could not put the original back (" + err.Error() + ")"
		}
		return "changed: the original was put back"
	}
	return "appeared: removed"
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
	// Shells, also at logout and in login shells.
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".profile",
	".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout", ".config/fish",
	// git and npm (git/config holds hooks and fsmonitor; npmrc scripts).
	".gitconfig", ".config/git", ".npmrc",
	// Claude Code: ~/.claude.json holds MCP servers (commands); settings
	// hold hooks.
	".claude.json", ".claude.json.backup", ".claude/settings.json", ".claude/settings.local.json",
	".claude/hooks", ".claude/commands", ".claude/agents", ".claude/plugins",
	// Codex, Gemini CLI, Qwen Code.
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

// projectConfigs are the files in a work tree that agent CLIs read as the
// project's own settings, and that can make them run commands (hooks, MCP
// servers) with that CLI's credentials. They are agent-writable in /work.
var projectConfigs = []string{
	".claude/settings.json", ".claude/settings.local.json", ".mcp.json",
	".codex/config.toml", ".gemini/settings.json", ".qwen/settings.json",
}

// pinProjectConfigs keeps an agent of this task from leaving project
// settings for the next agent's CLI (perhaps another provider's, with its
// own key): each of projectConfigs that exists in dir and differs from
// base (the commit the task started from; your committed and uncommitted
// versions are in it) gets base's version mounted read-only over it, or
// an empty one when base has none. The file on disk stays as the agent
// wrote it, to be reviewed and landed as usual. It returns the mounts and
// the files pinned. A symlink on such a path fails the run.
func pinProjectConfigs(dir, base, runDir string) ([]Mount, []string, error) {
	var mounts []Mount
	var pinned []string
	for i, rel := range projectConfigs {
		if link := symlinkOnPath(dir, strings.Split(rel, "/")); link != "" {
			return nil, nil, fmt.Errorf("%s in the work tree is a symlink: sy will not run an agent with it (remove it)", filepath.ToSlash(strings.TrimPrefix(link, dir+string(filepath.Separator))))
		}
		p := filepath.Join(dir, filepath.FromSlash(rel))
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue // absent (nothing to read), or a folder the CLI cannot read as settings
		}
		now, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, err
		}
		was, found := blobAt(dir, base, rel)
		if found && bytes.Equal(now, was) {
			continue
		}
		if !found {
			was = []byte("{}\n")
			if strings.HasSuffix(rel, ".toml") {
				was = nil
			}
		}
		src := filepath.Join(runDir, fmt.Sprintf("project-%d", i))
		if err := os.WriteFile(src, was, 0o644); err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, Mount{Source: src, Target: Work + "/" + rel})
		pinned = append(pinned, rel)
	}
	return mounts, pinned, nil
}

// blobAt is the content of rel (relative to dir) at commit base, and
// whether it is there.
func blobAt(dir, base, rel string) ([]byte, bool) {
	cmd := exec.Command("git", proc.GitArgs("cat-file", "blob", base+":./"+rel)...)
	cmd.Dir = dir
	proc.Background(cmd)
	out, err := cmd.Output()
	return out, err == nil
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
