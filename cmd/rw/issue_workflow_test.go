package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/forge"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/workflow"
	"gopkg.in/yaml.v3"
)

type issueWorkflowRunner struct{ t *testing.T }

func (r issueWorkflowRunner) Run(_ context.Context, s runner.Spec, _ func(event.Event)) runner.Result {
	if strings.Contains(s.Prompt, runner.MarkerPlan) && !strings.Contains(s.Prompt, runner.MarkerPlanReview) {
		return runner.Result{Final: `{"summary":"fix issue","subtasks":[{"id":"fix","title":"fix issue","kind":"edit","prompt":"fix it","files":["a.txt"]}]}`}
	}
	if strings.Contains(s.Prompt, runner.MarkerStep) {
		b, err := os.ReadFile(filepath.Join(s.Dir, "a.txt"))
		if err != nil || string(b) != "one\ntwo\nthree\n" {
			r.t.Errorf("issue did not start from HEAD: %q, %v", b, err)
		}
		write(r.t, s.Dir, "a.txt", "ONE\ntwo\nthree\n")
		return runner.Result{Final: "fixed", Files: []string{"a.txt"}}
	}
	return runner.Result{Final: `{"approve":true,"advice":"ok"}`}
}

func issueWorkflowConfig(t *testing.T, definition string) string {
	t.Helper()
	if err := workflow.Save([]byte("name: issue-fix\ndescription: Fix an issue\nprompt: 'WORKFLOW START {{task}} WORKFLOW END'\ntask_tokens: 15000\ntask_usd: 2\n"+definition), false); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Orchestrator.ApprovePlan = false
	cfg.Orchestrator.ReviewChanges = false
	cfg.Orchestrator.ReviewBeforeDone = false
	cfg.Orchestrator.IndependentTests = false
	cfg.Orchestrator.MaxFixRounds = 0
	cfg.Verify.Auto = false
	cfg.Notify.Enabled = false
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	old := headlessRunners
	t.Cleanup(func() { headlessRunners = old })
	headlessRunners = func(c *config.Config) runner.Set {
		if c.Budget.TaskTokens != 15000 || c.Budget.TaskUSD != 1 {
			t.Errorf("workflow did not preserve tighter budgets: %+v", c.Budget)
		}
		r := issueWorkflowRunner{t}
		return runner.Set{event.Codex: r, event.Claude: r}
	}
	return p
}

