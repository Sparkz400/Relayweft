package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The starter set: a small Python project with five tasks of different
// kinds (bug fix, feature, refactor-ish parsing, CLI change, read-only
// question) and a check script per task. Python because it is the runtime
// most machines with coding agents already have.
//
//go:embed all:starter
var starterFS embed.FS

const starterBench = `# sy bench starter set: five tasks on a small Python project.
# Each run starts from a clean checkout of this repo's HEAD.
# Checks live in bench_checks/ (agents can read them; the prompts do not mention them).

modes:
  - routed
  - single:codex:gpt-6.1-sol:high
  - single:claude:opus:high

timeout: 20m

tasks:
  - name: trailing-fields
    prompt: |
      parse_line in inventory/csvparse.py drops trailing empty fields: "a,b,," should give
      ["a", "b", "", ""]. Fix it and add a unit test.
    check: "PY bench_checks/check_trailing.py"
  - name: negative-stock
    prompt: |
      Stock.remove lets the quantity go below zero. Make it raise ValueError with a clear
      message instead (leaving the stock unchanged), and add tests.
    check: "PY bench_checks/check_negative.py"
  - name: quoted-fields
    prompt: |
      Support double-quoted CSV fields in parse_line: commas inside quotes stay in the field
      and "" inside quotes is a literal quote, e.g. a,"b,c",d -> ["a", "b,c", "d"].
      Keep the current behaviour for unquoted lines.
    check: "PY bench_checks/check_quoted.py"
  - name: json-report
    prompt: |
      Add a --json flag to the CLI (python -m inventory.cli --json data.csv) that prints the
      stock as a JSON list of objects with name, qty (int) and low (bool). The plain report
      stays the default.
    check: "PY bench_checks/check_json.py"
  - name: explain-low-stock
    prompt: |
      Where is the low-stock threshold defined and which functions use it? Write a short
      answer with file and function names to ANSWER.md. Do not change any code.
    check: "PY bench_checks/check_answer.py"
`

// writeStarter creates the starter repo in dir and commits it.
func writeStarter(dir string) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s exists and is not empty", dir)
	}
	py := "python3"
	if runtime.GOOS == "windows" {
		py = "python"
	}
	if _, err := exec.LookPath(py); err != nil {
		fmt.Printf("note: %s is not on PATH; the starter checks need Python 3\n", py)
	}
	err := fs.WalkDir(starterFS, "starter", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "starter"), "/")
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := starterFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return err
	}
	files := map[string]string{
		"bench.yaml": strings.ReplaceAll(starterBench, "PY ", py+" "),
		".gitignore": "__pycache__/\n*.pyc\nbench-results-*.md\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=sy bench", "-c", "user.email=sy-bench@localhost", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "sy bench starter"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v %s", args[0], err, out)
		}
	}
	return nil
}
