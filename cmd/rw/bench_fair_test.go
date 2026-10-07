package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
	"gopkg.in/yaml.v3"
)

func TestBenchTiersAndOrder(t *testing.T) {
	base := config.NewStore(config.Default(), "")
	m, err := parseBenchMode("routed-tiers")
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.store(base)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().Routing.Tiers != config.TiersAuto || base.Get().Routing.Tiers == config.TiersAuto {
		t.Fatal("tiers mode changed base config")
	}
	modes := []benchMode{{name: "single"}, {name: "routed"}, {name: "tiers"}, {name: "bestof"}}
	for round := 0; round < 4; round++ {
		order := benchModeOrder(modes, round)
		if order[0].name != modes[round].name {
			t.Fatal("mode order not balanced")
		}
		seen := map[string]bool{}
		for _, m := range order {
			seen[m.name] = true
		}
		if len(seen) != 4 {
			t.Fatal("mode omitted")
		}
	}
	p := filepath.Join(t.TempDir(), "results.json")
	if err := writeBenchResults(p, "commit", benchFile{Fair: true, Repeat: 3}, []benchResult{{task: "one", mode: "routed", passed: true}}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || len(b) == 0 {
		t.Fatal("missing machine-readable report")
	}
}

func TestBenchReviewModeEnablesOptionalGate(t *testing.T) {
	base := config.NewStore(config.Default(), "")
	m, err := parseBenchMode("routed-review")
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.store(base)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Get().Orchestrator.ReviewBeforeDone || s.Get().Orchestrator.FinalReview() != config.ReviewFailing || base.Get().Orchestrator.ReviewBeforeDone {
		t.Fatal("review experiment must enable its own gate without changing the default")
	}
}

func TestFairBenchMatrix(t *testing.T) {
	isolate(t)
	chdir(t, gitInit(t))
	old := benchRunners
	t.Cleanup(func() { benchRunners = old })
	policies := map[string]bool{}
	benchRunners = func(c *config.Config) runner.Set {
		if c.Routing.Learn != config.LearnOff || c.Orchestrator.Handoff {
			t.Error("fair run used persistent context/learning")
		}
		policies[string(c.Routing.Tiers)+"/"+string(c.Routing.BestOf.When)] = true
		return runner.NewFakeSet(0)
	}
	input := `fair: true
repeat: 2
modes: [single:claude:sonnet, routed, routed-tiers, routed-bestof]
tasks:
  - {name: check, prompt: "where is the readme", check: "git --version"}
`
	if err := os.WriteFile("bench.yaml", []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return cmdBench([]string{"--yes"}) }); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("bench-results-*.json")
	if len(files) != 1 {
		t.Fatal(files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Fair    bool
		Repeat  int
		Results []struct {
			Mode       string
			Repetition int
			Passed     bool
			WallMS     int64 `json:"wall_ms"`
		}
	}
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Fair || report.Repeat != 2 || len(report.Results) != 8 {
		t.Fatalf("incomplete matrix: %+v", report)
	}
	counts := map[string]int{}
	for _, r := range report.Results {
		counts[r.Mode]++
		if !r.Passed || r.WallMS <= 0 || r.Repetition < 1 || r.Repetition > 2 {
			t.Fatalf("bad row: %+v", r)
		}
	}
	if len(counts) != 4 || len(policies) != 3 {
		t.Fatalf("modes/policies missing: %v %v", counts, policies)
	}
	for mode, n := range counts {
		if n != 2 {
			t.Fatal(mode, n)
		}
	}
	if report.Results[0].Mode == report.Results[4].Mode {
		t.Fatal("second repetition did not rotate")
	}
}

func TestRealisticCorpusManifest(t *testing.T) {
	b, err := os.ReadFile("../../docs/bench/realistic.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var suite benchFile
	if err := yaml.Unmarshal(b, &suite); err != nil {
		t.Fatal(err)
	}
	if !suite.Fair || suite.Learn || suite.Repeat < 2 || len(suite.Tasks) != 10 || len(suite.Modes) != 4 {
		t.Fatal("realistic suite lost its fair matrix")
	}
	names := map[string]bool{}
	for _, task := range suite.Tasks {
		if task.Name == "" || names[task.Name] || len(task.Base) != 40 || task.Tests == nil || len(task.Tests.From) != 40 || len(task.Tests.Files) == 0 || task.Tests.Visible || task.Check == "" {
			t.Fatalf("invalid task: %+v", task)
		}
		names[task.Name] = true
	}
	for _, name := range suite.Modes {
		if _, err := parseBenchMode(name); err != nil {
			t.Fatal(err)
		}
	}
}
