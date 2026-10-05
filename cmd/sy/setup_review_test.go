package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
)

// Review finding: backups had second resolution, so two replaces in the
// same second overwrote the only backup of the original.
func TestSetupBackupsNeverOverwritten(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude"}, claudeOnly)
	dir := t.TempDir()
	chdir(t, dir)
	path, _ := userConfigPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	mine := []byte("theme: ascii # mine\n")
	os.WriteFile(path, mine, 0o644)
	for i := 0; i < 2; i++ {
		if _, err := runSetup(setupOpts{in: setupIn(""), out: &bytes.Buffer{}, force: true, noTask: true, path: path, dir: dir}); err != nil {
			t.Fatal(err)
		}
	}
	bs, _ := filepath.Glob(path + ".bak-*")
	if len(bs) != 2 {
		t.Fatalf("want 2 backups, got %v", bs)
	}
	found := false
	for _, b := range bs {
		data, _ := os.ReadFile(b)
		found = found || bytes.Equal(data, mine)
	}
	if !found {
		t.Error("no backup holds the original config")
	}
}

// Review finding: a config that cannot be read was replaced without a
// backup.
func TestSetupUnreadableConfig(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude"}, claudeOnly)
	dir := t.TempDir()
	chdir(t, dir)
	path, _ := userConfigPath()
	os.MkdirAll(filepath.Dir(path), 0o755)

	// A folder in its place: it can be found but not read.
	os.MkdirAll(path, 0o755)
	var out bytes.Buffer
	_, err := runSetup(setupOpts{in: setupIn(""), out: &out, force: true, noTask: true, path: path, dir: dir})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("folder: %v\n%s", err, out.String())
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Error("the folder was replaced")
	}
	os.Remove(path)

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return // no unreadable files by mode
	}
	mine := []byte("theme: ascii # mine\n")
	os.WriteFile(path, mine, 0o644)
	os.Chmod(path, 0)
	defer os.Chmod(path, 0o644)
	if _, err := runSetup(setupOpts{in: setupIn(""), out: &out, force: true, noTask: true, path: path, dir: dir}); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("chmod 000: %v", err)
	}
	os.Chmod(path, 0o644)
	if data, _ := os.ReadFile(path); !bytes.Equal(data, mine) {
		t.Errorf("an unreadable config was replaced: %q", data)
	}
	if bs, _ := filepath.Glob(path + ".bak-*"); len(bs) != 0 {
		t.Errorf("backups: %v", bs)
	}
}

// Review finding: the config (API keys in provider env, webhook URLs) and
// its backup were written 0644 whatever the original's mode.
func TestSetupConfigMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes")
	}
	isolate(t)
	fakeCLIs(t, []string{"claude"}, claudeOnly)
	dir := t.TempDir()
	chdir(t, dir)
	path, _ := userConfigPath()
	mode := func(p string) os.FileMode {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode().Perm()
	}
	if _, err := runSetup(setupOpts{in: setupIn(""), out: &bytes.Buffer{}, noTask: true, path: path, dir: dir}); err != nil {
		t.Fatal(err)
	}
	if m := mode(path); m != 0o600 {
		t.Errorf("new config: %v", m)
	}
	os.Chmod(path, 0o640)
	if _, err := runSetup(setupOpts{in: setupIn(""), out: &bytes.Buffer{}, force: true, noTask: true, path: path, dir: dir}); err != nil {
		t.Fatal(err)
	}
	bs, _ := filepath.Glob(path + ".bak-*")
	if len(bs) != 1 {
		t.Fatalf("backups: %v", bs)
	}
	if m, b := mode(path), mode(bs[0]); m != 0o640 || b != 0o640 {
		t.Errorf("replaced config %v, backup %v; want the original's 0640", m, b)
	}
}

// Review finding: a broken qwen was turned on because the Ollama check
// overwrote "does not run".
func TestProbeBrokenQwen(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude", "qwen", "ollama"}, func(cli, args string) (string, error) {
		switch cli + " " + args {
		case "ollama --version":
			return "ollama version is 0.35.1", nil
		case "ollama list":
			return "NAME ID SIZE MODIFIED\nqwen3.6:35b-a3b-coding abc 20 GB now\n", nil
		case "qwen --version":
			return "", errExit1
		}
		return claudeOnly(cli, args)
	})
	for _, s := range probeCLIs(config.Default()) {
		if s.name == "qwen" && (s.ready() || !strings.Contains(s.detail, "does not run")) {
			t.Errorf("broken qwen: %+v", s)
		}
		if s.name == "ollama" && !s.ready() {
			t.Errorf("ollama: %+v", s)
		}
	}
}

