package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/runner"
)

func TestParseCriteria(t *testing.T) {
	cases := []struct {
		name, task string
		want       []string
	}{
		{"none", "Fix the pagination helper", nil},
		{"plain", "Add retries.\n\nAcceptance criteria:\n- retries 3 times\n- gives up with an error\n", []string{"retries 3 times", "gives up with an error"}},
		{"markdown heading and checkboxes", "Add retries.\n\n## Done when\n\n1. [ ] retries 3 times\n2) [x] logs each retry\n", []string{"retries 3 times", "logs each retry"}},
		{"bold heading", "**Requirements:**\n* works on Windows\n", []string{"works on Windows"}},
		{"continuation", "Acceptance:\n- a long\n  criterion\n- b\n", []string{"a long criterion", "b"}},
		{"ends at text", "Acceptance criteria:\n- a\nThen also refactor x.\n- not a criterion\n", []string{"a"}},
		{"crlf", "Acceptance criteria:\r\n- a\r\n- b\r\n", []string{"a", "b"}},
		{"word inside a sentence is no heading", "The requirements: keep it short\n- a\n", nil},
	}
	for _, c := range cases {
		if got := ParseCriteria(c.task); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWithCriteria(t *testing.T) {
	task := WithCriteria("Add retries\n", []string{"retries 3 times", "  ", "gives\nup"})
	if got := ParseCriteria(task); !reflect.DeepEqual(got, []string{"retries 3 times", "gives up"}) {
		t.Errorf("round trip: %q from\n%s", got, task)
	}
	if WithCriteria("x", nil) != "x" {
		t.Error("no criteria changed the task")
	}
}

// A met requirement the reviewer approved anyway rejects; one it found
// unmet is added to the issues once.
func TestApplyRequirements(t *testing.T) {
	v := Verdict{Approve: true, Issues: []string{"missing: fallback"}, Requirements: []ReqVerdict{
		{ID: "R1", Requirement: "retries", Met: true}, {ID: "R2", Requirement: "fallback", Met: false}}}
	applyRequirements(&v)
	if v.Approve || len(v.Issues) != 1 {
		t.Errorf("verdict %+v", v)
	}
	ok := Verdict{Approve: true, Requirements: []ReqVerdict{{ID: "R1", Met: true}}}
	if applyRequirements(&ok); !ok.Approve {
		t.Error("all met rejected")
	}
}

func TestBuildAcceptance(t *testing.T) {
	found := func(file, name string) string {
		if name == "TestGone" {
			return "test TestGone not found in " + file
		}
		return ""
	}
	base := acceptanceInput{done: 1, steps: 1, allOK: true, edits: true, verifying: true, checksRan: true, verified: true,
		checkCmds: []string{"go test ./..."}, reviewed: true, testFound: found}
	reqs := []ReqVerdict{
		{ID: "R1", Requirement: "retries", Met: true, Evidence: "retry.go:10", TestFile: "retry_test.go", TestName: "TestRetry"},
		{ID: "R2", Requirement: "logs", Met: true, Evidence: "retry.go:20"},
		{ID: "R3", Requirement: "fallback", Met: true, Evidence: "retry.go:30", TestFile: "retry_test.go", TestName: "TestGone"},
	}

	in := base
	in.verdict = Verdict{Approve: true, Requirements: reqs}
	a := buildAcceptance(in)
	if a.Agents.Status != LevelPass || a.Checks.Status != LevelPass || a.Requirements.Status != LevelUnchecked || a.Explicit {
		t.Fatalf("levels %+v", a)
	}
	if got := statuses(a); !reflect.DeepEqual(got, []string{CritEvidence, CritEvidence, CritEvidence}) {
		t.Errorf("statuses %v", got)
	}
	if a.Criteria[1].Note != "no test named" || !strings.Contains(a.Criteria[2].Note, "not found") {
		t.Errorf("notes %q / %q", a.Criteria[1].Note, a.Criteria[2].Note)
	}
	if a.Requirements.Detail != "0/3 verified (3 with evidence but no passing test)" || a.SummaryPart() != "requirements 0/3 verified (3 with evidence but no passing test)" {
		t.Errorf("detail %q", a.Requirements.Detail)
	}

	// Failing checks: a named test does not verify anything.
	in = base
	in.verified = false
	in.verdict = Verdict{Requirements: reqs[:1]}
	a = buildAcceptance(in)
	if a.Checks.Status != LevelFail || a.Criteria[0].Status != CritEvidence || a.Criteria[0].Note != "the checks did not pass" {
		t.Errorf("failing checks: %+v", a)
	}

	// No checks configured: rw never ran the test.
	in = base
	in.verifying, in.checksRan = false, false
	in.verdict = Verdict{Requirements: reqs[:1]}
	a = buildAcceptance(in)
	if a.Checks.Status != LevelUnchecked || a.Criteria[0].Status != CritEvidence || !strings.Contains(a.Criteria[0].Note, "no checks configured") {
		t.Errorf("no checks: %+v", a)
	}

	// Unmet fails the level.
	in = base
	in.verdict = Verdict{Requirements: []ReqVerdict{reqs[0], {ID: "R2", Requirement: "logs", Met: false}}}
	if a = buildAcceptance(in); a.Requirements.Status != LevelFail || a.Criteria[1].Status != CritUnmet {
		t.Errorf("unmet: %+v", a)
	}

	// Even a test command passing does not prove that the cited test ran.
	in = base
	in.verdict = Verdict{Requirements: reqs[:1]}
	if a = buildAcceptance(in); a.Requirements.Status != LevelUnchecked || a.Criteria[0].Status != CritEvidence || a.Criteria[0].Note != "individual test execution was not confirmed" {
		t.Errorf("command result certified a test: %+v", a)
	}

	// Explicit criteria are matched by id; an unjudged one stays unchecked,
	// and the person's wording wins over the reviewer's.
	in = base
	in.criteria = []string{"retries three times", "logs each retry"}
	in.verdict = Verdict{Requirements: reqs[:1]}
	a = buildAcceptance(in)
	if !a.Explicit || len(a.Criteria) != 2 || a.Criteria[0].Text != "retries three times" || a.Criteria[0].Status != CritEvidence ||
		a.Criteria[1].Status != CritUnchecked || a.Criteria[1].Note != "the reviewer did not judge it" {
		t.Errorf("explicit: %+v", a.Criteria)
	}

	// No review: explicit criteria are listed, unchecked, with the reason;
	// derived ones do not exist.
	in = base
	in.reviewed, in.why = false, "the final review was skipped: small"
	in.criteria = []string{"retries"}
	a = buildAcceptance(in)
	if a.Requirements.Status != LevelUnchecked || a.Criteria[0].Status != CritUnchecked || a.Criteria[0].Note != in.why {
		t.Errorf("skipped review: %+v", a)
	}
	in.criteria = nil
	if a = buildAcceptance(in); len(a.Criteria) != 0 || a.Requirements.Detail != in.why || a.SummaryPart() != "" {
		t.Errorf("skipped review, no criteria: %+v", a)
	}

	// A failed subtask fails the agent level; a read-only task checks nothing.
	in = base
	in.done, in.steps, in.allOK = 1, 2, false
	if a = buildAcceptance(in); a.Agents.Status != LevelFail || a.Agents.Detail != "1/2 subtasks finished" {
		t.Errorf("agents: %+v", a.Agents)
	}
	in = base
	in.edits, in.reviewed, in.why = false, false, "a read-only task: no final review"
	if a = buildAcceptance(in); a != nil {
		t.Errorf("read-only question: %+v", a)
	}
	in.criteria = []string{"names every caller"}
	if a = buildAcceptance(in); a.Checks.Status != LevelUnchecked || a.Requirements.Status != LevelUnchecked {
		t.Errorf("read-only with criteria: %+v", a)
	}
}

func statuses(a *Acceptance) []string {
	var out []string
	for _, c := range a.Criteria {
		out = append(out, c.Status)
	}
	return out
}

func TestTestFoundIn(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "x_test.go"), []byte("package pkg\nfunc TestRetry(t *testing.T) {}\n"), 0o644)
	extra := t.TempDir()
	os.WriteFile(filepath.Join(extra, "a.test.ts"), []byte("it('retries', () => {})\n"), 0o644)
	tk := &task{dir: dir, repos: []*task{{dir: extra, repoName: "web"}}}
	found := tk.testFoundIn()
	cases := []struct{ file, name, want string }{
		{"pkg/x_test.go", "TestRetry", ""},
		{"pkg/x_test.go:2", "TestRetry/sub case", ""},
		{"`pkg/x_test.go`", "`TestRetry`", ""},
		{"pkg/x_test.go", "TestOther", "test TestOther not found in pkg/x_test.go"},
		{"pkg/none_test.go", "TestRetry", "test file pkg/none_test.go not found"},
		{"../outside_test.go", "TestRetry", "test file ../outside_test.go is outside the repo"},
		{"web: a.test.ts", "retries", ""},
		{"a.test.ts", "retries", ""}, // found in any repo of the task
	}
	for _, c := range cases {
		if got := found(c.file, c.name); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.file, c.name, got, c.want)
		}
	}
	if got := found(filepath.Join(dir, "pkg", "x_test.go"), "TestRetry"); !strings.Contains(got, "not a repo path") {
		t.Errorf("absolute path: %q", got)
	}
}

