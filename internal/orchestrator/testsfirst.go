package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
)

// Tests first (rw run --tests-first, orchestrator.tests_first): before any
// code is written, an agent writes acceptance tests for every requirement
// the task states, on another provider than the one that implements it, so
// the tests do not share the implementer's blind spots. On the bench,
// agents wrote plausible code that missed a named requirement and the
// review approved it; tests in the prompt were the one lever that helped.
//
//   - Only test files may change: other changes of the test writer are
//     undone, so no code exists before the tests.
//   - The tests run once before the work: they should fail. Tests that
//     pass already test nothing new, and rw says so.
//   - Their command joins the task's checks (verify.commands): the verify
//     and fix rounds run it, and writers may run it.
//   - Writers are told not to change the tests, and a change is put back
//     before each verify run, so the work cannot pass by weakening them.
//   - With no tests written, rw asks the person at the terminal to write
//     them; with nobody to ask, the task stops before any code is written.

// TestsStepID is the agent id of the test writer.
const TestsStepID = "tests"

// AcceptanceTest is one test the test writer wrote and the requirement it
// checks.
type AcceptanceTest struct {
	Requirement string `json:"requirement"`
	File        string `json:"file,omitempty"`
	Name        string `json:"name,omitempty"`
}

// TestsState is a tests-first task's acceptance tests, saved with the
// task so a resume keeps guarding them.
type TestsState struct {
	// Files are slash paths from the repo's top folder (from the project
	// folder outside git).
	Files []string `json:"files"`
	// Command runs the tests from the project folder; "" when the repo's
	// own checks run them.
	Command string `json:"command,omitempty"`
	// Commit is the snapshot that holds the tests as written (git only).
	Commit string           `json:"commit,omitempty"`
	Tests  []AcceptanceTest `json:"tests,omitempty"`
	Route  string           `json:"route,omitempty"` // the test writer's
}

// TestsQuestion is what a TestsApprover is asked: to approve the tests
// written (Why == ""), or to write them because none were (Why says why).
type TestsQuestion struct {
	Task    string           `json:"task"`
	Tests   []AcceptanceTest `json:"tests,omitempty"`
	Files   []string         `json:"files,omitempty"`
	Command string           `json:"command,omitempty"`
	// Red is how the tests did before any code: "fail" (as they should),
	// "pass" (they may test nothing new) or "" (not run).
	Red string `json:"red,omitempty"`
	Why string `json:"why,omitempty"`
}

// TestsAnswer is the person's answer to a TestsQuestion.
type TestsAnswer struct {
	// OK goes on; false stops the task before any code is written.
	OK bool
	// Command runs the tests ("" keeps the test writer's, or the repo's
	// checks).
	Command string
}

// TestsApprover is an Approver that can show a tests-first task's tests
// before any code is written, and ask the person for tests when none were
// written. Approvers without it are never asked.
type TestsApprover interface {
	ApproveTests(ctx context.Context, q TestsQuestion) TestsAnswer
}

// acceptTests are the acceptance tests of a running task.
type acceptTests struct {
	TestsState
	base    string            // the folder Files are relative to
	content map[string][]byte // each file as written: guardTests puts it back
}

// testsReply is the test writer's JSON reply.
type testsReply struct {
	Tests      []AcceptanceTest `json:"tests"`
	Files      []string         `json:"files"`
	Command    string           `json:"command"`
	Untestable []string         `json:"untestable"`
	Cannot     string           `json:"cannot"`
}

// testsAsker is who is asked about the tests: the task's own asker (the
// terminal of rw run --tests-first), else the approver if it can ask.
// Unattended tasks ask nobody.
func (o *Orchestrator) testsAsker(t *task) TestsApprover {
	if t.unattended {
		return nil
	}
	if t.testsAsker != nil {
		return t.testsAsker
	}
	a, _ := o.opts.Approver.(TestsApprover)
	return a
}

