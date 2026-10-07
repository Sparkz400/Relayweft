package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// reqCheck is a check that fails while the requirement test file exists
// and fix.txt does not: a stand-in for a test runner that picks up the
// independent test file.
func reqCheck() string {
	if runtime.GOOS == "windows" {
		return "if exist req_rwreq.txt (if exist fix.txt (exit 0) else (echo requirement: fix.txt is missing & exit 1)) else (exit 0)"
	}
	return "test ! -f req_rwreq.txt || test -f fix.txt || { echo requirement: fix.txt is missing; exit 1; }"
}

// reqRun scripts a one-step task: the worker writes out.txt but misses the
// requirement (fix.txt), the test writer writes req_rwreq.txt, and a fix
// agent adds fix.txt.
type reqRun struct {
	mu        sync.Mutex
	tester    []runner.Spec
	workerDir string
	fixes     []runner.Spec
	inTree    bool // the test file was in the fix agent's tree
	command   string
	reviews   int
	noFix     bool   // the fix agent leaves the change as it is
	fixReply  string // the fix agent's reply when noFix
}

func (r *reqRun) set() runner.Set {
	return both(func(s runner.Spec) runner.Result {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case strings.Contains(s.Prompt, runner.MarkerReqTests):
			r.tester = append(r.tester, s)
			os.WriteFile(filepath.Join(s.Dir, "req_rwreq.txt"), []byte("fix.txt must exist\n"), 0o644)
			return runner.Result{Final: "wrote it\nREQUIREMENTS:\n- fix.txt exists -> req_rwreq.txt\nCOMMAND: " + r.command}
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			r.reviews++
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFix):
			r.fixes = append(r.fixes, s)
			_, err := os.Stat(filepath.Join(s.Dir, "req_rwreq.txt"))
			r.inTree = err == nil
			if r.noFix {
				if r.fixReply != "" {
					return runner.Result{Final: r.fixReply}
				}
				return runner.Result{Final: "the test assumes something the task does not ask for"}
			}
			os.WriteFile(filepath.Join(s.Dir, "fix.txt"), []byte("fixed\n"), 0o644)
			return runner.Result{Final: "fixed"}
		}
		r.workerDir = s.Dir
		os.WriteFile(filepath.Join(s.Dir, "out.txt"), []byte("work\n"), 0o644)
		return runner.Result{Final: "done", SessionID: "sess-worker"}
	})
}

func reqCfg(c *config.Config) {
	shortcuts(c)
	c.Orchestrator.IndependentTests = true
	c.Verify.Commands = []string{fileCheck("out.txt"), reqCheck()}
}

// The worker's change passes the checks but misses a requirement; the
// independent tests, written on the other provider at the task's start,
// catch it, the fix round gets them in its tree, and they never stay there.
func TestIndependentTestsCatchMissedRequirement(t *testing.T) {
	dir := gitRepo(t)
	r := &reqRun{command: reqCheck()}
	o, rec := newOrc(t, dir, r.set(), reqCfg)
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK {
		t.Fatalf("task: %+v\nlog:\n%s", res, strings.Join(logLines(rec), "\n"))
	}
	if len(r.tester) != 1 {
		t.Fatalf("test writer ran %d times", len(r.tester))
	}
	ts := r.tester[0]
	if ts.Provider != event.Claude {
		t.Errorf("test writer on %s, want the provider the worker (codex) is not on", ts.Provider)
	}
	if samePath(ts.Dir, dir) || samePath(ts.Dir, r.workerDir) {
		t.Errorf("test writer worked in %s, the worker's tree", ts.Dir)
	}
	if _, err := os.Stat(filepath.Join(ts.Dir, "out.txt")); err == nil {
		t.Error("the test writer's worktree has the worker's change")
	}
	if len(r.fixes) != 1 {
		t.Fatalf("fix rounds = %d, want 1", len(r.fixes))
	}
	// The tests take the final review's place (review_when: untested).
	if r.reviews != 0 || !logged(rec, "final review skipped: the independent tests fail: their output advises the fix round") {
		t.Errorf("final reviews = %d, log:\n%s", r.reviews, strings.Join(logLines(rec), "\n"))
	}
	fp := r.fixes[0].Prompt
	if !strings.Contains(fp, "Independent requirement tests fail") || !strings.Contains(fp, "fix.txt is missing") {
		t.Errorf("fix prompt lacks the failing independent tests:\n%s", fp)
	}
	if !r.inTree {
		t.Error("the fix agent in the tree did not get the test file")
	}
	if _, err := os.Stat(filepath.Join(dir, "req_rwreq.txt")); err == nil {
		t.Error("the independent test file stayed in the tree")
	}
	if !strings.Contains(res.Summary, "independent tests pass") {
		t.Errorf("summary = %q", res.Summary)
	}
	kept := ""
	for _, l := range logLines(rec) {
		if _, p, ok := strings.Cut(l, "independent tests: a copy is in "); ok {
			kept = p
		}
	}
	if b, err := os.ReadFile(filepath.Join(kept, "files", "req_rwreq.txt")); kept == "" || err != nil || string(b) != "fix.txt must exist\n" {
		t.Errorf("no copy of the tests was kept (%q): %v", kept, err)
	}
}

