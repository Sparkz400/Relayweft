package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/dayplan"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func testPlanner(t *testing.T, tr *limits.Tracker, df dayFlags) *dayplan.Live {
	t.Helper()
	d, err := newDayPlanner(config.Default(), "", tr, df)
	if err != nil {
		t.Fatal(err)
	}
	d.ReadLogs = func(time.Time) []sessionlog.Record { return nil }
	return d
}

type fillCall struct {
	task    int
	lean    string
	strict  bool
	resumed bool
}

func TestRunFillWaitsResumesAndLeans(t *testing.T) {
	tr := limits.NewTracker()
	now := time.Now()
	// Codex's window is nearly spent for an hour; Claude is at its limit
	// for a moment.
	tr.SetQuota(event.Codex, event.QuotaInfo{Utilization: 0.95, Window: "5h", ResetsAt: now.Add(time.Hour)})
	tr.MarkLimited(event.Claude, now.Add(300*time.Millisecond))
	d := testPlanner(t, tr, dayFlags{})
	loadTask = func(id string) (*orchestrator.TaskState, error) {
		if id != "t1" {
			return nil, errors.New("no such task")
		}
		return &orchestrator.TaskState{ID: id, Task: "first"}, nil
	}
	defer func() { loadTask = orchestrator.LoadTask }()
	var calls []fillCall
	var out bytes.Buffer
	start := time.Now()
	ran, failed, left := runFill(context.Background(), &out, d, []string{"first", "second"}, func(i int, o orchestrator.TaskOptions) orchestrator.TaskResult {
		calls = append(calls, fillCall{i, o.Lean.Provider, o.Lean.Strict, o.Resume != nil})
		if len(calls) == 1 {
			// The first run hits Claude's limit again.
			o.Started("t1")
			tr.MarkLimited(event.Claude, time.Now().Add(200*time.Millisecond))
			return orchestrator.TaskResult{Summary: "every provider is at its usage limit"}
		}
		return orchestrator.TaskResult{OK: true}
	})
	if ran != 2 || failed != 0 || len(left) != 0 {
		t.Fatalf("ran %d, failed %d, left %v\n%s", ran, failed, left, out.String())
	}
	want := []fillCall{{0, event.Claude, true, false}, {0, event.Claude, true, true}, {1, event.Claude, true, false}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
	if el := time.Since(start); el < 450*time.Millisecond {
		t.Fatalf("finished after %s: it did not wait for the resets", el)
	}
	s := out.String()
	for _, w := range []string{"fill: nothing has room now; task 1 waits for claude's limit resets", "=== task 1/2 only on claude",
		"task 1 stopped at a usage limit; it resumes when a window has room", "=== task 2/2 only on claude"} {
		if !strings.Contains(s, w) {
			t.Errorf("output lacks %q:\n%s", w, s)
		}
	}
}

func TestRunFillLeavesWhatDoesNotFit(t *testing.T) {
	tr := limits.NewTracker()
	now := time.Now()
	tr.MarkLimited(event.Claude, now.Add(time.Hour))
	tr.MarkLimited(event.Codex, now.Add(2*time.Hour))
	d := testPlanner(t, tr, dayFlags{until: now.Add(30 * time.Minute).Format(time.RFC3339)})
	var out bytes.Buffer
	ran, failed, left := runFill(context.Background(), &out, d, []string{"a", "b"}, func(int, orchestrator.TaskOptions) orchestrator.TaskResult {
		t.Fatal("a task ran")
		return orchestrator.TaskResult{}
	})
	if ran != 0 || failed != 0 || len(left) != 2 || left[1].Task != 1 || !strings.Contains(left[0].Why, "claude is at its limit until") {
		t.Fatalf("ran %d failed %d left %+v", ran, failed, left)
	}
	// A real failure (no limit hit) is not retried.
	tr = limits.NewTracker()
	d = testPlanner(t, tr, dayFlags{})
	n := 0
	ran, failed, _ = runFill(context.Background(), &out, d, []string{"a"}, func(_ int, o orchestrator.TaskOptions) orchestrator.TaskResult {
		n++
		o.Started("x")
		return orchestrator.TaskResult{}
	})
	if n != 1 || ran != 1 || failed != 1 {
		t.Fatalf("runs %d ran %d failed %d", n, ran, failed)
	}
}

func TestPrintDayPlan(t *testing.T) {
	now := time.Date(2026, 10, 6, 23, 0, 0, 0, time.Local)
	provs := []dayplan.Provider{
		{Name: "codex", Used: 0.95, ResetsAt: now.Add(time.Hour), Note: "5h window 95% used, resets 00:00"},
		{Name: "claude", Capacity: 1_000_000, CapacitySamples: 4},
	}
	queue := []string{"fix the flaky test", "document the flag", "third"}
	h := dayplan.Learn(nil, "")
	tasks := []dayplan.Task{h.Task(queue[0]), h.Task(queue[1]), h.Task(queue[2])}
	o := dayplan.Options{Now: now, Until: now.Add(10 * time.Minute)}
	st := dayplan.State{Plan: dayplan.Make(tasks, provs, o), Providers: provs, Tasks: tasks, History: h, Options: o}
	var b bytes.Buffer
	st.Write(&b, queue)
	s := b.String()
	for _, w := range []string{
		"Day plan · 3 queued task(s) · now 23:00 · until 23:10",
		"codex   5h window 95% used, resets 00:00 · window size unknown: a task counts as 10%",
		"claude  no window reading yet: assumed unused · a window holds ~1.0M tokens (4 reading(s))",
		"a task: ~150k tokens, 8m0s (no finished tasks in the logs: a default)",
		"23:00  claude  1. fix the flaky test  · ~15% of the window · opens a window · only claude has room",
		"23:08  claude  2. document the flag",
		"not in this plan:\n  3. third - it would start after 23:10",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("output lacks %q:\n%s", w, s)
		}
	}
}

