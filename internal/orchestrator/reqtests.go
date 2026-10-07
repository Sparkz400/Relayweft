package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/affected"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Independent requirement tests (orchestrator.independent_tests). While the
// worker changes the code, an agent on another provider writes tests for
// the requirements the task names, in a pool worktree at the task's start:
// it never sees the change. After the work rw runs those tests against it,
// next to the checks. A failure starts a fix round with the output, like a
// failing check; what a failure does after that is the gate's
// (orchestrator.independent_tests_gate, reqGate). The tests never land in
// your tree: rw puts them in only while they run (and while a fix agent in
// your tree works) and removes them again.
//
// On the bench, final reviews approved 7 of 9 changes that failed the
// hidden tests (docs/bench/2026-10-06-phase3-shortcuts.md): a prose review
// reads plausible code as correct. A test written from the requirements
// alone does not have that bias.

// AgentTester is the independent test writer's agent id.
const AgentTester = "tester"

// Limits on what rw takes from the test writer.
const (
	reqMaxFiles     = 20
	reqMaxFileBytes = 256 << 10
	reqMaxBytes     = 1 << 20
	// reqTag is in every requirement test file's name, so the files can
	// never be mistaken for the project's own.
	reqTag = "rwreq"
)

// ReqTests is what the independent test writer produced.
type ReqTests struct {
	// Files are the new test files: repo-relative slash path -> content.
	Files map[string][]byte
	// Command runs only these tests ("" = the repo's checks with the
	// files in place).
	Command string
	// Requirements is the writer's list of requirements and the tests
	// that check each, as it reported them.
	Requirements string
	// Notes say what rw left out and why.
	Notes    []string
	Provider string
	Model    string
}

