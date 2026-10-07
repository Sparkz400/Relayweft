package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// rw bench --replay-tests measures orchestrator.independent_tests without
// running any worker again: for each task it writes the independent tests
// once at the task's base, then runs them against every saved run of that
// task (its solution.patch), the known solution (tests.from) and the base.
// Each run's hidden check result and final review verdict are already in
// the results, so the table says which failures the tests catch that the
// reviews let through, and how often they fail correct work.

// replayRun is one saved run of a results file.
type replayRun struct {
	Task      string `json:"task"`
	Mode      string `json:"mode"`
	Rep       int    `json:"repetition"`
	Passed    bool   `json:"passed"`
	Status    string `json:"status"`
	Artifacts string `json:"artifacts"`
	// Set by the replay:
	From   string `json:"from"`   // the results file
	Review string `json:"review"` // the final reviews' verdicts in order (A approve, R reject), "" = none ran
	Patch  string `json:"-"`
}

// replaySubject is a tree the tests run against: a commit, plus a saved
// run's change.
type replaySubject struct {
	name, mode, hidden, review, patch, commit string
}

// replayRow is one requirement test run.
type replayRow struct {
	Task    string `json:"task"`
	Writer  int    `json:"writer"` // which writer repetition
	Subject string `json:"subject"`
	Mode    string `json:"mode,omitempty"`
	Hidden  string `json:"hidden"` // pass, fail, or "" (the base, which fails by construction)
	Review  string `json:"review,omitempty"`
	Req     string `json:"req"` // pass, fail, error
	Report  string `json:"report"`
}

func loadReplayRuns(files []string) ([]replayRun, error) {
	var out []replayRun
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var doc struct {
			Results []replayRun `json:"results"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, r := range doc.Results {
			if r.Status != "completed" || r.Artifacts == "" {
				continue
			}
			dir, err := filepath.Abs(filepath.Join(filepath.Dir(f), filepath.FromSlash(r.Artifacts)))
			if err != nil {
				return nil, err
			}
			r.Patch = filepath.Join(dir, "solution.patch") // git applies it in the workspace
			if _, err := os.Stat(r.Patch); err != nil {
				continue
			}
			r.From = filepath.Base(f)
			r.Review = finalReviews(filepath.Join(dir, "events.jsonl"))
			out = append(out, r)
		}
	}
	return out, nil
}

// finalReviews reads the final reviews' verdicts from a run's events.
func finalReviews(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		// event.Kind is written as a string but does not read one back.
		var e struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
			OK   bool   `json:"ok"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind != "checkpoint" || !strings.HasPrefix(e.Text, "final") {
			continue
		}
		b.WriteString(map[bool]string{true: "A", false: "R"}[e.OK])
	}
	return b.String()
}

