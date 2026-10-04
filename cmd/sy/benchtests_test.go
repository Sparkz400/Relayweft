package main

import (
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

// {tests} and {test_dirs} become the task's test files that exist and a
// runner can take (not test data), quoted where needed.
func TestExpandTests(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"test/a_test.dart", "test/my dir/b_test.dart", "test/fixtures/x.json", "c_test.go"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	tests := &benchTests{Files: []string{"test/a_test.dart", "test/my dir/b_test.dart", "test/fixtures/x.json", "test/deleted_test.dart", "c_test.go"}}
	got, err := expandTests("flutter test {tests}", dir, tests)
	if want := `flutter test test/a_test.dart "test/my dir/b_test.dart" c_test.go`; err != nil || got != want {
		t.Errorf("{tests}: %q, %v; want %q", got, err, want)
	}
	got, _ = expandTests("go test {test_dirs}", dir, tests)
	if want := `go test ./test "./test/my dir" .`; got != want {
		t.Errorf("{test_dirs}: %q, want %q", got, want)
	}
	if got, err := expandTests("go test ./...", dir, nil); err != nil || got != "go test ./..." {
		t.Errorf("no placeholder: %q, %v", got, err)
	}
	if _, err := expandTests("pytest {tests}", dir, &benchTests{Files: []string{"test/fixtures/x.json"}}); err == nil {
		t.Error("no test file: no error")
	}
}

func TestNarrowCheck(t *testing.T) {
	files := []string{"calc/calc_test.go", "calc/testdata/in.txt", "test/a_test.dart", "integration_test/app_test.dart",
		"tests/test_x.py", "tests/conftest.py", "src/a.test.ts", "src/__tests__/b.js"}
	for _, c := range []struct{ check, want string }{
		{"go build ./... && go test ./...", "go build ./... && go test ./calc"},
		{"go test -race ./...", "go test -race ./calc"},
		{"go test ./pkg/...", ""},
		{"flutter test", "flutter test test/a_test.dart"},
		{"dart test --reporter=expanded", "dart test --reporter=expanded test/a_test.dart"},
		{"dart test --reporter expanded", ""}, // a flag value counts as an argument
		{"flutter test test/only_test.dart", ""},
		{"python -m pytest -q", "python -m pytest -q tests/test_x.py"},
		{"pytest -k slow", ""},
		{"npm test", "npm test -- src/a.test.ts src/__tests__/b.js"},
		{"npm test -- --ci", "npm test -- --ci src/a.test.ts src/__tests__/b.js"},
		{"npx vitest run", "npx vitest run src/a.test.ts src/__tests__/b.js"},
		{"cargo test", ""},
		{"make check", ""},
	} {
		got, ok := narrowCheck(c.check, files)
		if c.want == "" {
			if ok || got != c.check {
				t.Errorf("%q: narrowed to %q", c.check, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%q: %q (%v), want %q", c.check, got, ok, c.want)
		}
	}
	// No test file the runner takes: unchanged.
	if got, ok := narrowCheck("go test ./...", []string{"test/a_test.dart"}); ok || got != "go test ./..." {
		t.Errorf("no go test file: %q", got)
	}
}

// A check with {tests} needs tests.files to stand for.
func TestBenchRefusesTestsPlaceholderWithoutTests(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	os.WriteFile("bench.yaml", []byte("tasks:\n  - name: a\n    prompt: do it\n    check: \"pytest {tests}\"\n"), 0o644)
	if err := cmdBench([]string{"--yes"}); err == nil || !strings.Contains(err.Error(), "{tests}") {
		t.Errorf("err = %v", err)
	}
}

// One test that fails on every commit fails every candidate with the full
// suite. --own-tests (or {test_dirs} in --check) checks only the commit's
// tests, in validation and in the bench file, and the bench runs it.
func TestBenchFromHistoryOwnTests(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	if !orchestrator.SupportsMergeTree() {
		t.Skip("git < 2.38")
	}
	gocache, _ := exec.Command("go", "env", "GOCACHE").Output()
	isolate(t)
	if c := strings.TrimSpace(string(gocache)); c != "" {
		t.Setenv("GOCACHE", c)
	}
	dir := gitInit(t)
	chdir(t, dir)
	write := func(name, body string) {
		t.Helper()
		os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) string {
		t.Helper()
		run(t, dir, "add", "-A")
		run(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", msg)
		out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		return strings.TrimSpace(string(out))
	}
	write("go.mod", "module m\n\ngo 1.21\n")
	write("broken/broken_test.go", "package broken\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { t.Fatal(\"always\") }\n")
	write("calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	base := commit("Start the calculator with a broken test")
	write("calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Mul(a, b int) int { return a * b }\n")
	write("calc/square.go", "package calc\n\nfunc Square(x int) int { return Mul(x, x) }\n")
	write("calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestMul(t *testing.T) {\n\tif Square(3) != 9 {\n\t\tt.Fatal(\"square\")\n\t}\n}\n")
	target := commit("Add Mul and Square to the calculator")

	if err := cmdBench([]string{"--from-history", "--check", "go test ./..."}); err == nil || !strings.Contains(err.Error(), "fails on") {
		t.Fatalf("the full suite: %v", err)
	}
	if err := cmdBench([]string{"--from-history", "--own-tests", "--check", "cargo test"}); err == nil || !strings.Contains(err.Error(), "{tests}") {
		t.Errorf("--own-tests with an unknown runner: %v", err)
	}
	if err := cmdBench([]string{"--from-history", "--own-tests", "--check", "go vet ./... && go test ./..."}); err != nil {
		t.Fatal(err)
	}
	var bf benchFile
	data, _ := os.ReadFile("bench-history.yaml")
	if err := yaml.Unmarshal(data, &bf); err != nil || len(bf.Tasks) != 1 {
		t.Fatalf("%v\n%s", err, data)
	}
	if task := bf.Tasks[0]; task.Check != "go vet ./... && go test ./calc" || task.Base != base || task.Tests.From != target {
		t.Errorf("task %+v", task)
	}
	if err := cmdBench([]string{"--from-history", "--check", "go test {test_dirs}", "--file", "placeholder.yaml"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile("placeholder.yaml")
	if err := yaml.Unmarshal(data, &bf); err != nil || len(bf.Tasks) != 1 || bf.Tasks[0].Check != "go test {test_dirs}" {
		t.Fatalf("%v\n%s", err, data)
	}

	// The bench runs the narrowed check: an honest agent passes.
	bf.Modes = []string{"single:claude:sonnet"}
	data, _ = yaml.Marshal(bf)
	os.WriteFile("bench-run.yaml", data, 0o644)
	agent := func(s runner.Spec) runner.Result {
		os.WriteFile(filepath.Join(s.Dir, "calc", "calc.go"), []byte("package calc\n\nfunc Mul(a, b int) int { return a * b }\n"), 0o644)
		os.WriteFile(filepath.Join(s.Dir, "calc", "square.go"), []byte("package calc\n\nfunc Square(x int) int { return x * x }\n"), 0o644)
		return runner.Result{Final: "done", Files: []string{"calc/calc.go", "calc/square.go"}}
	}
	benchRunners = func(*config.Config) runner.Set {
		return runner.Set{event.Claude: scriptedRunner{event.Claude, agent}, event.Codex: scriptedRunner{event.Codex, agent}}
	}
	defer func() { benchRunners = runner.New }()
	out, err := captureStdout(t, func() error { return cmdBench([]string{"--file", "bench-run.yaml", "--yes"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1/1") {
		t.Errorf("the honest run failed:\n%s", out)
	}
}
