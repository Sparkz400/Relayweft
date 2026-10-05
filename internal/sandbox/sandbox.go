// Package sandbox runs an agent CLI, or a verify command, in a container
// (docker or podman) with only the step's folder writable (docs/sandbox.md).
//
// Layout in the container:
//
//	/work            the step's folder (a pool worktree or the project
//	                 folder), read-write; read-only for read-only agents
//	/work/.git       the repository's git folder, read-only: a main tree's
//	                 .git, or for a worktree a generated .git file that
//	                 points into /sy/git
//	/sy/git          the repository's shared git folder (a worktree's),
//	                 read-only
//	.../config       a copy of the repository's git config without remotes,
//	                 credentials or helpers, read-only
//	/sy/home         HOME: a folder sy keeps per project, so the CLIs'
//	                 session stores survive between runs (resume); the
//	                 credential files are mounted read-only inside it
//	/sy/run          this run's standard input, read-only
//	/sy/mcp          Claude's MCP config file, read-only
//
// Paths the agent prints (/work/...) are turned back into host paths
// (HostPath), so sy sees the same paths as without a sandbox on Windows,
// Linux and macOS.
//
// Stopping: the container is named and labelled (sy-<pid>-<start>-<rand>,
// switchyard.owner). Cancel runs `docker kill`; the container's standard
// input stays open while sy runs, and a small shell wrapper in the
// container ends it when that input closes, which happens when the docker
// client dies with sy (Windows' kill-on-exit job, a hard kill). The pid
// file of a pool worktree records the container (proc.NoteContainer), and
// the first sandboxed run of every sy removes containers of an sy that is
// gone (Sweep).
package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/canon"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/proc"
)

// Paths in the container.
const (
	Work     = "/work"
	gitMount = "/sy/git"
	Home     = "/sy/home"
	runMount = "/sy/run"
	// MCPDir is where Claude's MCP config folder is mounted.
	MCPDir = "/sy/mcp"
	stdin  = runMount + "/stdin"
)

// LookPath finds the container runtime; tests swap in a fake one.
var LookPath = proc.Resolve

// stateRoot is sy's own folder for sandbox state (per-project homes, run
// folders); tests point it elsewhere.
var stateRoot = func() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "switchyard", "sandbox")
	}
	return filepath.Join(os.TempDir(), "switchyard-sandbox")
}

// Spec is one command to run in a container.
type Spec struct {
	Cfg      config.SandboxCfg // the sandbox (On)
	Dir      string            // host folder mounted at /work
	ReadOnly bool              // mount it read-only
	Argv     []string          // the command in the container
	Stdin    string            // its standard input
	// Env holds NAME=value pairs for the command. The values reach the
	// container through the runtime's environment (-e NAME), never its
	// command line.
	Env []string
	// Mounts are more read-only mounts (Claude's MCP config folder).
	Mounts []Mount
	Label  string // what runs (an agent id, "verify"), for the logs
	// HomeName picks the project's home folder: one per provider, so an
	// agent of one cannot leave files that another provider's CLI reads
	// with that provider's credentials ("" = "checks", for verify and hooks).
	HomeName string
}

// Mount is one bind mount.
type Mount struct {
	Source, Target string
	Writable       bool
}

// Box is one container run.
type Box struct {
	Runtime string // docker or podman
	Bin     string // its path
	Name    string
	Image   string
	dir     string // host folder at /work
	runDir  string
	stdinW  io.Closer
	once    sync.Once
	links   map[string]linkState // submodules' .git before the run (Check)
}

