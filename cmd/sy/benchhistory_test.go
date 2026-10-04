package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sysload"
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
		"integration_test/app_test.dart":    true,
		"integration_test/robot.dart":       true,
		"lib/src/parser_test.dart":          true,
		"test_driver/main.dart":             true,
		"lib/src/parser.dart":               false,
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
		{SHA: "a8", Message: "Add retries to the uploader", Files: []orchestrator.HistoryFile{f("a.go", 0), f("b.go", 0), f("a_test.go", 0)}, Uncounted: true},
	}
	got, skipped := pickHistory(commits, lim)
	if len(got) != 1 || got[0].SHA != "a1" || len(got[0].code) != 2 || got[0].lines != 50 || len(got[0].tests) != 1 {
		t.Fatalf("picked %+v", got)
	}
	for _, why := range []string{"fewer than 2 code files", "more than 100 changed lines", "no test changes", "binary files outside the tests", "changed lines not counted"} {
		if skipped[why] != 1 {
			t.Errorf("skipped[%q] = %d (%v)", why, skipped[why], skipped)
		}
	}
	if n := skipped["message is not a task (merge, revert, sync, bump, wip or too short)"]; n != 2 {
		t.Errorf("not-a-task skipped %d (%v)", n, skipped)
	}
	// Before the line counts only the cheap checks apply.
	if _, why := judgeHistory(orchestrator.HistoryCommit{Message: "Add icons to the web UI", Uncounted: true,
		Files: []orchestrator.HistoryFile{f("a.go", 0), f("b.go", 0), f("a_test.go", 0)}}, lim); why != "" {
		t.Errorf("uncounted commit skipped: %s", why)
	}
}

// Mirror and sync commits are copies of work done elsewhere, not tasks.
func TestHistorySkipsSyncAndMirrorCommits(t *testing.T) {
	for s, want := range map[string]bool{
		"Sync from upstream (2026-10-01)":           false,
		"Synced with the internal repo":             false,
		"Auto-sync from monorepo":                   false,
		"Mirror of github.com/acme/tool@1a2b3c":     false,
		"Mirrored from gitlab":                      false,
		"Merge branch 'main' into feature":          false,
		"Merge pull request #12 from a/b":           false,
		"Revert \"Add the cache\"":                  false,
		"Import from the old repository":            false,
		"Update from upstream":                      false,
		"Sync the timer with the server clock":      true,
		"Add mirror support to the uploader":        true,
		"Fix the sync loop dropping the last batch": true,
	} {
		if got := taskLikeSubject(s); got != want {
			t.Errorf("taskLikeSubject(%q) = %v", s, got)
		}
	}
}

// When no commit passes validation, the error names the most common
// reason instead of always guessing a missing --setup.
func TestNoValidCommitNamesTheMainReason(t *testing.T) {
	fails := validateSkips(skipFailsOnCommit+": FAIL TestX", skipFailsOnCommit+": exit 1", skipFailsOnCommit+": FAIL TestY")
	err := noValidCommit(fails, "npm ci")
	if err == nil || !strings.Contains(err.Error(), "already fails on all 3 commits") || !strings.Contains(err.Error(), "fix or narrow the check (--check)") ||
		strings.Contains(err.Error(), "--setup") {
		t.Errorf("all fail: %v", err)
	}
	mixed := validateSkips(skipPassesBefore, skipPassesBefore, skipFailsOnCommit+": x")
	if err := noValidCommit(mixed, ""); err == nil || !strings.Contains(err.Error(), "already passes before the change on 2 of the 3") {
		t.Errorf("mostly passing: %v", err)
	}
	setup := validateSkips("on the commit: setup failed: npm ERR!", "on the parent: setup failed: npm ERR!")
	if err := noValidCommit(setup, "npm ci"); err == nil || !strings.Contains(err.Error(), "--setup command failed on all 2") {
		t.Errorf("setup: %v", err)
	}
	if err := noValidCommit(validateSkips("on the parent: timed out"), ""); err == nil || !strings.Contains(err.Error(), "--check-timeout") {
		t.Errorf("timeout: %v", err)
	}
}

func validateSkips(whys ...string) map[string]int {
	m := map[string]int{}
	for _, w := range whys {
		m[skipKind(w)]++
	}
	return m
}

