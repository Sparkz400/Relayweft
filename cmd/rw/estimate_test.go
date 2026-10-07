package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// captureStdout runs fn with os.Stdout going to a pipe and returns what it
// printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	got := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		got <- string(b)
	}()
	ferr := fn()
	os.Stdout = old
	w.Close()
	return <-got, ferr
}

// planOnly plans two edit steps; any other agent run is a test failure
// (and would write into the tree).
type planOnly struct{ t *testing.T }

func (x planOnly) Run(_ context.Context, s runner.Spec, _ func(event.Event)) runner.Result {
	if strings.Contains(s.Prompt, runner.MarkerPlan) && !strings.Contains(s.Prompt, runner.MarkerPlanReview) {
		b, _ := json.Marshal(map[string]any{"summary": "two edits", "subtasks": []map[string]any{
			{"id": "parser", "title": "fix the parser", "kind": "edit", "prompt": "fix it", "files": []string{"p.go"}},
			{"id": "docs", "title": "document it", "kind": "edit", "prompt": "docs", "depends_on": []string{"parser"}},
		}})
		return runner.Result{Final: "```json\n" + string(b) + "\n```", Tokens: event.TokenUsage{Input: 1200}}
	}
	x.t.Errorf("%s ran during --estimate", s.StepID)
	os.WriteFile(filepath.Join(s.Dir, "agent-was-here.txt"), []byte("x"), 0o644)
	return runner.Result{Final: "done"}
}

