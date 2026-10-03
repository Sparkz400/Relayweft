// Command sy is Switchyard: a terminal app that routes coding work between
// the Codex CLI and Claude Code, using your subscriptions only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
	"github.com/sparkz400/switchyard/internal/tui"
)

var version = "dev"

const demoTask = "Make the parser keep trailing empty fields and add a --strict flag that rejects malformed lines, with tests and README docs"

func main() {
	args := os.Args[1:]
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	var err error
	switch sub {
	case "":
		err = cmdTUI(args)
	case "run":
		err = cmdRun(args)
	case "stats":
		err = cmdStats(args)
	case "doctor":
		err = cmdDoctor(args)
	case "models":
		err = cmdModels(args)
	case "init":
		err = cmdInit(args)
	case "version", "--version":
		fmt.Println("switchyard", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, errTaskFailed) {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "sy:", err)
		os.Exit(1)
	}
}

var errTaskFailed = errors.New("task failed")

func usage() {
	fmt.Print(`Switchyard - route coding work between Codex and Claude subscriptions

Usage:
  sy [flags]                 start the TUI in the current directory
  sy --demo                  the full animated TUI driven by fake agents
  sy run [flags] "task"      run one task headless and print events
  sy run --single codex:gpt-6.1-sol:high "task"   single-agent baseline run
  sy stats [--here] [--since 7d]   usage per model and route, routed vs baseline
  sy models [--refresh] [--all]    show routes and catalogs; refresh Codex catalog
  sy doctor                  check CLIs, versions, git and terminal
  sy init [--global] [--force] [--print]   write the commented default config
  sy version

Flags (TUI and run):
  --config <file>            config file (default ./switchyard.yaml, then user config dir)
  --dir <path>               project directory (default current directory)
  --route role=provider:model[:effort]   override a route (repeatable)
  --prefer role=codex|claude|other|auto  override a role's provider choice (repeatable; role "all" ok)
  --provider codex|claude    force every role onto one provider
  --threads <n>  --no-parallel  --no-review  --judge
  --ascii | --unicode        force the ASCII or Unicode theme
  --demo  --speed <x>        demo mode (TUI only); speed multiplies animation pace

Roles: planner, worker, worker_high, explorer, researcher, reviewer, judge
`)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type common struct {
	configPath string
	dir        string
	routes     multiFlag
	prefers    multiFlag
	provider   string
	threads    int
	noParallel bool
	noReview   bool
	judge      bool
	ascii      bool
	unicode    bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.configPath, "config", "", "config file")
	fs.StringVar(&c.dir, "dir", "", "project directory")
	fs.Var(&c.routes, "route", "role=provider:model[:effort] (repeatable)")
	fs.Var(&c.prefers, "prefer", "role=codex|claude|other|auto (repeatable)")
	fs.StringVar(&c.provider, "provider", "", "force every role onto codex or claude")
	fs.IntVar(&c.threads, "threads", 0, "max parallel agents")
	fs.BoolVar(&c.noParallel, "no-parallel", false, "run subtasks one at a time")
	fs.BoolVar(&c.noReview, "no-review", false, "skip all review checkpoints")
	fs.BoolVar(&c.judge, "judge", false, "enable the LLM judge for unclear routing")
	fs.BoolVar(&c.ascii, "ascii", false, "ASCII theme")
	fs.BoolVar(&c.unicode, "unicode", false, "Unicode theme")
}