// Command builds the command that runs s in a container. The caller sets
// Stdout and Stderr, starts it, and calls Close after Wait. Cancelling ctx
// kills the container (docker kill) and the runtime client. Nothing runs on
// this machine instead when the runtime or the image is missing: the error
// (or Explain on the exit) says what to do.
func Command(ctx context.Context, s Spec) (*exec.Cmd, *Box, error) {
	rt := s.Cfg.Mode
	if !s.Cfg.On() {
		return nil, nil, fmt.Errorf("sandbox: mode %q is not a container runtime", rt)
	}
	bin, err := LookPath(rt)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %s is not installed or not on PATH (sandbox.mode: %s): install it, or set sandbox.mode: off to run agents without a sandbox; `sy doctor` checks the setup", rt, rt)
	}
	sweepFor(rt, bin)
	dir := s.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, nil, fmt.Errorf("sandbox: %s is not a folder", dir)
	}
	runDir, err := newRunDir()
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	b := &Box{Runtime: rt, Bin: bin, Name: containerName(), Image: s.Cfg.ImageName(), dir: dir, runDir: runDir}
	fail := func(err error) (*exec.Cmd, *Box, error) {
		os.RemoveAll(runDir)
		return nil, nil, fmt.Errorf("sandbox: %w", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "stdin"), []byte(s.Stdin), 0o600); err != nil {
		return fail(err)
	}
	gm, key, err := gitLayout(dir, runDir)
	if err != nil {
		return fail(err)
	}
	home, err := projectHome(key, s.HomeName)
	if err != nil {
		return fail(err)
	}
	extra, err := userMounts(s.Cfg, home)
	if err != nil {
		return fail(err)
	}
	resetHome(home, extra)
	mounts := []Mount{{Source: dir, Target: Work, Writable: !s.ReadOnly}}
	mounts = append(mounts, gm...)
	if len(gm) > 0 {
		mounts = append(mounts, b.guardLinks(s.ReadOnly)...)
	}
	mounts = append(mounts, Mount{Source: home, Target: Home, Writable: true}, Mount{Source: runDir, Target: runMount})
	mounts = append(mounts, s.Mounts...)
	mounts = append(mounts, extra...)
	var names []string
	for _, kv := range s.Env {
		if n, _, ok := strings.Cut(kv, "="); ok && n != "" && !proc.IsChildSecret(n) {
			names = append(names, n)
		}
	}
	args := runArgs(argSpec{
		runtime: rt, goos: runtime.GOOS, name: b.Name, image: b.Image, label: s.Label,
		owner: owner(), network: !s.Cfg.NetworkOff(), user: hostUser(), mounts: mounts, env: names, argv: s.Argv,
	})
	cmd := exec.CommandContext(ctx, bin, args...)
	proc.Prepare(cmd)
	cmd.Dir = dir
	// The runtime client gets your environment without sy's tokens, plus
	// the values the container is given by name; the container itself
	// gets only those.
	cmd.Env = proc.WithoutSecrets(os.Environ())
	for _, kv := range s.Env {
		if n, _, _ := strings.Cut(kv, "="); !proc.IsChildSecret(n) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// The container's input stays open while sy runs: when it closes (the
	// client died with sy), the wrapper ends the container.
	w, err := cmd.StdinPipe()
	if err != nil {
		return fail(err)
	}
	b.stdinW = w
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		b.kill()
		if kill != nil {
			return kill()
		}
		return nil
	}
	proc.NoteContainer(dir, rt, b.Name)
	diag.Logf("sandbox %s %s for %s: %s image=%s dir=%s readonly=%v network=%v env=%s", rt, b.Name, s.Label, bin, b.Image, dir, s.ReadOnly,
		!s.Cfg.NetworkOff(), strings.Join(names, ","))
	return cmd, b, nil
}

// Close cleans up after the run: the container is removed unless the run
// ended normally (then --rm already removed it), and its records go.
func (b *Box) Close(ok bool) {
	b.once.Do(func() {
		if b.stdinW != nil {
			b.stdinW.Close()
		}
		if !ok {
			removeContainer(b.Bin, b.Name)
		}
		proc.ForgetContainer(b.dir, b.Runtime, b.Name)
		os.RemoveAll(b.runDir)
	})
}

// removeContainer stops and removes one of sy's containers.
func removeContainer(bin, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "rm", "-f", name)
	proc.Background(cmd)
	cmd.Env = proc.WithoutSecrets(os.Environ())
	cmd.Run() // "No such container" when --rm was first
}