// The file's header says how to run it, with --dir when sy bench would not
// find the project from where the file is; an empty setup is left out.
func TestHistoryBenchFileHeader(t *testing.T) {
	repo, elsewhere := t.TempDir(), t.TempDir()
	out := filepath.Join(elsewhere, "bench-history.yaml")
	cmd := historyRunCmd(out, repo, true)
	if want := "sy bench --dir " + argQuote(repo) + " --file " + argQuote(out); cmd != want {
		t.Errorf("outside the repo: %q, want %q", cmd, want)
	}
	if got := historyRunCmd(filepath.Join(repo, "b.yaml"), repo, true); !strings.Contains(got, "--dir") {
		t.Errorf("--dir given: %q", got)
	}
	chdir(t, repo)
	if got := historyRunCmd("bench-history.yaml", repo, false); got != "sy bench --file bench-history.yaml" {
		t.Errorf("in the repo: %q", got)
	}
	// The project named through a symlink, the working directory by its
	// real path (as on macOS, where /var is /private/var).
	link := filepath.Join(elsewhere, "link")
	if err := os.Symlink(repo, link); err == nil {
		if got := historyRunCmd("bench-history.yaml", link, false); got != "sy bench --file bench-history.yaml" {
			t.Errorf("in the repo, named through a symlink: %q", got)
		}
	} else {
		t.Logf("no symlink test: %v", err)
	}
	if got := argQuote(`C:\My Repo`); got != `"C:\My Repo"` {
		t.Errorf("argQuote = %s", got)
	}
	h := historyCandidate{HistoryCommit: orchestrator.HistoryCommit{SHA: "abcdef123", Parent: "p"}, tests: []string{"a_test.go"}, prompt: "Add a"}
	data, err := historyBenchFile([]historyCandidate{h}, historyOpts{out: out, check: "go test ./...", runCmd: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# Run it with: "+cmd+"\n") || strings.Contains(string(data), "setup:") {
		t.Errorf("bench file:\n%s", data)
	}
}

// --no-validate without a check command lists the commits that fit and
// writes nothing.
func TestBenchFromHistoryNoValidateLists(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	os.MkdirAll("spec", 0o755)
	for name, body := range map[string]string{"a.lua": "return 1\n", "b.lua": "return 2\n", "spec/a_spec.lua": "-- a\n"} {
		os.WriteFile(name, []byte(body), 0o644)
	}
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "Add the a and b modules")
	out, err := captureStdout(t, func() error { return cmdBench([]string{"--from-history", "--no-validate"}) })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Add the a and b modules") || !strings.Contains(out, "listed only") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat("bench-history.yaml"); err == nil {
		t.Error("wrote a bench file without a check")
	}
	if err := cmdBench([]string{"--from-history"}); err == nil || !strings.Contains(err.Error(), "--check") {
		t.Errorf("validating without a check: %v", err)
	}
}

// Low free RAM holds the next history step, at most busy_max_wait.
func TestWaitForMemory(t *testing.T) {
	oldLoad, oldPoll := benchLoad, benchPoll
	t.Cleanup(func() { benchLoad, benchPoll = oldLoad, oldPoll })
	benchPoll = time.Millisecond
	reads := 0
	benchLoad = func() sysload.Sample {
		reads++
		if reads < 3 {
			return sysload.Sample{MemOK: true, MemFree: 100 << 20}
		}
		return sysload.Sample{MemOK: true, MemFree: 4 << 30}
	}
	oc := config.OrchestratorCfg{MinFreeMemoryMB: 1024, BusyMaxWait: config.Duration(time.Minute)}
	out, _ := captureStdout(t, func() error { waitForMemory(context.Background(), oc, "the scan"); return nil })
	if reads != 3 || !strings.Contains(out, "only 100 MB RAM free") {
		t.Errorf("%d reads, output %q", reads, out)
	}
	// Still low after busy_max_wait: go on.
	reads = -1000
	oc.BusyMaxWait = config.Duration(5 * time.Millisecond)
	out, _ = captureStdout(t, func() error { waitForMemory(context.Background(), oc, "the scan"); return nil })
	if !strings.Contains(out, "going on with the scan") {
		t.Errorf("output %q", out)
	}
	// Off when min_free_memory_mb is 0.
	reads = 0
	waitForMemory(context.Background(), config.OrchestratorCfg{}, "the scan")
	if reads != 0 {
		t.Error("read the load with the limit off")
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
