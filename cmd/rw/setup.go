package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
)

// rw setup is the guided first run (Phase 2 exit criterion: install to
// first task in under 5 minutes). It finds the agent CLIs, says how to
// install or log in to the ones that are missing, writes the config with
// the ready ones turned on, saves the repo's checks and offers a small
// read-only first task. rw, rw run and rw web start it by themselves when
// there is no config file yet and someone is at the terminal.
//
// The checks cost no quota: --version, the CLIs' own sign-in status
// commands, `ollama list` and Gemini's settings file. No prompt is sent.

// envSetupInteractive forces the questions without a terminal ("1"), so
// tests can answer them through a pipe.
const envSetupInteractive = "RW_SETUP_INTERACTIVE"

const (
	loginNA      = iota // nothing to sign in to
	loginOK             // signed in (or a local server that is ready)
	loginNo             // not signed in, or not ready
	loginUnknown        // the check failed: given the benefit of the doubt
)

// cliStatus is what rw setup found out about one agent CLI.
type cliStatus struct {
	name     string // the CLI: claude, codex, gemini, qwen, ollama
	provider string // the provider it becomes in the config
	optional bool   // gemini and the local models
	path     string // "" = not on PATH
	version  string
	tested   string
	verErr   error
	login    int
	detail   string   // what the sign-in check found
	fix      []string // what to do, in order
	models   []string // ollama: the pulled models
}

func (s cliStatus) ready() bool { return s.path != "" && s.login != loginNo }

// setupCLIs are the CLIs rw setup looks for, with the provider whose
// command and tested version each uses.
var setupCLIs = []struct {
	name, provider string
	optional       bool
}{
	{"claude", event.Claude, false},
	{"codex", event.Codex, false},
	{"gemini", event.Gemini, true},
	{"qwen", event.Qwen, true},
	{"ollama", "ollama-run", true},
}

// setupExec runs one quick check command with env added to rw's own
// (tests swap it in).
var setupExec = func(ctx context.Context, env []string, bin string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	proc.Prepare(cmd) // .cmd shims under paths with spaces; a timeout kills the tree
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	if ctx.Err() != nil {
		err = errNoAnswer
	}
	return o.String(), e.String(), err
}

const setupTimeout = 20 * time.Second

var errNoAnswer = fmt.Errorf("no answer within %s", setupTimeout)

// probeCLIs checks every CLI at once (each one is a Node or native start).
func probeCLIs(cfg *config.Config) []cliStatus {
	out := make([]cliStatus, len(setupCLIs))
	var wg sync.WaitGroup
	for i, c := range setupCLIs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = probeCLI(cfg, c.name, c.provider, c.optional)
		}()
	}
	wg.Wait()
	// Qwen Code's preset runs its model through Ollama.
	var ollama cliStatus
	for _, s := range out {
		if s.name == "ollama" {
			ollama = s
		}
	}
	for i, s := range out {
		if s.name == "qwen" && s.path != "" && s.login != loginNo { // loginNo: it does not run
			out[i] = qwenNeedsOllama(cfg, s, ollama)
		}
	}
	return out
}