// kill stops the container at once (cancel, timeout).
func (b *Box) kill() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Bin, "kill", b.Name)
	proc.Background(cmd)
	cmd.Env = proc.WithoutSecrets(os.Environ())
	out, err := cmd.CombinedOutput()
	diag.Logf("sandbox %s kill %s: err=%v %s", b.Runtime, b.Name, err, strings.TrimSpace(string(out)))
}

// HostPath turns a path the agent printed in the container (/work/...)
// into the host path, with forward slashes like the CLIs print them; other
// paths come back unchanged.
func (b *Box) HostPath(p string) string {
	if b == nil {
		return p
	}
	return hostPath(b.dir, p)
}

func hostPath(dir, p string) string {
	switch {
	case p == Work:
		return filepath.ToSlash(dir)
	case strings.HasPrefix(p, Work+"/"):
		rel := path.Clean(p[len(Work)+1:])
		// A backslash is a plain character in the container but a
		// separator on Windows: "/work/..\..\x" must not leave dir.
		if rel == ".." || strings.HasPrefix(rel, "../") || strings.ContainsAny(rel, `\:`) {
			return p
		}
		host := filepath.Join(dir, filepath.FromSlash(rel))
		if r, err := filepath.Rel(dir, host); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			return p
		}
		return filepath.ToSlash(host)
	}
	return p
}

// Explain turns a failed container start into an error that says what to
// do, or returns nil when the exit came from the command itself. code is
// the runtime client's exit code, stderr its error output.
func (b *Box) Explain(code int, stderr string) error {
	if b == nil {
		return nil
	}
	return explain(b.Runtime, b.Image, code, stderr)
}

var (
	reNoImage  = regexp.MustCompile(`(?i)unable to find image|no such image|pull access denied|repository does not exist|manifest unknown|image not known|failed to resolve reference|short-name .* did not resolve`)
	reNoDaemon = regexp.MustCompile(`(?i)cannot connect to the docker daemon|error during connect|docker daemon is not running|is the docker daemon running|cannot connect to podman|unable to connect to podman|open //\./pipe/docker|the system cannot find the file specified`)
	reNotFound = regexp.MustCompile(`(?i)(not found|no such file or directory|executable file not found)`)
)