func TestFinalReviewPromptCriteria(t *testing.T) {
	derived := finalReviewPrompt("task", Plan{Summary: "s"}, nil, "stat", "diff", nil, "", "", nil)
	explicit := finalReviewPrompt("task", Plan{Summary: "s"}, nil, "stat", "diff", nil, "", "", []string{"retries", "logs"})
	for _, p := range []string{derived, explicit} {
		if !strings.Contains(p, `"requirements": [{"id": "R1"`) || !strings.Contains(p, "test_file") {
			t.Errorf("prompt lacks the requirements reply:\n%s", p)
		}
	}
	if !strings.Contains(derived, "with ids R1, R2") || strings.Contains(derived, "ACCEPTANCE CRITERIA") {
		t.Errorf("derived prompt:\n%s", derived)
	}
	if !strings.Contains(explicit, "ACCEPTANCE CRITERIA") || !strings.Contains(explicit, "R1. retries\nR2. logs\n") || !strings.Contains(explicit, "Judge each ACCEPTANCE CRITERION") {
		t.Errorf("explicit prompt:\n%s", explicit)
	}
}

// acceptRun scripts a one-step task with acceptance criteria: the worker
// writes out.txt and a test, the reviewer judges with requirements.
type acceptRun struct {
	mu      sync.Mutex
	reviews []string
	fixes   int
	reply   func(n int) string // the n-th final review's reply (1-based)
}