// setup loads config and applies flag overrides.
func (c *common) setup() (*config.Store, string, error) {
	cfg, path, err := config.Load(c.configPath)
	if err != nil {
		return nil, "", err
	}
	store := config.NewStore(cfg, path)
	for _, r := range c.routes {
		role, spec, ok := strings.Cut(r, "=")
		if !ok {
			return nil, "", fmt.Errorf("--route %q: want role=provider:model[:effort]", r)
		}
		prov, route, err := config.ParseRouteSpec(spec)
		if err != nil {
			return nil, "", err
		}
		if err := store.SetRoute(role, prov, route); err != nil {
			return nil, "", err
		}
		// Naming a route on the command line means "use this one".
		if err := store.SetPrefer(role, prov); err != nil {
			return nil, "", err
		}
	}
	for _, p := range c.prefers {
		role, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, "", fmt.Errorf("--prefer %q: want role=codex|claude|other|auto", p)
		}
		roles := []string{role}
		if role == "all" {
			roles = event.Roles
		}
		for _, r := range roles {
			if err := store.SetPrefer(r, v); err != nil {
				return nil, "", err
			}
		}
	}
	if c.provider != "" && c.provider != event.Codex && c.provider != event.Claude {
		return nil, "", fmt.Errorf("--provider must be codex or claude")
	}
	err = store.Update(func(cf *config.Config) error {
		if c.threads > 0 {
			cf.Orchestrator.MaxThreads = c.threads
		}
		if c.noParallel {
			cf.Orchestrator.Parallel = false
		}
		if c.noReview {
			cf.Orchestrator.ReviewBeforePlan = false
			cf.Orchestrator.ReviewOnRepeatError = false
			cf.Orchestrator.ReviewBeforeDone = false
		}
		if c.judge {
			cf.Routing.Judge = true
		}
		if c.ascii {
			cf.Theme = "ascii"
		}
		if c.unicode {
			cf.Theme = "unicode"
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	dir := c.dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, "", err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, "", fmt.Errorf("--dir %s is not a directory", dir)
	}
	return store, dir, nil
}

func cmdTUI(args []string) error {
	fs := flag.NewFlagSet("sy", flag.ExitOnError)
	fs.Usage = usage
	var c common
	c.register(fs)
	demo := fs.Bool("demo", false, "demo mode with fake agents")
	speed := fs.Float64("speed", 1, "demo speed multiplier")
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (use `sy run \"task\"` for headless runs)", fs.Arg(0))
	}
	store, dir, err := c.setup()
	if err != nil {
		return err
	}
	_ = proc.Guard()
	cfg := store.Get()

	events := make(chan event.Event, 4096)
	var log *sessionlog.Writer
	runners := runner.New
	mode := "routed"
	if *demo {
		mode = "demo"
		fake := runner.NewFakeSet(*speed)
		runners = func(*config.Config) runner.Set { return fake }
	} else if log, err = sessionlog.Open(cfg.SessionDir(), dir); err != nil {
		fmt.Fprintln(os.Stderr, "warning: session log disabled:", err)
		log = nil
	}
	defer log.Close()
	orc := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: store, Runners: runners, Tracker: limits.NewTracker(), Log: log,
		Events: events, ForceProvider: c.provider, NoGit: *demo, Mode: mode,
	})
	m := tui.New(tui.Options{
		Orc: orc, Events: events, Dir: dir, Theme: tui.NewTheme(cfg.Theme), Demo: *demo,
		DemoTask: map[bool]string{true: demoTask}[*demo], SessionLog: log.Path(), Version: version,
	})
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, runErr := p.Run()
	// Keep draining events so the orchestrator can finish shutting down.
	go func() {
		for range events {
		}
	}()
	m.Shutdown()
	return runErr
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("sy run", flag.ExitOnError)
	fs.Usage = usage
	var c common
	c.register(fs)
	single := fs.String("single", "", "provider:model[:effort] - one agent, no planning or review (baseline)")
	quiet := fs.Bool("quiet", false, "only print routing, results and errors")
	fs.Parse(args)
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return errors.New(`usage: sy run [flags] "task"`)
	}
	store, dir, err := c.setup()
	if err != nil {
		return err
	}
	_ = proc.Guard()
	cfg := store.Get()
	log, err := sessionlog.Open(cfg.SessionDir(), dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: session log disabled:", err)
		log = nil
	}
	defer log.Close()
	events := make(chan event.Event, 4096)
	orc := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: store, Runners: runner.New, Tracker: limits.NewTracker(), Log: log,
		Events: events, ForceProvider: c.provider,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	printed := make(chan struct{})
	go func() {
		defer close(printed)
		for e := range events {
			printEvent(e, *quiet)
		}
	}()
	var res orchestrator.TaskResult
	if *single != "" {
		prov, route, err := config.ParseRouteSpec(*single)
		if err != nil {
			return err
		}
		res = orc.RunSingle(ctx, task, prov, route)
	} else {
		res = orc.Run(ctx, task)
	}
	close(events)
	<-printed
	status := "OK"
	if !res.OK {
		status = "FAILED"
	}
	fmt.Printf("\n%s in %s · %s tokens · %s\n", status, res.Duration.Round(time.Second), sessionlog.Human(res.Tokens.Total()), res.Summary)
	if log != nil {
		fmt.Println("session log:", log.Path())
	}
	if !res.OK {
		return errTaskFailed
	}
	return nil
}

var (
	stCodex  = lipgloss.NewStyle().Foreground(lipgloss.Color("#10A37F"))
	stClaude = lipgloss.NewStyle().Foreground(lipgloss.Color("#D97757"))
	stMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A8F98"))
	stRouter = lipgloss.NewStyle().Foreground(lipgloss.Color("#4FC1FF"))
	stRev    = lipgloss.NewStyle().Foreground(lipgloss.Color("#B48EFF"))
	stErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5C5C"))
	stOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("#3DDC84"))
)