func explain(rt, image string, code int, stderr string) error {
	last := lastLine(stderr)
	switch {
	case reNoDaemon.MatchString(stderr):
		start := "start Docker Desktop (or the docker service)"
		if rt == config.SandboxPodman {
			start = "start podman (podman machine start)"
		}
		return fmt.Errorf("sandbox: %s is not running: %s, then run `sy doctor` (%s)", rt, start, last)
	case code == 125 && reNoImage.MatchString(stderr):
		return fmt.Errorf("sandbox: image %q not found: build it with `%s build -t %s packaging/sandbox` (docs/sandbox.md) or set sandbox.image (%s)", image, rt, config.DefaultSandboxImage, last)
	case code == 125:
		return fmt.Errorf("sandbox: %s could not start the container: %s; run `sy doctor`", rt, last)
	case (code == 126 || code == 127) && reNotFound.MatchString(stderr):
		return fmt.Errorf("sandbox: the command is not in image %q (%s): install it in the image (docs/sandbox.md); for an agent CLI with another name there, set providers.<name>.sandbox.command", image, last)
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 300 {
		l = l[:300] + "..."
	}
	return l
}

// wrapper runs the command in the container ($2...) with $1 as its input
// and ends the container when the container's own input closes: sy keeps
// it open while it runs, and the runtime closes it when its client dies
// (docker run -i closes a container's input when the client goes away).
// The wrapper's exit is the command's.
const wrapper = `exec 3<&0
in=$1
shift
"$@" <"$in" &
child=$!
{ cat <&3 >/dev/null; kill -KILL $$ 2>/dev/null; } &
wait "$child"`

// argSpec is everything runArgs needs; it is a plain value so tests can
// check the command line for any OS.
type argSpec struct {
	runtime, goos, name, image, label, owner string
	network                                  bool
	user                                     string // uid:gid ("" = the image's user)
	mounts                                   []Mount
	env                                      []string // names only
	argv                                     []string
}

// runArgs is the `docker run` command line (without the runtime itself).
func runArgs(a argSpec) []string {
	// --pull never: the image is built here; a name typed wrong must not
	// fetch someone else's image from a registry.
	args := []string{"run", "--rm", "-i", "--init", "--pull", "never", "--name", a.name,
		"--label", "switchyard.sandbox=1", "--label", "switchyard.owner=" + a.owner,
		"--label", "switchyard.what=" + labelValue(a.label),
		// The agent needs no privileges: no capabilities, no setuid, a cap
		// on processes so a runaway build cannot starve the machine.
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "4096",
	}
	switch {
	case !a.network:
		args = append(args, "--network", "none")
	case a.runtime == config.SandboxDocker && a.goos == "linux":
		// Docker Desktop has it built in; Linux docker needs it to reach a
		// local model server on this machine.
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}
	if a.user != "" {
		// Files the agent writes in the bind mounts stay yours.
		args = append(args, "--user", a.user)
		if a.runtime == config.SandboxPodman && a.goos == "linux" {
			// Rootless podman: your uid inside is your uid outside.
			args = append(args, "--userns", "keep-id")
		}
	}
	for _, m := range a.mounts {
		args = append(args, "--mount", mountArg(m))
	}
	args = append(args, "-w", Work,
		// IS_SANDBOX tells Claude Code it runs in a sandbox (it then
		// accepts bypassPermissions as root).
		"-e", "HOME="+Home, "-e", "SY_SANDBOX=1", "-e", "IS_SANDBOX=1",
		// The mounts belong to another user as the container sees it, and
		// the git folder is read-only: git must neither refuse the
		// repository nor try to write its index.
		"-e", "GIT_CONFIG_COUNT=1", "-e", "GIT_CONFIG_KEY_0=safe.directory", "-e", "GIT_CONFIG_VALUE_0=*",
		"-e", "GIT_OPTIONAL_LOCKS=0")
	for _, n := range a.env {
		args = append(args, "-e", n)
	}
	args = append(args, "--entrypoint", "sh", a.image, "-c", wrapper, "sy-sandbox", stdin)
	return append(args, a.argv...)
}

// mountArg is one --mount value. Both runtimes read it as CSV, so a field
// with a comma or a quote is quoted.
func mountArg(m Mount) string {
	fields := []string{"type=bind", "source=" + m.Source, "target=" + m.Target}
	if !m.Writable {
		fields = append(fields, "readonly")
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Write(fields)
	w.Flush()
	return strings.TrimRight(b.String(), "\r\n")
}

func labelValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

// hostUser is the uid:gid the container runs as: yours on Linux and macOS,
// so files written into your folders stay yours. On Windows, Docker
// Desktop maps file ownership itself, and its file sharing cannot open
// files in a folder 17 or more levels deep for any user but root (seen
// with Docker Desktop on engine 29.8: "I/O error"), so it is root there.
// Either way the container has no capabilities (--cap-drop ALL).
func hostUser() string {
	if runtime.GOOS == "windows" {
		return "0:0"
	}
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}

// owner tells this machine's and user's containers from others on the same
// runtime (a shared Docker host): only those are ever swept.
func owner() string {
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	h := sha1.Sum([]byte(strings.ToLower(host) + "\x00" + strings.ToLower(home)))
	return hex.EncodeToString(h[:6])
}

// containerName is sy-<pid>-<start>-<rand>: the pid and start time of this
// sy, so a sweep can tell whether the sy that started it still runs.
func containerName() string {
	stamp := proc.Identity(os.Getpid())
	if stamp == "" {
		stamp = "0"
	}
	var r [4]byte
	rand.Read(r[:])
	return fmt.Sprintf("sy-%d-%s-%s", os.Getpid(), stamp, hex.EncodeToString(r[:]))
}

var reName = regexp.MustCompile(`^sy-(\d+)-(\d+)-[0-9a-f]+$`)

// ownerGone reports whether the sy that started the container named name
// has ended.
func ownerGone(name string) bool {
	m := reName.FindStringSubmatch(name)
	if m == nil {
		return false // not ours: never touched
	}
	pid, _ := strconv.Atoi(m[1])
	if m[2] == "0" {
		return !proc.Alive(pid) // no start times on this system
	}
	return proc.Identity(pid) != m[2]
}

var (
	sweepMu   sync.Mutex
	sweptOnce = map[string]bool{}
)

// sweepFor removes, once per process and runtime, the containers of an sy
// that is gone (Sweep).
func sweepFor(rt, bin string) {
	sweepMu.Lock()
	defer sweepMu.Unlock()
	if sweptOnce[rt] {
		return
	}
	sweptOnce[rt] = true
	if gone, err := Sweep(rt, bin, true); err != nil {
		diag.Logf("sandbox: looking for leftover containers: %v", err)
	} else if len(gone) > 0 {
		diag.Logf("sandbox: removed %d container(s) of an sy that ended: %s", len(gone), strings.Join(gone, " "))
		diag.Health("leftover", "what", "sandbox-container", "path", strings.Join(gone, " "))
	}
}

// Sweep lists this user's sy containers whose sy has ended, and removes
// them when remove is set. bin is the runtime's path.
func Sweep(rt, bin string, remove bool) ([]string, error) {
	names, err := Running(bin)
	if err != nil {
		return nil, fmt.Errorf("%s ps: %w", rt, err)
	}
	var gone []string
	for _, n := range names {
		if ownerGone(n) {
			gone = append(gone, n)
		}
	}
	if remove {
		for _, n := range gone {
			removeContainer(bin, n)
		}
	}
	return gone, nil
}

// gitLayout returns the mounts that keep git working in the container and
// the key of the project (its shared git folder, or dir without git).
func gitLayout(dir, runDir string) ([]Mount, string, error) {
	dotgit := filepath.Join(dir, ".git")
	st, err := os.Lstat(dotgit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, dir, nil
	}
	if err != nil {
		return nil, "", err
	}
	if st.IsDir() {
		cfg, err := safeGitConfig(dotgit, runDir)
		if err != nil {
			return nil, "", err
		}
		more, err := moreConfigs(dotgit, Work+"/.git", runDir)
		if err != nil {
			return nil, "", err
		}
		return append([]Mount{{Source: dotgit, Target: Work + "/.git"}, {Source: cfg, Target: Work + "/.git/config"}}, more...), dotgit, nil
	}
	gitdir, err := readGitFile(dotgit)
	if err != nil {
		return nil, "", err
	}
	common := gitdir
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitdir, c)
		}
		common = filepath.Clean(c)
	}
	rel, err := filepath.Rel(common, gitdir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		// A layout sy does not create: run without git rather than
		// mounting more than the repository.
		diag.Logf("sandbox: %s: git folder %s is outside %s; no git in the container", dir, gitdir, common)
		return nil, dir, nil
	}
	target := gitMount
	if rel != "." {
		target = gitMount + "/" + filepath.ToSlash(rel)
	}
	gf := filepath.Join(runDir, "dotgit")
	if err := os.WriteFile(gf, []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		return nil, "", err
	}
	cfg, err := safeGitConfig(common, runDir)
	if err != nil {
		return nil, "", err
	}
	more, err := moreConfigs(common, gitMount, runDir)
	if err != nil {
		return nil, "", err
	}
	return append([]Mount{{Source: common, Target: gitMount}, {Source: cfg, Target: gitMount + "/config"}, {Source: gf, Target: Work + "/.git"}}, more...), common, nil
}