func TestIssueWorkflowRun(t *testing.T) {
	for _, mode := range []string{"single", "scheduled-batch", "team"} {
		t.Run(mode, func(t *testing.T) {
			var dir, url string
			var api *fakeAPI
			var team *fakeTeam
			if mode == "team" {
				dir, team, url = teamSetup(t)
			} else {
				dir = prRepo(t)
				api, url = issueAPI(t)
			}
			chdir(t, dir)
			cfg := issueWorkflowConfig(t, "checks: [git --version]\nrequire_checks: true\n")
			if mode == "scheduled-batch" {
				factory := headlessRunners
				replaced := false
				headlessRunners = func(c *config.Config) runner.Set {
					// Editing the saved file after the batch starts must not
					// change the next issue's prompt, checks or budget.
					if !replaced {
						replaced = true
						if err := workflow.Save([]byte("name: issue-fix\ndescription: Changed mid-batch\nprompt: 'LATE {{task}}'\nchecks: [git rw-nonexistent-check]\ntask_tokens: 1\n"), true); err != nil {
							t.Error(err)
						}
					}
					return factory(c)
				}
			}
			prToken = func(forge.Kind, string) (string, string) { return "tok", "test" }
			var pushed []string
			prPush = func(_, _, branch string) error { pushed = append(pushed, branch); return nil }
			args := []string{"--config", cfg, "--workflow", "issue-fix", "--budget-task-usd", "1", "--api", url, "--pr", "--draft", "--with-comments", "--accept", "preserve other lines"}
			want := 2
			if mode == "single" {
				args = append(args, "--issue", "https://github.com/o/r/issues/3")
				want = 1
			} else {
				args = append(args, "--issues", "label:rw")
				if mode == "team" {
					args = append(args, "--team", "--allow-sleep")
				} else {
					args = append(args, "--in", "1ms", "--allow-sleep")
				}
			}
			out, err := captureStdout(t, func() error { return cmdRun(args) })
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if len(pushed) != want {
				t.Fatalf("pushed %v, want %d PRs\n%s", pushed, want, out)
			}
			history := orchestrator.History(dir, 10)
			if len(history) != want {
				t.Fatalf("history has %d tasks, want %d", len(history), want)
			}
			for _, st := range history {
				if st.Workflow == nil || st.Workflow.Name != "issue-fix" || !strings.HasPrefix(st.Task, "WORKFLOW START Fix GitHub issue #") || strings.Count(st.Task, "WORKFLOW START") != 1 || !strings.Contains(st.Task, "WORKFLOW END") || !strings.Contains(st.Task, "preserve other lines") {
					t.Errorf("lost workflow or issue context: %+v", st)
				}
				if st.Acceptance == nil || st.Acceptance.Checks.Status != orchestrator.LevelPass {
					t.Errorf("workflow checks did not pass: %+v", st.Acceptance)
				}
			}
			if api != nil {
				for i, pr := range api.created {
					n := []int{3, 9}[i]
					if !pr.Draft || !strings.Contains(pr.Body, fmt.Sprintf("Closes #%d", n)) || len(api.comments[fmt.Sprintf("/repos/o/r/issues/%d/comments", n)]) != 1 {
						t.Errorf("PR lost issue metadata: %+v", pr)
					}
				}
				if !strings.Contains(history[0].Task, "@bob wrote:\nsame here") {
					t.Error("issue comments missing from workflow task")
				}
			} else {
				for _, n := range []int{3, 9} {
					if got := team.bodies(n); len(got) != 1 || !strings.Contains(got[0], "state=done") || !strings.Contains(got[0], "pull/") {
						t.Errorf("claim was not completed: %q", got)
					}
				}
			}
			if mode != "single" && gitOut(t, dir, "status", "--porcelain") != "" {
				t.Error("batch left a dirty tree")
			}
		})
	}
}

func TestIssueWorkflowBlocksPR(t *testing.T) {
	for _, tc := range []struct{ name, definition string }{
		{"missing-checks", "require_checks: true\n"},
		{"failed-checks", "checks: [git rw-nonexistent-check]\nrequire_checks: true\n"},
		{"plan-declined", "checks: [git --version]\napprove_plan: true\n"},
		{"review-declined", "checks: [git --version]\nreview_changes: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := prRepo(t)
			chdir(t, dir)
			api, url := issueAPI(t)
			cfg := issueWorkflowConfig(t, tc.definition)
			oldIn := os.Stdin
			null, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			os.Stdin = null
			t.Cleanup(func() { os.Stdin = oldIn; null.Close() })
			out, err := captureStdout(t, func() error {
				return cmdRun([]string{"--config", cfg, "--workflow", "issue-fix", "--budget-task-usd", "1", "--api", url, "--issues", "label:rw", "--limit", "1", "--pr", "--no-review"})
			})
			if err == nil || len(api.created) != 0 {
				t.Fatalf("unsafe run accepted: %v, PRs %+v\n%s", err, api.created, out)
			}
			if tc.name == "missing-checks" {
				if !strings.Contains(err.Error(), "workflow requires checks") || api.requests.Load() != 0 {
					t.Fatalf("missing checks not rejected before issue fetch: %v", err)
				}
			} else if history := orchestrator.History(dir, 10); len(history) != 1 || history[0].Workflow == nil {
				t.Fatalf("workflow did not reach task execution: %v\n%s", err, out)
			}
			if strings.HasSuffix(tc.name, "declined") && gitOut(t, dir, "status", "--porcelain") != "" {
				t.Error("declined workflow changed the working tree")
			}
		})
	}
}