// Paths lists the files in order.
func (rt *ReqTests) Paths() []string {
	if rt == nil {
		return nil
	}
	out := make([]string, 0, len(rt.Files))
	for p := range rt.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (rt *ReqTests) empty() bool { return rt == nil || len(rt.Files) == 0 }

// reqWriter is a test writer started next to the worker.
type reqWriter struct {
	done chan struct{}
	rt   *ReqTests
	why  string // why there are no tests, "" with tests
}

// wait returns the tests once the writer is done (nil if it wrote none).
func (w *reqWriter) wait() *ReqTests {
	if w == nil {
		return nil
	}
	<-w.done
	return w.rt
}

// ReqTestsID is the step, and estimate row, of the independent test writer.
const ReqTestsID = "independent-tests"

const reqTestsTitle = "Write requirement tests"

// reqTestsWhyNot says why task t gets no independent tests ("" = it does).
func (o *Orchestrator) reqTestsWhyNot(t *task, p Plan) string {
	switch {
	case !t.cfg.Orchestrator.IndependentTests:
		return "off"
	case !hasEdits(p):
		return "the task changes no files"
	case t.resumed:
		return "a resumed task's tree already holds work, so a writer would not be independent of it"
	case !t.useGit || t.start == "":
		return "not in a git repo, so there is no start state to write them against"
	case len(t.repos) > 0:
		return "the task spans several repos"
	case len(t.cfg.Verify.Commands) == 0:
		return "verify.commands is empty, so rw has no test runner to run them with"
	}
	return ""
}

// reqTestsExpected reports whether plan p's task is expected to get
// independent tests, for its estimate: reqTestsWhyNot without what only a
// running task has (its start snapshot).
func reqTestsExpected(t *task, p Plan) bool {
	return t.cfg.Orchestrator.IndependentTests && hasEdits(p) && !t.resumed && t.root != "" && len(t.repos) == 0 && len(t.cfg.Verify.Commands) > 0
}

// planWorker is the provider of plan p's first writing step ("" = none).
func (o *Orchestrator) planWorker(p Plan) string {
	for _, st := range p.Subtasks {
		if !st.Kind.ReadOnly() {
			return o.router.Route(router.Step{ID: st.ID, Title: st.Title, Kind: st.Kind, Prompt: st.Prompt, Files: st.Files}).Provider
		}
	}
	return ""
}

// startReqTests starts the test writer for plan p (nil when the task gets
// none). The returned writer is waited for after the work.
func (o *Orchestrator) startReqTests(ctx context.Context, t *task, p Plan) *reqWriter {
	if why := o.reqTestsWhyNot(t, p); why != "" {
		if why != "off" {
			o.logf("independent tests: none (%s)", why)
		}
		return nil
	}
	worker := o.planWorker(p)
	w := &reqWriter{done: make(chan struct{})}
	go func() {
		defer close(w.done)
		w.rt, w.why = o.writeReqTests(ctx, t, worker)
		if w.why != "" && ctx.Err() == nil {
			o.logf("independent tests: none (%s)", w.why)
		}
	}()
	return w
}

// testerRoute is the test writer's route: the worker role on the first
// other provider that can write now, else the worker's own provider (a
// fresh agent there still never sees the change).
func (o *Orchestrator) testerRoute(t *task, worker string) event.Decision {
	cfg := t.cfg
	if worker == "" {
		worker = o.router.Route(router.Step{ID: ReqTestsID, Kind: router.KindEdit}).Provider
	}
	pick := func(p string) (event.Decision, bool) {
		r := cfg.Roles[event.RoleWorker].For(p)
		d := event.Decision{StepID: ReqTestsID, StepTitle: reqTestsTitle, Role: event.RoleWorker, Provider: p,
			Model: r.Model, Effort: r.Effort, Rule: router.RuleIndependentTests, Confidence: 1}
		if r.Model == "" || o.bestOfUnusable(t, d) != "" {
			return d, false
		}
		return d, true
	}
	for _, q := range cfg.Alternatives(worker) {
		if cfg.Providers[q].OnlyPreferred {
			continue
		}
		if d, ok := pick(q); ok {
			d.Reason = "independent tests: the worker route on another provider than the worker's (" + worker + ")"
			return d
		}
	}
	d, _ := pick(worker)
	d.Reason = "independent tests: no other provider can write now, so a fresh agent on the worker's provider"
	return d
}

// writeReqTests runs the test writer in a pool worktree at the task's
// start and collects the new test files it wrote.
func (o *Orchestrator) writeReqTests(ctx context.Context, t *task, worker string) (*ReqTests, string) {
	s, err := acquireSlot(t.root, t.start)
	if err != nil {
		return nil, "no worktree for the writer: " + clip(err.Error(), 200)
	}
	defer s.release()
	dir := s.path
	if rel, err := filepath.Rel(canonPath(t.root), canonPath(o.opts.Dir)); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		dir = filepath.Join(s.path, rel)
	}
	d := o.testerRoute(t, worker)
	step := router.Step{ID: ReqTestsID, Title: reqTestsTitle, Kind: router.KindEdit, Pin: &d}
	vc := t.cfg.Verify
	prompt := reqTestsPrompt(t.text, verifyAllowed(vc, dir), vc.Commands) + t.docsContextMax(reviewDocsMax)
	began := time.Now()
	_, res := o.runAgentAt(ctx, t, step, AgentTester, AgentMain, stepLoc{dir: dir, slot: s.path, base: t.start}, prompt, 1, nil)
	if ctx.Err() != nil {
		return nil, "cancelled"
	}
	if !res.OK() {
		return nil, "the writer failed: " + clip(errText(res.Err), 200)
	}
	rt, err := collectReqTests(s.path, t.start, res.Final, verifyAllowed(vc, dir))
	if err != nil {
		return nil, "could not read what the writer wrote: " + clip(err.Error(), 200)
	}
	rt.Provider, rt.Model = d.Provider, d.Model
	for _, n := range rt.Notes {
		o.logf("independent tests: %s", n)
	}
	if rt.empty() {
		return nil, "the writer wrote no new test files"
	}
	run := "with the checks"
	if rt.Command != "" {
		run = "with " + rt.Command
	}
	o.logf("independent tests: %s wrote %d file(s) in %s (%s); they run %s after the work", d.Label(), len(rt.Files),
		time.Since(began).Round(time.Second), strings.Join(rt.Paths(), ", "), run)
	if keep, err := keepReqTests(filepath.Join(repoCache(t.root), "reqtests", t.id+"-"+time.Now().Format("20060102-150405")), rt); err == nil {
		o.logf("independent tests: a copy is in %s", keep)
	}
	return rt, ""
}