func probeCLI(cfg *config.Config, name, provider string, optional bool) (s cliStatus) {
	pc := cfg.Providers[provider]
	s = cliStatus{name: name, provider: provider, optional: optional, tested: pc.TestedVersion}
	bin, err := proc.Resolve(pc.Command)
	if err != nil {
		s.fix = installSteps(name, cfg)
		return s
	}
	s.path = bin
	run := func(args ...string) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		return setupExec(ctx, nil, bin, args...)
	}
	out, errOut, err := run("--version")
	if s.version = firstLine(out); err != nil || s.version == "" {
		s.verErr = err
		if err == nil {
			s.verErr = errors.New("printed no version")
		}
		if e := oneLine(errOut, 100); e != "" {
			s.verErr = fmt.Errorf("%v: %s", s.verErr, e)
		}
	}
	defer func() {
		// No version and no sign-in: the CLI does not run at all. Gemini's
		// sign-in check reads files and runs nothing, so its version
		// check alone decides.
		if s.verErr != nil && (s.login != loginOK || name == "gemini") {
			s.login, s.detail = loginNo, "does not run ("+s.verErr.Error()+")"
			s.fix = installSteps(name, cfg)
		}
	}()
	switch name {
	case "claude":
		s.login, s.detail = claudeLogin(run)
		if s.login == loginNo {
			s.fix = []string{"log in: claude auth login"}
		}
	case "codex":
		s.login, s.detail = codexLogin(run)
		if s.login == loginNo {
			s.fix = []string{"log in: codex login"}
		}
	case "gemini":
		if msg := geminiAuthProblem(pc); msg != "" {
			s.login, s.detail = loginNo, msg // msg says what to do
		} else {
			s.login, s.detail = loginOK, "sign-in set up"
		}
	case "ollama":
		model := firstModel(cfg, provider)
		stdout, stderr, err := run("list")
		if err != nil {
			s.login, s.detail = loginNo, "the Ollama server is not running ("+oneLine(stderr+stdout, 80)+")"
			s.fix = []string{"start the Ollama app (or `ollama serve`)"}
			break
		}
		s.models = ollamaModels(stdout)
		if hasModel(s.models, model) {
			s.login, s.detail = loginOK, "server running, "+model+" pulled"
		} else {
			s.login, s.detail = loginNo, "server running, "+model+" not pulled"
			s.fix = []string{"ollama pull " + model}
		}
	}
	return s
}

// claudeLogin reads `claude auth status` (JSON; exit 1 when logged out).
func claudeLogin(run func(...string) (string, string, error)) (int, string) {
	stdout, stderr, err := run("auth", "status")
	var st struct {
		LoggedIn     *bool  `json:"loggedIn"`
		AuthMethod   string `json:"authMethod"`
		Subscription string `json:"subscriptionType"`
	}
	if json.Unmarshal([]byte(stdout), &st) == nil && st.LoggedIn != nil {
		if !*st.LoggedIn {
			if k := setEnv("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"); k != "" {
				return loginUnknown, "no login, but " + k + " is set (Claude Code may use it)"
			}
			return loginNo, "not logged in"
		}
		how := st.AuthMethod
		if st.Subscription != "" {
			how += ", " + st.Subscription
		}
		return loginOK, "logged in (" + how + ")"
	}
	if err == nil {
		err = errors.New("unexpected output")
	}
	return loginUnknown, fmt.Sprintf("could not check the login (claude auth status: %v %s)", err, oneLine(stderr, 80))
}

// codexLogin reads `codex login status` (exit 0 when logged in).
func codexLogin(run func(...string) (string, string, error)) (int, string) {
	stdout, stderr, err := run("login", "status")
	text := oneLine(stdout+" "+stderr, 80)
	switch {
	case err == nil:
		return loginOK, text
	case errors.Is(err, errNoAnswer) || !reNotLoggedIn.MatchString(text):
		// A timeout, or an older CLI without `login status`.
		return loginUnknown, fmt.Sprintf("could not check the login (codex login status: %v %s)", err, text)
	}
	if k := setEnv("OPENAI_API_KEY", "CODEX_API_KEY"); k != "" {
		return loginUnknown, "no login, but " + k + " is set (Codex may use it)"
	}
	return loginNo, text // "Not logged in", exit 1
}

var reNotLoggedIn = regexp.MustCompile(`(?i)not logged in|logged out|not signed in`)

// setEnv returns the first of these variables that is set ("" = none).
func setEnv(names ...string) string {
	for _, n := range names {
		if os.Getenv(n) != "" {
			return n
		}
	}
	return ""
}

func qwenNeedsOllama(cfg *config.Config, s, ollama cliStatus) cliStatus {
	model := firstModel(cfg, s.provider)
	switch {
	case ollama.path == "":
		s.login, s.detail = loginNo, "runs its model through Ollama, which is not installed"
		s.fix = installSteps("ollama", cfg)
	case ollama.login == loginNo && len(ollama.models) == 0:
		s.login, s.detail, s.fix = loginNo, "runs its model through Ollama: "+ollama.detail, ollama.fix
	case !hasModel(ollama.models, model):
		s.login, s.detail = loginNo, "runs "+model+" through Ollama, which is not pulled"
		s.fix = []string{"ollama pull " + model}
	default:
		s.login, s.detail = loginOK, "runs "+model+" on Ollama"
	}
	return s
}

