package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/proc"
)

// TestMain: with SY_FAKE_RUNTIME set the test binary is a fake docker. It
// appends its arguments to that file, prints SY_FAKE_PS for `ps`, and for
// `run` waits until its input closes when SY_FAKE_HANG is set.
func TestMain(m *testing.M) {
	if log := os.Getenv("SY_FAKE_RUNTIME"); log != "" {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
		f.Close()
		if len(os.Args) > 1 && os.Args[1] == "ps" {
			fmt.Print(os.Getenv("SY_FAKE_PS"))
		}
		if len(os.Args) > 1 && os.Args[1] == "run" && os.Getenv("SY_FAKE_HANG") != "" {
			buf := make([]byte, 1)
			os.Stdin.Read(buf) // returns when the input closes
		}
		os.Exit(0)
	}
	root, err := os.MkdirTemp("", "sy-sandbox-test-")
	if err != nil {
		panic(err)
	}
	stateRoot = func() string { return root }
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

// fakeRuntime makes LookPath find the test binary as the runtime and
// returns the file its calls are logged in.
func fakeRuntime(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("SY_FAKE_RUNTIME", log)
	old := LookPath
	LookPath = func(string) (string, error) { return exe, nil }
	t.Cleanup(func() { LookPath = old })
	sweepMu.Lock()
	sweptOnce = map[string]bool{}
	sweepMu.Unlock()
	return log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func hasArgs(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestRunArgs(t *testing.T) {
	base := argSpec{runtime: "docker", name: "sy-1-2-ab", image: "img", label: "work", owner: "o", network: true,
		mounts: []Mount{{Source: "/p", Target: Work, Writable: true}, {Source: "/p/.git", Target: Work + "/.git"}},
		env:    []string{"ANTHROPIC_API_KEY"}, argv: []string{"claude", "-p"}}

	linux := base
	linux.goos, linux.user = "linux", "1000:1000"
	a := runArgs(linux)
	for _, seq := range [][]string{
		// --pull never: a mistyped image name never fetches a stranger's.
		{"run", "--rm", "-i", "--init", "--pull", "never", "--name", "sy-1-2-ab"},
		{"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"},
		{"--add-host", "host.docker.internal:host-gateway"},
		{"--user", "1000:1000"},
		{"--mount", "type=bind,source=/p,target=/work"},
		{"--mount", "type=bind,source=/p/.git,target=/work/.git,readonly"},
		{"-e", "ANTHROPIC_API_KEY"},
		{"--entrypoint", "sh", "img", "-c", wrapper, "sy-sandbox", stdin, "claude", "-p"},
	} {
		if !hasArgs(a, seq...) {
			t.Errorf("linux args lack %q:\n%q", seq, a)
		}
	}
	if slices.Contains(a, "--userns") {
		t.Error("docker got podman's --userns")
	}

	// Windows: root in the container (Docker Desktop's file sharing fails
	// for other users in deep folders), no --add-host (built in).
	win := base
	win.goos, win.user = "windows", "0:0"
	a = runArgs(win)
	if !hasArgs(a, "--user", "0:0") || slices.Contains(a, "--add-host") {
		t.Errorf("windows args: %q", a)
	}

	pod := linux
	pod.runtime = "podman"
	if a = runArgs(pod); !hasArgs(a, "--userns", "keep-id") || slices.Contains(a, "--add-host") {
		t.Errorf("podman args: %q", a)
	}

	off := linux
	off.network = false
	if a = runArgs(off); !hasArgs(a, "--network", "none") || slices.Contains(a, "--add-host") {
		t.Errorf("network off: %q", a)
	}
	// Values never go on the command line: only names.
	for _, x := range runArgs(linux) {
		if strings.HasPrefix(x, "ANTHROPIC_API_KEY=") {
			t.Errorf("a value on the command line: %q", x)
		}
	}
}

func TestMountArgQuotesCommas(t *testing.T) {
	got := mountArg(Mount{Source: `C:\a,b "c"`, Target: "/work"})
	if got != `type=bind,"source=C:\a,b ""c""",target=/work,readonly` {
		t.Errorf("mountArg = %s", got)
	}
}

func TestHostPath(t *testing.T) {
	dir := "/home/u/proj"
	if runtime.GOOS == "windows" {
		dir = `D:\Entwicklung\proj`
	}
	slash := filepath.ToSlash(dir)
	for in, want := range map[string]string{
		"/work/a.txt":           slash + "/a.txt",
		"/work/notes dir/b.txt": slash + "/notes dir/b.txt",
		"/work":                 slash,
		"/work/../etc/passwd":   "/work/../etc/passwd",
		"/workshop/x":           "/workshop/x",
		"/sy/home/.claude/x":    "/sy/home/.claude/x",
		"relative/c.txt":        "relative/c.txt",
		"/work/sub/../c.txt":    slash + "/c.txt",
		"/work/./d.txt":         slash + "/d.txt",
		"/work/a/../../outside": "/work/a/../../outside",
		// Backslashes are plain characters in the container but
		// separators on Windows: never a way out of dir.
		`/work/..\..\x`:  `/work/..\..\x`,
		`/work/\..\..\x`: `/work/\..\..\x`,
		`/work/a\b.txt`:  `/work/a\b.txt`,
		"/work/C:/x":     "/work/C:/x",
	} {
		if got := hostPath(dir, in); got != want {
			t.Errorf("hostPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	// What must not reach the container.
	git(t, dir, "remote", "add", "origin", "https://x-access-token:ghs_secret@example.com/r.git")
	git(t, dir, "config", "http.extraheader", "AUTHORIZATION: basic c2VjcmV0")
	git(t, dir, "config", "credential.helper", "store")
	git(t, dir, "config", "core.fsmonitor", "touch /tmp/pwned")
	git(t, dir, "config", "core.hooksPath", "/tmp/hooks")
	git(t, dir, "config", "core.sshCommand", "evil")
	git(t, dir, "config", "extensions.worktreeConfig", "true")
	return dir
}

func checkSafeConfig(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{"ghs_secret", "extraheader", "c2VjcmV0", "helper", "fsmonitor", "pwned", "hookspath", "sshcommand", "evil", "origin"} {
		if strings.Contains(strings.ToLower(s), bad) {
			t.Errorf("the container's git config has %q:\n%s", bad, s)
		}
	}
	if !strings.Contains(s, "repositoryformatversion") || !strings.Contains(s, "worktreeconfig = true") {
		t.Errorf("the container's git config lost the format or the extensions:\n%s", s)
	}
}

// A main tree: its .git read-only at /work/.git, with a safe config.
func TestGitLayoutMainTree(t *testing.T) {
	dir := newRepo(t)
	run := t.TempDir()
	mounts, key, err := gitLayout(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if key != filepath.Join(dir, ".git") || len(mounts) != 2 {
		t.Fatalf("key %s mounts %+v", key, mounts)
	}
	if mounts[0] != (Mount{Source: filepath.Join(dir, ".git"), Target: "/work/.git"}) || mounts[1].Target != "/work/.git/config" || mounts[1].Writable {
		t.Errorf("mounts = %+v", mounts)
	}
	checkSafeConfig(t, mounts[1].Source)
}

// A pool worktree: its .git file points into the main repository on the
// host (D:\... on Windows), which means nothing in the container. The
// shared git folder is mounted read-only at /sy/git and a generated .git
// file points to the worktree's folder in it.
func TestGitLayoutWorktree(t *testing.T) {
	repo := newRepo(t)
	wt := filepath.Join(t.TempDir(), "pool", "3")
	git(t, repo, "worktree", "add", "--detach", wt)
	run := t.TempDir()
	mounts, key, err := gitLayout(wt, run)
	if err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(repo, ".git")
	if real, err := filepath.EvalSymlinks(common); err == nil && !samePath(key, common) {
		common = real
	}
	if !samePath(key, common) {
		t.Errorf("key = %s, want the shared git folder %s", key, common)
	}
	var gitfile, cfg Mount
	for _, m := range mounts {
		if m.Writable {
			t.Errorf("writable git mount %+v", m)
		}
		switch m.Target {
		case "/work/.git":
			gitfile = m
		case "/sy/git/config":
			cfg = m
		case "/sy/git":
			if !samePath(m.Source, common) {
				t.Errorf("/sy/git from %s, want %s", m.Source, common)
			}
		}
	}
	b, err := os.ReadFile(gitfile.Source)
	if err != nil {
		t.Fatalf("no generated .git file: %+v", mounts)
	}
	if string(b) != "gitdir: /sy/git/worktrees/3\n" {
		t.Errorf(".git file = %q", b)
	}
	checkSafeConfig(t, cfg.Source)
	// Both trees of one repository share the per-project home (sessions).
	h1, _ := ProjectHome(repo, "claude")
	h2, _ := ProjectHome(wt, "claude")
	if h1 == "" || h1 != h2 {
		t.Errorf("project homes differ: %s vs %s", h1, h2)
	}
}

func samePath(a, b string) bool {
	ca, _ := filepath.EvalSymlinks(a)
	cb, _ := filepath.EvalSymlinks(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func TestGitLayoutNoGit(t *testing.T) {
	dir := t.TempDir()
	mounts, key, err := gitLayout(dir, t.TempDir())
	if err != nil || len(mounts) != 0 || key != dir {
		t.Errorf("no git: %v %+v %s", err, mounts, key)
	}
}

func TestUserMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600)
	state := t.TempDir()
	cache := t.TempDir()
	ms, err := userMounts(config.SandboxCfg{
		Credentials: []string{"~/.codex/auth.json"},
		Mounts:      []config.SandboxMount{{Path: cache, Target: "/cache", Writable: true}},
	}, state)
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{Source: filepath.Join(home, ".codex", "auth.json"), Target: "/sy/home/.codex/auth.json"},
		{Source: cache, Target: "/cache", Writable: true},
	}
	if !slices.Equal(ms, want) {
		t.Errorf("mounts = %+v, want %+v", ms, want)
	}
	// The mount point exists as yours before the runtime would create it as root.
	if st, err := os.Stat(filepath.Join(state, ".codex", "auth.json")); err != nil || st.IsDir() {
		t.Errorf("mount point not prepared: %v", err)
	}
	if _, err := userMounts(config.SandboxCfg{Credentials: []string{"~/.missing"}}, state); err == nil {
		t.Error("a missing credential file was accepted")
	}
	if _, err := userMounts(config.SandboxCfg{Credentials: []string{cache}}, state); err == nil || !strings.Contains(err.Error(), "home") {
		t.Errorf("a credential outside your home without a target: %v", err)
	}
}

// The home folder is the agents' to write: a symlink one left there must
// not make sy create a file outside the sandbox when it prepares a mount
// point.
func TestUserMountsRefuseSymlinkInHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600)
	state, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(state, ".codex")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	_, err := userMounts(config.SandboxCfg{Credentials: []string{"~/.codex/auth.json"}}, state)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "auth.json")); !os.IsNotExist(err) {
		t.Error("sy created a file through the agent's symlink")
	}
}

func TestCLIName(t *testing.T) {
	for in, want := range map[string]string{
		"claude": "claude", `C:\Users\x\AppData\Roaming\npm\claude.cmd`: "claude", "/usr/local/bin/codex": "codex", "gemini.exe": "gemini",
	} {
		if got := CLIName(config.SandboxCfg{}, in); got != want {
			t.Errorf("CLIName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CLIName(config.SandboxCfg{Command: "my-claude"}, "claude"); got != "my-claude" {
		t.Errorf("command override: %s", got)
	}
}

func TestExplain(t *testing.T) {
	cases := []struct {
		code   int
		stderr string
		want   string // "" = not a sandbox failure
	}{
		{125, "Unable to find image 'img:1' locally\ndocker: Error response from daemon: pull access denied for img, repository does not exist", "build it"},
		{1, "error during connect: Get \"http://%2F%2F.%2Fpipe%2FdockerDesktopLinuxEngine/v1.51/containers/json\": open //./pipe/dockerDesktopLinuxEngine: The system cannot find the file specified.", "is not running"},
		{1, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", "is not running"},
		{125, "docker: Error response from daemon: invalid mount config", "could not start the container"},
		{127, "sy-sandbox: line 4: claude: not found", "not in image"},
		{1, "Error: model overloaded", ""},
		{127, "the agent itself exited 127", ""},
	}
	for _, c := range cases {
		err := explain("docker", "img:1", c.code, c.stderr)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%d %q: unexpected %v", c.code, c.stderr, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%d %q: got %v, want %q", c.code, c.stderr, err, c.want)
		}
	}
}

func TestPassEnv(t *testing.T) {
	look := func(n string) (string, bool) {
		v, ok := map[string]string{"A": "1", "EMPTY": "", "GITHUB_TOKEN": "ghs"}[n]
		return v, ok
	}
	if got := PassEnv([]string{"A", "EMPTY", "UNSET", "GITHUB_TOKEN"}, look); !slices.Equal(got, []string{"A=1"}) {
		t.Errorf("PassEnv = %v", got)
	}
}

// Fail closed: without the runtime nothing runs, and the error says what
// to do.
func TestCommandWithoutRuntime(t *testing.T) {
	old := LookPath
	LookPath = func(string) (string, error) { return "", errors.New("not found") }
	defer func() { LookPath = old }()
	_, _, err := Command(context.Background(), Spec{Cfg: config.SandboxCfg{Mode: "podman"}, Dir: t.TempDir(), Argv: []string{"claude"}})
	if err == nil || !strings.Contains(err.Error(), "podman is not installed") || !strings.Contains(err.Error(), "sandbox.mode: off") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := Command(context.Background(), Spec{Cfg: config.SandboxCfg{Mode: "off"}}); err == nil {
		t.Error("mode off built a container command")
	}
}

func TestOwnerGone(t *testing.T) {
	self := containerName()
	if ownerGone(self) {
		t.Errorf("%s: this sy counted as gone", self)
	}
	if !ownerGone("sy-999999999-12345-ab") {
		t.Error("a dead sy's container counted as alive")
	}
	if ownerGone("someone-elses-container") || ownerGone("sy-1-x-ab") {
		t.Error("a foreign name counted as ours")
	}
	if !reName.MatchString(self) || !proc.RemoveContainer("docker", "evil name") {
		t.Error("name or validation")
	}
}

// The first sandboxed run removes the containers of an sy that ended, and
// only those.
func TestSweep(t *testing.T) {
	log := fakeRuntime(t)
	self := containerName()
	dead := "sy-999999999-12345-ab"
	t.Setenv("SY_FAKE_PS", self+"\n"+dead+"\nforeign\n")
	exe, _ := os.Executable()
	gone, err := Sweep("docker", exe, true)
	if err != nil || !slices.Equal(gone, []string{dead}) {
		t.Fatalf("Sweep = %v %v", gone, err)
	}
	c := calls(t, log)
	if len(c) != 2 || !strings.HasPrefix(c[0], "ps -a --filter label=switchyard.owner=") || c[1] != "rm -f "+dead {
		t.Errorf("calls = %q", c)
	}
}

// Command and Close with a fake runtime: the stdin file, the pid file
// record of a pool worktree, and cleanup.
func TestCommandAndClose(t *testing.T) {
	log := fakeRuntime(t)
	t.Setenv("SY_FAKE_PS", "")
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	slot := t.TempDir()
	pidFile := slot + ".pid"
	untrack := proc.TrackDir(slot, pidFile)
	defer untrack()
	cmd, box, err := Command(context.Background(), Spec{Cfg: config.SandboxCfg{Mode: "docker", Image: "img"}, Dir: slot,
		Argv: []string{"claude", "-p"}, Stdin: "the prompt", Env: []string{"ANTHROPIC_API_KEY=sk-1", "GITHUB_TOKEN=ghs_secret"}, Label: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(box.runDir, "stdin")); err != nil || string(b) != "the prompt" {
		t.Errorf("stdin file: %q %v", b, err)
	}
	if p, _ := os.ReadFile(pidFile); !strings.Contains(string(p), "container docker "+box.Name) {
		t.Errorf("pid file lacks the container: %q", p)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Error("the forge token reached the runtime client")
		}
	}
	if !slices.Contains(cmd.Env, "ANTHROPIC_API_KEY=sk-1") || !hasArgs(cmd.Args, "-e", "ANTHROPIC_API_KEY") || hasArgs(cmd.Args, "-e", "GITHUB_TOKEN") {
		t.Errorf("env: %q / %q", cmd.Env, cmd.Args)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	box.Close(false)
	if _, err := os.Stat(box.runDir); !os.IsNotExist(err) {
		t.Error("run folder left behind")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("pid file still lists the container after Close")
	}
	if c := calls(t, log); c[len(c)-1] != "rm -f "+box.Name {
		t.Errorf("Close(false) did not remove the container: %q", c)
	}
}

// Cancelling stops the container (docker kill) as well as the client.
func TestCommandCancelKills(t *testing.T) {
	log := fakeRuntime(t)
	t.Setenv("SY_FAKE_PS", "")
	t.Setenv("SY_FAKE_HANG", "1")
	ctx, cancel := context.WithCancel(context.Background())
	cmd, box, err := Command(ctx, Spec{Cfg: config.SandboxCfg{Mode: "docker"}, Dir: t.TempDir(), Argv: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the client did not end after cancel")
	}
	box.Close(false)
	if !slices.Contains(calls(t, log), "kill "+box.Name) {
		t.Errorf("no docker kill on cancel: %q", calls(t, log))
	}
}