// keepReqTests saves a copy of rt in dir (the files, the requirement list
// and the command): the tests never stay in the tree, so this is where
// you, and the bench evidence, can read what they checked.
func keepReqTests(dir string, rt *ReqTests) (string, error) {
	for _, p := range rt.Paths() {
		dst := filepath.Join(dir, "files", filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, rt.Files[p], 0o644); err != nil {
			return "", err
		}
	}
	note := "REQUIREMENTS:\n" + rt.Requirements + "\n\nCOMMAND: " + rt.Command + "\n"
	if len(rt.Notes) > 0 {
		note += "\nNOTES:\n" + strings.Join(rt.Notes, "\n") + "\n"
	}
	return dir, os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(note), 0o644)
}

// collectReqTests reads the files the writer added in the worktree at root
// since base. Changes to existing files are left out: the tests must not
// depend on edits that will never be in the worker's tree.
func collectReqTests(root, base, final string, allowed []string) (*ReqTests, error) {
	g := git{root}
	now, _, err := g.snapshotSkipping("relayweft independent tests")
	if err != nil {
		return nil, err
	}
	out, err := g.run(nil, nil, "-c", "core.quotepath=false", "diff", "--name-status", "--no-renames", "-z", base, now)
	if err != nil {
		return nil, err
	}
	rt := &ReqTests{Files: map[string][]byte{}}
	var changed []string
	total := 0
	f := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		status, p := f[i], f[i+1]
		switch {
		case status != "A":
			changed = append(changed, p)
			continue
		case !strings.Contains(strings.ToLower(path.Base(p)), reqTag):
			rt.Notes = append(rt.Notes, fmt.Sprintf("left out %s: its name does not contain %q", p, reqTag))
			continue
		case !safeRel(p):
			rt.Notes = append(rt.Notes, "left out "+p+": not a plain path inside the repo")
			continue
		case len(rt.Files) >= reqMaxFiles:
			rt.Notes = append(rt.Notes, fmt.Sprintf("left out %s: more than %d files", p, reqMaxFiles))
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			rt.Notes = append(rt.Notes, "left out "+p+": "+err.Error())
			continue
		}
		if len(data) > reqMaxFileBytes || total+len(data) > reqMaxBytes {
			rt.Notes = append(rt.Notes, fmt.Sprintf("left out %s: over the size limit (%d KB a file, %d KB in all)", p, reqMaxFileBytes>>10, reqMaxBytes>>10))
			continue
		}
		total += len(data)
		rt.Files[p] = data
	}
	if len(changed) > 0 {
		rt.Notes = append(rt.Notes, "left out the writer's changes to existing files: "+clip(strings.Join(changed, ", "), 300))
	}
	rt.Requirements, rt.Command = parseReqReport(final)
	if rt.Command != "" && !reqCommandAllowed(rt.Command, allowed) {
		rt.Notes = append(rt.Notes, fmt.Sprintf("its command %q is not one of the checks or their narrowed forms, so the checks run them instead", clip(rt.Command, 200)))
		rt.Command = ""
	}
	return rt, nil
}

// safeRel reports whether p is a clean relative slash path inside the repo
// and outside .git.
func safeRel(p string) bool {
	if p == "" || strings.Contains(p, "\\") || path.IsAbs(p) || filepath.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// parseReqReport reads the writer's final message: the REQUIREMENTS block
// and the COMMAND line (the last one wins).
func parseReqReport(final string) (reqs, cmd string) {
	lines := strings.Split(strings.ReplaceAll(final, "\r\n", "\n"), "\n")
	var b strings.Builder
	in := false
	for _, l := range lines {
		s := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "`*"))
		switch {
		case strings.HasPrefix(strings.ToUpper(s), "COMMAND:"):
			cmd = strings.TrimSpace(strings.Trim(strings.TrimSpace(s[len("COMMAND:"):]), "`"))
			in = false
		case strings.HasPrefix(strings.ToUpper(s), "REQUIREMENTS:"):
			in = true
		case in && s != "":
			b.WriteString(strings.TrimSpace(l) + "\n")
		}
	}
	return clip(strings.TrimSpace(b.String()), 4000), cmd
}

// reqCommandAllowed reports whether cmd is one of the checks or starts with
// a command prefix a writing agent may run (verifyAllowed), without shell
// syntax: rw runs no other command an agent named.
func reqCommandAllowed(cmd string, allowed []string) bool {
	if slices.Contains(allowed, cmd) {
		return true
	}
	if affected.HasShellSyntax(cmd) {
		return false
	}
	for _, a := range allowed {
		if a != "" && strings.HasPrefix(cmd, a+" ") {
			return true
		}
	}
	return false
}

