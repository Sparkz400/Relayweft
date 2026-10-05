package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
)

// The Phase 2 exit criterion is "from install to the first task in under 5
// minutes". sy's own part of that must stay far below it: with scripted
// agents a whole first run takes seconds, so more than firstRunSlow is a
// warning and more than firstRunMax a failure.
const (
	firstRunBudget = 5 * time.Minute
	firstRunSlow   = 20 * time.Second
	firstRunMax    = 60 * time.Second
)

// firstRunCase is one way a new user meets sy for the first time.
type firstRunCase struct {
	typed     string   // what the user types
	args      []string // sy's arguments
	answers   string   // the Enter presses piped in ("" = no terminal)
	repo      bool     // run in a git repo (else in a plain folder: the sample project)
	questions int      // how many questions setup must ask
	saves     bool     // setup saves the repo's checks (not when started by sy run)
}

var firstRunCases = []firstRunCase{
	{typed: "sy setup --yes", args: []string{"setup", "--yes"}, repo: true, saves: true},
	{typed: `sy run "` + firstTaskRepo + `", then Enter`, args: []string{"run", firstTaskRepo}, answers: "\n", repo: true, questions: 1},
	{typed: "sy setup outside a repo, then Enter twice", args: []string{"setup"}, answers: "\n\n", questions: 2},
}

// firstRun runs the guided setup as a new user meets it. Each case starts
// from a fresh profile with no config, and a PATH with only git, the system
// folders and scripted CLIs: Claude Code logged in and Codex logged out (on
// Windows as npm-style .cmd shims in a profile with spaces). It checks what
// the user has to type and answer, what setup wrote, that the first task
// ran, and how long sy took.
func (t *selftest) firstRun() {
	var total time.Duration
	for i, c := range firstRunCases {
		took, ok := t.firstRunCase(i+1, c)
		if !ok {
			return
		}
		total += took
	}
	t.check(markInfo, "first run", "sy's own time for all %d first runs: %s (the criterion is %s from install to the first task)",
		len(firstRunCases), total.Round(100*time.Millisecond), firstRunBudget)
}

