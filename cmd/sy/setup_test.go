package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/runner"
)

// fakeCLIs puts empty stand-ins for the named CLIs on PATH (found by name
// only: setupExec is faked) and answers the quick checks from answers,
// keyed by "<cli> <args>".
func fakeCLIs(t *testing.T, names []string, answers func(cli, args string) (string, error)) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		file := n
		if runtime.GOOS == "windows" {
			file += ".cmd"
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte("stand-in\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// git stays findable for the repo checks and the first task.
	path := dir
	if g, err := lookGit(); err == nil {
		path += string(os.PathListSeparator) + filepath.Dir(g)
	}
	if runtime.GOOS == "windows" {
		path += string(os.PathListSeparator) + filepath.Join(os.Getenv("SystemRoot"), "System32")
	}
	t.Setenv("PATH", path)
	t.Setenv(envSetupInteractive, "")
	old := setupExec
	setupExec = func(_ context.Context, bin string, args ...string) (string, string, error) {
		cli := strings.TrimSuffix(filepath.Base(bin), filepath.Ext(bin))
		out, err := answers(cli, strings.Join(args, " "))
		return out, "", err
	}
	t.Cleanup(func() { setupExec = old })
}

func lookGit() (string, error) {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		for _, n := range []string{"git", "git.exe"} {
			if p := filepath.Join(d, n); fileExists(p) {
				return p, nil
			}
		}
	}
	return "", errors.New("no git")
}

var errExit1 = errors.New("exit status 1")

// claudeOnly: Claude Code logged in, Codex installed and logged out.
func claudeOnly(cli, args string) (string, error) {
	switch cli + " " + args {
	case "claude --version":
		return "2.1.288 (Claude Code)", nil
	case "claude auth status":
		return `{"loggedIn": true, "authMethod": "claude.ai", "subscriptionType": "max"}`, nil
	case "codex --version":
		return "codex-cli 0.160.0", nil
	case "codex login status":
		return "Not logged in", errExit1
	}
	return "", errors.New("unexpected " + cli + " " + args)
}

func goRepo(t *testing.T) string {
	dir := gitInit(t)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.24\n"), 0o644)
	return dir
}

func setupIn(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func TestProbeCLIs(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude", "codex", "qwen", "ollama"}, func(cli, args string) (string, error) {
		switch cli + " " + args {
		case "claude --version":
			return "2.1.200 (Claude Code)", nil
		case "claude auth status":
			return `{"loggedIn": false, "authMethod": "none"}`, errExit1
		case "codex --version":
			return "codex-cli 0.160.0", nil
		case "codex login status":
			return "Logged in using ChatGPT", nil
		case "qwen --version":
			return "0.24.7", nil
		case "ollama --version":
			return "ollama version is 0.35.1", nil
		case "ollama list":
			return "NAME                      ID     SIZE   MODIFIED\nllama3:latest  abc  4 GB  2 days ago\n", nil
		}
		return "", errors.New("unexpected")
	})
	got := map[string]cliStatus{}
	for _, s := range probeCLIs(config.Default()) {
		got[s.name] = s
	}
	if s := got["claude"]; s.ready() || s.login != loginNo || !strings.Contains(strings.Join(s.fix, " "), "claude auth login") || s.version != "2.1.200 (Claude Code)" {
		t.Errorf("claude logged out: %+v", s)
	}
	if s := got["codex"]; !s.ready() || s.login != loginOK || s.detail != "Logged in using ChatGPT" {
		t.Errorf("codex logged in: %+v", s)
	}
	if s := got["gemini"]; s.path != "" || s.ready() || len(s.fix) == 0 {
		t.Errorf("gemini missing: %+v", s)
	}
	if s := got["ollama"]; s.ready() || !strings.Contains(strings.Join(s.fix, " "), "ollama pull qwen3.6:35b-a3b-coding") {
		t.Errorf("ollama without the model: %+v", s)
	}
	if s := got["qwen"]; s.ready() || !strings.Contains(s.detail, "not pulled") {
		t.Errorf("qwen without the model: %+v", s)
	}
	if m := readyMain(probeCLIs(config.Default())); strings.Join(m, ",") != "codex" {
		t.Errorf("ready: %v", m)
	}
	var b bytes.Buffer
	printCLIs(&b, probeCLIs(config.Default()))
	out := b.String()
	for _, want := range []string{"tested with 2.1.288 (Claude Code)", "-> log in: claude auth login", "gemini: not installed (optional"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// A CLI that neither prints a version nor answers its sign-in check does
// not run: it must not be turned on (found with a broken .cmd shim).
func TestProbeBrokenCLI(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude"}, func(cli, args string) (string, error) {
		return "The system cannot find the path specified.", errExit1
	})
	for _, s := range probeCLIs(config.Default()) {
		if s.name == "claude" && (s.ready() || !strings.Contains(s.detail, "does not run") || !strings.Contains(strings.Join(s.fix, " "), "install")) {
			t.Errorf("broken claude: %+v", s)
		}
	}
}