// testsRoute is the test writer's route: the worker route on the next
// provider that can write, other than the one the work routes to.
func (o *Orchestrator) testsRoute(t *task) event.Decision {
	impl := o.router.Route(router.Step{ID: "work", Title: "work", Kind: router.KindEdit, Prompt: t.text, Files: t.shape.files, MainProvider: t.mainProv})
	for _, q := range t.cfg.Alternatives(impl.Provider) {
		route := t.cfg.Roles[event.RoleWorker].For(q)
		if t.cfg.Providers[q].OnlyPreferred || route.Model == "" {
			continue
		}
		d := event.Decision{StepID: TestsStepID, StepTitle: "acceptance tests", Role: event.RoleWorker, Provider: q, Model: route.Model, Effort: route.Effort,
			Rule: router.RuleTestsFirst, Confidence: 1,
			Reason: fmt.Sprintf("tests first: the worker route on %s, not %s, which implements the task, so the tests do not share its blind spots", q, impl.Provider)}
		if o.bestOfUnusable(t, d) == "" {
			return d
		}
	}
	d := impl
	d.StepID, d.StepTitle, d.Rule, d.Confidence = TestsStepID, "acceptance tests", router.RuleTestsFirst, 1
	d.Reason = fmt.Sprintf("tests first: no other provider can write files now, so %s writes the tests in a fresh session that has seen no implementation", impl.Provider)
	return d
}

// writeTests is the tests-first phase, before the plan. ok is false when
// the task stops before any code is written, with res saying why.
func (o *Orchestrator) writeTests(ctx context.Context, t *task) (res TaskResult, ok bool) {
	o.emit(event.Event{Kind: event.Phase, Text: "tests"})
	if t.state != nil {
		t.state.TestsFirst, t.state.Phase = true, "tests"
		t.state.save()
	}
	if len(t.repos) > 0 {
		return TaskResult{Summary: "tests first takes a one-repo task: nothing was run"}, false
	}
	at := &acceptTests{base: o.opts.Dir}
	if t.useGit {
		at.base = t.root
	}
	d := o.testsRoute(t)
	at.Route = d.Label()
	o.logf("tests first: %s writes acceptance tests before any code (%s)", d.Label(), d.Reason)
	reply, files, why := o.runTestWriter(ctx, t, d)
	if ctx.Err() != nil {
		return TaskResult{Summary: "cancelled while the acceptance tests were written"}, false
	}
	var tests, other []string
	for _, f := range files {
		if isTestPath(f) {
			tests = append(tests, f)
		} else {
			other = append(other, f)
		}
	}
	if len(other) > 0 {
		o.undoNonTest(t, at, other)
	}
	at.Tests = reply.Tests
	if len(tests) == 0 && why == "" {
		why = "the test writer wrote no test files"
		if c := strings.TrimSpace(reply.Cannot); c != "" {
			why = c
		}
	}
	if why == "" {
		at.Command = strings.TrimSpace(reply.Command)
		if at.Command != "" {
			if bad := testCommandProblem(at.Command, t.cfg.Verify.Commands); bad != "" {
				o.logf("tests first: not running the test writer's command %q (%s); the repo's checks run the tests", clip(at.Command, 200), bad)
				at.Command = ""
			}
		}
		if at.Command == "" && len(t.cfg.Verify.Commands) == 0 {
			why = "the tests were written, but there is no command to run them (no verify.commands, and the test writer named none rw may run)"
		}
	}
	if len(reply.Untestable) > 0 {
		o.logf("tests first: no automated test for: %s", clip(strings.Join(reply.Untestable, "; "), 600))
	}

	asker := o.testsAsker(t)
	q := TestsQuestion{Task: t.text, Tests: at.Tests, Files: tests, Command: at.Command}
	switch {
	case why != "" && asker == nil:
		return TaskResult{Summary: "tests first: no acceptance tests (" + clip(why, 300) + "); no code was written. " +
			"Write the tests, add the command that runs them to verify.commands, and run the task again, or run it without --tests-first"}, false
	case why != "":
		o.logf("tests first: no acceptance tests (%s); asking you for them", clip(why, 300))
		q.Why = why
		ans := asker.ApproveTests(ctx, q)
		if ctx.Err() != nil {
			return TaskResult{Summary: "cancelled while waiting for acceptance tests"}, false
		}
		if !ans.OK {
			return TaskResult{Summary: "tests first: no acceptance tests: no code was written"}, false
		}
		// The person's command is theirs to choose: it is not checked.
		at.Command = strings.TrimSpace(ans.Command)
		if at.Command == "" && len(t.cfg.Verify.Commands) == 0 {
			return TaskResult{Summary: "tests first: no command runs the acceptance tests: no code was written"}, false
		}
	default:
		if at.Command != "" {
			q.Red = o.redRun(ctx, t, at.Command)
		}
		if asker != nil && o.approving(t) {
			o.emit(event.Event{Kind: event.Phase, Text: "approve-tests"})
			o.logf("waiting for you to approve the acceptance tests")
			ans := asker.ApproveTests(ctx, q)
			if ctx.Err() != nil {
				return TaskResult{Summary: "cancelled at test approval"}, false
			}
			if !ans.OK {
				return TaskResult{Summary: "acceptance tests not approved: no code was written (the tests stay in your tree; rw undo removes them)"}, false
			}
			if c := strings.TrimSpace(ans.Command); c != "" {
				at.Command = c
			}
		}
	}
	// The files as they are now: the person may have written or changed
	// tests while asked.
	if t.useGit {
		now, err := o.agentChanges(t)
		if err == nil {
			tests = slices.DeleteFunc(now, func(f string) bool { return !isTestPath(f) })
		}
	}
	at.Files = tests
	at.content = map[string][]byte{}
	for _, f := range at.Files {
		if b, err := os.ReadFile(filepath.Join(at.base, filepath.FromSlash(f))); err == nil {
			at.content[f] = b
		} else {
			delete(at.content, f) // deleted: nothing to guard
		}
	}
	at.Files = slices.DeleteFunc(at.Files, func(f string) bool { _, ok := at.content[f]; return !ok })
	t.noteFiles(at.base, at.Files)
	if t.useGit {
		// Writers in pool worktrees start from the snapshot: it must hold
		// the tests. The task's start stays, so undo and the final diff
		// include them.
		if snap, err := (git{t.root}).snapshot("relayweft: acceptance tests"); err == nil {
			t.snapshot, at.Commit = snap, snap
		} else {
			o.logf("tests first: could not snapshot the tests (%v); writers work in your tree, not in worktrees", err)
			t.wtOK = false
		}
	}
	o.useTests(t, at)
	if t.cfg.Orchestrator.IndependentTests {
		// The writers see these tests, so a second, hidden set written
		// next to them (reqtests.go) would only cost another agent.
		t.cfg.Orchestrator.IndependentTests = false
		o.logf("independent tests: off for this task, whose acceptance tests were written first")
	}
	if t.state != nil {
		ts := at.TestsState
		t.state.Tests = &ts
		t.state.save()
	}
	what := fmt.Sprintf("%d test file(s)", len(at.Files))
	if n := len(at.Tests); n > 0 {
		what = fmt.Sprintf("%d test(s) in %s", n, what)
	}
	if len(at.Files) > 0 {
		what += ": " + clip(strings.Join(at.Files, ", "), 300)
	}
	run := at.Command
	if run == "" {
		run = "the repo's checks"
	}
	o.logf("tests first: %s; run by %s", what, run)
	return TaskResult{}, true
}