// With independent_tests_gate: strict, independent tests that keep failing
// fail the task and say so.
func TestIndependentTestsStillFailing(t *testing.T) {
	dir := gitRepo(t)
	r := &reqRun{command: reqCheck(), noFix: true}
	o, _ := newOrc(t, dir, r.set(), func(c *config.Config) {
		reqCfg(c)
		c.Orchestrator.MaxFixRounds = 1
		c.Orchestrator.IndependentTestsGate = config.ReqGateStrict
	})
	res := o.Run(context.Background(), oneStepTask)
	if res.OK || !strings.Contains(res.Summary, "independent requirement tests still fail") {
		t.Fatalf("task: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "req_rwreq.txt")); err == nil {
		t.Error("the independent test file stayed in the tree")
	}
	if recs := reqChoices(t, o); len(recs) != 0 {
		t.Errorf("strict gate logged advisory choices: %+v", recs)
	}
}

// reqChoices are the task's independent-test gate choices.
func reqChoices(t *testing.T, o *Orchestrator) []sessionlog.Record {
	t.Helper()
	recs, err := sessionlog.ReadDir(o.logDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []sessionlog.Record
	for _, r := range recs {
		if r.Type == sessionlog.TypeChoice && r.Step == sessionlog.ChoiceReqTests {
			out = append(out, r)
		}
	}
	return out
}

// By default (independent_tests_gate: soft) failing independent tests get
// one fix round, even when more could follow; the fix agent's dispute is
// recorded, and tests that still fail after it are reported with the
// result instead of failing the task.
func TestIndependentTestsSoftGate(t *testing.T) {
	dir := gitRepo(t)
	r := &reqRun{command: reqCheck(), noFix: true,
		fixReply: "kept the change\n- DISPUTE: `req_rwreq.txt`: the task never asks for fix.txt"}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		reqCfg(c)
		c.Orchestrator.MaxFixRounds = 2
	})
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK || !strings.Contains(res.Summary, "independent tests still fail, advisory") {
		t.Fatalf("task: %+v\nlog:\n%s", res, strings.Join(logLines(rec), "\n"))
	}
	if len(r.fixes) != 1 {
		t.Errorf("fix rounds = %d, want 1: the tests fail a round only once", len(r.fixes))
	}
	recs := reqChoices(t, o)
	if len(recs) != 2 {
		t.Fatalf("choices = %+v", recs)
	}
	if d := recs[0]; d.Kind != sessionlog.ChoiceDisputed || d.Attempt != 1 || d.Reason != "req_rwreq.txt: the task never asks for fix.txt" {
		t.Errorf("dispute = %+v", d)
	}
	a := recs[1]
	if a.Kind != sessionlog.ChoiceAdvisory || a.Attempt != 2 || !strings.Contains(a.Reason, "still fail after their fix round") ||
		!strings.Contains(a.Reason, "the fix agent disputed req_rwreq.txt: the task never asks for fix.txt") {
		t.Errorf("advisory = %+v", a)
	}
	if _, err := os.Stat(filepath.Join(dir, "req_rwreq.txt")); err == nil {
		t.Error("the independent test file stayed in the tree")
	}
}