// readGitFile reads a worktree's .git file ("gitdir: <path>").
func readGitFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if !strings.HasPrefix(s, "gitdir:") {
		return "", fmt.Errorf("%s is not a git file", p)
	}
	d := strings.TrimSpace(strings.TrimPrefix(s, "gitdir:"))
	if !filepath.IsAbs(d) {
		d = filepath.Join(filepath.Dir(p), d)
	}
	return filepath.Clean(d), nil
}

// reSafeKey is the git config the container's git needs to read the
// repository: its format and extensions. Remotes (URLs may hold tokens),
// credential helpers, http headers, hooks and aliases stay out.
var reSafeKey = regexp.MustCompile(`^(core\.(repositoryformatversion|bare|worktree|ignorecase|precomposeunicode|symlinks|filemode|autocrlf|eol)|extensions\.[a-z0-9]+)$`)

// safeGitConfig writes the safe part of the repository's git config
// (gitDir/config) into runDir and returns its path.
func safeGitConfig(gitDir, runDir string) (string, error) {
	p := filepath.Join(runDir, "gitconfig")
	return p, safeConfigCopy(filepath.Join(gitDir, "config"), p)
}

// moreConfigs are the other git config files in the shared git folder
// common: each submodule's (modules/**/config: its remote URL may hold a
// token) and each worktree's config.worktree. They get safe copies over
// them in the container, under root (the folder's place there).
func moreConfigs(common, root, runDir string) ([]Mount, error) {
	var found []string
	filepath.WalkDir(filepath.Join(common, "modules"), func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir() && (d.Name() == "objects" || d.Name() == "refs" || d.Name() == "logs" || d.Name() == "lfs"):
			return filepath.SkipDir
		case !d.IsDir() && (d.Name() == "config" || d.Name() == "config.worktree"):
			found = append(found, p)
		}
		return nil
	})
	wts, _ := filepath.Glob(filepath.Join(common, "worktrees", "*", "config.worktree"))
	found = append(found, wts...)
	if _, err := os.Lstat(filepath.Join(common, "config.worktree")); err == nil {
		found = append(found, filepath.Join(common, "config.worktree"))
	}
	var mounts []Mount
	for i, p := range found {
		rel, err := filepath.Rel(common, p)
		if err != nil || strings.HasPrefix(rel, "..") || strings.ContainsAny(filepath.ToSlash(rel), `\,"`) {
			continue
		}
		dst := filepath.Join(runDir, fmt.Sprintf("gitconfig-%d", i))
		if err := safeConfigCopy(p, dst); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{Source: dst, Target: root + "/" + filepath.ToSlash(rel)})
	}
	return mounts, nil
}