func firstModel(cfg *config.Config, provider string) string {
	if ms := cfg.Providers[provider].Models; len(ms) > 0 {
		return ms[0].ID
	}
	return ""
}

// ollamaModels reads the NAME column of `ollama list`.
func ollamaModels(out string) []string {
	var names []string
	for i, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if i == 0 || len(f) == 0 || f[0] == "NAME" {
			continue
		}
		names = append(names, f[0])
	}
	return names
}

func hasModel(models []string, m string) bool {
	for _, x := range models {
		if x == m || x == m+":latest" {
			return true
		}
	}
	return false
}

// installSteps says how to install a CLI on this OS and sign in.
func installSteps(name string, cfg *config.Config) []string {
	win, mac := runtime.GOOS == "windows", runtime.GOOS == "darwin"
	switch name {
	case "claude":
		native := "curl -fsSL https://claude.ai/install.sh | bash"
		if win {
			native = "irm https://claude.ai/install.ps1 | iex   (PowerShell)"
		}
		return []string{"install: " + native + "   or: npm install -g @anthropic-ai/claude-code", "log in: claude auth login"}
	case "codex":
		inst := "npm install -g @openai/codex"
		if mac {
			inst += "   or: brew install --cask codex"
		}
		return []string{"install: " + inst, "log in: codex login"}
	case "gemini":
		return []string{"install: npm install -g @google/gemini-cli, then set GEMINI_API_KEY (personal Google sign-in no longer works)"}
	case "qwen":
		return []string{"install: npm install -g @qwen-code/qwen-code (runs a local model through Ollama)"}
	case "ollama":
		inst := "curl -fsSL https://ollama.com/install.sh | sh"
		switch {
		case win:
			inst = "winget install Ollama.Ollama"
		case mac:
			inst = "brew install ollama"
		}
		return []string{"install: " + inst + ", then: ollama pull " + firstModel(cfg, "ollama-run")}
	}
	return nil
}

// nodeHint says how to get npm, for the npm install lines.
func nodeHint() string {
	switch runtime.GOOS {
	case "windows":
		return "winget install OpenJS.NodeJS.LTS"
	case "darwin":
		return "brew install node"
	}
	return "Node.js 20 or newer from your package manager or nodejs.org"
}

// isTerminal reports whether f is a console or terminal (not a pipe, a
// file or Git Bash's mintty pipes).
func isTerminal(f *os.File) bool { return term.IsTerminal(f.Fd()) }

// setupInteractive reports whether rw setup may ask questions.
func setupInteractive(needOut bool) bool {
	if os.Getenv(envSetupInteractive) == "1" {
		return true
	}
	return isTerminal(os.Stdin) && (!needOut || isTerminal(os.Stdout))
}

// userConfigPath is where rw setup writes: the user config, which every
// project uses.
func userConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no user config folder (%v): pass --config <file>", err)
	}
	return filepath.Join(dir, "relayweft", config.FileName), nil
}

// needsSetup reports whether rw would start with no config file at all.
func needsSetup(cfgPath string) bool {
	if cfgPath != "" {
		return false
	}
	if _, err := os.Stat(config.FileName); err == nil {
		return false
	}
	p, err := userConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return errors.Is(err, os.ErrNotExist)
}

// firstRun runs the guided setup before rw, rw run or rw web when there is
// no config yet and someone is at the terminal. Scripts and CI (no
// terminal) keep the built-in defaults, as before.
func firstRun(cfgPath, dir string, tui bool) error {
	if !needsSetup(cfgPath) || !setupInteractive(true) || autoSetupOff() {
		return nil
	}
	firstRunWith(bufio.NewReader(os.Stdin), os.Stdout, dir, tui)
	return nil
}

// envNoSetup set to anything keeps rw, rw run and rw web from starting the
// setup (a terminal nobody answers: docker run -t, IDE runners).
const envNoSetup = "RW_NO_SETUP"

// autoSetupOff reports whether the automatic setup is switched off: by
// RW_NO_SETUP, or in CI, where a terminal may have nobody at it.
func autoSetupOff() bool {
	if os.Getenv(envSetupInteractive) == "1" {
		return false
	}
	return os.Getenv(envNoSetup) != "" || os.Getenv("CI") != ""
}

