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
	"text/tabwriter"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
	"gopkg.in/yaml.v3"
)

const benchExample = `# sy bench: compare Switchyard (routed) with single agents on YOUR tasks.
#
# Every run starts from a clean checkout of HEAD in a separate worktree
# (outside your repo), so your working tree is never touched. Commit first:
# uncommitted changes are not part of the runs. Each run uses real quota.
#
# A run passes when its check command exits with 0.

modes:
  - routed                          # Switchyard with your switchyard.yaml routes
  # - routed-nohandoff              # the same without the context hand-off (repo map, notes), to measure it
  - single:codex:gpt-6.1-sol:high   # one agent, no planning or review
  - single:claude:opus:high

setup: ""        # optional command run before every run, e.g. "npm ci" (ignored files are kept between runs)
timeout: 30m     # per run, including the check

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
	Setup   string          `yaml:"setup"`
	Timeout config.Duration `yaml:"timeout"`
	Tasks   []struct {
		Name   string `yaml:"name"`
		Prompt string `yaml:"prompt"`
		Check  string `yaml:"check"`
	} `yaml:"tasks"`
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
	fs := flag.NewFlagSet("sy bench", flag.ExitOnError)
	var c common
	c.register(fs)
	file := fs.String("file", "bench.yaml", "benchmark task file")
	initFile := fs.Bool("init", false, "write an example bench.yaml and exit")
	starter := fs.String("starter", "", "create the starter set (a small Python repo with 5 tasks) in this new directory and exit")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	only := fs.String("only", "", "comma-separated task names to run")
	fs.Parse(args)

	if *starter != "" {
		if err := writeStarter(*starter); err != nil {
			return err
		}
		fmt.Printf("wrote the starter set to %s\nnext: cd %s && sy bench\n", *starter, *starter)
		return nil
	}
	if *initFile {
		if _, err := os.Stat(*file); err == nil {
			return fmt.Errorf("%s exists", *file)
		}
		if err := os.WriteFile(*file, []byte(benchExample), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s - edit the tasks, commit, then run `sy bench`\n", *file)
		return nil
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("%v (create one with `sy bench --init`)", err)
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
	type modeSpec struct {
		name, provider string
		route          config.Route
		noHandoff      bool
	}
	var modes []modeSpec
	for _, m := range bf.Modes {
		if m == "routed" || m == "routed-nohandoff" {
			modes = append(modes, modeSpec{name: m, noHandoff: m == "routed-nohandoff"})
			continue
		}
		spec, ok := strings.CutPrefix(m, "single:")
		if !ok {
			return fmt.Errorf("mode %q: want routed, routed-nohandoff or single:<provider>:<model>[:effort]", m)
		}
		prov, route, err := config.ParseRouteSpec(spec)
		if err != nil {
			return fmt.Errorf("mode %q: %w", m, err)
		}
		modes = append(modes, modeSpec{name: m, provider: prov, route: route})
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
	if ws.Dirty() {
		fmt.Println("note: your working tree has uncommitted changes; the bench runs on HEAD without them.")
	}
	unlock, err := ws.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	runs := len(tasks) * len(modes)
	fmt.Printf("%d task(s) x %d mode(s) = %d runs from %s, in %s\n", len(tasks), len(modes), runs, head[:min(10, len(head))], ws.Path)
	if !*yes {
		fmt.Print("This uses real Codex/Claude quota. Start? [y/N] ")
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
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
		fmt.Fprintln(os.Stderr, "\ncancelling the bench... (Ctrl+C again to force quit)")
	}()

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
			if err := ws.Reset(head); err != nil {
				r.note = "workspace: " + err.Error()
				results = append(results, r)
				fmt.Println("  ", r.note)
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, bf.Timeout.D())
			if bf.Setup != "" {
				if ok, out := shell(rctx, ws.Path, bf.Setup); !ok {
					r.note = "setup failed: " + lastLine(out)
					cancel()
					results = append(results, r)
					fmt.Println("  ", r.note)
					continue
				}
			}
			events := make(chan event.Event, 4096)
			printed := make(chan struct{})
			go func() {
				defer close(printed)
				for e := range events {
					printEvent(e, true)
				}
			}()
			runStore := store
			if m.noHandoff {
				// Same routes without the context hand-off, to measure it.
				cfgNo := store.Get()
				cfgNo.Orchestrator.Handoff = false
				runStore = config.NewStore(cfgNo, store.Path())
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
			ok, out := shell(rctx, ws.Path, t.Check)
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
	if err := os.WriteFile(out, []byte("# sy bench results\n\nCommit "+head+"\n\n```\n"+report+"```\n"), 0o644); err == nil {
		fmt.Println("saved", out)
	}
	diag.Logf("bench finished: %d runs", len(results))
	return nil
}

func modes2names(m []string) []string { return m }

// benchReport renders per-task rows and per-mode totals.
func benchReport(rs []benchResult, modes []string) string {
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tMODE\tCHECK\tWALL\tCODEX TOK\tCLAUDE TOK\tNOTE")
	for _, r := range rs {
		mark := "pass"
		switch {
		case r.note == "cancelled" || r.note == "timed out":
			mark = "-"
		case !r.passed:
			mark = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.task, r.mode, mark, r.wall.Round(time.Second),
			event.HumanTokens(r.cost.PerProvider[event.Codex].Total()), event.HumanTokens(r.cost.PerProvider[event.Claude].Total()), oneLine(r.note, 60))
	}
	tw.Flush()
	b.WriteString("\n")
	tw = tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODE\tPASSED\tAVG WALL\tCODEX TOK\tCLAUDE TOK\t≈$ API-EQUIV")
	for _, m := range modes {
		var pass, total int
		var wall time.Duration
		var cx, cl int64
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
			cx += r.cost.PerProvider[event.Codex].Total()
			cl += r.cost.PerProvider[event.Claude].Total()
			usd += r.cost.CostUSD
		}
		if total == 0 {
			continue
		}
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\t%.2f\n", m, pass, total, (wall / time.Duration(total)).Round(time.Second),
			event.HumanTokens(cx), event.HumanTokens(cl), usd)
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
