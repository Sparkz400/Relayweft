package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
	"github.com/sparkz400/relayweft/internal/sysload"
)

// runTask runs one task with scripted agents and returns its state and
// the session log folder.
func runTask(t *testing.T, cfg *config.Config, dir, task string, fn func(runner.Spec) runner.Result) (*orchestrator.TaskState, string) {
	t.Helper()
	set := runner.Set{event.Codex: scripted{event.Codex, fn}, event.Claude: scripted{event.Claude, fn}}
	logDir := t.TempDir()
	log, err := sessionlog.Open(logDir, dir)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan event.Event, 256)
	go func() {
		for range ch {
		}
	}()
	o := orchestrator.New(orchestrator.Options{
		Dir: dir, Store: config.NewStore(cfg, filepath.Join(t.TempDir(), "rw.yaml")), Mode: "routed",
		Runners: func(*config.Config) runner.Set { return set }, Tracker: limits.NewTracker(),
		Log: log, Events: ch, Load: func() sysload.Sample { return sysload.Sample{} },
	})
	o.Run(context.Background(), task)
	log.Close()
	close(ch)
	hist := orchestrator.History(dir, 1)
	if len(hist) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	st, err := orchestrator.LoadTask(hist[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, logDir
}

// seedHistory writes n earlier runs of a route, dated before the task, so
// its estimate comes from history.
func seedHistory(t *testing.T, logDir, cwd string, r Route, n int, tokens int64, before time.Time) {
	t.Helper()
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		rec := sessionlog.Record{Type: sessionlog.TypeAgentEnd, TS: before.Add(-time.Duration(i+1) * time.Hour), Session: "20000101-000000-aaaa",
			Cwd: cwd, TaskID: "old", Agent: r.Step, Step: r.Step, Kind: r.Kind, Attempt: 1, Role: r.Role, Provider: r.Provider, Model: r.Model, Effort: r.Effort,
			OK: sessionlog.Bool(true), Tokens: &event.TokenUsage{Input: tokens}, DurationMS: 1000}
		j, _ := json.Marshal(rec)
		b.Write(append(j, '\n'))
	}
	if err := os.WriteFile(filepath.Join(logDir, "20000101-000000-aaaa.jsonl"), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExplainPlannedTaskWithEscalation(t *testing.T) {
	isolate(t)
	dir := gitRepo(t)
	task := "Add a greeting to main.go, then write notes.md about it, with tests and documentation for everything please"
	greetTries := 0
	fn := func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: "```json\n" + `{"summary":"two steps","subtasks":[` +
				`{"id":"greet","title":"greeting","kind":"edit","prompt":"edit main.go","files":["main.go"]},` +
				`{"id":"notes","title":"notes","kind":"edit","prompt":"write notes","files":["notes.md"],"depends_on":["greet"]}]}` + "\n```"}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview), strings.Contains(s.Prompt, runner.MarkerErrorReview):
			return runner.Result{Final: `{"approve": true, "advice": "ok"}`}
		case s.StepID == "greet":
			greetTries++
			if greetTries <= 2 {
				return runner.Result{Err: errors.New("main.go:3: undefined: fmt"), Tokens: event.TokenUsage{Input: 900}}
			}
			os.WriteFile(filepath.Join(s.Dir, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi\") }\n"), 0o644)
			return runner.Result{Final: "added the greeting", Files: []string{"main.go"}, Tokens: event.TokenUsage{Input: 5000, Output: 700}}
		case s.StepID == "notes":
			os.WriteFile(filepath.Join(s.Dir, "notes.md"), []byte("# Notes\n"), 0o644)
			return runner.Result{Final: "wrote notes", Files: []string{"notes.md"}, Tokens: event.TokenUsage{Input: 3000, Output: 300}}
		}
		return runner.Result{Final: "done"}
	}
	cfg := config.Default()
	cfg.Orchestrator.Classic()
	cfg.Verify.Commands = []string{"git --version"}
	st, logDir := runTask(t, cfg, dir, task, fn)

	// Earlier runs of the notes step's route: its estimate has history.
	pre := Explain(st, Options{SessionDir: logDir})
	var notes Route
	for _, r := range pre.Runs {
		if r.Step == "notes" {
			notes = r.Route
		}
	}
	if notes.Provider == "" {
		t.Fatalf("no notes run: %+v", pre.Runs)
	}
	seedHistory(t, logDir, dir, notes, 4, 2000, st.Created)

	e := Explain(st, Options{SessionDir: logDir})
	if e.Shape != "planned" || !strings.Contains(e.ShapeWhy, "the planner made 2 steps") || e.Steps != 2 {
		t.Errorf("shape = %q: %q (%d steps)", e.Shape, e.ShapeWhy, e.Steps)
	}
	if len(e.Notes) > 0 {
		t.Errorf("notes: %v", e.Notes)
	}
	var esc *Escalation
	for i, x := range e.Escalations {
		if x.Step == "greet" && strings.Contains(x.Cause, "the same error twice") {
			esc = &e.Escalations[i]
		}
	}
	if esc == nil || !strings.Contains(esc.Cause, "undefined: fmt") || !strings.Contains(esc.What, "worker → worker_high") {
		t.Errorf("escalations = %+v", e.Escalations)
	}
	if len(e.Reviews) != 1 || e.Reviews[0].Outcome != "runs" || e.Reviews[0].Why == "" {
		t.Errorf("review choices = %+v", e.Reviews)
	}
	var seeded, defaults bool
	for _, r := range e.Runs {
		if r.Step == "notes" {
			seeded = r.Estimate.Source == sessionlog.SourceRepo && r.Estimate.Samples == 4 && r.Estimate.Tokens.Mid == 2000
		}
		if r.Step == "plan" {
			defaults = r.Estimate.Source == sessionlog.SourceNone && r.Estimate.Tokens.Mid > 0
		}
	}
	if !seeded || !defaults {
		t.Errorf("estimates: seeded %v, defaults %v: %+v", seeded, defaults, e.Runs)
	}
	if e.Tokens.Total() == 0 || e.EstTokens.Mid == 0 || e.NoHistory == 0 {
		t.Errorf("totals: est %+v actual %+v no history %d", e.EstTokens, e.Tokens, e.NoHistory)
	}
	if len(e.Providers) == 0 || e.Providers[0].Runs == 0 || len(e.Providers[0].Rules) == 0 {
		t.Errorf("providers = %+v", e.Providers)
	}
	var out bytes.Buffer
	if err := e.Text(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Why several agents (2 steps)", "planned: ", "error-repeats", "est ", "this repo, 4 runs", "Estimated vs actual", "Escalations", "greet attempt", "final review runs"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := json.Marshal(e); err != nil {
		t.Errorf("json: %v", err)
	}

	// The HTML report carries the same explanation.
	d := Build(st, Options{SessionDir: logDir})
	if d.Why == nil || len(d.RouteRows()) != len(d.Routes) || d.RouteRows()[0].Estimate.Source == "" {
		t.Fatalf("report explanation: %+v", d.Why)
	}
	out.Reset()
	if err := d.HTML(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Why it ran this way", "several agents (2 steps)", "the planner made 2 steps", "worker → worker_high", "undefined: fmt",
		"Estimated vs actual", `title="this repo"`, "final review <b>runs</b>", "<th class=\"n\">Est.</th>"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	out.Reset()
	if err := d.Markdown(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "## Why it ran this way") || !strings.Contains(out.String(), "escalation at greet attempt") {
		t.Errorf("Markdown lacks the explanation:\n%s", out.String())
	}
}