// runTestWriter runs the test writer and returns its reply and the files
// it changed. why is set when it did not finish.
func (o *Orchestrator) runTestWriter(ctx context.Context, t *task, d event.Decision) (reply testsReply, files []string, why string) {
	step := router.Step{ID: TestsStepID, Title: "acceptance tests", Kind: router.KindEdit, Prompt: t.text, Pin: &d}
	prompt := testsPrompt(t.text, t.cfg.Verify.Commands, t.docsContextMax(reviewDocsMax))
	var res runner.Result
	limits := 0
	for attempt := 1; ; attempt++ {
		_, res = o.runAgent(ctx, t, step, TestsStepID, AgentMain, o.opts.Dir, prompt, attempt)
		if res.OK() || res.Killed || ctx.Err() != nil {
			break
		}
		if res.LimitHit && limits < 2 {
			limits++
			step.Pin = nil // routed as usual: rule 1 moves it off the limited provider
			attempt--
			continue
		}
		if attempt >= max(1, t.cfg.Orchestrator.MaxAttempts) {
			break
		}
	}
	if res.Err != nil {
		why = "the test writer failed: " + clip(res.Err.Error(), 300)
	}
	if err := extractJSON(res.Final, &reply); err != nil && why == "" && res.OK() {
		o.logf("tests first: the test writer's reply has no JSON (%v); using the files it changed", err)
	}
	if t.useGit {
		changed, err := o.agentChanges(t)
		if err == nil {
			return reply, changed, why
		}
		o.logf("tests first: could not list the changed files (%v); using what the test writer reported", err)
	}
	seen := map[string]bool{}
	for _, f := range append(append([]string(nil), reply.Files...), res.Files...) {
		if filepath.IsAbs(f) {
			rel, err := filepath.Rel(o.opts.Dir, f)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			f = rel
		}
		f = filepath.ToSlash(filepath.Clean(f))
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	return reply, files, why
}

// agentChanges lists the files changed since the snapshot (slash paths
// from the repo's top folder).
func (o *Orchestrator) agentChanges(t *task) ([]string, error) {
	files, _, err := (git{t.root}).changedSince(t.snapshot)
	return files, err
}

// undoNonTest undoes the test writer's changes outside test files: no
// code may exist before the tests.
func (o *Orchestrator) undoNonTest(t *task, at *acceptTests, files []string) {
	var undone, failed []string
	for _, f := range files {
		p := filepath.Join(at.base, filepath.FromSlash(f))
		var err error
		if t.useGit {
			g := git{t.root}
			var in string
			if in, err = g.out("ls-tree", "--name-only", t.snapshot, "--", f); err == nil && in == "" {
				err = os.Remove(p) // new since the snapshot
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
			} else if err == nil {
				var b string
				if b, err = g.run(nil, nil, "cat-file", "--filters", t.snapshot+":"+f); err == nil {
					err = writeKeepMode(p, []byte(b))
				}
			}
		} else {
			err = errors.New("not a git repo")
		}
		if err != nil {
			failed = append(failed, f)
		} else {
			undone = append(undone, f)
		}
	}
	if len(undone) > 0 {
		o.logf("tests first: undid the test writer's changes outside test files: %s", clip(strings.Join(undone, ", "), 400))
	}
	if len(failed) > 0 {
		o.emit(event.Event{Kind: event.Error, Text: "tests first: the test writer changed files that are not tests, and rw could not undo them: " + clip(strings.Join(failed, ", "), 400)})
	}
}

// writeKeepMode writes a file, keeping its mode if it exists.
func writeKeepMode(p string, b []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(p); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, mode)
}