// firstRunWith is the automatic setup. Whatever happens, the command you
// asked for goes on afterwards: with the new config, or, if the setup
// stopped, with the built-in defaults as before.
func firstRunWith(in *bufio.Reader, out io.Writer, dir string, tui bool) {
	path, err := userConfigPath()
	if err == nil {
		_, err = runSetup(setupOpts{in: in, out: out, interactive: true, path: path, dir: dir, auto: true})
	}
	if err != nil {
		fmt.Fprintf(out, "\nSetup stopped: %v\nrw goes on with the built-in defaults. Run `rw setup` any time. "+
			"`rw init --global` writes the default config, so this setup is not offered again (or set %s=1).\n", err, envNoSetup)
	}
	if tui {
		// The TUI's screen would hide what setup printed.
		a := &asker{r: in, out: out, interactive: true}
		a.line("\nPress Enter to start Relayweft. ")
	}
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("rw setup", flag.ExitOnError)
	yes := fs.Bool("yes", false, "no questions: take every default and run the first task")
	force := fs.Bool("force", false, "replace an existing config (it is backed up first)")
	noTask := fs.Bool("no-task", false, "skip the first task")
	cfgPath := fs.String("config", "", "config file to write (default: your user config)")
	dir := fs.String("dir", "", "project folder: its repo's checks are saved and the first task explains it (default: current folder)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: rw setup [--yes] [--force] [--no-task] [--config <file>] [--dir <path>]

The guided first run: finds Claude Code, Codex, Gemini CLI, Qwen Code and
Ollama, checks their versions and sign-in (no quota used), says how to
install or log in to the missing ones, writes your config with the ready
ones turned on, saves this repo's checks and offers a small read-only first
task. Enter takes the default at every question. Without a terminal it asks
nothing, writes the config only and skips the first task; --yes also saves
the checks and runs the task. An existing config is kept unless you agree
to replace it (or pass --force); it is backed up first.
`)
	}
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	path := *cfgPath
	if path == "" {
		p, err := userConfigPath()
		if err != nil {
			return err
		}
		path = p
	}
	interactive := !*yes && setupInteractive(false)
	_, err := runSetup(setupOpts{in: bufio.NewReader(os.Stdin), out: os.Stdout, interactive: interactive,
		yes: *yes, force: *force, noTask: *noTask, path: path, dir: *dir})
	return err
}

type setupOpts struct {
	in          *bufio.Reader
	out         io.Writer
	interactive bool
	yes         bool   // take every default, and run the first task
	force       bool   // replace an existing config (backed up)
	noTask      bool   // no first task
	auto        bool   // started by rw, rw run or rw web: they go on afterwards
	path        string // the config file to write
	dir         string // the project folder ("" = current)
}

type setupResult struct {
	wrote   bool
	enabled []string
	asked   int
	own     time.Duration // rw's own time: the run minus waiting for answers and the first task
}

// asker asks the setup's questions, or takes their defaults.
type asker struct {
	r           *bufio.Reader
	out         io.Writer
	interactive bool
	yes         bool
	asked       int
	waited      time.Duration
	eof         bool // the input ended: every later answer is the default
}

// line asks and returns the trimmed answer ("" at EOF).
func (a *asker) line(q string) string {
	fmt.Fprint(a.out, q)
	began := time.Now()
	s, err := a.r.ReadString('\n')
	a.waited += time.Since(began)
	a.asked++
	if err != nil && s == "" {
		a.eof = true
		fmt.Fprintln(a.out)
	}
	return strings.TrimSpace(s)
}

// yesNo asks a yes/no question. Without questions it takes def, or with
// --yes the answer yes.
func (a *asker) yesNo(q string, def bool) bool {
	if !a.interactive {
		v := def || a.yes
		why := "no terminal: the default"
		if a.yes {
			why = "--yes"
		}
		fmt.Fprintf(a.out, "%s %s (%s)\n", q, map[bool]string{true: "yes", false: "no"}[v], why)
		return v
	}
	hint := " [y/N] "
	if def {
		hint = " [Y/n] "
	}
	for i := 0; i < 3; i++ {
		switch strings.ToLower(a.line(q + hint)) {
		case "":
			return def
		case "y", "yes", "j", "ja":
			return true
		case "n", "no", "nein":
			return false
		}
		fmt.Fprintln(a.out, "Please answer y or n.")
	}
	return def
}

func runSetup(o setupOpts) (setupResult, error) {
	began := time.Now()
	w := o.out
	a := &asker{r: o.in, out: w, interactive: o.interactive, yes: o.yes}
	var res setupResult
	finish := func() {
		res.asked = a.asked
		res.own = time.Since(began) - a.waited
	}
	dir := o.dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if d, err := filepath.Abs(dir); err == nil {
		dir = d
	}
	bold := stRouter.Bold(true)
	if o.auto {
		fmt.Fprintln(w, bold.Render("Welcome to Relayweft.")+" There is no config yet, so a short setup comes first.")
	} else {
		fmt.Fprintln(w, bold.Render("Relayweft setup"))
	}
	if o.interactive {
		fmt.Fprintln(w, stMuted.Render("Enter takes the default at every question; Ctrl+C stops. Nothing is written before the providers question."))
	}

	// 1. An existing config is never replaced without asking, and never
	// without a backup.
	replace := true
	if _, err := os.Stat(o.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		finish()
		return res, fmt.Errorf("cannot read %s: %w (nothing was changed)", o.path, err)
	} else if err == nil {
		switch {
		case o.force:
		case o.interactive:
			replace = a.yesNo(fmt.Sprintf("\n%s exists. Replace it? It is backed up first.", o.path), false)
		default:
			replace = false
		}
		if !replace {
			fmt.Fprintf(w, "\nKeeping your config %s (rw setup --force replaces it, after a backup).\n", o.path)
		}
	}

	// 2. The CLIs.
	cfg := config.Default()
	var clis []cliStatus
	var checks time.Duration
	for {
		fmt.Fprintln(w, "\n"+bold.Render("Agent CLIs")+stMuted.Render(" (checking versions and sign-in; no quota is used)"))
		t0 := time.Now()
		clis = probeCLIs(cfg)
		checks += time.Since(t0)
		printCLIs(w, clis)
		printGit(w)
		fmt.Fprintln(w, stMuted.Render(fmt.Sprintf("checked in %s", time.Since(t0).Round(100*time.Millisecond))))
		if !replace || len(readyMain(clis)) > 0 {
			break
		}
		msg := "no agent CLI is ready: install and log in to Claude Code or Codex (see above), then run `rw setup` again"
		if !o.interactive {
			finish()
			return res, errors.New(msg)
		}
		fmt.Fprintln(w, "\nNo agent CLI is ready yet. One is enough: Claude Code or Codex.")
		if ans := a.line("Do the steps above in another window, then press Enter to check again (q to quit): "); strings.EqualFold(ans, "q") || a.eof {
			finish()
			return res, errors.New(msg)
		}
	}

	// 3. The config.
	if replace {
		enabled := chooseProviders(a, clis)
		res.enabled = enabled
		on := map[string]bool{}
		for _, s := range clis {
			on[s.provider] = false
		}
		for _, p := range enabled {
			on[p] = true
		}
		backup, err := writeSetupConfig(o.path, on)
		if err != nil {
			finish()
			return res, err
		}
		res.wrote = true
		fmt.Fprintf(w, "wrote %s (providers on: %s)\n", o.path, strings.Join(enabled, ", "))
		if backup != "" {
			fmt.Fprintf(w, "your old config is kept as %s\n", backup)
		}
		// rw reads ./relayweft.yaml before the user config.
		if local, err := filepath.Abs(config.FileName); err == nil && !samePath(local, o.path) {
			if _, err := os.Stat(local); err == nil {
				fmt.Fprintf(w, "note: %s in this folder is used here instead of the file above\n", local)
			}
		}
	}

	// 4. The repo's checks, as rw init does.
	root := ""
	if r, err := gitRoot(dir); err == nil && strings.TrimSpace(r) != "" {
		root = filepath.Clean(filepath.FromSlash(r))
	}
	setupChecks(a, w, root, !o.auto)

	// 5. The first task.
	var taskTook time.Duration
	var taskErr error
	if !o.auto && !o.noTask {
		taskTook, taskErr = setupFirstTask(a, w, o.path, root)
		if taskErr != nil {
			fmt.Fprintf(w, "%s the first task did not finish: %v\n  run `rw doctor`, then try `rw run \"explain this repo\"` in a git repo\n", stErr.Render("FAIL"), taskErr)
			if !errors.Is(taskErr, errTaskFailed) {
				taskErr = fmt.Errorf("first task: %w", taskErr)
			}
		}
	}
	finish()
	res.own -= taskTook
	fmt.Fprintf(w, "\nSetup done: %d question(s) answered, %s of rw's own time (CLI checks %s)",
		res.asked, res.own.Round(100*time.Millisecond), checks.Round(100*time.Millisecond))
	if taskTook > 0 {
		fmt.Fprintf(w, "; first task %s", taskTook.Round(100*time.Millisecond))
	}
	fmt.Fprintln(w, ".")
	if !o.auto {
		fmt.Fprintln(w, "Next: `rw` in a git repo opens the TUI (`rw web` the browser UI); `rw run \"task\"` runs one task. `rw doctor` checks the setup again.")
	}
	return res, taskErr // a script sees a failed first task in the exit code
}