func TestRunEstimateLeavesTheTreeAlone(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# edited, not committed\n"), 0o644)
	headlessRunners = func(*config.Config) runner.Set {
		return runner.Set{event.Codex: planOnly{t}, event.Claude: planOnly{t}}
	}
	defer func() { headlessRunners = runner.New }()
	status, head := gitOut(t, dir, "status", "--porcelain"), gitOut(t, dir, "rev-parse", "HEAD")

	out, err := captureStdout(t, func() error {
		return cmdRun([]string{"--estimate", "--plan", "--quiet", "fix the parser so that it keeps trailing empty fields, then document the new behaviour"})
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Explicit planning estimates both workers; the optional final review is off.
	for _, want := range []string{"Plan: two edits", "1. [edit, auto] fix the parser", "~ worker on codex:gpt-6.1-sol:medium:",
		"Estimate: ~", "2 of 2 steps without history", "estimate only: nothing was run"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if s := gitOut(t, dir, "status", "--porcelain"); s != status {
		t.Fatalf("working tree changed:\n%s\nwas:\n%s", s, status)
	}
	if h := gitOut(t, dir, "rev-parse", "HEAD"); h != head {
		t.Fatal("HEAD moved")
	}
	if refs := gitOut(t, dir, "for-each-ref", "refs/relayweft"); refs != "" {
		t.Fatalf("undo refs written: %s", refs)
	}
	if hist := orchestrator.History(dir, 10); len(hist) != 0 {
		t.Fatalf("an estimate became a task: %+v", hist)
	}

	// --estimate takes exactly one task.
	if err := cmdRun([]string{"--estimate", "--single", "claude:sonnet", "a task"}); err == nil || !strings.Contains(err.Error(), "--estimate takes one task") {
		t.Fatalf("--estimate --single: %v", err)
	}
}

func TestTermApproverShowsEstimate(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	est := func(p orchestrator.Plan) orchestrator.PlanEstimate {
		calls++
		var e orchestrator.PlanEstimate
		for _, st := range p.Subtasks {
			role := st.Role
			if role == "" {
				role = event.RoleWorker
			}
			se := orchestrator.StepEstimate{StepID: st.ID, Role: role, Route: "claude:sonnet:medium"}
			se.Tokens = sessionlog.Spread{Low: 10_000, Mid: 20_000, High: 40_000}
			se.Seconds = sessionlog.Spread{Low: 60, Mid: 90, High: 120}
			se.USD = sessionlog.Spread{Low: 0.1, Mid: 0.2, High: 0.4}
			se.Source, se.Samples = sessionlog.SourceRepo, 6
			e.Steps = append(e.Steps, se)
			e.Tokens = e.Tokens.Add(se.Tokens)
			e.USD = e.USD.Add(se.USD)
			e.Seconds = e.Seconds.Add(se.Seconds)
		}
		e.Warnings = []string{"likely over today's cost: ~$0.40 ($0.20-$0.80), $0.10 left of $5.00"}
		return e
	}
	a := newTermApprover(strings.NewReader("r 2 worker_high\ny\n"), &out)
	p, ok := a.ApprovePlanEstimate(context.Background(), "task", testPlan(), est)
	if !ok || p.Subtasks[1].Role != event.RoleWorkerHigh {
		t.Fatalf("plan %+v ok=%v", p.Subtasks, ok)
	}
	s := out.String()
	if calls != 2 {
		t.Fatalf("estimated %d times, want once per showing of the plan", calls)
	}
	for _, want := range []string{
		"~ worker on claude:sonnet:medium: 20k tok (10k-40k) · 1m30s (1m0s-2m0s) · $0.20 ($0.10-$0.40) · this repo, 6 runs",
		"~ worker_high on claude:sonnet:medium:", "Estimate: ~40k tok (20k-80k) · 3m0s (2m0s-4m0s) · $0.40 ($0.20-$0.80)",
		"warning: likely over today's cost"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	// Without an estimate the plan looks as before.
	out.Reset()
	a = newTermApprover(strings.NewReader("y\n"), &out)
	if _, ok := a.ApprovePlan(context.Background(), "task", testPlan()); !ok || strings.Contains(out.String(), "Estimate") {
		t.Fatalf("plain approval:\n%s", out.String())
	}
}

// rw tune --apply learns from this repo's logs and prints the diff; the
// next run uses the route unless a flag sets the role; --learned shows it,
// --reset forgets it.
func TestTuneApplyLearnedReset(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	w, err := sessionlog.Open(config.Default().SessionDir(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		for _, r := range []struct {
			prov, model string
			ok          bool
		}{{event.Codex, "gpt-6.1-sol", i < 4}, {event.Claude, "sonnet", true}} {
			w.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TS: time.Now().Add(-time.Hour), TaskID: "t", Step: "s", Kind: "edit", Attempt: 1,
				Role: event.RoleWorker, Provider: r.prov, Model: r.model, Effort: "medium", OK: sessionlog.Bool(r.ok),
				Tokens: &event.TokenUsage{Input: 30_000}, DurationMS: 90_000})
		}
	}
	w.Close()

	out, err := captureStdout(t, func() error { return cmdTune(nil) })
	if err != nil || !strings.Contains(out, "`rw tune --apply` would change") || !strings.Contains(out, "worker: codex:gpt-6.1-sol:medium -> claude:sonnet:medium") {
		t.Fatalf("plain tune: %v\n%s", err, out)
	}
	if l, _ := config.LoadLearned(dir); len(l.Routes) != 0 {
		t.Fatal("plain rw tune saved learned routes")
	}
	out, err = captureStdout(t, func() error { return cmdTune([]string{"--apply"}) })
	for _, want := range []string{"worker: codex:gpt-6.1-sol:medium -> claude:sonnet:medium", "succeeded 100% of 10 runs vs 40% of 10",
		"ROUTE", "claude:sonnet:medium", "saved: ", "they apply from the next task on"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("--apply output lacks %q (%v):\n%s", want, err, out)
		}
	}
	out, err = captureStdout(t, func() error { return cmdTune([]string{"--learned"}) })
	if err != nil || !strings.Contains(out, "worker: claude:sonnet:medium\n") || !strings.Contains(out, "succeeded 100%") {
		t.Fatalf("--learned: %v\n%s", err, out)
	}

	var c common
	store, _, err := c.setup()
	if err != nil {
		t.Fatal(err)
	}
	if w := store.Get().Roles[event.RoleWorker]; w.Prefer != event.Claude || w.Claude.Model != "sonnet" {
		t.Fatalf("worker after --apply = %+v", w)
	}
	// A flag always wins over a learned route.
	c = common{routes: multiFlag{"worker=codex:gpt-6.1-sol:high"}}
	if store, _, err = c.setup(); err != nil {
		t.Fatal(err)
	}
	if w := store.Get().Roles[event.RoleWorker]; w.Prefer != event.Codex || w.Codex.Effort != "high" || store.Get().Learned[event.RoleWorker].Model != "" {
		t.Fatalf("worker with --route = %+v", w)
	}

	out, err = captureStdout(t, func() error { return cmdTune([]string{"--reset"}) })
	if err != nil || !strings.Contains(out, "forgot 1 learned route(s)") {
		t.Fatalf("--reset: %v\n%s", err, out)
	}
	out, _ = captureStdout(t, func() error { return cmdTune([]string{"--learned"}) })
	if !strings.Contains(out, "none yet") {
		t.Fatalf("after reset:\n%s", out)
	}
	if err := cmdTune([]string{"--apply", "--reset"}); err == nil {
		t.Fatal("--apply --reset accepted")
	}
}