// redRun runs the acceptance tests before any code: "fail" as they
// should, "pass" when they test nothing new.
func (o *Orchestrator) redRun(ctx context.Context, t *task, command string) string {
	timeout := t.cfg.Verify.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, done, err := checkCmd(cctx, o.taskCfg(t), o.opts.Dir, command)
	if err != nil {
		o.logf("tests first: could not run the acceptance tests before the work: %v", err)
		return ""
	}
	out, err := cmd.CombinedOutput()
	if why := done(err, out); why != "" && err == nil {
		err = errors.New(why)
	}
	if ctx.Err() != nil {
		return ""
	}
	if err == nil {
		o.emit(event.Event{Kind: event.Error, Text: fmt.Sprintf("tests first: the acceptance tests already pass before any code is written (%s): they may not test what the task asks", command)})
		return "pass"
	}
	o.logf("tests first: the acceptance tests fail before any code is written, as they should (%s): %s", command, clip(lastLines(string(out), 1), 200))
	return "fail"
}

// useTests makes the tests part of the task: their command runs first in
// the task's checks, and writers are told about them.
func (o *Orchestrator) useTests(t *task, at *acceptTests) {
	t.tests = at
	if c := at.Command; c != "" && !slices.Contains(t.cfg.Verify.Commands, c) {
		t.cfg.Verify.Commands = append([]string{c}, t.cfg.Verify.Commands...)
	}
}

// loadTests takes up a resumed task's tests: as written (from their
// snapshot), so a change made before rw stopped is put back too.
func (o *Orchestrator) loadTests(t *task, s TestsState) {
	at := &acceptTests{TestsState: s, base: o.opts.Dir, content: map[string][]byte{}}
	if t.useGit {
		at.base = t.root
	}
	for _, f := range s.Files {
		if s.Commit != "" && t.useGit {
			if b, err := (git{t.root}).run(nil, nil, "cat-file", "--filters", s.Commit+":"+f); err == nil {
				at.content[f] = []byte(b)
				continue
			}
		}
		if b, err := os.ReadFile(filepath.Join(at.base, filepath.FromSlash(f))); err == nil {
			at.content[f] = b
		}
	}
	o.logf("tests first: guarding the %d acceptance test file(s) written before the interruption", len(at.content))
	o.useTests(t, at)
}

// guardTests puts back acceptance tests an agent changed or deleted, and
// returns them.
func (o *Orchestrator) guardTests(t *task) []string {
	at := t.tests
	if at == nil {
		return nil
	}
	var changed []string
	for _, f := range at.Files {
		want, ok := at.content[f]
		if !ok {
			continue
		}
		p := filepath.Join(at.base, filepath.FromSlash(f))
		if got, err := os.ReadFile(p); err == nil && bytes.Equal(got, want) {
			continue
		}
		if err := writeKeepMode(p, want); err != nil {
			o.emit(event.Event{Kind: event.Error, Text: fmt.Sprintf("tests first: could not put back the acceptance test %s: %v", f, err)})
			continue
		}
		changed = append(changed, f)
	}
	if len(changed) > 0 {
		o.logf("tests first: an agent changed the acceptance tests %s; they were put back as written", strings.Join(changed, ", "))
	}
	return changed
}