// readyMain returns the ready providers that can do every job: not only
// local stand-ins (only_preferred), which never take over on their own.
func readyMain(clis []cliStatus) []string {
	cfg := config.Default()
	var out []string
	for _, s := range clis {
		if s.ready() && !cfg.Providers[s.provider].OnlyPreferred {
			out = append(out, s.provider)
		}
	}
	return out
}

func printCLIs(w io.Writer, clis []cliStatus) {
	npm := false
	var extra []string // optional CLIs that are not installed
	for _, s := range clis {
		var mark, text string
		switch {
		case s.path == "" && s.optional:
			extra = append(extra, s.name)
			continue
		case s.path == "":
			mark, text = stMuted.Render("--  "), "not installed"
		case s.verErr != nil && !s.ready():
			mark, text = stRev.Render("warn"), s.detail // does not run
		case s.verErr != nil:
			mark, text = stRev.Render("warn"), "version check failed ("+s.verErr.Error()+"), "+s.detail
		default:
			mark, text = stOK.Render("ok  "), s.version
			if !s.ready() || s.login == loginUnknown {
				mark = stRev.Render("warn")
			}
			if s.tested != "" && s.version != s.tested {
				text += stMuted.Render(" (tested with " + s.tested + "; it may still work)")
			}
			if s.detail != "" {
				text += ", " + s.detail
			}
		}
		fmt.Fprintf(w, "  %s %-7s %s\n", mark, s.name, text)
		for _, f := range s.fix {
			fmt.Fprintf(w, "               -> %s\n", f)
			npm = npm || strings.Contains(f, "npm install")
		}
	}
	if _, err := proc.Resolve("npm"); npm && err != nil {
		fmt.Fprintf(w, "               (npm comes with Node.js: %s)\n", nodeHint())
	}
	if len(extra) > 0 {
		fmt.Fprintf(w, "  %s %s: not installed (optional; docs/providers.md says how to add them)\n", stMuted.Render("--  "), strings.Join(extra, ", "))
	}
}

