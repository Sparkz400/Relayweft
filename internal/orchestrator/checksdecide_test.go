package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
)

// checksRun is a one-step task whose check passes once the fix agent ran
// (fixes: how many fix agents ran; fixWrites: whether a fix writes the
// file the check wants).
type checksRun struct {
	mu                     sync.Mutex
	finals, fixes, workers int
	fixWrites              bool
	fixPrompts             []string
}

func (r *checksRun) set() runner.Set {
	return both(func(s runner.Spec) runner.Result {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			r.finals++
			return runner.Result{Final: `{"approve": false, "advice": "write fixed.txt"}`}
		case strings.Contains(s.Prompt, runner.MarkerFix):
			r.fixes++
			r.fixPrompts = append(r.fixPrompts, s.Prompt)
			if r.fixWrites {
				os.WriteFile(filepath.Join(s.Dir, "fixed.txt"), []byte("x\n"), 0o644)
			}
			return runner.Result{Final: "fixed", Files: []string{"fixed.txt"}}
		}
		r.workers++
		os.WriteFile(filepath.Join(s.Dir, "out.txt"), []byte("x\n"), 0o644)
		return runner.Result{Final: "done", Files: []string{"out.txt"}}
	})
}

// review_when: untested (the default): the checks decide and no final
// review runs; the fix agent gets what fails, not a reviewer's advice.
func TestChecksReplaceReview(t *testing.T) {
	dir := gitRepo(t)
	r := &checksRun{fixWrites: true}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		shortcuts(c)
		c.Verify.Commands = []string{fileCheck("fixed.txt")}
	})
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK || r.finals != 0 || r.fixes != 1 {
		t.Fatalf("final reviews=%d fixes=%d: %+v", r.finals, r.fixes, res)
	}
	for _, want := range []string{"final review skipped: the checks fail: their output advises the fix round (review_when: untested)", "final review skipped: the checks pass"} {
		if !logged(rec, want) {
			t.Errorf("log lacks %q:\n%s", want, strings.Join(logLines(rec), "\n"))
		}
	}
	if p := r.fixPrompts[0]; !strings.Contains(p, "WHAT FAILS:") || !strings.Contains(p, "missing fixed.txt") || strings.Contains(p, "reviewer") {
		t.Errorf("fix prompt %q", p)
	}
	if strings.Contains(res.Summary, "review") || !strings.Contains(res.Summary, "checks pass") {
		t.Errorf("summary %q", res.Summary)
	}
}

// review_when: failing: a failing check run gets a review that advises the
// fix round; once the checks pass, no review runs.
func TestReviewOnlyWhenChecksFail(t *testing.T) {
	dir := gitRepo(t)
	r := &checksRun{fixWrites: true}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		shortcuts(c)
		c.Orchestrator.ReviewWhen = config.ReviewFailing
		c.Verify.Commands = []string{fileCheck("fixed.txt")}
	})
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK || r.finals != 1 || r.fixes != 1 {
		t.Fatalf("final reviews=%d fixes=%d: %+v", r.finals, r.fixes, res)
	}
	for _, want := range []string{"final review runs: the checks fail: the reviewer advises the fix round", "final review skipped: the checks pass (review_when: failing)"} {
		if !logged(rec, want) {
			t.Errorf("log lacks %q:\n%s", want, strings.Join(logLines(rec), "\n"))
		}
	}
	if !strings.Contains(res.Summary, "final review skipped (the checks pass)") {
		t.Errorf("summary %q", res.Summary)
	}
}

// After the last fix round no review runs: nothing would follow its advice.
func TestNoReviewAfterLastRound(t *testing.T) {
	dir := gitRepo(t)
	r := &checksRun{}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		shortcuts(c)
		c.Orchestrator.ReviewWhen = config.ReviewFailing
		c.Verify.Commands = []string{fileCheck("fixed.txt")}
	})
	res := o.Run(context.Background(), oneStepTask)
	if res.OK || r.finals != 1 || r.fixes != 1 {
		t.Fatalf("final reviews=%d fixes=%d: %+v", r.finals, r.fixes, res)
	}
	if !logged(rec, "final review skipped: no fix round is left for a review to advise") || !strings.Contains(res.Summary, "checks still fail") {
		t.Errorf("summary %q, log:\n%s", res.Summary, strings.Join(logLines(rec), "\n"))
	}
}

// A repo without verify.commands gets its checks from its build files;
// with verify.auto off it has none, and the reviewer decides: the one case
// review_when: untested still reviews.
func TestChecksDetectedFromBuildFiles(t *testing.T) {
	for _, auto := range []bool{true, false} {
		dir := gitRepo(t)
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.21\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644)
		r := &checksRun{}
		o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
			shortcuts(c)
			c.Verify.Commands, c.Verify.Auto = nil, auto
		})
		res := o.Run(context.Background(), oneStepTask)
		detected := logged(rec, "checks: no verify.commands: detected go build ./..., go test ./... from the build files")
		if detected != auto {
			t.Errorf("auto=%v: detected=%v, log:\n%s", auto, detected, strings.Join(logLines(rec), "\n"))
		}
		if auto && (!res.OK || r.finals != 0 || !logged(rec, "verify ✓ go test ./...")) {
			t.Errorf("auto: final reviews=%d: %+v\n%s", r.finals, res, strings.Join(logLines(rec), "\n"))
		}
		if !auto && (r.finals == 0 || !logged(rec, "final review runs: no checks or tests to decide, so the reviewer does")) {
			t.Errorf("no checks: final reviews=%d\n%s", r.finals, strings.Join(logLines(rec), "\n"))
		}
	}
}