func TestCodexLoginTimeoutIsUnknown(t *testing.T) {
	login, _ := codexLogin(func(...string) (string, string, error) { return "", "", errNoAnswer })
	if login != loginUnknown {
		t.Errorf("a timeout must not count as logged out: %d", login)
	}
}

func TestSetupYes(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude", "codex"}, claudeOnly)
	fake := runner.NewFakeSet(0)
	headlessRunners = func(*config.Config) runner.Set { return fake }
	defer func() { headlessRunners = runner.New }()
	repo := goRepo(t)
	chdir(t, repo)
	path, _ := userConfigPath()
	var out bytes.Buffer
	res, err := runSetup(setupOpts{in: setupIn(""), out: &out, yes: true, path: path, dir: repo})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["claude"].Disabled || !cfg.Providers["codex"].Disabled || !cfg.Providers["gemini"].Disabled {
		t.Errorf("claude only: %v", cfg.Enabled())
	}
	if b, _ := os.ReadFile(filepath.Join(repo, config.RepoFileName)); !strings.Contains(string(b), `"go test ./..."`) {
		t.Errorf("checks not saved:\n%s", b)
	}
	if res.asked != 0 || !strings.Contains(out.String(), "Setup done: 0 question(s)") || strings.Contains(out.String(), "FAIL") {
		t.Errorf("asked %d:\n%s", res.asked, out.String())
	}
	if !strings.Contains(out.String(), "First task") || !strings.Contains(out.String(), "first task ") {
		t.Errorf("no first task:\n%s", out.String())
	}
	if needsSetup("") {
		t.Error("needsSetup after setup")
	}
}

// Without a terminal and without --yes: no questions, the config only; no
// repo file, no task (it would spend quota).
func TestSetupNoTerminal(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude", "codex"}, claudeOnly)
	repo := goRepo(t)
	chdir(t, repo)
	path, _ := userConfigPath()
	var out bytes.Buffer
	if _, err := runSetup(setupOpts{in: setupIn(""), out: &out, path: path, dir: repo}); err != nil {
		t.Fatal(err)
	}
	if !fileExists(path) {
		t.Error("no config written")
	}
	if fileExists(filepath.Join(repo, config.RepoFileName)) {
		t.Error("repo file written without --yes")
	}
	if !strings.Contains(out.String(), "Run it now? no (no terminal: the default)") {
		t.Errorf("first task:\n%s", out.String())
	}
}

func TestSetupInteractiveDefaults(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude", "codex"}, claudeOnly)
	repo := goRepo(t)
	chdir(t, repo)
	path, _ := userConfigPath()
	var out bytes.Buffer
	// Enter at the providers and the checks; "n" to the first task.
	res, err := runSetup(setupOpts{in: setupIn("\n\nn\n"), out: &out, interactive: true, path: path, dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if res.asked != 3 || strings.Join(res.enabled, ",") != "claude" {
		t.Errorf("asked %d, enabled %v:\n%s", res.asked, res.enabled, out.String())
	}
	if !fileExists(filepath.Join(repo, config.RepoFileName)) {
		t.Error("Enter must save the checks")
	}
}

// Safe by default: an existing config is replaced only when you agree,
// and backed up first.
func TestSetupKeepsExistingConfig(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude"}, claudeOnly)
	dir := t.TempDir()
	chdir(t, dir)
	path, _ := userConfigPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	mine := []byte("theme: ascii # mine\n")
	reset := func() { os.WriteFile(path, mine, 0o644) }
	backups := func() []string { m, _ := filepath.Glob(path + ".bak-*"); return m }
	for _, c := range []struct {
		name        string
		in          string
		interactive bool
		yes, force  bool
		replaced    bool
	}{
		{name: "no terminal", replaced: false},
		{name: "--yes", yes: true, replaced: false},
		{name: "Enter", in: "\n", interactive: true, replaced: false},
		{name: "y", in: "y\n\n", interactive: true, replaced: true},
		{name: "--force", force: true, replaced: true},
	} {
		reset()
		for _, b := range backups() {
			os.Remove(b)
		}
		var out bytes.Buffer
		_, err := runSetup(setupOpts{in: setupIn(c.in), out: &out, interactive: c.interactive, yes: c.yes, force: c.force, noTask: true, path: path, dir: dir})
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out.String())
		}
		now, _ := os.ReadFile(path)
		if got := !bytes.Equal(now, mine); got != c.replaced {
			t.Errorf("%s: replaced = %v\n%s", c.name, got, out.String())
		}
		bs := backups()
		if c.replaced {
			if len(bs) != 1 {
				t.Fatalf("%s: backups %v", c.name, bs)
			}
			if b, _ := os.ReadFile(bs[0]); !bytes.Equal(b, mine) {
				t.Errorf("%s: backup holds %q", c.name, b)
			}
		} else if len(bs) != 0 {
			t.Errorf("%s: backup made without replacing: %v", c.name, bs)
		}
	}
}