// Review finding: gemini's sign-in check runs nothing, so a gemini that
// does not start counted as ready when a key was set.
func TestProbeBrokenGemini(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"gemini"}, func(cli, args string) (string, error) { return "", errExit1 })
	t.Setenv("GEMINI_API_KEY", "k")
	for _, s := range probeCLIs(config.Default()) {
		if s.name == "gemini" && s.ready() {
			t.Errorf("broken gemini: %+v", s)
		}
	}
}

// Review finding: quitting the automatic setup stopped the command it
// came before (sy web and the TUI used to start with the defaults).
func TestAutoSetupGoesOnWhenStopped(t *testing.T) {
	isolate(t)
	fakeCLIs(t, nil, claudeOnly) // no CLI at all
	dir := t.TempDir()
	chdir(t, dir)
	var out bytes.Buffer
	firstRunWith(setupIn("q\n"), &out, dir, false)
	if !strings.Contains(out.String(), "goes on with the built-in defaults") || !strings.Contains(out.String(), "sy init --global") {
		t.Errorf("no way out shown:\n%s", out.String())
	}
	if p, _ := userConfigPath(); fileExists(p) {
		t.Error("config written")
	}
}

// Review finding: the automatic setup wrote .switchyard.yaml (default yes),
// and the untracked file stopped `sy run --issues` (clean tree only).
func TestAutoSetupLeavesRepoAlone(t *testing.T) {
	isolate(t)
	fakeCLIs(t, []string{"claude"}, claudeOnly)
	repo := goRepo(t)
	run(t, repo, "add", "go.mod")
	run(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "go")
	chdir(t, repo)
	var out bytes.Buffer
	firstRunWith(setupIn("\n"), &out, repo, false)
	if p, _ := userConfigPath(); !fileExists(p) {
		t.Fatalf("no config:\n%s", out.String())
	}
	if fileExists(filepath.Join(repo, config.RepoFileName)) {
		t.Errorf("auto setup wrote %s", config.RepoFileName)
	}
	if st, _ := gitStatus(repo); st != "" {
		t.Errorf("the tree is not clean:\n%s", st)
	}
	if !strings.Contains(out.String(), "sy init --repo") || !strings.Contains(out.String(), "Setup done: 1 question(s)") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestAutoSetupOff(t *testing.T) {
	for _, c := range []struct {
		ci, no, forced string
		off            bool
	}{
		{"", "", "", false},
		{"true", "", "", true},
		{"", "1", "", true},
		{"true", "", "1", false},
	} {
		t.Setenv("CI", c.ci)
		t.Setenv(envNoSetup, c.no)
		t.Setenv(envSetupInteractive, c.forced)
		if got := autoSetupOff(); got != c.off {
			t.Errorf("%+v: off = %v", c, got)
		}
	}
}

// Plausible review finding: an API key or a cloud account in the
// environment, or an older CLI, must not block a provider.
func TestLoginWithKeysAndOldCLIs(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "OPENAI_API_KEY", "CODEX_API_KEY"} {
		t.Setenv(k, "")
	}
	claudeOut := func(...string) (string, string, error) { return `{"loggedIn": false}`, "", errExit1 }
	codexOut := func(...string) (string, string, error) { return "Not logged in", "", errExit1 }
	if l, _ := claudeLogin(claudeOut); l != loginNo {
		t.Errorf("claude without a key: %d", l)
	}
	if l, _ := codexLogin(codexOut); l != loginNo {
		t.Errorf("codex without a key: %d", l)
	}
	old := func(...string) (string, string, error) {
		return "", "error: unrecognized subcommand 'status'", errors.New("exit status 2")
	}
	if l, _ := codexLogin(old); l != loginUnknown {
		t.Errorf("older codex: %d", l)
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	if l, d := claudeLogin(claudeOut); l != loginUnknown || !strings.Contains(d, "CLAUDE_CODE_USE_BEDROCK") {
		t.Errorf("claude on Bedrock: %d %s", l, d)
	}
	t.Setenv("OPENAI_API_KEY", "sk-x")
	if l, _ := codexLogin(codexOut); l != loginUnknown {
		t.Errorf("codex with a key: %d", l)
	}
}