// placeReqTests writes rt's files into the tree at root for a run. A file
// whose path is already taken is left out (the worker wrote one there).
// remove takes the files out again, and the folders placing them created.
func placeReqTests(root string, rt *ReqTests) (remove func(), skipped []string, err error) {
	var made, dirs []string
	remove = func() {
		for _, f := range made {
			os.Remove(f)
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			os.Remove(dirs[i]) // only when empty
		}
	}
	for _, p := range rt.Paths() {
		dst := filepath.Join(root, filepath.FromSlash(p))
		if _, err := os.Lstat(dst); err == nil {
			skipped = append(skipped, p)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			remove()
			return func() {}, nil, err
		}
		var missing []string
		for d := filepath.Dir(dst); d != root && len(d) > len(root); d = filepath.Dir(d) {
			if _, err := os.Lstat(d); err == nil {
				break
			}
			missing = append(missing, d)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			remove()
			return func() {}, nil, err
		}
		for i := len(missing) - 1; i >= 0; i-- {
			dirs = append(dirs, missing[i])
		}
		if err := os.WriteFile(dst, rt.Files[p], 0o644); err != nil {
			remove()
			return func() {}, nil, err
		}
		made = append(made, dst)
	}
	return remove, skipped, nil
}

// runReqTests runs rt against the task's tree as it is now and returns
// whether they pass plus a report for the reviewer and the fix agent.
func (o *Orchestrator) runReqTests(ctx context.Context, t *task, rt *ReqTests) (bool, string) {
	remove, skipped, err := placeReqTests(t.root, rt)
	if err != nil {
		o.logf("independent tests: could not put them in place: %v", err)
		return true, ""
	}
	defer remove()
	if len(skipped) > 0 {
		o.logf("independent tests: left out %s: the change has a file there", strings.Join(skipped, ", "))
	}
	if len(skipped) == len(rt.Files) {
		return true, ""
	}
	o.emit(event.Event{Kind: event.Phase, Text: "verify"})
	vc := t.cfg.Verify
	cmds := vc.Commands
	why := "independent requirement tests, with the checks"
	if rt.Command != "" {
		cmds, why = []string{rt.Command}, "independent requirement tests"
	}
	ok, rep, _ := o.runChecks(ctx, t, vc, checkSite{dir: o.opts.Dir, label: "independent tests: "}, fullRuns(cmds, why))
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeReqTests, TaskID: t.id, Provider: rt.Provider, Model: rt.Model,
		OK: sessionlog.Bool(ok), Files: rt.Paths(), Text: clip(rt.Command, 500)})
	if rep == "cancelled" {
		return false, rep
	}
	return ok, reqReport(rt, rep, ok)
}

// reqReport is the verify report's part about the independent tests.
func reqReport(rt *ReqTests, rep string, ok bool) string {
	var b strings.Builder
	b.WriteString("\nINDEPENDENT REQUIREMENT TESTS (another agent wrote them from the TASK text alone, without seeing the change; files: " +
		strings.Join(rt.Paths(), ", ") + "):\n")
	if rt.Requirements != "" && !ok {
		b.WriteString("Requirements they check, as their writer listed them:\n" + rt.Requirements + "\n")
	}
	b.WriteString(rep)
	return b.String()
}

// The gate (orchestrator.independent_tests_gate). In a replay on saved runs
// (docs/bench/2026-10-06-independent-tests.md) the known solution failed a
// writer test in 3 of 5 tasks: a writer reads the task its own way, at
// times stricter than it is. Neither the base tree (a test that passed
// before the change) nor the kind of failure told those apart from the
// catches, so soft gives a failure the one fix round that can turn a catch
// into a fix, and then reports what still fails instead of failing the
// task. strict fails every round, as a check does.

// reqGate is what the independent tests may still decide in a task.
type reqGate struct {
	strict   bool
	fixed    bool // they failed a round and a fix round followed
	disputes []reqDispute
}

// decides reports whether failing independent tests fail round (0-based)
// of a task with maxFix fix rounds: always when strict; when soft, only
// before their fix round and only when a fix round can follow.
func (g *reqGate) decides(round, maxFix int) bool {
	return g.strict || (!g.fixed && round < maxFix)
}