// Without a review, tests written first supply evidence, but a passing
// command alone cannot verify their individual requirements.
func TestAcceptanceFromTestsFirst(t *testing.T) {
	tests := []AcceptanceTest{{Requirement: "keeps the last page", File: "page_test.go", Name: "TestLastPage"}, {Requirement: "empty list", File: "page_test.go", Name: "TestEmpty"}}
	in := acceptanceInput{done: 1, steps: 1, allOK: true, edits: true, verifying: true, checksRan: true, verified: true,
		checkCmds: []string{"go test ./..."}, why: "the final review was skipped: the checks pass", firstTests: tests}
	a := buildAcceptance(in)
	if a.Requirements.Status != LevelUnchecked || len(a.Criteria) != 2 || a.Criteria[0].Test != "page_test.go: TestLastPage" ||
		a.Criteria[0].Status != CritEvidence || a.Criteria[0].Note != "individual test execution was not confirmed" {
		t.Errorf("passing: %+v", a)
	}
	in.verified = false
	if a := buildAcceptance(in); a.Requirements.Status != LevelUnchecked || a.Criteria[1].Status != CritUnchecked {
		t.Errorf("failing: %+v", a)
	}
}

func TestReviewWhenDefaults(t *testing.T) {
	c := config.Default()
	if c.Orchestrator.FinalReview() != config.ReviewUntested || !c.Orchestrator.IndependentTests || !c.Verify.Auto {
		t.Errorf("review_when %q, independent_tests %v, verify.auto %v", c.Orchestrator.ReviewWhen, c.Orchestrator.IndependentTests, c.Verify.Auto)
	}
	if (config.OrchestratorCfg{}).FinalReview() != config.ReviewUntested {
		t.Error("an empty review_when is not untested")
	}
	c.Orchestrator.Classic()
	if c.Orchestrator.FinalReview() != config.ReviewAlways || c.Orchestrator.IndependentTests {
		t.Errorf("classic review_when %q, independent_tests %v", c.Orchestrator.ReviewWhen, c.Orchestrator.IndependentTests)
	}
}

// review_when: untested skips the final review whenever there are checks,
// and says which of them decided.
func TestUntestedReviewReasons(t *testing.T) {
	tk := &task{cfg: config.Default()}
	o := &Orchestrator{}
	for _, tc := range []struct {
		verifying, verified bool
		tests               string
		allOK, skip         bool
		why                 string
	}{
		{false, false, reqTestsNone, true, false, "no checks or tests to decide, so the reviewer does"},
		{true, true, reqTestsNone, true, true, "the checks pass"},
		{true, true, reqTestsPass, true, true, "the checks and independent tests pass"},
		{true, true, reqTestsAdvise, true, true, "the checks pass; the independent tests that fail only advise"},
		{true, true, reqTestsFail, true, true, "the independent tests fail: their output advises the fix round (review_when: untested)"},
		{true, false, reqTestsPass, true, true, "the checks fail: their output advises the fix round (review_when: untested)"},
		{true, false, reqTestsFail, true, true, "the checks and independent tests fail: their output advises the fix round (review_when: untested)"},
		{true, true, reqTestsPass, false, true, "a step failed, so the task fails whatever a review says (review_when: untested)"},
	} {
		skip, why := o.skipFinalReview(tk, tc.verifying, tc.verified, tc.tests, tc.allOK, false, false)
		if skip != tc.skip || why != tc.why {
			t.Errorf("%+v: skip=%v why=%q", tc, skip, why)
		}
	}
}

// With review_when: untested a task with checks is estimated with the test
// writer, on the other provider, in place of the final review; without
// checks, with the review.
func TestUntestedEstimate(t *testing.T) {
	o, _ := newOrc(t, "", runner.Set{event.Codex: scripted{}, event.Claude: scripted{}}, shortcuts)
	cfg := o.opts.Store.Get()
	cfg.Orchestrator.IndependentTests = true
	cfg.Verify.Commands = []string{"go test ./..."}
	tk := &task{cfg: cfg, runners: o.opts.Runners(cfg), root: t.TempDir()}
	plan := Plan{Subtasks: []Subtask{{ID: "w", Title: "work", Kind: router.KindEdit, Prompt: "change it"}}}
	e := o.estimatePlan(tk, nil, plan)
	w, okW := e.Step(ReqTestsID)
	if _, ok := e.Step(FinalReviewID); ok || !okW || !strings.HasPrefix(w.Route, "claude:") {
		t.Errorf("with checks: steps %+v", e.Steps)
	}
	cfg.Verify.Commands = nil
	e = o.estimatePlan(tk, nil, plan)
	if _, ok := e.Step(FinalReviewID); !ok || len(e.Steps) != 2 {
		t.Errorf("without checks: steps %+v", e.Steps)
	}
}