// safeConfigCopy writes the safe part of the git config file src to dst.
func safeConfigCopy(src, dst string) error {
	var b strings.Builder
	b.WriteString("# The repository's git config as the sandbox sees it (sy): format and extensions only.\n")
	cmd := exec.Command("git", "config", "--file", src, "--get-regexp", `^(core|extensions)\.`)
	proc.Background(cmd)
	out, _ := cmd.Output()
	sections := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		if !reSafeKey.MatchString(k) || strings.ContainsAny(v, "\"\\\n") {
			continue
		}
		sec, name, _ := strings.Cut(k, ".")
		sections[sec] = append(sections[sec], "\t"+name+" = "+v+"\n")
	}
	if len(sections["core"]) == 0 {
		sections["core"] = []string{"\trepositoryformatversion = 0\n"}
	}
	for _, sec := range []string{"core", "extensions"} {
		if len(sections[sec]) > 0 {
			b.WriteString("[" + sec + "]\n" + strings.Join(sections[sec], ""))
		}
	}
	return os.WriteFile(dst, []byte(b.String()), 0o644)
}

// projectHome is the per-project HOME folder (session stores) for key.
func projectHome(key, name string) (string, error) {
	// A worktree's .git file names the real path (macOS /private/var, a
	// Windows long name) while the main tree may be reached another way
	// (/var, an 8.3 short name, other letter case): one project, one home.
	h := sha1.Sum([]byte(canon.Path(key)))
	if name == "" {
		name = "checks"
	}
	if !reHomeName.MatchString(name) {
		return "", fmt.Errorf("home name %q", name)
	}
	home := filepath.Join(stateRoot(), hex.EncodeToString(h[:])[:12], name)
	return home, os.MkdirAll(home, 0o700)
}

