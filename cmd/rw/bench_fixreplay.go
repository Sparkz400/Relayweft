package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// rw bench --replay-fix measures the fix round that failing independent
// tests start, without running any worker again. It takes a --replay-tests
// result and, for every saved run (and known solution) whose independent
// tests failed, runs the task from the run's change: the change is the
// task's one step, the same writer's tests are its independent tests, and
// the checks, the final review and the fix rounds run as in a routed-tests
// task under orchestrator.independent_tests_gate. Then the hidden check
// scores the result. A fix agent starts fresh: the saved runs' sessions
// are gone, where a live task continues the worker's session.

// fixReplayRow is one replayed subject.
type fixReplayRow struct {
	Task    string `json:"task"`
	Writer  int    `json:"writer"`
	Subject string `json:"subject"`
	Mode    string `json:"mode,omitempty"`
	Review  string `json:"review,omitempty"` // the saved run's final reviews
	// Hidden and Req are the hidden check and the independent tests on
	// the change before the task, as rw sees it here; ...After at its end.
	Hidden      string         `json:"hidden"`
	Req         string         `json:"req"`
	HiddenAfter string         `json:"hidden_after"`
	ReqAfter    string         `json:"req_after"`
	FixRounds   int            `json:"fix_rounds"`
	OK          bool           `json:"ok"`
	Summary     string         `json:"summary"`
	Wall        int64          `json:"wall_ms"`
	Cost        event.TaskCost `json:"cost"`
	Artifacts   string         `json:"artifacts"`
	Note        string         `json:"note,omitempty"`
}

