package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
	"gopkg.in/yaml.v3"
)

const benchExample = `# rw bench: compare Relayweft (routed) with single agents on YOUR tasks.
#
# Every run starts in a fresh repository outside your repo that holds only
# HEAD's files, as one commit without the history, so your working tree is
# never touched. Commit first: uncommitted changes are not part of the runs.
# Each run uses real quota.
#
# A run passes when its check command exits with 0. In a task with tests
# (tests: {from: <commit>, files: [...]}), {tests} in the check stands for
# its test files and {test_dirs} for their folders: "flutter test {tests}",
# "go test {test_dirs}".

modes:
  - routed                          # Relayweft with your relayweft.yaml routes
  # - routed-nohandoff              # the same without the context hand-off (repo map, notes), to measure it
  # - routed-bestof                 # every writing step as best of N (routing.best_of), to measure it
  # - routed:worker=claude:sonnet:medium   # routed with a role on another route: evidence for learned routes
  - single:codex:gpt-6.1-sol:high   # one agent, no planning or review
  - single:claude:opus:high

setup: ""        # optional command run before every run, e.g. "npm ci" (ignored files are kept between runs)
timeout: 30m     # per run, including the check
learn: false     # true: update this repo's learned routes from the results when the bench ends

tasks:
  - name: example-fix
    prompt: |
      Describe a real change here, the way you would ask for it.
    check: "go test ./..."          # "npm test", "pytest -q", ...
`

// benchRunners builds the agents for bench runs (tests swap in fakes).
var benchRunners = runner.New

type benchFile struct {
	Modes   []string        `yaml:"modes"`
	Setup   string          `yaml:"setup,omitempty"`
	Timeout config.Duration `yaml:"timeout"`
	// Learn updates the repo's learned routes when the bench ends
	// (benchlearn.go).
	Learn bool        `yaml:"learn,omitempty"`
	Tasks []benchTask `yaml:"tasks"`
}

type benchTask struct {
	Name   string `yaml:"name"`
	Prompt string `yaml:"prompt"`
	Check  string `yaml:"check"`
	// Base is the commit the run starts from (default: HEAD); history
	// tasks start from the parent of the commit they come from.
	Base  string      `yaml:"base,omitempty"`
	Tests *benchTests `yaml:"tests,omitempty"`
}

// benchTests are a history task's test files. Before the check they are
// set to their content at From (so editing or deleting them cannot pass
// it); Visible also puts them in place before the agent starts.
type benchTests struct {
	From    string   `yaml:"from"`
	Files   []string `yaml:"files,flow"`
	Visible bool     `yaml:"visible"`
}