func (r *acceptRun) set() runner.Set {
	return both(func(s runner.Spec) runner.Result {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			r.reviews = append(r.reviews, s.Prompt)
			return runner.Result{Final: r.reply(len(r.reviews))}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview):
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFix):
			r.fixes++
		}
		os.WriteFile(filepath.Join(s.Dir, "out.txt"), []byte("work\n"), 0o644)
		os.WriteFile(filepath.Join(s.Dir, "out_test.go"), []byte("package x\nfunc TestOut(t *testing.T) {}\n"), 0o644)
		return runner.Result{Final: "done"}
	})
}

const acceptTask = oneStepTask + "\n\nAcceptance criteria:\n- the last page is kept\n- an empty list has no pages\n"

// The criteria reach the reviewer, the small change is still reviewed,
// and the result keeps the three levels apart.
func TestAcceptanceEndToEnd(t *testing.T) {
	dir := gitRepo(t)
	r := &acceptRun{reply: func(int) string {
		return "```json\n" + `{"approve": true, "advice": "", "issues": [], "requirements": [
			{"id": "R1", "requirement": "last page", "met": true, "evidence": "out.txt:1", "test_file": "out_test.go", "test_name": "TestOut"},
			{"id": "R2", "requirement": "empty list", "met": true, "evidence": "out.txt:1", "test_file": "", "test_name": ""}]}` + "\n```"
	}}
	o, rec := newOrc(t, dir, r.set(), func(c *config.Config) {
		smallSkip(c)
		c.Verify.Commands = []string{fileCheck("out.txt")}
	})
	res := o.Run(context.Background(), acceptTask)
	if !res.OK || len(r.reviews) != 1 {
		t.Fatalf("reviews=%d: %+v", len(r.reviews), res)
	}
	if !strings.Contains(r.reviews[0], "R1. the last page is kept\nR2. an empty list has no pages") {
		t.Errorf("review prompt lacks the criteria:\n%s", r.reviews[0])
	}
	if !logged(rec, "final review runs: the task lists acceptance criteria") {
		t.Errorf("log:\n%s", strings.Join(logLines(rec), "\n"))
	}
	a := res.Acceptance
	if a == nil || !a.Explicit || a.Agents.Status != LevelPass || a.Checks.Status != LevelPass || a.Requirements.Status != LevelUnchecked {
		t.Fatalf("acceptance %+v", a)
	}
	if got := statuses(a); !reflect.DeepEqual(got, []string{CritEvidence, CritEvidence}) || a.Criteria[0].Text != "the last page is kept" || a.Criteria[0].Test != "out_test.go: TestOut" {
		t.Errorf("criteria %+v", a.Criteria)
	}
	if !strings.Contains(res.Summary, "checks pass; requirements 0/2 verified") {
		t.Errorf("summary %q", res.Summary)
	}
	lines := strings.Join(a.Lines(), "\n")
	for _, want := range []string{"agent finished:        ✓ 1/1 subtasks finished", "checks passed:         ✓ passed: ", "requirements verified: ? 0/2 verified",
		"~ R1 the last page is kept", "test: out_test.go: TestOut; evidence: out.txt:1", "~ R2 an empty list has no pages", "(no test named)"} {
		if !strings.Contains(lines, want) {
			t.Errorf("lines lack %q:\n%s", want, lines)
		}
	}
}