func TestExplainOneAgentTask(t *testing.T) {
	isolate(t)
	dir := gitRepo(t)
	fn := func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return runner.Result{Final: `{"approve": true}`}
		}
		os.WriteFile(filepath.Join(s.Dir, "main.go"), []byte("package main\n\n// Greeting.\nfunc main() {}\n"), 0o644)
		return runner.Result{Final: "done", Files: []string{"main.go"}, Tokens: event.TokenUsage{Input: 1000, Output: 100}}
	}
	cfg := config.Default()
	cfg.Orchestrator.ApprovePlan = false
	cfg.Verify.Commands = []string{"git --version"}
	st, logDir := runTask(t, cfg, dir, "Add a short comment above the main function in main.go explaining that it is the entry point of the tool", fn)
	e := Explain(st, Options{SessionDir: logDir})
	if e.Shape != "one agent" || !strings.Contains(e.ShapeWhy, "skipping the planner") {
		t.Errorf("shape = %q: %q", e.Shape, e.ShapeWhy)
	}
	for _, r := range e.Runs {
		if r.Role == event.RolePlanner {
			t.Errorf("a planner ran: %+v", r)
		}
	}
	if len(e.Reviews) != 1 || e.Reviews[0].Outcome != "skipped" {
		t.Errorf("review choices = %+v", e.Reviews)
	}
	var out bytes.Buffer
	e.Text(&out)
	if !strings.Contains(out.String(), "Why one agent") || !strings.Contains(out.String(), "none: every run kept its first route") {
		t.Errorf("text:\n%s", out.String())
	}
}