// reHomeName is a provider name (config validates them) or "checks".
var reHomeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ProjectHome is the HOME folder the containers of the project at dir get
// for a provider (name; "" = verify commands and hooks) (sy doctor, tests).
func ProjectHome(dir, name string) (string, error) {
	runDir, err := os.MkdirTemp("", "sy-sandbox-probe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(runDir)
	_, key, err := gitLayout(dir, runDir)
	if err != nil {
		return "", err
	}
	return projectHome(key, name)
}

var runSweep sync.Once

// newRunDir makes a fresh folder for one run's files; the first call of a
// process removes run folders a hard kill left behind.
func newRunDir() (string, error) {
	root := filepath.Join(stateRoot(), "runs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	runSweep.Do(func() {
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.RemoveAll(filepath.Join(root, e.Name()))
			}
		}
	})
	return os.MkdirTemp(root, "run-")
}

// userMounts resolves the credential files and extra mounts of cfg. A
// target under the container's home gets its mount point created in home
// first, as yours: the runtime would create it as root.
func userMounts(cfg config.SandboxCfg, home string) ([]Mount, error) {
	userHome, _ := os.UserHomeDir()
	var out []Mount
	add := func(p, target string, writable bool, what string) error {
		src, err := expandHome(p, userHome)
		if err != nil {
			return err
		}
		st, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("%s %s: %w", what, p, err)
		}
		if sock := holdsSocket(src); sock != "" {
			return fmt.Errorf("%s %s holds %s, which would give the agent the container runtime and with it this machine", what, p, sock)
		}
		if target == "" {
			rel, err := filepath.Rel(userHome, src)
			if userHome == "" || err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				return fmt.Errorf("%s %s is not inside your home folder: give it a target", what, p)
			}
			target = Home + "/" + filepath.ToSlash(rel)
		}
		target = path.Clean(target)
		if strings.HasPrefix(target, Home+"/") {
			if err := mountPoint(home, strings.TrimPrefix(target, Home+"/"), st.IsDir()); err != nil {
				return err
			}
		}
		out = append(out, Mount{Source: src, Target: target, Writable: writable})
		return nil
	}
	for _, p := range cfg.Credentials {
		if err := add(p, "", false, "credential file"); err != nil {
			return nil, err
		}
	}
	for _, m := range cfg.Mounts {
		if err := add(m.Path, m.Target, m.Writable, "mount"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// mountPoint creates rel (a folder, or an empty file) inside home, where a
// mount will go. Agents write to home, so every part of the path must be a
// plain folder or file, never a symlink an agent left there: sy would
// otherwise create files wherever it points on this machine.
func mountPoint(home, rel string, dir bool) error {
	parts := strings.Split(rel, "/")
	p := home
	for i, part := range parts {
		p = filepath.Join(p, part)
		last := i == len(parts)-1
		for try := 0; ; try++ {
			st, err := os.Lstat(p)
			if err == nil && st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s in the sandbox's home folder is a symlink (left by an agent?): remove it", p)
			}
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if dir || !last {
				err = os.Mkdir(p, 0o700)
			} else {
				var f *os.File
				if f, err = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
					f.Close()
				}
			}
			// Another agent of this sy may have created it a moment ago:
			// look again (it must still not be a symlink).
			if err == nil || !errors.Is(err, os.ErrExist) || try > 0 {
				if err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// expandHome resolves ~ and makes p absolute.
func expandHome(p, home string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home == "" {
			return "", fmt.Errorf("%s: no home folder", p)
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}

// PassEnv returns NAME=value for the variables in names that are set
// (lookup nil = the environment). sy's forge and CI tokens never pass.
func PassEnv(names []string, lookup func(string) (string, bool)) []string {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var out []string
	for _, n := range names {
		if proc.IsChildSecret(n) {
			continue
		}
		if v, ok := lookup(n); ok && v != "" {
			out = append(out, n+"="+v)
		}
	}
	return out
}

// CLIName is the CLI's command in the image: the sandbox's command, or the
// provider's command without its folder and Windows extension.
func CLIName(cfg config.SandboxCfg, command string) string {
	if cfg.Command != "" {
		return cfg.Command
	}
	base := filepath.Base(strings.ReplaceAll(command, `\`, "/"))
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".cmd", ".exe", ".bat", ".ps1":
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return base
}