func printEvent(e event.Event, quiet bool) {
	ts := stMuted.Render(e.Timestamp.Format("15:04:05"))
	who := e.AgentID
	if who == "" {
		who = "sy"
	}
	ps := stMuted
	switch e.Provider {
	case event.Codex:
		ps = stCodex
	case event.Claude:
		ps = stClaude
	}
	tag := ps.Render(fmt.Sprintf("%-10s", who))
	line := func(st lipgloss.Style, s string) { fmt.Printf("%s %s %s\n", ts, tag, st.Render(oneLine(s, 220))) }
	switch e.Kind {
	case event.Route:
		d := e.Decision
		line(stRouter, fmt.Sprintf("route %s -> %s [%s %.2f] %s", d.Role, d.Label(), d.Rule, d.Confidence, d.Reason))
	case event.Phase:
		line(stRouter, "== "+e.Text+" ==")
	case event.Checkpoint:
		v := "approved"
		if !e.OK {
			v = "changes requested"
		}
		line(stRev, "review "+v+": "+e.Text)
	case event.Merge:
		st := stOK
		if !e.OK {
			st = stErr
		}
		line(st, "merge "+e.Text)
	case event.Error, event.LimitHit:
		line(stErr, e.Kind.String()+": "+e.Text)
	case event.ProviderState, event.Log, event.TaskStart:
		line(stMuted, e.Text)
	case event.Done:
		st := stOK
		if !e.OK {
			st = stErr
		}
		line(st, fmt.Sprintf("done (%s tok): %s", sessionlog.Human(e.Tokens.Total()), e.Text))
	case event.TaskDone, event.AgentQueued, event.Quota, event.Usage:
		// summarized elsewhere
	case event.Started:
		if !quiet {
			line(ps, "started "+e.Role+" "+e.Model)
		}
	case event.ToolCall, event.FileEdit, event.Message, event.Thinking:
		if !quiet {
			st := stMuted
			if e.Kind == event.FileEdit {
				st = lipgloss.NewStyle()
			}
			line(st, e.Kind.String()+" "+e.Text)
		}
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("sy stats", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	here := fs.Bool("here", false, "only sessions run in the current directory")
	since := fs.String("since", "", "only records newer than this (e.g. 24h, 7d)")
	fs.Parse(args)
	cfg, _, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	recs, err := sessionlog.ReadDir(cfg.SessionDir())
	if err != nil {
		return err
	}
	var f sessionlog.Filter
	if *here {
		f.Cwd, _ = os.Getwd()
	}
	if *since != "" {
		d, err := parseSince(*since)
		if err != nil {
			return err
		}
		f.Since = time.Now().Add(-d)
	}
	sessionlog.Aggregate(recs, f).Print(os.Stdout)
	fmt.Println("\nlogs:", cfg.SessionDir())
	return nil
}

func parseSince(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err != nil {
			return 0, fmt.Errorf("--since %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("sy doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	fs.Parse(args)
	cfg, path, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ok := func(b bool) string {
		if b {
			return stOK.Render("ok  ")
		}
		return stErr.Render("FAIL")
	}
	warn := stRev.Render("warn")
	problems := 0
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("%s config      %s\n", ok(true), path)
	} else {
		fmt.Printf("%s config      built-in defaults (run `sy init` to create %s)\n", stMuted.Render("info"), path)
	}
	for _, p := range event.Providers {
		pc := cfg.Providers[p]
		bin, err := proc.Resolve(pc.Command)
		if err != nil {
			problems++
			fmt.Printf("%s %-11s %q not found on PATH", ok(false), p, pc.Command)
			if p == event.Codex {
				fmt.Print(" - npm install -g @openai/codex, then `codex login`")
			} else {
				fmt.Print(" - npm install -g @anthropic-ai/claude-code, then run `claude` once to log in")
			}
			fmt.Println()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, verr := exec.CommandContext(ctx, bin, "--version").Output()
		cancel()
		v := strings.TrimSpace(string(out))
		if verr != nil {
			fmt.Printf("%s %-11s %s (version check failed: %v)\n", warn, p, bin, verr)
			continue
		}
		mark := ok(true)
		note := ""
		if pc.TestedVersion != "" && v != pc.TestedVersion {
			mark = warn
			note = stMuted.Render(fmt.Sprintf("  (tested with %s; output format may differ)", pc.TestedVersion))
		}
		fmt.Printf("%s %-11s %s  %s%s\n", mark, p, v, bin, note)
		if p == event.Codex {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			out, err := exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
			cancel()
			s := oneLine(string(out), 100)
			if err != nil {
				fmt.Printf("%s codex login %s - run `codex login`\n", warn, s)
			} else {
				fmt.Printf("%s codex login %s\n", ok(true), s)
			}
		}
	}
	if a, b, err := orchestrator.GitVersion(); err != nil {
		fmt.Printf("%s git         not found - worktrees and diffs disabled\n", warn)
	} else if orchestrator.SupportsMergeTree() {
		fmt.Printf("%s git         %d.%d (parallel worktrees supported)\n", ok(true), a, b)
	} else {
		fmt.Printf("%s git         %d.%d - parallel writing agents need git >= 2.38\n", warn, a, b)
	}
	theme := tui.NewTheme(cfg.Theme)
	tname := "unicode"
	if theme.ASCII {
		tname = "ascii (legacy console detected; use Windows Terminal for the full look)"
	}
	fmt.Printf("%s terminal    theme %s\n", ok(true), tname)
	dir := cfg.SessionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		problems++
		fmt.Printf("%s logs        %s: %v\n", ok(false), dir, err)
	} else {
		fmt.Printf("%s logs        %s\n", ok(true), dir)
	}
	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	fmt.Println("\nReady. Try `sy --demo` for the look, then `sy` in a git repo.")
	return nil
}

func cmdModels(args []string) error {
	fs := flag.NewFlagSet("sy models", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	refresh := fs.Bool("refresh", false, "read the Codex catalog from `codex debug models` and save it")
	all := fs.Bool("all", false, "with --refresh: include hidden models")
	fs.Parse(args)
	cfg, path, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *refresh {
		n, err := refreshCodex(cfg, *all)
		if err != nil {
			return err
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		fmt.Printf("Codex catalog refreshed: %d models, saved to %s\n\n", n, path)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tPREFER\tCODEX\tCLAUDE")
	for _, r := range event.Roles {
		rc := cfg.Roles[r]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r, rc.Prefer, routeStr(rc.Codex), routeStr(rc.Claude))
	}
	tw.Flush()
	for _, p := range event.Providers {
		pc := cfg.Providers[p]
		fmt.Printf("\n%s models (efforts: %s)\n", p, strings.Join(pc.Efforts, ", "))
		for _, m := range pc.Models {
			fmt.Printf("  %-22s %-18s %s\n", m.ID, m.Label, m.Tier)
		}
	}
	fmt.Println("\nChange with: sy --route worker=claude:sonnet:high · /route in the TUI · m (model picker)")
	return nil
}

func routeStr(r config.Route) string {
	if r.Model == "" {
		return "-"
	}
	if r.Effort == "" {
		return r.Model
	}
	return r.Model + " @" + r.Effort
}

// refreshCodex replaces the Codex catalog with `codex debug models`.
func refreshCodex(cfg *config.Config, all bool) (int, error) {
	pc := cfg.Providers[event.Codex]
	bin, err := proc.Resolve(pc.Command)
	if err != nil {
		return 0, fmt.Errorf("codex not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "debug", "models").Output()
	if err != nil {
		return 0, fmt.Errorf("codex debug models: %w", err)
	}
	var cat struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
			Levels      []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &cat); err != nil {
		return 0, fmt.Errorf("unexpected `codex debug models` output: %w", err)
	}
	var models []config.ModelInfo
	efforts := map[string]bool{}
	for _, m := range cat.Models {
		if m.Slug == "" || (!all && m.Visibility != "" && m.Visibility != "list") {
			continue
		}
		tier := "standard"
		switch {
		case strings.Contains(m.Slug, "luna") || strings.Contains(m.Slug, "mini"):
			tier = "fast"
		case strings.Contains(m.Slug, "sol") || strings.Contains(m.Slug, "astra") || strings.Contains(m.Slug, "pro"):
			tier = "strong"
		}
		models = append(models, config.ModelInfo{ID: m.Slug, Label: m.DisplayName, Tier: tier})
		for _, l := range m.Levels {
			efforts[l.Effort] = true
		}
	}
	if len(models) == 0 {
		return 0, errors.New("codex reported no models")
	}
	pc.Models = models
	order := []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
	var effs []string
	for _, e := range order {
		if efforts[e] {
			effs = append(effs, e)
			delete(efforts, e)
		}
	}
	var rest []string
	for e := range efforts {
		rest = append(rest, e)
	}
	sort.Strings(rest)
	pc.Efforts = append(effs, rest...)
	cfg.Providers[event.Codex] = pc
	return len(models), nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("sy init", flag.ExitOnError)
	global := fs.Bool("global", false, "write to the user config dir instead of ./switchyard.yaml")
	force := fs.Bool("force", false, "overwrite an existing file")
	print := fs.Bool("print", false, "print the default config instead of writing it")
	fs.Parse(args)
	if *print {
		os.Stdout.Write(config.DefaultYAML())
		return nil
	}
	path := config.FileName
	if *global {
		dir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		path = filepath.Join(dir, "switchyard", config.FileName)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s exists (use --force to overwrite)", path)
	}
	if err := os.WriteFile(path, config.DefaultYAML(), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}