func TestSetupNoCLIReady(t *testing.T) {
	isolate(t)
	var mu sync.Mutex
	checks := 0 // logged in from the third sign-in check on
	fakeCLIs(t, []string{"claude"}, func(cli, args string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args {
		case "--version":
			return "2.1.288 (Claude Code)", nil
		case "auth status":
			if checks++; checks >= 3 {
				return `{"loggedIn": true, "authMethod": "claude.ai"}`, nil
			}
			return `{"loggedIn": false}`, errExit1
		}
		return "", errors.New("unexpected")
	})
	dir := t.TempDir()
	chdir(t, dir)
	path, _ := userConfigPath()

	// Scripts get the next step as an error, and no config.
	var out bytes.Buffer
	_, err := runSetup(setupOpts{in: setupIn(""), out: &out, yes: true, path: path, dir: dir})
	if err == nil || !strings.Contains(err.Error(), "install and log in") || fileExists(path) {
		t.Fatalf("no CLI ready: %v, config written: %v\n%s", err, fileExists(path), out.String())
	}
	if !strings.Contains(out.String(), "-> log in: claude auth login") {
		t.Errorf("no login step:\n%s", out.String())
	}
	// At the terminal: log in elsewhere, press Enter to check again, and
	// setup goes on (Enter at the providers too).
	out.Reset()
	res, err := runSetup(setupOpts{in: setupIn("\n\n"), out: &out, interactive: true, noTask: true, path: path, dir: dir})
	if err != nil || !fileExists(path) || strings.Join(res.enabled, ",") != "claude" {
		t.Fatalf("after logging in: %v %v\n%s", err, res.enabled, out.String())
	}
	if n := strings.Count(out.String(), "Agent CLIs"); n != 2 || res.asked != 2 {
		t.Errorf("checked %d time(s), asked %d:\n%s", n, res.asked, out.String())
	}
	// q, or the end of the input, stops without a config (no endless
	// checking when stdin is closed).
	for _, in := range []string{"q\n", ""} {
		os.Remove(path)
		checks = -10
		done := make(chan error, 1)
		go func() {
			_, err := runSetup(setupOpts{in: setupIn(in), out: io.Discard, interactive: true, path: path, dir: dir})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || fileExists(path) {
				t.Errorf("%q: %v, config written: %v", in, err, fileExists(path))
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%q: setup did not stop", in)
		}
	}
}

func TestChooseProviders(t *testing.T) {
	clis := []cliStatus{
		{name: "claude", provider: "claude", path: "x", login: loginOK},
		{name: "codex", provider: "codex", path: "x", login: loginNo},
		{name: "qwen", provider: "qwen", path: "x", login: loginOK},
		{name: "gemini", provider: "gemini"},
	}
	for _, c := range []struct{ in, want, says string }{
		{"\n", "claude,qwen", ""},
		{"claude\n", "claude", ""},
		{"codex, claude\n", "codex,claude", "codex is not signed in yet"},
		{"qwen\n\n", "claude,qwen", "A local model alone"},
		{"gemini\nclaude\n", "claude", `"gemini" is not an installed CLI`},
		{"n\nclaude\n", "claude", "Type the ones you want"},
	} {
		var out bytes.Buffer
		a := &asker{r: setupIn(c.in), out: &out, interactive: true}
		if got := strings.Join(chooseProviders(a, clis), ","); got != c.want || !strings.Contains(out.String(), c.says) {
			t.Errorf("%q: got %s, want %s\n%s", c.in, got, c.want, out.String())
		}
	}
}

func TestNeedsSetup(t *testing.T) {
	isolate(t)
	chdir(t, t.TempDir())
	if !needsSetup("") {
		t.Error("no config anywhere")
	}
	if needsSetup("other.yaml") {
		t.Error("--config given")
	}
	os.WriteFile(config.FileName, []byte("theme: ascii\n"), 0o644)
	if needsSetup("") {
		t.Error("./switchyard.yaml exists")
	}
	os.Remove(config.FileName)
	p, _ := userConfigPath()
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("theme: ascii\n"), 0o644)
	if needsSetup("") {
		t.Error("user config exists")
	}
	// Without a terminal (go test), sy, sy run and sy web never ask.
	os.Remove(p)
	t.Setenv(envSetupInteractive, "")
	if err := firstRun("", "", false); err != nil || fileExists(p) {
		t.Errorf("firstRun without a terminal: %v, wrote %v", err, fileExists(p))
	}
}

// The real quick checks through a .cmd shim (Windows) or a script, in a
// folder with spaces, parentheses and non-ASCII letters: how npm installs
// the CLIs under a user name like "Jane Doe (Work)".
func TestSetupExecShimWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Jane Doe (ä)", "npm")
	os.MkdirAll(dir, 0o755)
	name, body := "claude", "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '2.1.288 (Claude Code)'; exit 0; fi\necho '{\"loggedIn\": false}'; exit 1\n"
	if runtime.GOOS == "windows" {
		name = "claude.cmd"
		body = "@echo off\r\nif \"%~1\"==\"--version\" goto version\r\necho {\"loggedIn\": false}\r\nexit /b 1\r\n:version\r\necho 2.1.288 (Claude Code)\r\nexit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	isolate(t)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := probeCLI(config.Default(), "claude", "claude", false)
	if s.version != "2.1.288 (Claude Code)" || s.login != loginNo || s.ready() {
		t.Errorf("through the shim: %+v", s)
	}
}
