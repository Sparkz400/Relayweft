package main

import (
	"archive/zip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/runner"
)

func gitInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "core.autocrlf", "false"}} {
		run(t, dir, args...)
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# x\n"), 0o644)
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	return dir
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// isolate points config, logs and caches at temp dirs.
func isolate(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("AppData", filepath.Join(home, "config"))
	t.Setenv("LocalAppData", filepath.Join(home, "cache"))
	t.Setenv("HOME", home)
	return home
}

func chdir(t *testing.T, dir string) {
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestBenchEndToEndWithFakeAgents(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	fake := runner.NewFakeSet(0)
	benchRunners = func(*config.Config) runner.Set { return fake }
	defer func() { benchRunners = runner.New }()
	os.WriteFile("bench.yaml", []byte(`
modes: [routed, "single:claude:sonnet"]
timeout: 2m
tasks:
  - name: passes
    prompt: "where is the readme"
    check: "git --version"
  - name: fails
    prompt: "where is the readme"
    check: "exit 3"
`), 0o644)
	if err := cmdBench([]string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob("bench-results-*.md")
	if len(matches) != 1 {
		t.Fatalf("results file: %v", matches)
	}
	rep, _ := os.ReadFile(matches[0])
	s := string(rep)
	for _, want := range []string{"passes  routed", "fails   routed", "FAIL", "1/2"} {
		if !strings.Contains(s, want) {
			t.Errorf("report misses %q:\n%s", want, s)
		}
	}
	// The user's tree is untouched.
	if out, _ := exec.Command("git", "-C", dir, "status", "--porcelain", "--untracked-files=no").Output(); len(out) != 0 {
		t.Errorf("bench changed the user's tree: %s", out)
	}
}

// A bench that ends normally must not print the Ctrl+C notice (seen in a
// real bench run: the end cancels the signal context too).
func TestBenchQuietAtNormalEnd(t *testing.T) {
	isolate(t)
	chdir(t, gitInit(t))
	benchRunners = func(*config.Config) runner.Set { return runner.NewFakeSet(0) }
	defer func() { benchRunners = runner.New }()
	os.WriteFile("bench.yaml", []byte("modes: [routed]\ntasks:\n  - {name: t, prompt: \"where is the readme\", check: \"git --version\"}\n"), 0o644)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	got := make(chan string)
	go func() { b, _ := io.ReadAll(r); got <- string(b) }()
	_, err = captureStdout(t, func() error { return cmdBench([]string{"--yes"}) })
	time.Sleep(200 * time.Millisecond) // a stray notice would print by now
	os.Stderr = old
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if s := <-got; strings.Contains(s, "cancelling") {
		t.Errorf("a normal end printed the Ctrl+C notice:\n%s", s)
	}
}

func TestBenchInit(t *testing.T) {
	chdir(t, t.TempDir())
	if err := cmdBench([]string{"--init"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdBench([]string{"--init"}); err == nil {
		t.Error("overwrote bench.yaml")
	}
	if data, _ := os.ReadFile("bench.yaml"); !strings.Contains(string(data), "modes:") {
		t.Error("example missing")
	}
}

func TestBugreportZip(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	chdir(t, dir)
	startDiag("test")
	t.Cleanup(diag.Close) // an open log blocks TempDir cleanup on Windows
	out := filepath.Join(dir, "r.zip")
	if err := cmdBugreport([]string{"--out", out}); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"environment.txt", "doctor.txt", "config.yaml", "logs/sy-debug.log", "logs/sy-health.log"} {
		if !names[want] {
			t.Errorf("zip misses %s (has %v)", want, names)
		}
	}
}

func TestUndoCLIList(t *testing.T) {
	dir := gitInit(t)
	chdir(t, dir)
	if err := cmdUndo([]string{"--list"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdUndo([]string{"--yes"}); err == nil || !strings.Contains(err.Error(), "no task to undo") {
		t.Errorf("err = %v", err)
	}
}