// advisoryWhy says why the failing tests in rep no longer fail the round,
// with the fix agent's reasons against them.
func (g *reqGate) advisoryWhy(rep string) string {
	names := reqFailingTests(rep)
	what := "the independent tests"
	if len(names) > 0 {
		what = strings.Join(names, ", ")
	}
	why := what + " still fail after their fix round"
	if !g.fixed {
		why = what + " fail in the last round, so no fix round can follow"
	}
	why += "; independent_tests_gate: soft reports them instead of failing the task"
	for _, d := range g.disputes {
		if len(names) == 0 || slices.Contains(names, d.Test) {
			why += "; the fix agent disputed " + d.Test
			if d.Why != "" {
				why += ": " + d.Why
			}
		}
	}
	return why
}

// noteDisputes records the tests the fix agent's reply (final) disputes.
func (o *Orchestrator) noteDisputes(t *task, g *reqGate, final string, round int) {
	for _, d := range parseDisputes(final) {
		g.disputes = append(g.disputes, d)
		why := d.Test
		if d.Why != "" {
			why += ": " + d.Why
		}
		o.logf("independent tests: the fix agent disputes %s", why)
		o.noteChoice(t, sessionlog.ChoiceReqTests, sessionlog.ChoiceDisputed, why, round)
	}
}

// reqDispute is a test the fix agent says asks for more than the task.
type reqDispute struct{ Test, Why string }

// parseDisputes reads the "DISPUTE: <test>: <why>" lines of a fix agent's
// reply (at most 10).
func parseDisputes(final string) []reqDispute {
	var out []reqDispute
	for _, l := range strings.Split(strings.ReplaceAll(final, "\r\n", "\n"), "\n") {
		s := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "`*-• "))
		if len(out) == 10 || !strings.HasPrefix(strings.ToUpper(s), "DISPUTE:") {
			continue
		}
		// ": " and not ":", so a pytest id (file.py::test) stays whole.
		test, why, _ := strings.Cut(strings.TrimSpace(s[len("DISPUTE:"):]), ": ")
		if test = strings.Trim(strings.TrimSpace(test), "`*\"'"); test != "" {
			out = append(out, reqDispute{Test: clip(test, 120), Why: clip(strings.TrimSpace(why), 300)})
		}
	}
	return out
}

// reFailedTest finds failing test names in a test runner's output: go
// test's "--- FAIL: TestX (0.01s)" and pytest's "FAILED path::test_x".
var reFailedTest = regexp.MustCompile(`(?m)^\s*(?:--- FAIL: ([^\s/]+)|FAILED (\S+))`)

// reqFailingTests lists the failing tests a report names (go's top-level
// tests only, at most 10); none when the runner's output has no names rw
// reads.
func reqFailingTests(rep string) []string {
	var out []string
	for _, m := range reFailedTest.FindAllStringSubmatch(rep, -1) {
		name := m[1] + m[2]
		if !slices.Contains(out, name) && len(out) < 10 {
			out = append(out, name)
		}
	}
	return out
}

// reqFixAdvice is the fix agent's advice when the independent tests fail.
// inTree: the tests are in the fix agent's tree while it works.
func reqFixAdvice(rt *ReqTests, inTree bool) string {
	s := "\n\nIndependent requirement tests fail (output above). Another agent wrote them from the TASK text without seeing your change, so a failure most likely means your change misses a requirement the TASK names: make the change meet it. " +
		"If a test assumes something the TASK does not ask for (a name, a signature, a format the TASK leaves open), keep your change and add a line to your reply for each such test: DISPUTE: <test name>: <why>. rw records it and shows it with the result. Never edit, move or delete these test files: rw restores them before every run and removes them when the task ends."
	if !inTree {
		return s + " They are not in your tree; rw runs them after your fix.\n"
	}
	run := "the checks"
	if rt.Command != "" {
		run = rt.Command
	}
	return s + " They are in your tree while you work (" + strings.Join(rt.Paths(), ", ") + "); run them with " + run + ".\n"
}

// reqFixWhy adds the failing independent tests to why a fix round starts.
func reqFixWhy(why string, pass bool) string {
	switch {
	case pass:
		return why
	case why == "the round did not pass":
		return "independent requirement tests fail"
	}
	return why + "; independent requirement tests fail"
}