// The independent-test gate's choices (a dispute, tests made advisory)
// show with the task's other choices, in the text and in Markdown.
func TestExplainIndependentTestsGate(t *testing.T) {
	e := &Explanation{ID: "t1", Task: "x"}
	e.shape([]Choice{
		{What: sessionlog.ChoiceShape, Outcome: sessionlog.ChoiceOne, Why: "a small task"},
		{What: sessionlog.ChoiceReqTests, Outcome: sessionlog.ChoiceDisputed, Why: "TestRwReqB: stricter than the task", Round: 1},
		{What: sessionlog.ChoiceReqTests, Outcome: sessionlog.ChoiceAdvisory, Why: "TestRwReqB still fail after their fix round", Round: 2},
	}, &orchestrator.TaskState{})
	if len(e.ReqTests) != 2 {
		t.Fatalf("req test choices = %+v", e.ReqTests)
	}
	var out bytes.Buffer
	e.Text(&out)
	for _, want := range []string{"independent tests disputed (round 1): TestRwReqB: stricter than the task",
		"independent tests advisory (round 2): TestRwReqB still fail after their fix round"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, out.String())
		}
	}
}

func TestExplainEscalationCauses(t *testing.T) {
	e := &Explanation{Runs: []RunWhy{
		{Route: Route{Step: "a", Attempt: 1, Role: "worker", Provider: "claude", Model: "m", Rule: router.RuleDefault, Fallback: true, From: "codex",
			Reason: "codex at limit -> claude (default: default worker route)", Ran: true, OK: true}},
		{Route: Route{Step: "b", Attempt: 1, Role: "worker", Provider: "codex", Model: "big", Tier: "strong", Rule: router.RuleDefault,
			Reason: "default worker route; tier strong (difficulty 0.81: concurrency), worker_high route instead of worker", Ran: true, OK: true}},
		{Route: Route{Step: "c", Attempt: 1, Role: "worker_high", Provider: "codex", Model: "big", Rule: router.RuleLargeDiff, Reason: "sensitive: auth/; tier strong (difficulty 0.5)", Ran: true}},
		{Route: Route{Step: "c", Attempt: 2, Role: "worker_high", Provider: "codex", Model: "big", Rule: router.RuleLargeDiff, Reason: "sensitive: auth/", Ran: true, OK: true}},
		{Route: Route{Step: "d", Attempt: 1, Role: "explorer", Provider: "codex", Model: "m", Rule: router.RuleJudge, Judged: true, Reason: "judge picked explorer", Ran: true, OK: true}},
		{Route: Route{Step: "fix-1", Attempt: 1, Role: "worker", Provider: "codex", Model: "m", Rule: router.RuleDefault, Ran: true, OK: true}},
	}}
	recs := []sessionlog.Record{{Type: sessionlog.TypeLimit, Provider: "codex", Text: "usage limit reached, resets 14:00"}}
	e.escalations(recs, nil)
	want := map[string]string{
		"codex → claude":               "codex was at its usage limit or unavailable: usage limit reached, resets 14:00",
		"tier strong (codex:big)":      "tier strong (difficulty 0.81: concurrency), worker_high route instead of worker",
		"worker_high (codex:big)":      "sensitive: auth/",
		"retry (attempt 2)":            "the previous attempt failed",
		"judge set the role: explorer": "the rules were not confident about this step",
		"fix round 1":                  "predates recorded reasons",
	}
	got := map[string]string{}
	for _, x := range e.Escalations {
		got[x.What] = x.Cause
	}
	for what, cause := range want {
		if !strings.Contains(got[what], cause) {
			t.Errorf("%s: cause %q, want %q (all: %+v)", what, got[what], cause, e.Escalations)
		}
	}
	if len(e.Escalations) != len(want) {
		t.Errorf("escalations = %+v", e.Escalations)
	}
}

