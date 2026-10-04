package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/runner"
	"gopkg.in/yaml.v3"
)

func TestIsTestPath(t *testing.T) {
	for p, want := range map[string]bool{
		"internal/gh/gh_test.go":            true,
		"internal/gh/gh.go":                 false,
		"tests/test_api.py":                 true,
		"pkg/test_utils.py":                 true,
		"pkg/conftest.py":                   true,
		"src/app.spec.ts":                   true,
		"src/__tests__/app.tsx":             true,
		"src/app.ts":                        false,
		"internal/router/testdata/a.json":   true,
		"src/test/java/a/FooTest.java":      true,
		"Foo.Tests/BarTests.cs":             true,
		"src/latest.go":                     false,
		"contest/main.py":                   false,
		"spec/models/user_spec.rb":          true,
		"docs/testing.md":                   false,
		"internal/web/__snapshots__/x.snap": true,
	} {
		if got := isTestPath(p); got != want {
			t.Errorf("isTestPath(%q) = %v", p, got)
		}
	}
}

func TestCleanCommitMessage(t *testing.T) {
	msg := "Fix the parser\r\n\r\nQuoted fields keep commas.\r\n\r\nSigned-off-by: A <a@x>\r\nCo-Authored-By: Claude <noreply@anthropic.com>\r\n"
	if got := cleanCommitMessage(msg); got != "Fix the parser\n\nQuoted fields keep commas." {
		t.Errorf("got %q", got)
	}
	msg = "Add sy watch\n\nWatches PRs.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude <x>"
	if got := cleanCommitMessage(msg); got != "Add sy watch\n\nWatches PRs." {
		t.Errorf("got %q", got)
	}
	// A body that only looks like a trailer in its first paragraph stays.
	if got := cleanCommitMessage("Note: this is a subject line"); got != "Note: this is a subject line" {
		t.Errorf("got %q", got)
	}
}

func TestPickHistory(t *testing.T) {
	f := func(p string, lines int) orchestrator.HistoryFile {
		return orchestrator.HistoryFile{Path: p, Lines: lines}
	}
	lim := historyLimits{minFiles: 2, maxFiles: 5, maxLines: 100}
	commits := []orchestrator.HistoryCommit{
		{SHA: "a1", Message: "Add quoted CSV fields to the parser", Files: []orchestrator.HistoryFile{f("p.go", 30), f("q.go", 20), f("p_test.go", 500), f("go.sum", 900)}},
		{SHA: "a2", Message: "Add quoted CSV fields", Files: []orchestrator.HistoryFile{f("p.go", 30), f("README.md", 5), f("p_test.go", 5)}},
		{SHA: "a3", Message: "Bump golang.org/x/sys from 0.38.0 to 0.48.0", Files: []orchestrator.HistoryFile{f("a.go", 1), f("b.go", 1), f("a_test.go", 1)}},
		{SHA: "a4", Message: "Rework the whole router package", Files: []orchestrator.HistoryFile{f("a.go", 90), f("b.go", 90), f("a_test.go", 1)}},
		{SHA: "a5", Message: "Speed up the pool reset path", Files: []orchestrator.HistoryFile{f("a.go", 9), f("b.go", 9)}},
		{SHA: "a6", Message: "wip", Files: []orchestrator.HistoryFile{f("a.go", 9), f("b.go", 9), f("a_test.go", 1)}},
		{SHA: "a7", Message: "Add icons to the web UI", Files: []orchestrator.HistoryFile{f("a.go", 9), f("b.go", 9), f("a_test.go", 1), {Path: "icon.png", Binary: true}}},
	}
	got, skipped := pickHistory(commits, lim)
	if len(got) != 1 || got[0].SHA != "a1" || len(got[0].code) != 2 || got[0].lines != 50 || len(got[0].tests) != 1 {
		t.Fatalf("picked %+v", got)
	}
	for _, why := range []string{"fewer than 2 code files", "more than 100 changed lines", "no test changes", "binary files outside the tests"} {
		if skipped[why] != 1 {
			t.Errorf("skipped[%q] = %d (%v)", why, skipped[why], skipped)
		}
	}
	if n := skipped["message is not a task (merge, revert, bump, wip or too short)"]; n != 2 {
		t.Errorf("not-a-task skipped %d (%v)", n, skipped)
	}
}

func TestSlug(t *testing.T) {
	if got := slug("Fix: the CSV parser (#12)!", 12); got != "-fix-the-csv" {
		t.Errorf("got %q", got)
	}
	if got := slug("!!!", 10); got != "" {
		t.Errorf("got %q", got)
	}
}