// A requirement the reviewer marks unmet starts a fix round although it
// voted to approve.
func TestAcceptanceUnmetStartsFix(t *testing.T) {
	dir := gitRepo(t)
	r := &acceptRun{reply: func(n int) string {
		met := map[bool]string{true: "true", false: "false"}[n > 1]
		return `{"approve": true, "advice": "", "issues": [], "requirements": [{"id": "R1", "requirement": "last page", "met": true, "evidence": "out.txt:1", "test_file": "out_test.go", "test_name": "TestOut"}, {"id": "R2", "requirement": "empty list", "met": ` + met + `, "evidence": "", "test_file": "", "test_name": ""}]}`
	}}
	o, _ := newOrc(t, dir, r.set(), func(c *config.Config) {
		smallSkip(c) // the review judges the criteria
		c.Verify.Commands = []string{fileCheck("out.txt")}
	})
	res := o.Run(context.Background(), acceptTask)
	if !res.OK || r.fixes != 1 || len(r.reviews) != 2 {
		t.Fatalf("fixes=%d reviews=%d: %+v", r.fixes, len(r.reviews), res)
	}
	if a := res.Acceptance; a.Criteria[1].Status != CritUnchecked || a.Criteria[1].Note != "the reviewer gave no evidence" {
		t.Errorf("after the fix: %+v", a.Criteria[1])
	}
}

// The acceptance is saved with the task, for rw report and the history.
func TestAcceptanceSaved(t *testing.T) {
	dir := gitRepo(t)
	r := &acceptRun{reply: func(int) string { return `{"approve": true}` }}
	o, _ := newOrc(t, dir, r.set(), shortcuts)
	var id string
	res := o.RunWith(context.Background(), acceptTask, TaskOptions{Started: func(s string) { id = s }})
	st, err := LoadTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Acceptance == nil || !reflect.DeepEqual(st.Acceptance, res.Acceptance) {
		t.Errorf("saved %+v, result %+v", st.Acceptance, res.Acceptance)
	}
	// The reviewer listed nothing: the explicit criteria stay, unjudged.
	if a := st.Acceptance; a.Checks.Status != LevelUnchecked || len(a.Criteria) != 2 || a.Criteria[0].Status != CritUnchecked {
		t.Errorf("acceptance %+v", a)
	}
}