// loadSavedReqTests reads a writer's tests as --replay-tests saved them.
func loadSavedReqTests(wdir string) (*orchestrator.ReqTests, error) {
	rt := &orchestrator.ReqTests{Files: map[string][]byte{}, Provider: event.Claude}
	files := filepath.Join(wdir, "files")
	err := filepath.WalkDir(files, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(files, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		rt.Files[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(rt.Files) == 0 {
		return nil, fmt.Errorf("%s: no test files", wdir)
	}
	req, err := os.ReadFile(filepath.Join(wdir, "requirements.txt"))
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(req), "\r\n", "\n")
	reqs, rest, _ := strings.Cut(text, "\n\nCOMMAND: ")
	rt.Requirements = strings.TrimSpace(reqs)
	cmd, _, _ := strings.Cut(rest, "\n")
	rt.Command = strings.TrimSpace(cmd)
	return rt, nil
}

// solutionPatch writes the known solution's change (base..from in the
// source repo) to path. It has the solution's own test changes, the hidden
// tests among them, as the replay of the tests ran the solution: without
// them its older tests fail the public checks.
func solutionPatch(dir string, t benchTask, path string) error {
	out, err := exec.Command("git", proc.GitArgs("-C", dir, "diff", "--binary", "--no-color", "--no-ext-diff", "--no-textconv", t.Base, t.Tests.From, "--")...).Output()
	if err != nil {
		return fmt.Errorf("git diff for the solution: %w", err)
	}
	return os.WriteFile(path, out, 0o600)
}

func replayBenchFix(c common, ws *orchestrator.BenchWorkspace, store *config.Store, dir, head string, bf benchFile, tasks []benchTask, replayDir string, subjects []string, gate string, yes bool) error {
	data, err := os.ReadFile(filepath.Join(replayDir, "replay.json"))
	if err != nil {
		return err
	}
	var rep struct {
		Results []string    `json:"results"`
		Rows    []replayRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		return fmt.Errorf("%s: %w", replayDir, err)
	}
	var files []string
	for _, f := range rep.Results {
		if !filepath.IsAbs(f) {
			f = filepath.Join(dir, f)
		}
		files = append(files, f)
	}
	runs, err := loadReplayRuns(files)
	if err != nil {
		return err
	}
	patches := map[string]string{}
	for _, r := range runs {
		patches[r.From+"/"+filepath.Base(filepath.Dir(r.Patch))] = r.Patch
	}
	byName := map[string]benchTask{}
	for _, t := range tasks {
		byName[t.Name] = t
	}
	var todo []replayRow
	for _, r := range rep.Rows {
		if r.Subject == "base" || r.Req != "fail" {
			continue
		}
		if _, ok := byName[r.Task]; !ok {
			continue
		}
		if len(subjects) > 0 && !matchesAny(r.Task+"/"+r.Subject, subjects) {
			continue
		}
		todo = append(todo, r)
	}
	if len(todo) == 0 {
		return fmt.Errorf("no subject in %s failed its independent tests (with --only and --replay-subjects)", replayDir)
	}
	fmt.Printf("replaying the fix round: %d change(s) whose independent tests failed, independent_tests_gate: %s, in %s\n", len(todo), gate, ws.Path)
	for _, r := range todo {
		fmt.Printf("  %s · %s\n", r.Task, r.Subject)
	}
	if !yes {
		fmt.Print("Each runs a task's verify, review and fix rounds on real provider quota. Start? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return nil
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log, err := sessionlog.Open(store.Get().SessionDir(), dir)
	if err != nil {
		log = nil
	}
	defer log.Close()
	tracker := limits.NewTracker()
	out := filepath.Join(dir, "bench-fixreplay-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	fmt.Println("results:", out)
	var rows []fixReplayRow
	save := func() error {
		data, _ := json.MarshalIndent(map[string]any{"commit": head, "replay": replayDir, "gate": gate, "rows": rows}, "", " ")
		if err := benchAtomicWrite(filepath.Join(out, "fixreplay.json"), data); err != nil {
			return err
		}
		return benchAtomicWrite(filepath.Join(out, "fixreplay.md"), []byte(fixReplayReport(rows, gate)))
	}
	for n, s := range todo {
		if ctx.Err() != nil {
			break
		}
		t := byName[s.Task]
		fmt.Printf("\n[%d/%d] %s · %s\n", n+1, len(todo), s.Task, s.Subject)
		art := filepath.Join(out, fmt.Sprintf("run-%03d", n+1))
		if err := os.MkdirAll(art, 0o755); err != nil {
			return err
		}
		row := fixReplayRow{Task: s.Task, Writer: s.Writer, Subject: s.Subject, Mode: s.Mode, Review: s.Review, Artifacts: filepath.ToSlash(art)}
		fail := func(note string) error {
			row.Note = note
			rows = append(rows, row)
			fmt.Println("  =>", note)
			return save()
		}
		rt, err := loadSavedReqTests(filepath.Join(replayDir, s.Task, fmt.Sprintf("writer-%d", s.Writer)))
		if err != nil {
			return err
		}
		patch := patches[s.Subject]
		if s.Subject == "solution" {
			if t.Tests == nil {
				return fail("the task has no tests.from")
			}
			patch = filepath.Join(art, "change.patch")
			if err := solutionPatch(dir, t, patch); err != nil {
				return fail(err.Error())
			}
		}
		if patch == "" {
			if err := fail("no saved solution.patch for this subject"); err != nil {
				return err
			}
			continue
		}
		// The starting point as rw sees it here: hidden check and tests.
		if bf.Fair {
			if err := ws.Clear(); err != nil {
				return err
			}
		}
		if note := prepareBenchRun(ctx, ws, head, bf.Setup, t); note != "" {
			if err := fail(note); err != nil {
				return err
			}
			continue
		}
		if err := ws.ApplyPatch(patch); err != nil {
			if err := fail("applying the change: " + err.Error()); err != nil {
				return err
			}
			continue
		}
		hiddenOK, hiddenOut := benchCheck(ctx, ws, t, nil)
		row.Hidden = passFail(hiddenOK)
		if err := os.WriteFile(filepath.Join(art, "check-before.txt"), []byte(hiddenOut), 0o600); err != nil {
			return fmt.Errorf("save starting check: %w", err)
		}

		// The task, from the base with the change as its step.
		if note := prepareBenchRun(ctx, ws, head, bf.Setup, t); note != "" {
			if err := fail(note); err != nil {
				return err
			}
			continue
		}
		base, err := ws.StartCommit()
		if err != nil {
			return err
		}
		runStore, err := benchMode{name: "routed-tests", reqTests: true}.store(store)
		if err != nil {
			return err
		}
		runCfg := runStore.Get()
		runCfg.Orchestrator.IndependentTestsGate = gate
		if len(t.AgentChecks) > 0 {
			runCfg.Verify.Preflight = append([]string(nil), t.AgentChecks...)
			runCfg.Verify.Commands = append([]string(nil), t.AgentChecks...)
		}
		runStore = config.NewStore(runCfg, runStore.Path())
		events := make(chan event.Event, 4096)
		eventFile, err := os.Create(filepath.Join(art, "events.jsonl"))
		if err != nil {
			return err
		}
		printed := make(chan struct{})
		var eventErr error // read only after printed closes
		fixes := 0
		go func() {
			defer close(printed)
			enc := json.NewEncoder(eventFile)
			for e := range events {
				if err := enc.Encode(e); err != nil && eventErr == nil {
					eventErr = err
				}
				if e.Kind == event.Phase && e.Text == "fix" {
					fixes++
				}
				printEvent(e, true)
			}
			if err := eventFile.Sync(); err != nil && eventErr == nil {
				eventErr = err
			}
			if err := eventFile.Close(); err != nil && eventErr == nil {
				eventErr = err
			}
		}()
		orc := orchestrator.New(orchestrator.Options{Dir: ws.Path, Store: runStore, Runners: benchRunners, Tracker: tracker, Log: log,
			Events: events, ForceProvider: c.provider, Mode: "routed", Bench: t.Name, TaskIDPrefix: fmt.Sprintf("fixreplay%d-", n+1)})
		rctx, cancel := context.WithTimeout(ctx, bf.Timeout.D())
		// The orchestrator emits until its last test run below.
		closed := false
		done := func() {
			if !closed {
				closed = true
				close(events)
				<-printed
				row.FixRounds = fixes
			}
		}
		res := orc.RunWith(rctx, t.Prompt, orchestrator.TaskOptions{Unattended: true, Replay: &orchestrator.ReplayWork{
			Apply:   func() error { return ws.ApplyPatch(patch) },
			Tests:   rt,
			Summary: "Implemented the task; the change is in the diff.",
		}})
		row.OK, row.Summary, row.Wall, row.Cost = res.OK, res.Summary, res.Duration.Milliseconds(), res.Cost
		if err := ws.Evidence(base, art); err != nil {
			cancel()
			done()
			return fmt.Errorf("save the result (workspace kept): %w", err)
		}
		if err := rctx.Err(); err != nil {
			cancel()
			done()
			if eventErr != nil {
				return fmt.Errorf("save replay events: %w", eventErr)
			}
			if err := fail(map[bool]string{true: "timed out", false: "cancelled"}[err == context.DeadlineExceeded]); err != nil {
				return err
			}
			continue
		}
		reqOK, reqRep := orc.RunRequirementTests(rctx, rt)
		row.ReqAfter = passFail(reqOK)
		if err := os.WriteFile(filepath.Join(art, "req-after.txt"), []byte(reqRep), 0o600); err != nil {
			cancel()
			done()
			return fmt.Errorf("save requirement check: %w", err)
		}
		hiddenOK, hiddenOut = benchCheck(rctx, ws, t, runStore.Get())
		cancel()
		done()
		if eventErr != nil {
			return fmt.Errorf("save replay events: %w", eventErr)
		}
		row.HiddenAfter = passFail(hiddenOK)
		if err := os.WriteFile(filepath.Join(art, "check.txt"), []byte(hiddenOut), 0o600); err != nil {
			return fmt.Errorf("save hidden check: %w", err)
		}
		row.Req = "fail" // as in the replay; the task's first round says it again (events.jsonl)
		rows = append(rows, row)
		fmt.Printf("  => hidden %s -> %s, independent tests fail -> %s, %d fix round(s), %s\n", row.Hidden, row.HiddenAfter, row.ReqAfter, row.FixRounds, oneLine(row.Summary, 160))
		if err := save(); err != nil {
			return err
		}
	}
	fmt.Println("\n" + fixReplayReport(rows, gate))
	return ctx.Err()
}

func passFail(ok bool) string { return map[bool]string{true: "pass", false: "fail"}[ok] }

func matchesAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// fixReplayReport is the replay's table and what the fix rounds turned.
func fixReplayReport(rows []fixReplayRow, gate string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Fix round replay (independent_tests_gate: %s)\n\n", gate)
	b.WriteString("| Task | Subject | Mode | Hidden before | Fix rounds | Independent tests after | Hidden after | Task ok | Fresh tokens | Note |\n|---|---|---|---|---:|---|---|---|---:|---|\n")
	sorted := append([]fixReplayRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Task < sorted[j].Task })
	var turned, fixedReq, broke, keptOK, fails, oks int
	for _, r := range sorted {
		var fresh int64
		for _, u := range r.Cost.PerProvider {
			fresh += u.Total()
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %s | %s | %v | %s | %s |\n", r.Task, r.Subject, dash(r.Mode), dash(r.Hidden), r.FixRounds,
			dash(r.ReqAfter), dash(r.HiddenAfter), r.OK, event.HumanTokens(fresh), oneLine(r.Note, 80))
		if r.Note != "" {
			continue
		}
		if r.ReqAfter == "pass" {
			fixedReq++
		}
		switch r.Hidden {
		case "fail":
			fails++
			if r.HiddenAfter == "pass" {
				turned++
			}
		case "pass":
			oks++
			if r.HiddenAfter == "pass" {
				keptOK++
			} else {
				broke++
			}
		}
	}
	fmt.Fprintf(&b, "\n- Failing changes the independent tests caught: the fix rounds turned %d of %d into a hidden pass.\n", turned, fails)
	fmt.Fprintf(&b, "- Correct changes the independent tests failed: %d of %d still pass the hidden check after the fix rounds (%d broken).\n", keptOK, oks, broke)
	fmt.Fprintf(&b, "- The independent tests pass at the end in %d of %d.\n", fixedReq, fails+oks)
	return b.String()
}