func printGit(w io.Writer) {
	a, b, err := orchestrator.GitVersion()
	switch {
	case err != nil:
		hint := "https://git-scm.com/"
		if runtime.GOOS == "windows" {
			hint = "winget install Git.Git"
		}
		fmt.Fprintf(w, "  %s git     not found: rw needs Git 2.38 or newer -> %s\n", stErr.Render("FAIL"), hint)
	case !orchestrator.SupportsMergeTree():
		fmt.Fprintf(w, "  %s git     %d.%d: parallel writing agents need 2.38 or newer\n", stRev.Render("warn"), a, b)
	default:
		fmt.Fprintf(w, "  %s git     %d.%d\n", stOK.Render("ok  "), a, b)
	}
}

// chooseProviders asks which providers to turn on: by default every ready
// one. A logged-out one may be typed in (log in before the first task).
func chooseProviders(a *asker, clis []cliStatus) []string {
	var ready, installed []string
	for _, s := range clis {
		if s.ready() {
			ready = append(ready, s.provider)
		}
		if s.path != "" {
			installed = append(installed, s.provider)
		}
	}
	q := "\nTurn on " + strings.Join(ready, ", ") + "?"
	if !a.interactive {
		a.yesNo(q, true)
		return ready
	}
	main := map[string]bool{}
	for _, p := range readyMain(clis) {
		main[p] = true
	}
	for i := 0; i < 3; i++ {
		ans := strings.ToLower(a.line(q + " [Y, or type the ones you want] "))
		if ans == "" || ans == "y" || ans == "yes" {
			return ready
		}
		if ans == "n" || ans == "no" {
			fmt.Fprintf(a.out, "Type the ones you want, e.g. %s\n", readyMain(clis)[0])
			continue
		}
		var picked []string
		bad := ""
		okMain := false
		for _, f := range strings.FieldsFunc(ans, func(r rune) bool { return r == ' ' || r == ',' }) {
			if f == "ollama" {
				f = "ollama-run"
			}
			if !contains(installed, f) {
				bad = f
				break
			}
			if !contains(picked, f) {
				picked = append(picked, f)
			}
			okMain = okMain || !config.Default().Providers[f].OnlyPreferred
		}
		switch {
		case bad != "":
			fmt.Fprintf(a.out, "%q is not an installed CLI here; choose from: %s\n", bad, strings.Join(installed, ", "))
		case !okMain:
			fmt.Fprintln(a.out, "A local model alone cannot run every job: add claude, codex or gemini.")
		default:
			for _, p := range picked {
				if !contains(ready, p) {
					fmt.Fprintf(a.out, "note: %s is not signed in yet; do that before its first task (see above)\n", p)
				}
			}
			return picked
		}
	}
	return ready
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// writeSetupConfig writes the commented default config with the providers
// turned on or off, after backing up a file already there. It returns the
// backup's path.
func writeSetupConfig(path string, on map[string]bool) (backup string, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// The config can hold API keys (provider env) and webhook URLs: a new
	// file is private, a replaced one keeps its mode, and so does its backup.
	mode := os.FileMode(0o600)
	st, err := os.Stat(path)
	switch {
	case err == nil:
		mode = st.Mode().Perm()
		old, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read %s to back it up: %w (nothing was changed)", path, err)
		}
		if backup, err = writeBackup(path, old, mode); err != nil {
			return "", fmt.Errorf("back up %s: %w (nothing was changed)", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("cannot read %s: %w (nothing was changed)", path, err)
	}
	data := config.WithEnabled(config.DefaultYAML(), on)
	tmp := path + ".new"
	os.Remove(tmp) // a leftover would keep its own mode
	if err := writeFileMode(tmp, data, mode, false); err != nil {
		return backup, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return backup, err
	}
	// Written by you through rw: what it sets applies without `rw trust`
	// even where it is a ./relayweft.yaml.
	if err := config.TrustLocal(path); err != nil {
		return backup, err
	}
	if _, _, err := config.Load(path); err != nil {
		// Put back what was there.
		if old, rerr := os.ReadFile(backup); backup != "" && rerr == nil {
			os.WriteFile(path, old, mode)
		} else if backup == "" {
			os.Remove(path)
		}
		return backup, fmt.Errorf("the new config does not load (%v), so it was not kept; please report this with `rw bugreport`", err)
	}
	return backup, nil
}

// writeBackup writes data to a new <path>.bak-<time> file that did not
// exist before, so a second replace in the same second never overwrites
// the backup of the original.
func writeBackup(path string, data []byte, mode os.FileMode) (string, error) {
	base := path + ".bak-" + time.Now().Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		err := writeFileMode(name, data, mode, true)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return name, err
	}
	return "", fmt.Errorf("too many backups named %s-*", base)
}

// writeFileMode writes data to name with mode; with excl, name must not
// exist yet.
func writeFileMode(name string, data []byte, mode os.FileMode, excl bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if excl {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(name, flags, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	return os.Chmod(name, mode) // the umask may have narrowed it
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// setupChecks saves the repo's checks to its .relayweft.yaml, as
// rw init --repo does (without offer, it only names them).
func setupChecks(a *asker, w io.Writer, root string, offer bool) {
	fmt.Fprintln(w, "\n"+stRouter.Bold(true).Render("This repo's checks"))
	if root == "" {
		fmt.Fprintln(w, "  not in a git repo: nothing to detect (run rw in a repo; `rw init --repo` saves its checks)")
		return
	}
	if p := filepath.Join(root, config.RepoFileName); fileExists(p) {
		fmt.Fprintf(w, "  %s already exists: kept\n", p)
		return
	}
	checks := config.DetectVerify(root)
	if len(checks) == 0 {
		fmt.Fprintf(w, "  no test command found in %s: set verify.commands later (`rw init --repo`)\n", root)
		return
	}
	fmt.Fprintf(w, "  found: %s\n  Agents may run them, and rw runs them before the final review.\n", strings.Join(checks, ", "))
	if !offer {
		// Started by rw run or rw web: a new untracked file could stop what
		// you asked for (rw run --issues needs a clean working tree).
		fmt.Fprintln(w, "  `rw init --repo` saves them to "+config.RepoFileName)
		return
	}
	if !a.yesNo("  Save them to "+filepath.Join(root, config.RepoFileName)+" (commit it to share)?", a.interactive) {
		fmt.Fprintln(w, "  not saved (`rw init --repo` saves them later)")
		return
	}
	p, err := writeRepoFile(root, checks)
	if err != nil {
		fmt.Fprintf(w, "  %s could not write %s: %v (rw init --repo tries again)\n", stErr.Render("FAIL"), p, err)
		return
	}
	fmt.Fprintf(w, "  wrote %s (trusted for you)\n", p)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The first tasks: short questions, so rw skips the planner and runs one
// read-only explorer step (the explorer's cheap model).
const (
	firstTaskRepo    = "Explain in five sentences what this repository does"
	firstTaskStarter = "Where is the low-stock threshold defined, and which functions use it?"
)

// setupFirstTask offers a read-only first task: in the current repo, or in
// the starter project in a temporary folder.
func setupFirstTask(a *asker, w io.Writer, cfgPath, root string) (time.Duration, error) {
	fmt.Fprintln(w, "\n"+stRouter.Bold(true).Render("First task"))
	cfg, _, err := config.Load(cfgPath)
	if err != nil {
		return 0, err
	}
	who := explorerRoute(cfg)
	task, where := firstTaskRepo, "explains this repo"
	if root == "" {
		where = "answers a question about a small sample project in a temporary folder"
		task = firstTaskStarter
	}
	fmt.Fprintf(w, "  A read-only task to see a whole run: %s %s (one short call, a little quota).\n", who, where)
	if !a.yesNo("  Run it now?", a.interactive) {
		fmt.Fprintln(w, "  skipped")
		return 0, nil
	}
	dir := root
	if dir == "" {
		d, err := os.MkdirTemp("", "rw-sample-")
		if err != nil {
			return 0, err
		}
		dir = d
		defer func() {
			if err := removeAllRetry(d); err != nil {
				fmt.Fprintf(w, "note: could not remove the sample project %s: %v\n", d, err)
			}
		}()
		if err := writeStarter(dir); err != nil {
			return 0, fmt.Errorf("create the sample project: %w", err)
		}
		fmt.Fprintf(w, "  sample project (removed afterwards): %s\n", dir)
	}
	fmt.Fprintf(w, "  $ rw run %q\n\n", task)
	t0 := time.Now()
	err = cmdRun([]string{"--config", cfgPath, "--dir", dir, task})
	return time.Since(t0), err
}

// explorerRoute names who runs a read-only step, e.g. "claude haiku".
func explorerRoute(cfg *config.Config) string {
	rc := cfg.Roles[event.RoleExplorer]
	usable := func(p string) bool {
		pc, ok := cfg.Providers[p]
		return ok && !pc.Disabled && rc.For(p).Model != ""
	}
	p := rc.Prefer
	if !usable(p) {
		p = ""
		for _, q := range cfg.Enabled() {
			if usable(q) && !cfg.Providers[q].OnlyPreferred {
				p = q
				break
			}
		}
	}
	if p == "" {
		return "the explorer"
	}
	return p + " " + rc.For(p).Model
}