type benchResult struct {
	task, mode string
	passed     bool
	agentOK    bool
	wall       time.Duration
	cost       event.TaskCost
	note       string
}

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("rw bench", flag.ExitOnError)
	var c common
	c.register(fs)
	file := fs.String("file", "bench.yaml", "benchmark task file")
	initFile := fs.Bool("init", false, "write an example bench.yaml and exit")
	starter := fs.String("starter", "", "create the starter set (a small Python repo with 5 tasks) in this new directory and exit")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	only := fs.String("only", "", "comma-separated task names to run")
	learnFlag := fs.Bool("learn", false, "update this repo's learned routes from the results when the bench ends (default: the file's learn setting)")
	noLearn := fs.Bool("no-learn", false, "do not update the learned routes, even if the file sets learn: true")
	fromHistory := fs.Bool("from-history", false, "write bench-history.yaml (or --file) with tasks made from past multi-file commits of this repo, and exit")
	var ho historyOpts
	fs.IntVar(&ho.count, "count", 10, "--from-history: number of tasks")
	fs.IntVar(&ho.scan, "scan", 300, "--from-history: how many recent commits to look at")
	fs.StringVar(&ho.check, "check", "", "--from-history: check command (default: verify.commands, else detected from the build files)")
	fs.StringVar(&ho.setup, "setup", "", "--from-history: setup command run before every check and run, e.g. \"npm ci\"")
	fs.IntVar(&ho.lim.minFiles, "min-files", 2, "--from-history: minimum code files a commit changed (tests, docs and lock files do not count)")
	fs.IntVar(&ho.lim.maxFiles, "max-files", 15, "--from-history: maximum files a commit changed")
	fs.IntVar(&ho.lim.maxLines, "max-lines", 800, "--from-history: maximum changed lines outside tests and lock files")
	fs.BoolVar(&ho.ownTests, "own-tests", false, "--from-history: each task's check runs only the commit's test files (go test, flutter/dart test, pytest, jest, vitest, npm test; or put {tests} or {test_dirs} in --check)")
	fs.BoolVar(&ho.hidden, "hidden-tests", false, "--from-history: keep the commit's tests from the agents until the check (default: in place from the start)")
	fs.BoolVar(&ho.noValidate, "no-validate", false, "--from-history: skip running the check on each commit and its parent")
	fs.DurationVar(&ho.timeout, "check-timeout", 15*time.Minute, "--from-history: time limit per validation check")
	parseFlags(fs, args)

	if *starter != "" {
		if py := starterPython(); !onPath(py) {
			fmt.Printf("note: %s is not on PATH; the starter checks need Python 3\n", py)
		}
		if err := writeStarter(*starter); err != nil {
			return err
		}
		fmt.Printf("wrote the starter set to %s\nnext: cd %s && rw bench\n", *starter, *starter)
		return nil
	}
	if *fromHistory {
		ho.out = "bench-history.yaml"
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "file" {
				ho.out = *file
			}
		})
		if ho.count < 1 || ho.scan < 1 {
			return fmt.Errorf("--count and --scan must be at least 1")
		}
		return benchFromHistory(c, ho)
	}
	if *initFile {
		if _, err := os.Stat(*file); err == nil {
			return fmt.Errorf("%s exists", *file)
		}
		if err := os.WriteFile(*file, []byte(benchExample), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s - edit the tasks, commit, then run `rw bench`\n", *file)
		return nil
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("%v (create one with `rw bench --init`)", err)
	}
	var bf benchFile
	if err := yaml.Unmarshal(data, &bf); err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}
	if len(bf.Modes) == 0 {
		bf.Modes = []string{"routed"}
	}
	if bf.Timeout == 0 {
		bf.Timeout = config.Duration(30 * time.Minute)
	}
	if *learnFlag && *noLearn {
		return fmt.Errorf("give --learn or --no-learn, not both")
	}
	learn := (bf.Learn || *learnFlag) && !*noLearn
	var modes []benchMode
	for _, m := range bf.Modes {
		bm, err := parseBenchMode(m)
		if err != nil {
			return err
		}
		modes = append(modes, bm)
	}
	want := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	tasks := bf.Tasks[:0]
	for _, t := range bf.Tasks {
		if t.Prompt == "" || t.Check == "" {
			return fmt.Errorf("task %q needs a prompt and a check", t.Name)
		}
		if usesTestsPlaceholder(t.Check) && (t.Tests == nil || len(t.Tests.Files) == 0) {
			return fmt.Errorf("task %q: the check uses %s or %s, but the task has no tests.files", t.Name, phTests, phTestDirs)
		}
		if len(want) == 0 || want[t.Name] {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		return fmt.Errorf("no tasks to run")
	}

	store, dir, err := c.setup()
	if err != nil {
		return err
	}
	ws, err := orchestrator.NewBenchWorkspace(dir)
	if err != nil {
		return err
	}
	head, err := ws.Head()
	if err != nil {
		return fmt.Errorf("the repo needs at least one commit: %w", err)
	}
	history := 0
	for i := range tasks {
		t := &tasks[i]
		if t.Base != "" {
			sha, err := ws.Resolve(t.Base)
			if err != nil {
				return fmt.Errorf("task %q: base %q is not a commit of this repo", t.Name, t.Base)
			}
			t.Base = sha
			history++
		}
		if t.Tests != nil {
			sha, err := ws.Resolve(t.Tests.From)
			if err != nil {
				return fmt.Errorf("task %q: tests.from %q is not a commit of this repo", t.Name, t.Tests.From)
			}
			t.Tests.From = sha
		}
	}
	if ws.Dirty() {
		fmt.Println("note: your working tree has uncommitted changes; the bench runs on HEAD without them.")
	}
	if bad := unlearnable(store.Get(), modes); len(bad) > 0 {
		fmt.Printf("note: %s: not a configured, usable route, so learned routes never pick it\n", strings.Join(bad, ", "))
	}
	unlock, err := ws.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	runs := len(tasks) * len(modes)
	from := head[:min(10, len(head))]
	if history > 0 {
		from += fmt.Sprintf(" (%d task(s) from their own base commit)", history)
	}
	fmt.Printf("%d task(s) x %d mode(s) = %d runs from %s, in %s\n", len(tasks), len(modes), runs, from, ws.Path)
	if learn {
		fmt.Println("afterwards the results update this repo's learned routes (--no-learn skips that)")
	}
	if !*yes {
		fmt.Print("This uses real provider quota. Start? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return nil
		}
	}

	_ = proc.Guard()
	cfg := store.Get()
	log, err := sessionlog.Open(cfg.SessionDir(), dir)
	if err != nil {
		log = nil
	}
	defer log.Close()
	tracker := limits.NewTracker()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	// The normal end cancels ctx too, which must not print the Ctrl+C notice.
	var closing atomic.Bool
	watched := make(chan struct{})
	go func() { watchInterrupt(ctx, stop, &closing, os.Stderr); close(watched) }()
	defer func() { closing.Store(true); stop(); <-watched }()

	var results []benchResult
	n := 0
	for _, t := range tasks {
		for _, m := range modes {
			if ctx.Err() != nil {
				break
			}
			n++
			fmt.Printf("\n[%d/%d] %s · %s\n", n, runs, t.Name, m.name)
			r := benchResult{task: t.Name, mode: m.name}
			rctx, cancel := context.WithTimeout(ctx, bf.Timeout.D())
			if note := prepareBenchRun(rctx, ws, head, bf.Setup, t); note != "" {
				r.note = note
				cancel()
				results = append(results, r)
				fmt.Println("  ", r.note)
				continue
			}
			events := make(chan event.Event, 4096)
			printed := make(chan struct{})
			go func() {
				defer close(printed)
				for e := range events {
					printEvent(e, true)
				}
			}()
			// Same routes without the context hand-off, or a route variant.
			runStore, err := m.store(store)
			if err != nil {
				cancel()
				close(events)
				<-printed
				return err
			}
			orc := orchestrator.New(orchestrator.Options{
				Dir: ws.Path, Store: runStore, Runners: benchRunners, Tracker: tracker, Log: log,
				Events: events, ForceProvider: c.provider, Mode: map[bool]string{true: "routed", false: "single"}[m.provider == ""],
				Bench: t.Name, TaskIDPrefix: fmt.Sprintf("bench%d-", n),
			})
			var res orchestrator.TaskResult
			if m.provider == "" {
				res = orc.Run(rctx, t.Prompt)
			} else {
				res = orc.RunSingle(rctx, t.Prompt, m.provider, m.route)
			}
			close(events)
			<-printed
			r.agentOK, r.wall, r.cost = res.OK, res.Duration, res.Cost
			if err := rctx.Err(); err != nil {
				// Cancelled or out of time: the check cannot tell anything.
				cancel()
				r.note = map[bool]string{true: "timed out", false: "cancelled"}[err == context.DeadlineExceeded]
				results = append(results, r)
				fmt.Println("  =>", r.note)
				continue
			}
			ok, out := benchCheck(rctx, ws, t, runStore.Get())
			cancel()
			r.passed = ok
			if !ok {
				r.note = "check failed: " + lastLine(out)
			}
			log.Write(sessionlog.Record{Type: "bench", Task: t.Prompt, Bench: t.Name, Mode: m.name,
				Passed: sessionlog.Bool(ok), DurationMS: res.Duration.Milliseconds(), Cost: &r.cost, Text: r.note})
			mark := "PASS"
			if !ok {
				mark = "FAIL"
			}
			fmt.Printf("  => %s in %s · %s\n", mark, res.Duration.Round(time.Second), res.Cost.Summary())
			results = append(results, r)
		}
	}
	report := benchReport(results, modes2names(bf.Modes))
	fmt.Println("\n" + report)
	out := "bench-results-" + time.Now().Format("20060102-150405") + ".md"
	hdr := "Commit " + head + "\n"
	for _, t := range tasks {
		if t.Base != "" {
			hdr += fmt.Sprintf("- %s starts at %s", t.Name, t.Base)
			if t.Tests != nil {
				hdr += ", tests from " + t.Tests.From
			}
			hdr += "\n"
		}
	}
	if err := os.WriteFile(out, []byte("# rw bench results\n\n"+hdr+"\n```\n"+report+"```\n"), 0o644); err == nil {
		fmt.Println("saved", out)
	}
	diag.Logf("bench finished: %d runs", len(results))
	switch {
	case !learn:
	case ctx.Err() != nil:
		fmt.Println("learned routes: not updated, the bench was cancelled (`rw tune --apply` learns from the runs so far)")
	default:
		fmt.Println()
		if err := benchLearn(os.Stdout, store, dir); err != nil {
			fmt.Fprintln(os.Stderr, "learned routes: not updated:", err)
		}
	}
	return nil
}