func (t *selftest) firstRunCase(n int, c firstRunCase) (time.Duration, bool) {
	name := fmt.Sprintf("first run %d", n)
	profile := filepath.Join(t.work, "Users", fmt.Sprintf("New User %d (ä)", n))
	tmp := filepath.Join(profile, "AppData", "Local", "Temp")
	for _, d := range []string{tmp, filepath.Join(profile, ".config"), filepath.Join(profile, ".cache")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.check(markFail, name, "%v", err)
			return 0, false
		}
	}
	// A fresh install of sy in this profile (cmd.exe reads a .cmd shim in
	// the OEM code page, so its path to sy must stay inside the profile).
	bin := filepath.Join(profile, "AppData", "Local", "Programs", "sy tools", filepath.Base(t.bin))
	if err := copyExe(t.bin, bin); err != nil {
		t.check(markFail, name, "copy sy: %v", err)
		return 0, false
	}
	npm := filepath.Join(profile, "AppData", "Roaming", "npm")
	for _, cli := range []string{"claude", "codex"} {
		if _, err := writeAgentShim(npm, bin, cli); err != nil {
			t.check(markFail, name, "%v", err)
			return 0, false
		}
	}
	dir := filepath.Join(t.work, "projects", fmt.Sprintf("first run %d ä", n))
	if c.repo {
		if err := t.goRepo(dir); err != nil {
			t.check(markFail, name, "%v", err)
			return 0, false
		}
	} else {
		os.MkdirAll(dir, 0o755)
		if _, err := gitRoot(dir); err == nil {
			t.check(markSkip, name, "%s is inside a git repo, so the sample-project case cannot run here", dir)
			return 0, true
		}
	}
	state := filepath.Join(t.work, "agent "+name)
	os.MkdirAll(state, 0o755)
	path, err := barePath(npm, filepath.Join(profile, "tools"))
	if err != nil {
		t.check(markFail, name, "%v", err)
		return 0, false
	}
	env := []string{envSelftestDir + "=" + state, "PATH=" + path, "TMPDIR=" + tmp, envNoSetup + "=", "CI="}
	if runtime.GOOS == "windows" {
		env = append(env, "USERPROFILE="+profile)
	} else {
		env = append(env, "HOME="+profile)
	}
	if c.answers != "" {
		env = append(env, envSetupInteractive+"=1")
	}

	began := time.Now()
	out, err := t.syIn(bin, profile, dir, name, env, c.answers, c.args...)
	took := time.Since(began)
	fail := func(format string, args ...any) (time.Duration, bool) {
		t.check(markFail, name, "%s: %s\n%s", c.typed, fmt.Sprintf(format, args...), tailLines(out, 25))
		return took, false
	}
	if err != nil {
		return fail("sy failed: %v", err)
	}
	want := fmt.Sprintf("Setup done: %d question(s) answered", c.questions)
	if !strings.Contains(out, want) {
		return fail("want %q", want)
	}
	if !strings.Contains(out, "ok   claude") || !strings.Contains(out, "Not logged in") || !strings.Contains(out, "log in: codex login") {
		return fail("setup must find Claude Code logged in, and say how to log in to Codex")
	}
	cfg, _, err := config.Load(profileConfig(profile))
	if err != nil {
		return fail("the config does not load: %v", err)
	}
	if cfg.Providers["claude"].Disabled || !cfg.Providers["codex"].Disabled {
		return fail("the config must turn claude on and the logged-out codex off")
	}
	b, err := os.ReadFile(filepath.Join(dir, config.RepoFileName))
	switch {
	case c.saves && !strings.Contains(string(b), `"go test ./..."`):
		return fail("%s lacks the detected checks", config.RepoFileName)
	case !c.saves && err == nil:
		// An untracked file would stop sy run --issues (clean tree only).
		return fail("%s was written, but sy run must leave the repo alone", config.RepoFileName)
	}
	explored := false
	for _, call := range readCalls(state) {
		explored = explored || call == "explore"
	}
	if !strings.Contains(out, "OK in") || !explored {
		return fail("the first task did not run (calls: %s)", strings.Join(readCalls(state), " "))
	}
	m := markOK
	note := ""
	switch {
	case took > firstRunMax:
		m, note = markFail, fmt.Sprintf(" - too slow: more than %s", firstRunMax)
	case took > firstRunSlow:
		m, note = markWarn, " - slow"
	}
	t.check(m, name, "%s: %d question(s), config written, first task done; %s in all%s", c.typed, c.questions, took.Round(100*time.Millisecond), note)
	return took, m != markFail
}

// goRepo creates a small Go repo (so setup detects go build and go test).
func (t *selftest) goRepo(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"go.mod":    "module firstrun\n\ngo 1.24\n",
		"main.go":   "package main\n\nfunc main() { println(\"hello\") }\n",
		"README.md": "# first run\n",
	}
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"},
		{"-c", "user.name=sy selftest", "-c", "user.email=selftest@localhost", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v %s", args[0], err, out)
		}
	}
	return nil
}

// barePath is a PATH with the scripted CLIs, git and the system folders
// only, so the real agent CLIs on this machine are not found. On Unix git
// gets a folder of its own (tools), since /usr/bin may hold a distro's
// gemini or ollama.
func barePath(cliDir, tools string) (string, error) {
	g, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		// Git for Windows' cmd folder holds only git and its GUIs.
		root := os.Getenv("SystemRoot")
		return strings.Join([]string{cliDir, filepath.Dir(g), filepath.Join(root, "System32"), root}, ";"), nil
	}
	if err := os.MkdirAll(tools, 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink(g, filepath.Join(tools, "git")); err != nil && !os.IsExist(err) {
		return "", err
	}
	return cliDir + ":" + tools, nil
}

// profileConfig is where sy finds the user config in a test profile.
func profileConfig(profile string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(profile, "AppData", "Roaming", "switchyard", config.FileName)
	case "darwin":
		return filepath.Join(profile, "Library", "Application Support", "switchyard", config.FileName)
	}
	return filepath.Join(profile, ".config", "switchyard", config.FileName)
}

// syIn runs bin in dir as a user of profile, with stdin.
func (t *selftest) syIn(bin, profile, dir, logName string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = profileEnv(profile, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := runTimeout(cmd, 3*time.Minute)
	os.WriteFile(filepath.Join(t.logs, logName+".log"), buf.Bytes(), 0o644)
	return buf.String(), err
}
