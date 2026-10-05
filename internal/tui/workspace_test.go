package tui

import (
	"testing"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func TestPlanRepoCycle(t *testing.T) {
	single := orchestrator.Plan{Subtasks: []orchestrator.Subtask{{ID: "a"}}}
	if planRepo(single, single.Subtasks[0]) != "" || cycleRepo(single, "") != "" {
		t.Error("a single-repo plan shows or cycles repos")
	}
	p := orchestrator.Plan{Repos: []string{orchestrator.PrimaryRepo, "web", "docs"}}
	if planRepo(p, orchestrator.Subtask{}) != orchestrator.PrimaryRepo || planRepo(p, orchestrator.Subtask{Repo: "web"}) != "web" {
		t.Error("planRepo")
	}
	got := []string{}
	cur := ""
	for i := 0; i < 3; i++ {
		cur = cycleRepo(p, cur)
		got = append(got, cur)
	}
	if got[0] != "web" || got[1] != "docs" || got[2] != "" {
		t.Errorf("cycle = %q", got)
	}
}