func TestRunFillFlags(t *testing.T) {
	isolate(t)
	chdir(t, gitInit(t))
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--until", "07:00", "a task"}, "--until and --fresh-at go with --fill"},
		{[]string{"--fill", "--single", "claude:sonnet", "a task"}, "--fill runs a task file or a task"},
		{[]string{"--fill", "--estimate", "a task"}, "--estimate takes one task"},
	} {
		if err := cmdRun(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("rw run %v: %v, want %q", c.args, err, c.want)
		}
	}
}

// provLog records which provider ran each agent.
type provLog struct {
	mu  sync.Mutex
	ran []string
}

type recRunner struct {
	runner.Runner
	name string
	log  *provLog
}

func (r recRunner) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	r.log.mu.Lock()
	r.log.ran = append(r.log.ran, r.name+":"+s.StepID)
	r.log.mu.Unlock()
	return r.Runner.Run(ctx, s, emit)
}

func TestRunFileFillEndToEnd(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	// The logs say Codex's window is nearly spent: the night goes to Claude,
	// planner and review included.
	logDir := config.Default().SessionDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	q := event.QuotaInfo{Utilization: 0.95, Window: "5h", ResetsAt: time.Now().Add(3 * time.Hour)}
	line, _ := json.Marshal(sessionlog.Record{Type: sessionlog.TypeQuota, TS: time.Now(), Provider: event.Codex, Quota: &q})
	os.WriteFile(filepath.Join(logDir, "old.jsonl"), append(line, '\n'), 0o644)
	os.WriteFile(filepath.Join(dir, "tasks.txt"), []byte("add a line to the readme\n"), 0o644)
	var log provLog
	headlessRunners = func(*config.Config) runner.Set {
		return runner.Set{
			event.Codex:  recRunner{&runner.Fake{Provider: event.Codex}, event.Codex, &log},
			event.Claude: recRunner{&runner.Fake{Provider: event.Claude}, event.Claude, &log},
		}
	}
	defer func() { headlessRunners = runner.New }()
	out, err := captureStdout(t, func() error { return cmdRun([]string{"--file", "tasks.txt", "--fill", "--quiet", "--allow-sleep"}) })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, w := range []string{"Day plan · 1 queued task(s)", "codex   5h window 95% used", "=== task 1/1 only on claude", "the day plan leans on claude", "1 of 1 task(s) ran, 0 failed"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.ran) == 0 {
		t.Fatal("no agent ran")
	}
	for _, r := range log.ran {
		if !strings.HasPrefix(r, event.Claude+":") {
			t.Fatalf("an agent ran on codex: %v", log.ran)
		}
	}
}