// testsHint is the writers' note about the acceptance tests ("" without).
func (t *task) testsHint() string {
	at := t.tests
	if at == nil || (len(at.Files) == 0 && at.Command == "") {
		return ""
	}
	run := "the repo's checks"
	if at.Command != "" {
		run = at.Command
	}
	where := ""
	if len(at.Files) > 0 {
		where = " in " + strings.Join(at.Files, ", ")
	}
	return "\nACCEPTANCE TESTS: before any code was written, tests for this task's requirements were written" + where +
		". Run them with " + run + ". They fail until the task is done; the task is done when they pass. " +
		"Do not edit, skip, weaken or delete them: rw puts them back as written before it checks the work. " +
		"If you are sure a test is wrong, leave it and say which test and why in your reply.\n"
}

// testsChangedAdvice is the fix round's note when an agent changed the
// acceptance tests.
func testsChangedAdvice(files []string) string {
	return "The acceptance tests " + strings.Join(files, ", ") + " were changed; rw put them back as written. " +
		"Do not edit them: change the implementation until they pass."
}

func testsPrompt(task string, verify []string, docs string) string {
	var b strings.Builder
	b.WriteString(runner.MarkerTests + " You are the test writer in Relayweft, a team of coding agents. Before anyone implements the TASK below, " +
		"write the acceptance tests that decide when it is done. Another agent implements the task afterwards and may not change your tests.\n\n")
	b.WriteString("TASK:\n" + task + "\n")
	b.WriteString(docs)
	b.WriteString(`
How to work:
1. List every requirement the TASK states: each behaviour, edge case, fallback, platform and constraint it names, and every acceptance criterion it lists.
2. Read only what you need to see how this repo tests such code: its test framework, file layout and naming, helpers and fixtures. Follow them.
3. Write at least one test for each requirement an automated test can check. Test the behaviour the TASK names, through the API, command or output it describes; do not guess at internals it leaves open.
4. Write test files only: new test files, or new tests in existing test files (fixtures in test folders are fine). DO NOT implement the task and DO NOT change other files: rw undoes every change outside test files. Where the code under test does not exist yet, call it as the TASK describes it; the tests may fail to compile until it is written.
5. Run your tests: they must fail now, because the task is not done, and fail for the reason the TASK gives, not on a typo. A test that passes now tests nothing new: change it.
`)
	if len(verify) > 0 {
		b.WriteString("\nThe repo's checks: " + strings.Join(verify, "; ") + "\n")
	}
	if run := checkRunners(verify); len(run) > 0 {
		b.WriteString("You may run " + strings.Join(run, ", ") + " with any arguments, so you can run just your tests, and the checks above. " +
			"Run each from the project folder, without cd and without chaining commands with && or ;: other commands need approval, which nobody gives.\n")
	}
	if runtime.GOOS == "windows" {
		b.WriteString("\nYour command runs in cmd.exe: quote with double quotes, not single quotes.\n")
	}
	b.WriteString("\nReply with ONLY this JSON in a json code block:\n" +
		"```json\n" +
		`{"tests": [{"requirement": "<requirement, as the TASK states it>", "file": "<test file>", "name": "<test name>"}],` + "\n" +
		` "files": ["<every file you wrote or changed>"],` + "\n" +
		` "command": "<one command, from the project folder, that runs just these tests, e.g. go test ./pkg -run 'TestA|TestB'>",` + "\n" +
		` "untestable": ["<a requirement no automated test can check, and why>"],` + "\n" +
		` "cannot": ""}` + "\n```\n" +
		"If you cannot write any test (the repo has no test setup, or nothing in the TASK can be checked by a test), write no files and say why in \"cannot\".\n")
	return b.String()
}

var (
	// reTestDir and reTestFile find test files by the usual conventions:
	// test folders, and _test/.test/.spec/test_ file names.
	reTestDir  = regexp.MustCompile(`(?i)(^|/)(tests?|__tests__|spec|specs|testdata|test_data|fixtures|e2e|testing)/`)
	reTestFile = regexp.MustCompile(`(?i)([._-](test|tests|spec|specs)\.[a-z0-9]+$)|((^|/)test_[^/]+$)|((^|/)conftest\.py$)`)
	// reTestClass finds CamelCase test classes (FooTest.java, FooTests.cs).
	reTestClass = regexp.MustCompile(`[a-z0-9]Tests?\.(java|kt|kts|cs|fs|vb|scala|groovy|swift|php|m|mm|cpp|cc|h)$`)
)