func modes2names(m []string) []string { return m }

// prepareBenchRun gives a run its clean start: the task's base commit (HEAD
// when it has none), the setup command and visible tests. It returns why
// the run cannot start, or "".
func prepareBenchRun(ctx context.Context, ws *orchestrator.BenchWorkspace, head, setup string, t benchTask) string {
	base := head
	if t.Base != "" {
		base = t.Base
	}
	if err := ws.Reset(base); err != nil {
		return "workspace: " + err.Error()
	}
	if setup != "" {
		if ok, out := shell(ctx, ws.Path, setup); !ok {
			return "setup failed: " + lastLine(out)
		}
	}
	if t.Tests != nil && t.Tests.Visible {
		if err := ws.RestoreFiles(t.Tests.From, t.Tests.Files); err != nil {
			return "tests: " + err.Error()
		}
	}
	return ""
}

// benchCheck runs a task's check, with its tests set to the reference
// commit's version first and its placeholders filled in. With cfg, after
// agents wrote the code, it runs in the sandbox when agents write in one
// (orchestrator.RunCheck); nil runs it here (a commit of your history).
func benchCheck(ctx context.Context, ws *orchestrator.BenchWorkspace, t benchTask, cfg *config.Config) (bool, string) {
	if t.Tests != nil {
		if err := ws.RestoreFiles(t.Tests.From, t.Tests.Files); err != nil {
			return false, "restoring the tests: " + err.Error()
		}
	}
	check, err := expandTests(t.Check, ws.Path, t.Tests)
	if err != nil {
		return false, err.Error()
	}
	if cfg != nil {
		return orchestrator.RunCheck(ctx, cfg, ws.Path, check)
	}
	return shell(ctx, ws.Path, check)
}

