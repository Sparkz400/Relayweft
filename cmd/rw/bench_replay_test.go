package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
)

// The replay writes the independent tests once at the task's base and runs
// them against each saved run: the run that misses the requirement fails
// them, the one that meets it and the known solution pass, the base fails.
func TestBenchReplayTests(t *testing.T) {
	isolate(t)
	dir := gitInit(t)
	chdir(t, dir)
	head := func() string {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	base := head()
	os.WriteFile("fix.txt", []byte("fixed\n"), 0o644)
	os.WriteFile("hidden.txt", []byte("the hidden test\n"), 0o644)
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "solution")
	solution := head()
	run(t, dir, "checkout", "-q", base)

	old := benchRunners
	t.Cleanup(func() { benchRunners = old })
	writers := 0
	benchRunners = func(*config.Config) runner.Set {
		fn := func(s runner.Spec) runner.Result {
			switch {
			case s.CheckOnly: // Claude's permission preflight
				return runner.Result{Final: "ran them", Commands: s.AllowedCommands}
			case strings.Contains(s.Prompt, runner.MarkerReqTests):
				writers++
				os.WriteFile(filepath.Join(s.Dir, "req_rwreq.txt"), []byte("fix.txt must exist\n"), 0o644)
				return runner.Result{Final: "REQUIREMENTS:\n- fix.txt exists -> req_rwreq.txt\nCOMMAND: " + replayCheck()}
			case s.Provider == "codex":
				os.WriteFile(filepath.Join(s.Dir, "wrong.txt"), []byte("plausible\n"), 0o644)
			default:
				os.WriteFile(filepath.Join(s.Dir, "fix.txt"), []byte("fixed\n"), 0o644)
			}
			return runner.Result{Final: "done"}
		}
		return runner.Set{"codex": scriptedRunner{provider: "codex", fn: fn}, "claude": scriptedRunner{provider: "claude", fn: fn}}
	}
	hidden := ""
	if runtime.GOOS == "windows" {
		hidden = "if exist fix.txt (exit 0) else (exit 1)"
	} else {
		hidden = "test -f fix.txt"
	}
	yamlText := "fair: true\nmodes: [single:codex:gpt-6.1-sol:high, single:claude:sonnet:medium]\ntasks:\n" +
		"- name: req\n  prompt: make fix.txt exist\n  check: '" + hidden + "'\n  agent_checks: ['" + replayCheck() + "']\n" +
		"  base: " + base + "\n  tests: {from: " + solution + ", files: [hidden.txt], visible: false}\n"
	if err := os.WriteFile("bench.yaml", []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return cmdBench([]string{"--yes"}) }); err != nil {
		t.Fatal(err)
	}
	results, _ := filepath.Glob("bench-results-*.json")
	if len(results) != 1 {
		t.Fatal(results)
	}
	if b, _ := os.ReadFile(results[0]); strings.Count(string(b), `"completed"`) != 2 {
		t.Fatalf("bench runs:\n%s", b)
	}
	out, err := captureStdout(t, func() error { return cmdBench([]string{"--yes", "--replay-tests", results[0]}) })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if writers != 1 {
		t.Fatalf("test writers = %d, want 1", writers)
	}
	reps, _ := filepath.Glob("bench-replay-*/replay.json")
	if len(reps) != 1 {
		t.Fatal(reps)
	}
	data, _ := os.ReadFile(reps[0])
	var doc struct {
		Rows []replayRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range doc.Rows {
		key := r.Subject
		if r.Mode != "" {
			key = r.Mode
		}
		got[key] = r.Hidden + "/" + r.Req
		if r.Req == "error" {
			t.Errorf("%s: %s", key, r.Report)
		}
	}
	want := map[string]string{
		"base":                          "/fail",
		"solution":                      "pass/pass",
		"single:codex:gpt-6.1-sol:high": "fail/fail",
		"single:claude:sonnet:medium":   "pass/pass",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: hidden/independent = %q, want %q (all: %v)", k, got[k], w, got)
		}
	}
	md, _ := os.ReadFile(filepath.Join(filepath.Dir(reps[0]), "replay.md"))
	if !strings.Contains(string(md), "caught 1 of 1") || !strings.Contains(string(md), "failed 0 of 2 (false alarms)") {
		t.Errorf("report:\n%s", md)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(reps[0]), "req", "writer-1", "files", "req_rwreq.txt")); err != nil {
		t.Errorf("the written tests were not saved: %v", err)
	}

	// The fix round replay: the caught run's change gets the same tests
	// and a fix round, which here makes fix.txt; no worker or writer runs.
	fixes := 0
	benchRunners = func(*config.Config) runner.Set {
		fn := func(s runner.Spec) runner.Result {
			switch {
			case s.CheckOnly:
				return runner.Result{Final: "ran them", Commands: s.AllowedCommands}
			case strings.Contains(s.Prompt, runner.MarkerFix):
				fixes++
				if !strings.Contains(s.Prompt, "INDEPENDENT REQUIREMENT TESTS") {
					t.Errorf("the fix agent did not get the failing tests:\n%s", s.Prompt)
				}
				os.WriteFile(filepath.Join(s.Dir, "fix.txt"), []byte("fixed\n"), 0o644)
				return runner.Result{Final: "fixed"}
			case strings.Contains(s.Prompt, runner.MarkerReqTests):
				t.Error("a test writer ran in the fix replay")
			}
			return runner.Result{Final: "done"}
		}
		return runner.Set{"codex": scriptedRunner{provider: "codex", fn: fn}, "claude": scriptedRunner{provider: "claude", fn: fn}}
	}
	out, err = captureStdout(t, func() error { return cmdBench([]string{"--yes", "--replay-fix", filepath.Dir(reps[0])}) })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if fixes != 1 {
		t.Fatalf("fix agents = %d, want 1\n%s", fixes, out)
	}
	fr, _ := filepath.Glob("bench-fixreplay-*/fixreplay.json")
	if len(fr) != 1 {
		t.Fatal(fr)
	}
	data, _ = os.ReadFile(fr[0])
	var fdoc struct {
		Rows []fixReplayRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &fdoc); err != nil {
		t.Fatal(err)
	}
	if len(fdoc.Rows) != 1 {
		t.Fatalf("rows: %+v", fdoc.Rows)
	}
	r := fdoc.Rows[0]
	if r.Mode != "single:codex:gpt-6.1-sol:high" || r.Hidden != "fail" || r.HiddenAfter != "pass" || r.ReqAfter != "pass" || r.FixRounds != 1 || r.Note != "" {
		t.Errorf("row: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(fr[0]), "run-001", "solution.patch")); !strings.Contains(string(b), "wrong.txt") || !strings.Contains(string(b), "fix.txt") {
		t.Errorf("the result has the change and the fix:\n%s", b)
	}
	if md, _ := os.ReadFile(filepath.Join(filepath.Dir(fr[0]), "fixreplay.md")); !strings.Contains(string(md), "turned 1 of 1") {
		t.Errorf("report:\n%s", md)
	}
}

// replayCheck fails while the independent test file is there and fix.txt
// is not.
func replayCheck() string {
	if runtime.GOOS == "windows" {
		return "if exist req_rwreq.txt (if exist fix.txt (exit 0) else (exit 1)) else (exit 0)"
	}
	return "test ! -f req_rwreq.txt || test -f fix.txt"
}

// The final reviews' verdicts come from a run's saved events, as rw bench
// wrote them.
func TestFinalReviews(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	lines := `{"agent_id":"reviewer","kind":"checkpoint","text":"plan: approved","ok":true}
{"agent_id":"reviewer","kind":"checkpoint","text":"final: changes requested\n- x","tokens":{},"ts":"2026-10-06T14:42:01+02:00"}
not json
{"agent_id":"reviewer","kind":"checkpoint","text":"final: approved","tokens":{},"ts":"2026-10-06T14:42:01.7959946+02:00","ok":true,"until":"0001-01-01T00:00:00Z"}
`
	os.WriteFile(p, []byte(lines), 0o644)
	if got := finalReviews(p); got != "RA" {
		t.Errorf("finalReviews = %q, want RA", got)
	}
}