// Soft, with no fix round to give: failing independent tests are advisory
// from the start, and the log says why.
func TestIndependentTestsSoftGateNoFixRounds(t *testing.T) {
	dir := gitRepo(t)
	r := &reqRun{command: reqCheck()}
	o, _ := newOrc(t, dir, r.set(), func(c *config.Config) {
		reqCfg(c)
		c.Orchestrator.MaxFixRounds = 0
	})
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK || len(r.fixes) != 0 {
		t.Fatalf("fixes=%d task: %+v", len(r.fixes), res)
	}
	recs := reqChoices(t, o)
	if len(recs) != 1 || recs[0].Kind != sessionlog.ChoiceAdvisory || !strings.Contains(recs[0].Reason, "so no fix round can follow") {
		t.Errorf("choices = %+v", recs)
	}
}

func TestReqGateDecides(t *testing.T) {
	soft, strict := &reqGate{}, &reqGate{strict: true}
	for _, c := range []struct {
		g             *reqGate
		fixed         bool
		round, maxFix int
		want          bool
	}{
		{soft, false, 0, 1, true},
		{soft, false, 1, 1, false}, // the last round: no fix round can follow
		{soft, true, 1, 3, false},  // had its fix round
		{strict, true, 1, 3, true},
		{strict, false, 1, 1, true},
	} {
		c.g.fixed = c.fixed
		if got := c.g.decides(c.round, c.maxFix); got != c.want {
			t.Errorf("strict=%v fixed=%v round %d of %d: decides = %v", c.g.strict, c.fixed, c.round, c.maxFix, got)
		}
	}
}