// benchReport renders per-task rows and per-mode totals.
func benchReport(rs []benchResult, modes []string) string {
	var b bytes.Buffer
	// One token column per provider: codex and claude, and any other that ran.
	used := map[string]bool{event.Codex: true, event.Claude: true}
	for _, r := range rs {
		for p := range r.cost.PerProvider {
			used[p] = true
		}
	}
	cols := event.ProvidersOf(used)
	heads := make([]string, len(cols))
	for i, p := range cols {
		heads[i] = strings.ToUpper(p) + " TOK"
	}
	toks := func(per func(string) int64) string {
		out := make([]string, len(cols))
		for i, p := range cols {
			out[i] = event.HumanTokens(per(p))
		}
		return strings.Join(out, "\t")
	}
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tMODE\tCHECK\tWALL\t"+strings.Join(heads, "\t")+"\tNOTE")
	for _, r := range rs {
		mark := "pass"
		switch {
		case r.note == "cancelled" || r.note == "timed out":
			mark = "-"
		case !r.passed:
			mark = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.task, r.mode, mark, r.wall.Round(time.Second),
			toks(func(p string) int64 { return r.cost.PerProvider[p].Total() }), oneLine(r.note, 60))
	}
	tw.Flush()
	b.WriteString("\n")
	tw = tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODE\tPASSED\tAVG WALL\t"+strings.Join(heads, "\t")+"\t≈$ API-EQUIV")
	for _, m := range modes {
		var pass, total int
		var wall time.Duration
		per := map[string]int64{}
		var usd float64
		for _, r := range rs {
			if r.mode != m || r.note == "cancelled" {
				continue
			}
			total++
			if r.passed {
				pass++
			}
			wall += r.wall
			for p, u := range r.cost.PerProvider {
				per[p] += u.Total()
			}
			usd += r.cost.CostUSD
		}
		if total == 0 {
			continue
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%.2f\n", m, pass, total, (wall / time.Duration(total)).Round(time.Second),
			toks(func(p string) int64 { return per[p] }), usd)
	}
	tw.Flush()
	return b.String()
}

// shell runs a command line in dir and reports success and combined output.
func shell(ctx context.Context, dir, line string) (bool, string) {
	cmd := proc.Shell(ctx, line)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	diag.Logf("bench shell %q in %s: err=%v", line, dir, err)
	return err == nil, string(out)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return oneLine(lines[len(lines)-1], 120)
}