func replayBenchTests(c common, ws *orchestrator.BenchWorkspace, store *config.Store, dir, head string, bf benchFile, tasks []benchTask, files []string, writers int, yes bool) error {
	runs, err := loadReplayRuns(files)
	if err != nil {
		return err
	}
	byTask := map[string][]replayRun{}
	for _, r := range runs {
		byTask[r.Task] = append(byTask[r.Task], r)
	}
	var todo []benchTask
	for _, t := range tasks {
		if len(byTask[t.Name]) > 0 {
			todo = append(todo, t)
		}
	}
	if len(todo) == 0 {
		return fmt.Errorf("no completed run with a solution.patch in %s matches the tasks", strings.Join(files, ", "))
	}
	total := 0
	for _, t := range todo {
		total += len(byTask[t.Name])
	}
	fmt.Printf("replaying independent tests: %d task(s) x %d writer(s) against %d saved run(s), plus each task's solution and base, in %s\n", len(todo), writers, total, ws.Path)
	if !yes {
		fmt.Print("Each writer is one agent on real provider quota. Start? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return nil
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg := store.Get()
	log, err := sessionlog.Open(cfg.SessionDir(), dir)
	if err != nil {
		log = nil
	}
	defer log.Close()
	tracker := limits.NewTracker()
	out := filepath.Join(dir, "bench-replay-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	fmt.Println("results:", out)
	var rows []replayRow
	type writerCost struct {
		Task    string         `json:"task"`
		Writer  int            `json:"writer"`
		OK      bool           `json:"ok"`
		Summary string         `json:"summary,omitempty"`
		Wall    int64          `json:"wall_ms"`
		Cost    event.TaskCost `json:"cost"`
		Files   []string       `json:"files"`
		Command string         `json:"command"`
	}
	var costs []writerCost
	save := func() error {
		data, _ := json.MarshalIndent(map[string]any{"commit": head, "results": files, "writers": costs, "rows": rows}, "", " ")
		if err := benchAtomicWrite(filepath.Join(out, "replay.json"), data); err != nil {
			return err
		}
		return benchAtomicWrite(filepath.Join(out, "replay.md"), []byte(replayReport(rows, len(costs))))
	}
	for _, t := range todo {
		runCfg := store.Get()
		runCfg.Orchestrator.IndependentTests = true
		if len(t.AgentChecks) > 0 {
			runCfg.Verify.Commands = append([]string(nil), t.AgentChecks...)
		}
		runCfg.Verify.Preflight = nil
		runStore := config.NewStore(runCfg, store.Path())
		events := make(chan event.Event, 4096)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for e := range events {
				printEvent(e, true)
			}
		}()
		orc := orchestrator.New(orchestrator.Options{Dir: ws.Path, Store: runStore, Runners: benchRunners, Tracker: tracker, Log: log,
			Events: events, ForceProvider: c.provider, Mode: "routed", Bench: t.Name, TaskIDPrefix: "replay-"})
		for w := 1; w <= writers && ctx.Err() == nil; w++ {
			fmt.Printf("\n%s · writer %d\n", t.Name, w)
			if note := prepareBenchRun(ctx, ws, head, bf.Setup, t); note != "" {
				close(events)
				<-done
				return fmt.Errorf("%s: %s", t.Name, note)
			}
			rt, res := orc.WriteRequirementTests(ctx, t.Prompt, "")
			wc := writerCost{Task: t.Name, Writer: w, OK: rt != nil, Summary: res.Summary, Wall: res.Duration.Milliseconds(), Cost: res.Cost}
			wdir := filepath.Join(out, t.Name, fmt.Sprintf("writer-%d", w))
			if rt != nil {
				wc.Files, wc.Command = rt.Paths(), rt.Command
				for _, p := range rt.Paths() {
					dst := filepath.Join(wdir, "files", filepath.FromSlash(p))
					os.MkdirAll(filepath.Dir(dst), 0o755)
					os.WriteFile(dst, rt.Files[p], 0o644)
				}
				os.WriteFile(filepath.Join(wdir, "requirements.txt"), []byte(rt.Requirements+"\n\nCOMMAND: "+rt.Command+"\n\nNOTES:\n"+strings.Join(rt.Notes, "\n")+"\n"), 0o644)
			}
			costs = append(costs, wc)
			if rt == nil {
				fmt.Println("  no tests:", res.Summary)
				if err := save(); err != nil {
					return err
				}
				continue
			}
			subjects := []replaySubject{{name: "base", commit: t.Base}}
			if t.Tests != nil {
				subjects = append(subjects, replaySubject{name: "solution", hidden: "pass", commit: t.Tests.From})
			}
			for _, r := range byTask[t.Name] {
				subjects = append(subjects, replaySubject{
					name: r.From + "/" + filepath.Base(filepath.Dir(r.Patch)), mode: r.Mode, hidden: map[bool]string{true: "pass", false: "fail"}[r.Passed],
					review: r.Review, patch: r.Patch, commit: t.Base})
			}
			for _, s := range subjects {
				if ctx.Err() != nil {
					break
				}
				row := replayRow{Task: t.Name, Writer: w, Subject: s.name, Mode: s.mode, Hidden: s.hidden, Review: s.review}
				err := ws.Reset(s.commit)
				if err == nil && s.patch != "" {
					err = ws.ApplyPatch(s.patch)
				}
				if err != nil {
					row.Req, row.Report = "error", err.Error()
				} else {
					ok, rep := orc.RunRequirementTests(ctx, rt)
					row.Req, row.Report = map[bool]string{true: "pass", false: "fail"}[ok], rep
				}
				rows = append(rows, row)
				fmt.Printf("  %-58s hidden=%-4s review=%-3s independent=%s\n", s.name, s.hidden, s.review, row.Req)
				if err := save(); err != nil {
					return err
				}
			}
		}
		close(events)
		<-done
	}
	fmt.Println("\n" + replayReport(rows, len(costs)))
	return ctx.Err()
}

// replayReport is the replay's summary: per subject, and how the
// independent tests compare with the hidden checks and the reviews.
func replayReport(rows []replayRow, writers int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Independent tests replay\n\n%d writer run(s), %d test run(s).\n\n", writers, len(rows))
	b.WriteString("| Task | Writer | Subject | Mode | Hidden | Final review | Independent tests |\n|---|---:|---|---|---|---|---|\n")
	sorted := append([]replayRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Task < sorted[j].Task })
	for _, r := range sorted {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s |\n", r.Task, r.Writer, r.Subject, r.Mode, dash(r.Hidden), dash(r.Review), r.Req)
	}
	var caught, missed, falseAlarm, okPass, approvedFail, approvedCaught, baseFail, bases int
	for _, r := range rows {
		switch {
		case r.Subject == "base":
			bases++
			if r.Req == "fail" {
				baseFail++
			}
			continue
		case r.Req == "error":
			continue
		case r.Hidden == "fail" && r.Req == "fail":
			caught++
		case r.Hidden == "fail":
			missed++
		case r.Req == "fail":
			falseAlarm++
		default:
			okPass++
		}
		if r.Hidden == "fail" && strings.HasSuffix(r.Review, "A") {
			approvedFail++
			if r.Req == "fail" {
				approvedCaught++
			}
		}
	}
	fmt.Fprintf(&b, "\n- Hidden check fails: independent tests caught %d of %d.\n", caught, caught+missed)
	fmt.Fprintf(&b, "- Hidden check fails and the last final review approved: caught %d of %d.\n", approvedCaught, approvedFail)
	fmt.Fprintf(&b, "- Hidden check passes (solutions and passing runs): independent tests failed %d of %d (false alarms).\n", falseAlarm, falseAlarm+okPass)
	fmt.Fprintf(&b, "- Base (nothing done yet): independent tests failed %d of %d.\n", baseFail, bases)
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splitList splits a comma-separated flag, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