// Another task with the same text, started in the same folder while the
// first one was still listed as updated, is not a resume of it.
func TestTaskRecordsSkipAnotherTaskWithSameText(t *testing.T) {
	isolate(t)
	dir, logDir := t.TempDir(), t.TempDir()
	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	s1, s2 := created.Format("20060102-150405")+"-aaaa", created.Add(20*time.Second).Format("20060102-150405")+"-bbbb"
	st := &orchestrator.TaskState{ID: s1 + "-task-1", Task: "Explain main.go", Dir: dir, Status: "done", Created: created, Updated: created.Add(30 * time.Second)}
	write := func(session string, at time.Time, recs ...sessionlog.Record) {
		var b bytes.Buffer
		for _, r := range recs {
			r.Session, r.TaskID, r.Cwd, r.TS = session, "task-1", dir, at
			j, _ := json.Marshal(r)
			b.Write(append(j, '\n'))
		}
		os.WriteFile(filepath.Join(logDir, session+".jsonl"), b.Bytes(), 0o644)
	}
	start := sessionlog.Record{Type: sessionlog.TypeTask, Task: st.Task, Mode: "routed"}
	route := func(p string) sessionlog.Record {
		return sessionlog.Record{Type: sessionlog.TypeDecision, Agent: "explore", Step: "explore", Attempt: 1, Role: "explorer", Provider: p, Model: "m", Rule: "read-only"}
	}
	write(s1, created, start, route("codex"))
	write(s2, created.Add(20*time.Second), start, route("ollama"))

	// Without a state of its own, the second session looks like a resume.
	if recs, parts := taskRecords(logDir, st); parts != 2 || len(recs) != 4 {
		t.Fatalf("resume: %d parts, %d records", parts, len(recs))
	}
	// With one, it is another task.
	cfgDir, _ := os.UserConfigDir()
	tasks := filepath.Join(cfgDir, "relayweft", "tasks")
	os.MkdirAll(tasks, 0o755)
	other, _ := json.Marshal(orchestrator.TaskState{ID: s2 + "-task-1", Task: st.Task, Dir: dir, Status: "done", Created: created.Add(20 * time.Second)})
	if err := os.WriteFile(filepath.Join(tasks, s2+"-task-1.json"), other, 0o644); err != nil {
		t.Fatal(err)
	}
	e := Explain(st, Options{SessionDir: logDir})
	if len(e.Runs) != 1 || e.Runs[0].Provider != "codex" {
		t.Errorf("runs = %+v", e.Runs)
	}
}