// isTestPath reports whether a slash path looks like a test file.
func isTestPath(p string) bool {
	p = filepath.ToSlash(p)
	return reTestDir.MatchString(p) || reTestFile.MatchString(p) || reTestClass.MatchString(p)
}

// testRunners are the commands an acceptance test command may start with:
// rw runs the test writer's command as a check and lets writers run it,
// so it must be a plain test run.
var testRunners = []string{
	"go test", "gotestsum",
	"npm test", "npm run test", "npx vitest", "npx jest", "npx mocha", "npx playwright test",
	"yarn test", "yarn vitest", "yarn jest", "pnpm test", "pnpm vitest", "pnpm jest", "bun test", "deno test",
	"pytest", "python -m pytest", "python3 -m pytest", "py -m pytest", "uv run pytest", "poetry run pytest",
	"python -m unittest", "python3 -m unittest",
	"cargo test", "cargo nextest run", "dotnet test", "mvn test", "./mvnw test", "gradle test", "./gradlew test", "gradlew test",
	"bundle exec rspec", "rspec", "bundle exec rake test", "rake test", "phpunit", "vendor/bin/phpunit", "./vendor/bin/phpunit",
	"mix test", "swift test", "ctest", "zig build test", "dart test", "flutter test",
}

// checkRunners are the test runners the configured checks start with
// ("go test" for "go test -count=1 ./pkg"). The test writer may run them
// with any arguments: an exact check, or its prefix before the packages,
// does not cover running just the new tests ("go test -run 'TestA|TestB'
// ./pkg"), and on the bench Claude refused those in 3 of 4 runs, so the
// writer never saw its tests fail. It gains nothing it lacked: a check
// already runs any test file it writes.
func checkRunners(checks []string) []string {
	var out []string
	for _, c := range checks {
		if shellOperator(c) != "" {
			continue
		}
		line, best := strings.Join(strings.Fields(c), " "), ""
		for _, r := range testRunners {
			if (line == r || strings.HasPrefix(line, r+" ")) && len(r) > len(best) {
				best = r
			}
		}
		if best != "" && !slices.Contains(out, best) {
			out = append(out, best)
		}
	}
	return out
}

// testCommandProblem says why rw will not run a test writer's command
// ("" when it may): a configured check, or one of testRunners without
// shell operators.
func testCommandProblem(cmd string, configured []string) string {
	if slices.Contains(configured, cmd) {
		return ""
	}
	if op := shellOperator(cmd); op != "" {
		return "it uses the shell operator " + op
	}
	f := strings.Fields(cmd)
	for _, a := range f {
		switch strings.ToLower(strings.SplitN(a, "=", 2)[0]) {
		case "-exec", "--exec", "-toolexec", "--toolexec":
			return "it runs another program through " + a
		}
	}
	line := strings.Join(f, " ")
	for _, r := range testRunners {
		if line == r || strings.HasPrefix(line, r+" ") {
			return ""
		}
	}
	for _, c := range configured {
		if c := strings.Join(strings.Fields(c), " "); c != "" && strings.HasPrefix(line, c+" ") {
			return ""
		}
	}
	return "it is not a known test runner or one of verify.commands"
}

// shellOperator returns the first shell operator outside quotes in the
// shell checks run in ("" when none).
func shellOperator(s string) string { return shellOperatorIn(s, runtime.GOOS == "windows") }

// shellOperatorIn is shellOperator for sh, or for cmd.exe on Windows.
// sh: ; & | < > and newlines chain or redirect commands outside quotes,
// and ` and $( substitute them also inside double quotes. cmd.exe: only
// double quotes quote, ^ escapes, and % expands variables everywhere.
func shellOperatorIn(s string, windows bool) string {
	var quote rune
	prev := rune(0)
	for _, c := range s {
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '`':
			return "`"
		case prev == '$' && c == '(':
			return "$("
		case windows && (c == '%' || c == '^' || c == '!'):
			return string(c)
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '"' || (c == '\'' && !windows):
			quote = c
		case strings.ContainsRune(";&|<>\n\r", c):
			return string(c)
		}
		prev = c
	}
	if quote != 0 {
		return "unclosed " + string(quote)
	}
	return ""
}