// End to end on a small Go repo: --from-history keeps the one commit whose
// tests fail before and pass after, and the bench restores its tests
// before the check (an agent that rewrites them still fails).
func TestBenchFromHistoryEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	if !orchestrator.SupportsMergeTree() {
		t.Skip("git < 2.38")
	}
	gocache, _ := exec.Command("go", "env", "GOCACHE").Output()
	isolate(t)
	if c := strings.TrimSpace(string(gocache)); c != "" {
		t.Setenv("GOCACHE", c) // warm cache: the checks compile in seconds
	}
	dir := gitInit(t)
	chdir(t, dir)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg ...string) string {
		t.Helper()
		run(t, dir, "add", "-A")
		args := []string{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q"}
		for _, m := range msg {
			args = append(args, "-m", m)
		}
		run(t, dir, args...)
		out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		return strings.TrimSpace(string(out))
	}
	const addTest = "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"add\")\n\t}\n}\n"
	write("go.mod", "module calc\n\ngo 1.21\n")
	write("calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	write("calc_test.go", addTest)
	commit("calculator init") // too short to be a task
	write("README.md", "# calc\n")
	base := commit("Document the calculator in the readme") // no tests: skipped
	write("calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Mul(a, b int) int { return a * b }\n")
	write("square.go", "package calc\n\n// Square is x times x.\nfunc Square(x int) int { return Mul(x, x) }\n")
	write("calc_test.go", addTest+"\nfunc TestMul(t *testing.T) {\n\tif Mul(2, 3) != 6 || Square(4) != 16 {\n\t\tt.Fatal(\"mul\")\n\t}\n}\n")
	target := commit("Add Mul and Square to the calculator", "Square builds on Mul.", "Co-Authored-By: Someone <s@x>")
	write("calc.go", "package calc\n\n// Add adds.\nfunc Add(a, b int) int { return a + b }\n\nfunc Mul(a, b int) int { return a * b }\n")
	write("square.go", "package calc\n\n// Square is x squared.\nfunc Square(x int) int { return Mul(x, x) }\n")
	write("calc_test.go", addTest+"\nfunc TestMul(t *testing.T) {\n\tif Mul(2, 3) != 6 || Square(4) != 16 {\n\t\tt.Fatal(\"mul\")\n\t}\n}\n\nfunc TestAddZero(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n")
	commit("Reword the comments in calc and square") // its new test already passes on the parent

	if err := cmdBench([]string{"--from-history", "--count", "5", "--check", "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("bench-history.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var bf benchFile
	if err := yaml.Unmarshal(data, &bf); err != nil {
		t.Fatal(err)
	}
	if len(bf.Tasks) != 1 {
		t.Fatalf("tasks: %+v\n%s", bf.Tasks, data)
	}
	task := bf.Tasks[0]
	if task.Base != base || task.Tests == nil || task.Tests.From != target || strings.Join(task.Tests.Files, ",") != "calc_test.go" || !task.Tests.Visible {
		t.Errorf("task %+v tests %+v (base %s, target %s)", task, task.Tests, base, target)
	}
	if !strings.Contains(task.Prompt, "Square builds on Mul.") || strings.Contains(task.Prompt, "Co-Authored-By") ||
		!strings.Contains(task.Prompt, "already in place: calc_test.go") || task.Check != "go test ./..." {
		t.Errorf("prompt %q check %q", task.Prompt, task.Check)
	}
	if err := cmdBench([]string{"--from-history"}); err == nil {
		t.Error("overwrote bench-history.yaml")
	}

	// Run it: one agent rewrites the test file to pass, one implements.
	honest := task
	honest.Name, honest.Prompt = "honest", task.Prompt+"HONEST"
	cheat := task
	cheat.Name = "cheat"
	bf.Tasks = []benchTask{cheat, honest}
	bf.Modes = []string{"single:claude:sonnet"}
	data, _ = yaml.Marshal(bf)
	os.WriteFile("bench-run.yaml", data, 0o644)
	sawTests := false
	agent := func(s runner.Spec) runner.Result {
		if test, _ := os.ReadFile(filepath.Join(s.Dir, "calc_test.go")); strings.Contains(string(test), "TestMul") {
			sawTests = true
		}
		if strings.Contains(s.Prompt, "HONEST") {
			os.WriteFile(filepath.Join(s.Dir, "calc.go"), []byte("package calc\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Mul(a, b int) int { return a * b }\n"), 0o644)
			os.WriteFile(filepath.Join(s.Dir, "square.go"), []byte("package calc\n\nfunc Square(x int) int { return x * x }\n"), 0o644)
			return runner.Result{Final: "done", Files: []string{"calc.go", "square.go"}}
		}
		os.WriteFile(filepath.Join(s.Dir, "calc_test.go"), []byte("package calc\n"), 0o644)
		return runner.Result{Final: "done", Files: []string{"calc_test.go"}}
	}
	benchRunners = func(*config.Config) runner.Set {
		return runner.Set{event.Claude: scriptedRunner{event.Claude, agent}, event.Codex: scriptedRunner{event.Codex, agent}}
	}
	defer func() { benchRunners = runner.New }()
	if err := cmdBench([]string{"--file", "bench-run.yaml", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if !sawTests {
		t.Error("the agents did not see the commit's tests")
	}
	matches, _ := filepath.Glob("bench-results-*.md")
	if len(matches) != 1 {
		t.Fatalf("results file: %v", matches)
	}
	rep, _ := os.ReadFile(matches[0])
	for _, want := range []string{"cheat", "FAIL", "1/2"} {
		if !strings.Contains(string(rep), want) {
			t.Errorf("report misses %q:\n%s", want, rep)
		}
	}
	if strings.Contains(string(rep), "honest  single:claude:sonnet  FAIL") {
		t.Errorf("the honest run failed:\n%s", rep)
	}
}

type scriptedRunner struct {
	provider string
	fn       func(s runner.Spec) runner.Result
}

func (x scriptedRunner) Run(ctx context.Context, s runner.Spec, emit func(event.Event)) runner.Result {
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Model: s.Model, Kind: event.Started}.Stamp())
	r := x.fn(s)
	emit(event.Event{AgentID: s.AgentID, Provider: x.provider, Kind: event.Done, OK: r.OK()}.Stamp())
	return r
}