func TestParseDisputes(t *testing.T) {
	got := parseDisputes("Fixed the rest.\r\n**DISPUTE: TestRwReqAzureTokenNotSentOnRedirect: the task does not ask to drop the token on redirects**\n" +
		"dispute: tests/test_rwreq.py::test_rw_req_limit: asks for 255, the task says 256\nDISPUTE:\nDISPUTE: TestRwReqBare\nno dispute here")
	want := []reqDispute{
		{"TestRwReqAzureTokenNotSentOnRedirect", "the task does not ask to drop the token on redirects"},
		{"tests/test_rwreq.py::test_rw_req_limit", "asks for 255, the task says 256"},
		{"TestRwReqBare", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("disputes = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dispute %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReqFailingTests(t *testing.T) {
	rep := "Requirements: - x -> TestRwReqA\nFAILED: independent tests: go test ./x\n" +
		"--- FAIL: TestRwReqA (0.00s)\n    --- FAIL: TestRwReqA/sub (0.00s)\n--- FAIL: TestRwReqB (0.01s)\n" +
		"FAILED tests/test_rwreq.py::test_rw_req_c - AssertionError\n--- PASS: TestRwReqD (0.00s)\n"
	got := strings.Join(reqFailingTests(rep), ",")
	if got != "TestRwReqA,TestRwReqB,tests/test_rwreq.py::test_rw_req_c" {
		t.Errorf("failing = %s", got)
	}
	g := &reqGate{fixed: true, disputes: []reqDispute{{"TestRwReqB", "stricter than the task"}, {"TestRwReqGone", "passes now"}}}
	why := g.advisoryWhy(rep)
	if !strings.Contains(why, "TestRwReqA, TestRwReqB, tests/test_rwreq.py::test_rw_req_c still fail after their fix round") ||
		!strings.Contains(why, "disputed TestRwReqB: stricter than the task") || strings.Contains(why, "TestRwReqGone") {
		t.Errorf("why = %s", why)
	}
}

// On by default, in place of the final review; turned off, no test writer
// runs.
func TestIndependentTestsOff(t *testing.T) {
	if !config.Default().Orchestrator.IndependentTests {
		t.Fatal("independent_tests is off by default")
	}
	dir := gitRepo(t)
	r := &reqRun{command: reqCheck()}
	o, _ := newOrc(t, dir, r.set(), func(c *config.Config) {
		reqCfg(c)
		c.Orchestrator.IndependentTests = false
	})
	if res := o.Run(context.Background(), oneStepTask); !res.OK || len(r.tester) != 0 {
		t.Fatalf("tester runs=%d: %+v", len(r.tester), res)
	}
}

// Without checks there is no runner for the tests: no writer, and the log
// says why.
func TestIndependentTestsNeedChecks(t *testing.T) {
	dir := gitRepo(t)
	r := &reqRun{}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		reqCfg(c)
		c.Verify.Commands = nil
	})
	o.Run(context.Background(), oneStepTask)
	if len(r.tester) != 0 || !logged(rec, "independent tests: none (verify.commands is empty") {
		t.Fatalf("tester runs=%d\nlog:\n%s", len(r.tester), strings.Join(logLines(rec), "\n"))
	}
}

// rw takes only new files with the tag in their name, never changes to
// existing files, and runs only a command agents may run anyway.
func TestCollectReqTests(t *testing.T) {
	dir := gitRepo(t)
	base := headOf(t, dir)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("changed by the writer\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "rwreq_test.go"), []byte("package pkg\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pkg", "helper_test.go"), []byte("package pkg\n"), 0o644)
	allowed := []string{"go test ./...", "go test"}
	final := "done\n**REQUIREMENTS:**\n- a -> TestRwReqA\n- b -> untested: needs a new name\n`COMMAND: go test ./pkg -run TestRwReq`"
	rt, err := collectReqTests(dir, base, final, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.Paths(); len(got) != 1 || got[0] != "pkg/rwreq_test.go" {
		t.Errorf("files = %v", got)
	}
	if rt.Command != "go test ./pkg -run TestRwReq" {
		t.Errorf("command = %q", rt.Command)
	}
	if !strings.Contains(rt.Requirements, "TestRwReqA") || !strings.Contains(rt.Requirements, "untested") {
		t.Errorf("requirements = %q", rt.Requirements)
	}
	notes := strings.Join(rt.Notes, "\n")
	if !strings.Contains(notes, "shared.txt") || !strings.Contains(notes, "helper_test.go") {
		t.Errorf("notes = %s", notes)
	}
	rt, _ = collectReqTests(dir, base, "COMMAND: go test ./pkg; rm -rf /", allowed)
	if rt.Command != "" || !strings.Contains(strings.Join(rt.Notes, "\n"), "not one of the checks") {
		t.Errorf("a command with shell syntax was kept: %q", rt.Command)
	}
	rt, _ = collectReqTests(dir, base, "COMMAND: curl example.com", allowed)
	if rt.Command != "" {
		t.Errorf("an unrelated command was kept: %q", rt.Command)
	}
}

func TestReqCommandAllowed(t *testing.T) {
	allowed := []string{"go test -count=1 ./internal/affected", "go test -count=1", "make check && echo ok"}
	for cmd, want := range map[string]bool{
		"go test -count=1 ./internal/affected":                true,
		"go test -count=1 ./internal/affected -run TestRwReq": true,
		"make check && echo ok":                               true, // a configured check as it is
		"make check && echo ok && curl x":                     false,
		"go test -count=1x":                                   false,
		"go test -count=1 ./x -run '^TestRwReq$'":             false,
		"go vet ./...":                                        false,
		"":                                                    false,
	} {
		if got := reqCommandAllowed(cmd, allowed); got != want {
			t.Errorf("reqCommandAllowed(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// Placing the tests never overwrites a file of the change, and removing
// them also removes the folders placing them created.
func TestPlaceReqTests(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "taken_rwreq.txt"), []byte("the worker's\n"), 0o644)
	rt := &ReqTests{Files: map[string][]byte{
		"taken_rwreq.txt":        []byte("mine\n"),
		"new/deep/rwreq_test.go": []byte("package deep\n"),
	}}
	remove, skipped, err := placeReqTests(root, rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "taken_rwreq.txt" {
		t.Errorf("skipped = %v", skipped)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "taken_rwreq.txt")); string(b) != "the worker's\n" {
		t.Errorf("overwrote the change's file: %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "deep", "rwreq_test.go")); err != nil {
		t.Fatal(err)
	}
	remove()
	if _, err := os.Stat(filepath.Join(root, "new")); err == nil {
		t.Error("the folder placing created is still there")
	}
	if _, err := os.Stat(filepath.Join(root, "taken_rwreq.txt")); err != nil {
		t.Error("removed the change's file")
	}
}

func TestSafeRel(t *testing.T) {
	for p, want := range map[string]bool{
		"a/rwreq_test.go": true, "rwreq.py": true,
		"../x": false, "/abs": false, "a/../b": false, ".git/hooks/x": false, "sub/.GIT/x": false, `a\b`: false, "": false,
	} {
		if got := safeRel(p); got != want {
			t.Errorf("safeRel(%q) = %v, want %v", p, got, want)
		}
	}
}