// reqTestsPrompt asks for tests of the task's requirements. allowed are the
// command prefixes the writer may run (and name as COMMAND); checks are
// the repo's checks.
func reqTestsPrompt(task string, allowed, checks []string) string {
	var b strings.Builder
	b.WriteString(runner.MarkerReqTests + ` You are a test writer in Relayweft. Another agent is changing this repo for the TASK below right now. You do not see its change: this is the repo as it was before. Write tests that pass only when the TASK's requirements are met, so rw can run them against the other agent's change.

TASK (what the other agent was asked to do):
` + task + `

1. List every requirement the TASK names: behavior a caller, a user or a file could observe. Include edge cases the TASK names explicitly.
2. Read the code the TASK is about and its existing tests, then write at least one focused test per requirement, in the style of the existing tests.
3. Test through what the TASK names or what the repo already exposes (existing functions, types, commands, files). The other agent may give new internals other names, so never depend on a name, signature or message the TASK does not give. A requirement you cannot test that way: list it as untested, with why.
4. Put the tests in NEW files whose names contain "` + reqTag + `" and that the project's test runner picks up (for example ` + reqTag + `_test.go next to the code in Go, test_` + reqTag + `.py in Python, ` + reqTag + `.test.ts in TypeScript). Prefix every test and helper name you add with RwReq (or rw_req_), so nothing clashes with tests the other agent adds. Do not change, move or delete any existing file: rw ignores such changes.
5. Your tests will fail now where the change is not made yet; that is expected. Run them once if you can to make sure everything else is right: they parse or compile against the current code (except for what the TASK itself adds), and each failure message says which requirement failed.
6. Assert only what the TASK requires. A test that fails on a correct change costs a needless fix round.

End your final message with exactly this, after everything else:
REQUIREMENTS:
- <requirement> -> <test names>  (or: untested: <why>)
COMMAND: <one command that runs all of your test files and as little else as it can, no shell syntax (no quotes, pipes, ; or &&)>
`)
	if len(allowed) > 0 {
		b.WriteString("\nThe COMMAND must be one of these or start with one of them followed by a space: " + strings.Join(allowed, " | ") + "\n")
	}
	if len(checks) > 0 {
		b.WriteString("The repo's checks are: " + strings.Join(checks, "; ") + ". Without a COMMAND, rw runs those with your files in place.\n")
	}
	return b.String()
}

// The bench replays the test writer against earlier runs' saved changes
// (rw bench --replay-tests): one writer per task, its tests run against
// every run.

// WriteRequirementTests runs only the independent test writer for text, in
// a pool worktree at o's folder as it is now. worker is the provider the
// tests are meant to be independent of ("" = the worker route's).
func (o *Orchestrator) WriteRequirementTests(ctx context.Context, text, worker string) (*ReqTests, TaskResult) {
	began := time.Now()
	t := o.replayTask(text)
	o.snapshotBefore(t)
	if !t.useGit {
		return nil, TaskResult{Summary: "not a git repo"}
	}
	if len(t.cfg.Verify.Commands) == 0 {
		return nil, TaskResult{Summary: "verify.commands is empty"}
	}
	// No permission preflight, as in a task: the writer does not need to
	// run the checks, only to write files.
	bctx := o.startBudget(ctx, t)
	rt, why := o.writeReqTests(bctx, t, worker)
	res := TaskResult{OK: rt != nil, Duration: time.Since(began), Tokens: t.usage(), Cost: o.cost(t), Summary: why}
	return rt, res
}

// RunRequirementTests runs rt against o's folder as it is now.
func (o *Orchestrator) RunRequirementTests(ctx context.Context, rt *ReqTests) (bool, string) {
	t := o.replayTask("")
	root, err := repoRoot(o.opts.Dir)
	if err != nil {
		return false, err.Error()
	}
	t.root = root
	if rt.empty() {
		return true, ""
	}
	return o.runReqTests(ctx, t, rt)
}

func (o *Orchestrator) replayTask(text string) *task {
	o.mu.Lock()
	o.taskSeq++
	seq := o.taskSeq
	o.mu.Unlock()
	cfg := o.opts.Store.Get()
	setPoolLimits(cfg)
	t := &task{id: fmt.Sprintf("%stask-%d", o.opts.TaskIDPrefix, seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg)}
	t.key = o.opts.Log.Session() + "-" + t.id
	return t
}
