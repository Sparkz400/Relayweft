package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
)

func TestBenchCheckpointsBeforeNextRun(t *testing.T) {
	isolate(t)
	chdir(t, gitInit(t))
	old := benchRunners
	t.Cleanup(func() { benchRunners = old })
	calls := 0
	benchRunners = func(*config.Config) runner.Set {
		return runner.Set{"codex": scriptedRunner{provider: "codex", fn: func(s runner.Spec) runner.Result {
			calls++
			if calls == 2 {
				files, _ := filepath.Glob("bench-results-*.json")
				if len(files) != 1 {
					t.Fatalf("no checkpoint before next agent: %v", files)
				}
				data, _ := os.ReadFile(files[0])
				var report struct {
					Results []struct {
						Passed    bool
						Artifacts string
					}
				}
				if err := json.Unmarshal(data, &report); err != nil {
					t.Fatal(err)
				}
				if len(report.Results) < 1 || !report.Results[0].Passed {
					t.Fatalf("lost completed result: %s", data)
				}
				for name, want := range map[string]string{"check.txt": "first diagnostic", "solution.patch": "new file content"} {
					b, err := os.ReadFile(filepath.Join(report.Results[0].Artifacts, name))
					if err != nil || !strings.Contains(string(b), want) {
						t.Fatalf("missing %s: %s %v", name, b, err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(s.Dir, "new file.txt"), []byte("new file content\n"), 0600); err != nil {
				t.Fatal(err)
			}
			return runner.Result{Final: "done", Files: []string{"new file.txt"}}
		}}}
	}
	input := "fair: true\nrepeat: 2\nmodes: [single:codex:gpt-6.1-sol:high]\ntasks:\n- {name: check, prompt: fix it, check: 'echo first diagnostic'}\n"
	if err := os.WriteFile("bench.yaml", []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return cmdBench([]string{"--yes"}) }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

type interruptedBenchRunner struct{}

func (interruptedBenchRunner) Run(ctx context.Context, s runner.Spec, _ func(event.Event)) runner.Result {
	err := os.WriteFile(filepath.Join(s.Dir, "partial.txt"), []byte("unfinished but preserved\n"), 0600)
	if err != nil {
		return runner.Result{Err: err}
	}
	<-ctx.Done()
	return runner.Result{Err: ctx.Err(), Killed: true, Files: []string{"partial.txt"}, Tokens: event.TokenUsage{Input: 123, Incomplete: true}}
}

func TestBenchTimeoutPersistsPartialEvidence(t *testing.T) {
	isolate(t)
	chdir(t, gitInit(t))
	old := benchRunners
	t.Cleanup(func() { benchRunners = old })
	benchRunners = func(*config.Config) runner.Set { return runner.Set{"codex": interruptedBenchRunner{}} }
	input := "fair: true\ntimeout: 3s\nmodes: [single:codex:gpt-6.1-sol:high]\ntasks:\n- {name: interrupted, prompt: fix it, check: 'echo must-not-run'}\n"
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
	b, _ := os.ReadFile(files[0])
	var report struct {
		Results []struct {
			Status, Artifacts string
			Cost              event.TaskCost
			Passed            bool
		}
	}
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 {
		t.Fatal(string(b))
	}
	r := report.Results[0]
	if r.Passed || r.Status != "timed out" || r.Cost.PerProvider["codex"].Total() != 123 || !r.Cost.PerProvider["codex"].Incomplete {
		t.Fatal(string(b))
	}
	patch, err := os.ReadFile(filepath.Join(r.Artifacts, "solution.patch"))
	if err != nil || !strings.Contains(string(patch), "unfinished but preserved") {
		t.Fatal("partial work lost", err)
	}
	if _, err := os.Stat(filepath.Join(r.Artifacts, "check.txt")); !os.IsNotExist(err) {
		t.Fatal("scored a timed-out run")
	}
}

func TestBenchAtomicReplacementFailureKeepsPrevious(t *testing.T) {
	p := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := benchAtomicWrite(p, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := benchAtomicWrite(p, []byte("second")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "second" {
		t.Fatal(string(b))
	}
	// An invalid target must return a write error, never claim to save.
	if err := benchAtomicWrite(filepath.Join(p, "no.json"), []byte("third")); err == nil {
		t.Fatal("accepted invalid path")
	}
	b, _ = os.ReadFile(p)
	if string(b) != "second" {
		t.Fatal("previous checkpoint lost")
	}
}
