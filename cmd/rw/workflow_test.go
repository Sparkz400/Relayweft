package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/workflow"
)

func TestWorkflowRunUsesChecksAndBudgets(t *testing.T) {
	isolate(t)
	chdir(t, gitInit(t))
	data := []byte("name: fixture\ndescription: test workflow\nprompt: 'Investigate {{task}} and report findings.'\nchecks: [git --version]\nrequire_checks: true\ntask_tokens: 15000\ntask_usd: 2\napprove_plan: true\nreview_changes: true\n")
	if err := workflow.Save(data, false); err != nil {
		t.Fatal(err)
	}
	if got := complete([]string{"run", "--workflow", "fi"}); len(got.cands) != 1 || got.cands[0].value != "fixture" {
		t.Fatal("saved workflow missing from completion", got)
	}
	old := headlessRunners
	t.Cleanup(func() { headlessRunners = old })
	called := false
	headlessRunners = func(c *config.Config) runner.Set {
		called = true
		if c.Budget.TaskUSD != 1 || c.Budget.TaskTokens != 15000 || !c.Orchestrator.ReviewChanges || !c.Orchestrator.ApprovePlan {
			t.Errorf("workflow constraints not applied: %+v", c.Budget)
		}
		if len(c.Verify.Commands) != 1 || c.Verify.Commands[0] != "git --version" {
			t.Error("workflow checks absent")
		}
		return runner.Set{event.Codex: planOnly{t}, event.Claude: planOnly{t}}
	}
	out, err := captureStdout(t, func() error {
		return cmdRun([]string{"--workflow", "fixture", "--budget-task-usd", "1", "--estimate", "make the parser keep trailing empty fields and document behavior"})
	})
	if err != nil || !called || !strings.Contains(out, "estimate only") {
		t.Fatalf("workflow estimate: %v\n%s", err, out)
	}
	if err := cmdRun([]string{"--workflow", "fixture", "--single", "codex:fixture", "task"}); err == nil {
		t.Fatal("approval-bypassing single mode accepted")
	}
}

// A task file (unattended) under a workflow that asks for plan approval
// still asks, on the terminal, and announces it; a closed stdin declines,
// so nothing runs.
func TestWorkflowFileRunKeepsApprovals(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	data := []byte("name: gated\ndescription: test workflow\nprompt: 'Investigate {{task}} and report findings.'\napprove_plan: true\n")
	if err := workflow.Save(data, false); err != nil {
		t.Fatal(err)
	}
	old, oldIn, oldNotify := headlessRunners, os.Stdin, waitingNotify
	t.Cleanup(func() { headlessRunners, os.Stdin, waitingNotify = old, oldIn, oldNotify })
	headlessRunners = func(*config.Config) runner.Set {
		return runner.Set{event.Codex: planOnly{t}, event.Claude: planOnly{t}}
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	os.Stdin = null
	var alerts []string
	waitingNotify = func(_, body string) error { alerts = append(alerts, body); return nil }
	file := filepath.Join(t.TempDir(), "tasks.txt")
	os.WriteFile(file, []byte("the parser drops trailing empty fields in quoted rows\n"), 0o644)
	out, err := captureStdout(t, func() error {
		return cmdRun([]string{"--workflow", "gated", "--no-review", "--file", file})
	})
	if err == nil || !strings.Contains(out, "Run it?") || !strings.Contains(out, "not approved") {
		t.Fatalf("gated task file: %v\n%s", err, out)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "approve the plan") {
		t.Fatalf("alerts = %q", alerts)
	}
	history := orchestrator.History(dir, 1)
	if len(history) != 1 || history[0].Workflow == nil {
		t.Fatal("workflow was not saved with the task")
	}
	// No --approve flag: the saved workflow itself must retain the gate.
	out, err = captureStdout(t, func() error {
		return cmdResume([]string{"--force", "--no-review", history[0].ID})
	})
	if err == nil || !strings.Contains(out, "Run it?") || !strings.Contains(out, "not approved") {
		t.Fatalf("gated resume: %v\n%s", err, out)
	}
}
