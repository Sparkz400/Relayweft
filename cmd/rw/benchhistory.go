package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/sysload"
	"gopkg.in/yaml.v3"
)

const historyHeader = `# rw bench tasks from this repo's history (written by rw bench --from-history).
#
# Each task is a past commit. The run starts from the commit's parent, the
# prompt is its commit message, and the check is the repo's tests with the
# commit's own test files in place. The test files are restored before every
# check, so an agent cannot pass by editing or deleting them. In a check,
# {tests} stands for the task's test files and {test_dirs} for their folders.
# Validated tasks failed the check on the parent and passed it on the commit.
#
# Every run starts in a fresh repository that holds only the parent's files
# as one commit: no later history, no remote and no objects shared with this
# repo, so an agent cannot look the solution up with git.
#
# Read the prompts before running: commit messages are often terser than a
# real request, and you can reword them. Delete the tasks you do not want.
#
# learn: true feeds the results into this repo's learned routes when the
# bench ends (like rw tune --apply), so the next tasks use what clearly
# won here. A mode like routed:worker=claude:sonnet:medium runs the routed
# pipeline with that role on another route: that is what gives the learner
# an alternative to the current route. Single-agent modes do not count.
#
# Run it with: %s
`

// historyLimits are the size limits for picking commits.
type historyLimits struct {
	minFiles, maxFiles, maxLines int
}

// historyCandidate is a commit that passed the static filters.
type historyCandidate struct {
	orchestrator.HistoryCommit
	code, tests []string
	lines       int
	prompt      string // the cleaned commit message
	check       string // the task's check ("": historyOpts.check)
}

func (h historyCandidate) short() string { return h.SHA[:min(7, len(h.SHA))] }

func (h historyCandidate) subject() string {
	s, _, _ := strings.Cut(h.prompt, "\n")
	return s
}

// historyOpts are the --from-history flags.
type historyOpts struct {
	out, check, setup  string
	count, scan        int
	lim                historyLimits
	hidden, noValidate bool
	ownTests           bool
	timeout            time.Duration
	runCmd             string // how to run the written file
}

// benchFromHistory writes a bench file whose tasks are past multi-file
// commits of the repo, each checked by the repo's tests with the commit's
// test files in place. With --no-validate and no check command it only
// lists the commits that fit.
func benchFromHistory(c common, o historyOpts) error {
	store, dir, err := c.setup()
	if err != nil {
		return err
	}
	if o.check == "" {
		cmds := store.Get().Verify.Commands
		if len(cmds) == 0 {
			cmds = config.DetectVerify(dir)
		}
		o.check = strings.Join(cmds, " && ")
	}
	listOnly := o.check == "" && o.noValidate
	if o.check == "" && !listOnly {
		return fmt.Errorf("no test command found in verify.commands or the build files: pass --check \"<command>\" (or --no-validate to only list the commits)")
	}
	if _, err := os.Stat(o.out); err == nil && !listOnly {
		return fmt.Errorf("%s exists (choose another with --file)", o.out)
	}
	if o.ownTests && !listOnly && !usesTestsPlaceholder(o.check) {
		if _, ok := narrowCheck(o.check, []string{"a_test.go", "test/a_test.dart", "test_a.py", "a.test.js"}); !ok {
			return fmt.Errorf("--own-tests: no test runner it knows in %q (go test ./..., flutter/dart test, pytest, jest, vitest, npm/yarn/pnpm test): put %s or %s in --check instead", o.check, phTests, phTestDirs)
		}
	}
	o.runCmd = historyRunCmd(o.out, dir, c.dir != "")
	ws, err := orchestrator.NewBenchWorkspace(dir)
	if err != nil {
		return err
	}
	oc := store.Get().Orchestrator
	proc.SetLowPriority(oc.LowPriority)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	waitForMemory(ctx, oc, "reading the history")
	// Names first; line counts only for the commits that can still fit.
	commits, err := ws.History(o.scan, func(hc orchestrator.HistoryCommit) bool {
		_, why := judgeHistory(hc, o.lim)
		return why == ""
	})
	if err != nil {
		return fmt.Errorf("reading the history: %w", err)
	}
	cands, skipped := pickHistory(commits, o.lim)
	fmt.Printf("%d of the last %d commits fit (%d+ code files, at most %d files and %d changed code lines, with test changes)\n",
		len(cands), len(commits), o.lim.minFiles, o.lim.maxFiles, o.lim.maxLines)
	printSkipped(skipped)
	if len(cands) == 0 {
		return fmt.Errorf("no commit fits: try --scan, --min-files, --max-files or --max-lines")
	}

	var picked []historyCandidate
	if o.noValidate {
		picked = cands[:min(o.count, len(cands))]
		for i, h := range picked {
			fmt.Printf("  %s %s (%d code files, %d lines; tests: %s)\n", h.short(), oneLine(h.subject(), 60), len(h.code), h.lines, oneLine(strings.Join(h.tests, ", "), 80))
			if !listOnly {
				if note := setHistoryCheck(ws, &picked[i], o); note != "" {
					fmt.Println("    " + note)
				}
			}
		}
		if listOnly {
			fmt.Println("no check command (verify.commands, the build files or --check): listed only, no file written")
			return nil
		}
	} else {
		unlock, err := ws.Lock()
		if err != nil {
			return err
		}
		defer unlock()
		own := ""
		if o.ownTests && !usesTestsPlaceholder(o.check) {
			own = ", narrowed to each commit's own tests"
		}
		fmt.Printf("validating with %q%s: the check must pass on the commit and fail on its parent (in %s)\n", o.check, own, ws.Path)
		skips := map[string]int{}
		for _, h := range cands {
			if len(picked) == o.count || ctx.Err() != nil {
				break
			}
			waitForMemory(ctx, oc, "the next check")
			fmt.Printf("  %s %s ... ", h.short(), oneLine(h.subject(), 60))
			if note := setHistoryCheck(ws, &h, o); note != "" {
				fmt.Print("(" + note + ") ")
			}
			why := validateHistory(ctx, ws, h, o)
			if why != "" {
				fmt.Println("skip:", why)
				skips[skipKind(why)]++
				continue
			}
			fmt.Println("ok")
			picked = append(picked, h)
		}
		switch {
		case len(picked) == 0 && ctx.Err() != nil:
			return fmt.Errorf("cancelled before a commit passed validation")
		case len(picked) == 0:
			return noValidCommit(skips, o.setup)
		case ctx.Err() != nil:
			fmt.Println("cancelled; writing the tasks validated so far")
		}
	}
	data, err := historyBenchFile(picked, o, historyVariants(store.Get()))
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.out, data, 0o644); err != nil {
		return err
	}
	if len(picked) < o.count {
		fmt.Printf("note: found %d of the %d tasks asked for; --scan looks further back\n", len(picked), o.count)
	}
	fmt.Printf("wrote %d task(s) to %s\nnext: read the prompts, then run `%s`\n", len(picked), o.out, o.runCmd)
	diag.Logf("bench from history: %d candidates, %d picked", len(cands), len(picked))
	return nil
}

// historyRunCmd is the command that runs the bench file out of the project
// in dir. It names the project (--dir) unless rw bench finds it on its own:
// a file inside the project, written without --dir.
func historyRunCmd(out, dir string, dirFlag bool) string {
	abs, err := filepath.Abs(out)
	if err != nil {
		abs = out
	}
	// Compare real paths: the working directory can be the same folder by
	// another name (macOS: /var is /private/var).
	rel, err := filepath.Rel(realPath(dir), filepath.Join(realPath(filepath.Dir(abs)), filepath.Base(abs)))
	inside := err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	if inside && !dirFlag {
		return "rw bench --file " + argQuote(out)
	}
	return "rw bench --dir " + argQuote(dir) + " --file " + argQuote(abs)
}

// realPath is p with symlinks resolved, or p when that fails.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// argQuote quotes a path for a command line when it needs it.
func argQuote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"'&|<>;()$`") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

// benchLoad reads the machine's load (tests swap it); benchPoll is how
// often a held step looks again.
var (
	benchLoad = sysload.NewSampler(2 * time.Second).Get
	benchPoll = 5 * time.Second
)

// waitForMemory holds the next step while less RAM is free than
// orchestrator.min_free_memory_mb, like rw holds new agents: at most
// busy_max_wait, then it goes on anyway.
func waitForMemory(ctx context.Context, oc config.OrchestratorCfg, what string) {
	if oc.MinFreeMemoryMB <= 0 {
		return
	}
	low := func() (bool, uint64) {
		s := benchLoad()
		return s.MemOK && s.MemFree < uint64(oc.MinFreeMemoryMB)<<20, s.MemFree >> 20
	}
	isLow, free := low()
	if !isLow {
		return
	}
	fmt.Printf("only %d MB RAM free (< min_free_memory_mb %d): holding %s until memory frees up (at most %s)\n",
		free, oc.MinFreeMemoryMB, what, oc.BusyMaxWait.D())
	deadline := time.Now().Add(oc.BusyMaxWait.D())
	for isLow && time.Now().Before(deadline) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(benchPoll):
		}
		isLow, _ = low()
	}
	if isLow && ctx.Err() == nil {
		fmt.Printf("RAM still low after %s: going on with %s\n", oc.BusyMaxWait.D(), what)
	}
}

// setHistoryCheck sets a candidate's check: with --own-tests narrowed to
// its test files. It returns a note when that was not possible.
func setHistoryCheck(ws *orchestrator.BenchWorkspace, h *historyCandidate, o historyOpts) string {
	h.check = o.check
	if !o.ownTests || usesTestsPlaceholder(o.check) {
		return ""
	}
	files, err := ws.FilesAt(h.SHA, h.tests)
	if err != nil {
		return "the full check: " + err.Error()
	}
	if n, ok := narrowCheck(o.check, files); ok {
		h.check = n
		return ""
	}
	return "the full check: no test file of the commit fits its runner"
}

// Why validation skipped a commit.
const (
	skipFailsOnCommit = "the check fails on the commit itself"
	skipPassesBefore  = "the check already passes before the change"
)

// validateHistory checks that a commit makes a meaningful task: its check
// passes on the commit and fails on the parent with the commit's tests in
// place. It returns why not, or "".
func validateHistory(ctx context.Context, ws *orchestrator.BenchWorkspace, h historyCandidate, o historyOpts) string {
	// run reports checked=false when the check did not get to run to the
	// end (setup failed, cancelled, timed out).
	check := h.check
	if check == "" {
		check = o.check
	}
	run := func(base string, tests *benchTests) (ok, checked bool, why string) {
		rctx, cancel := context.WithTimeout(ctx, o.timeout)
		defer cancel()
		t := benchTask{Check: check, Base: base, Tests: tests}
		note := prepareBenchRun(rctx, ws, base, o.setup, t)
		ok, out := false, ""
		if note == "" {
			ok, out = benchCheck(rctx, ws, t, nil)
		}
		switch {
		case ctx.Err() != nil:
			return false, false, "cancelled"
		case rctx.Err() != nil:
			return false, false, "timed out"
		case note != "":
			return false, false, note
		}
		return ok, true, lastLine(out)
	}
	var own *benchTests
	if usesTestsPlaceholder(check) {
		own = &benchTests{From: h.SHA, Files: h.tests} // what the placeholders stand for
	}
	switch ok, checked, why := run(h.SHA, own); {
	case !checked:
		return "on the commit: " + why
	case !ok:
		return skipFailsOnCommit + ": " + why
	}
	switch ok, checked, why := run(h.Parent, &benchTests{From: h.SHA, Files: h.tests}); {
	case !checked:
		return "on the parent: " + why
	case ok:
		return skipPassesBefore
	}
	return ""
}

// skipKind groups validation skips for the final hint.
func skipKind(why string) string {
	switch {
	case strings.HasPrefix(why, skipFailsOnCommit):
		return "fails"
	case why == skipPassesBefore:
		return "passes"
	case strings.Contains(why, "setup failed"):
		return "setup"
	case strings.HasSuffix(why, "timed out"):
		return "timeout"
	}
	return "other"
}

// noValidCommit says why no commit passed validation, by the most common
// reason.
func noValidCommit(skips map[string]int, setup string) error {
	total, top := 0, "other"
	for _, k := range []string{"fails", "setup", "passes", "timeout", "other"} {
		total += skips[k]
		if skips[k] > skips[top] {
			top = k
		}
	}
	n := skips[top]
	of := fmt.Sprintf("%d of the %d commits tried", n, total)
	if n == total {
		of = fmt.Sprintf("all %d commits tried", total)
	}
	switch {
	case total == 0 || top == "other":
		return fmt.Errorf("no commit passed validation: see the reasons above")
	case top == "fails":
		hint := ""
		if setup == "" {
			hint = "; if it only needs its dependencies installed, add a --setup command like \"npm ci\""
		}
		return fmt.Errorf("no commit passed validation: the check already fails on %s, with the commit's own change in place (tests that were failing then?): fix or narrow the check (--check) first%s", of, hint)
	case top == "setup":
		return fmt.Errorf("no commit passed validation: the --setup command failed on %s: fix it first", of)
	case top == "passes":
		return fmt.Errorf("no commit passed validation: the check already passes before the change on %s (its tests do not catch the change): try another --check, or --scan further back", of)
	}
	return fmt.Errorf("no commit passed validation: the check timed out on %s: raise --check-timeout", of)
}

// historyTask turns a candidate into a bench task.
func historyTask(h historyCandidate, o historyOpts) benchTask {
	prompt := h.prompt
	if !o.hidden {
		prompt += "\n\nThe tests for this change are already in place: " + strings.Join(h.tests, ", ") +
			". Make them pass without changing them."
	}
	check := h.check
	if check == "" {
		check = o.check
	}
	return benchTask{
		Name:   "h-" + h.short() + slug(h.subject(), 32),
		Prompt: prompt + "\n",
		Check:  check,
		Base:   h.Parent,
		Tests:  &benchTests{From: h.SHA, Files: h.tests, Visible: !o.hidden},
	}
}

// historyBenchFile writes the bench file: routed, the route variants, then
// the single agents, with learn on.
func historyBenchFile(picked []historyCandidate, o historyOpts, variants []string) ([]byte, error) {
	modes := append([]string{"routed"}, variants...)
	bf := benchFile{
		Modes:   append(modes, "single:codex:gpt-6.1-sol:high", "single:claude:opus:high"),
		Setup:   o.setup,
		Timeout: config.Duration(30 * time.Minute),
		Learn:   true,
	}
	for _, h := range picked {
		bf.Tasks = append(bf.Tasks, historyTask(h, o))
	}
	runCmd := o.runCmd
	if runCmd == "" {
		runCmd = "rw bench --file " + argQuote(o.out)
	}
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf(historyHeader, runCmd) + "\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(bf); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

// pickHistory keeps the commits that make good tasks, newest first, and
// counts why the others were left out.
func pickHistory(commits []orchestrator.HistoryCommit, lim historyLimits) ([]historyCandidate, map[string]int) {
	skipped := map[string]int{}
	var out []historyCandidate
	for _, c := range commits {
		h, why := judgeHistory(c, lim)
		if why == "" && c.Uncounted {
			why = "changed lines not counted"
		}
		if why != "" {
			skipped[why]++
			continue
		}
		out = append(out, h)
	}
	return out, skipped
}

// judgeHistory sorts a commit's files and says why it makes no task (""
// when it does). The checks that need line counts (binary files, changed
// lines) only apply once the commit is counted.
func judgeHistory(c orchestrator.HistoryCommit, lim historyLimits) (historyCandidate, string) {
	h := historyCandidate{HistoryCommit: c, prompt: cleanCommitMessage(c.Message)}
	binary := false
	for _, f := range c.Files {
		switch {
		case isTestPath(f.Path):
			h.tests = append(h.tests, f.Path)
		case isLockPath(f.Path):
		case f.Binary:
			binary = true
		default:
			h.lines += f.Lines
			if !isDocPath(f.Path) {
				h.code = append(h.code, f.Path)
			}
		}
	}
	switch {
	case !taskLikeSubject(h.subject()):
		return h, "message is not a task (merge, revert, sync, bump, wip or too short)"
	case len(h.tests) == 0:
		return h, "no test changes"
	case len(h.code) < lim.minFiles:
		return h, fmt.Sprintf("fewer than %d code files", lim.minFiles)
	case len(c.Files) > lim.maxFiles:
		return h, fmt.Sprintf("more than %d files", lim.maxFiles)
	case c.Uncounted:
		return h, ""
	case binary:
		return h, "binary files outside the tests"
	case h.lines > lim.maxLines:
		return h, fmt.Sprintf("more than %d changed lines", lim.maxLines)
	}
	return h, ""
}

func printSkipped(skipped map[string]int) {
	if len(skipped) == 0 {
		return
	}
	var reasons []string
	for r := range skipped {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return skipped[reasons[i]] > skipped[reasons[j]] })
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, r := range reasons {
		fmt.Fprintf(tw, "  skipped %d\t%s\n", skipped[r], r)
	}
	tw.Flush()
}

// isTestPath reports whether p is a test or test data file, by the common
// conventions of Go, Python, JS/TS, Rust, Java/Kotlin, C#, Ruby and
// Dart/Flutter.
func isTestPath(p string) bool {
	l := strings.ToLower(p)
	for _, seg := range strings.Split(path.Dir(l), "/") {
		switch seg {
		case "test", "tests", "__tests__", "spec", "specs", "testdata", "fixtures", "__snapshots__", "__fixtures__",
			"integration_test", "integration_tests", "test_driver":
			return true
		}
	}
	return isTestFile(p) || path.Base(l) == "conftest.py"
}

// isTestFile reports whether p is a file of tests a test runner can be
// given (not test data or helpers), by its name or, for JS/TS, a
// __tests__ folder.
func isTestFile(p string) bool {
	l := strings.ToLower(p)
	base := path.Base(l)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	switch {
	case strings.Contains(base, ".test.") || strings.Contains(base, ".spec."):
		return true
	case ext == ".py" && (strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test")):
		return true
	case (ext == ".go" || ext == ".rb" || ext == ".exs" || ext == ".dart") && (strings.HasSuffix(name, "_test") || strings.HasSuffix(name, "_spec")):
		return true
	case (ext == ".java" || ext == ".kt" || ext == ".cs" || ext == ".scala") && (strings.HasSuffix(name, "test") || strings.HasSuffix(name, "tests")):
		return true
	case strings.Contains("/"+l, "/__tests__/"):
		switch ext {
		case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts":
			return true
		}
	}
	return false
}

func isDocPath(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".txt", ".rst", ".adoc":
		return true
	}
	return false
}

func isLockPath(p string) bool {
	switch path.Base(p) {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb", "bun.lock", "Cargo.lock",
		"poetry.lock", "uv.lock", "Pipfile.lock", "composer.lock", "Gemfile.lock", "packages.lock.json":
		return true
	}
	return false
}

var (
	reTrailer   = regexp.MustCompile(`^[A-Za-z][A-Za-z-]*: `)
	reSlugJunk  = regexp.MustCompile(`[^a-z0-9]+`)
	reGenerated = regexp.MustCompile(`(?i)generated with \[?claude code`)

	// Merges, reverts and mirror or sync commits ("Sync from upstream",
	// "Mirror of ...") are not something a person asked for.
	reNotATask = regexp.MustCompile(`(?i)^(merge|revert|bump|release|fixup!|squash!|amend!|wip\b|chore\(deps|chore\(release|v?\d+\.\d+|` +
		`(auto[- ]?)?sync(ed|ing)?\s+(from|with|to)\b|mirror(ed|ing)?\b|import(ed)?\s+from\b|update(d)?\s+from\s+upstream\b)`)
)

// cleanCommitMessage drops the trailer block (Signed-off-by, Co-Authored-By,
// ...) and tool footers from a commit message.
func cleanCommitMessage(msg string) string {
	msg = strings.ReplaceAll(strings.TrimSpace(msg), "\r\n", "\n")
	paras := strings.Split(msg, "\n\n")
	for len(paras) > 1 {
		last := strings.Split(strings.TrimSpace(paras[len(paras)-1]), "\n")
		trailers := true
		for _, l := range last {
			if !reTrailer.MatchString(l) && !reGenerated.MatchString(l) {
				trailers = false
				break
			}
		}
		if !trailers {
			break
		}
		paras = paras[:len(paras)-1]
	}
	return strings.TrimSpace(strings.Join(paras, "\n\n"))
}

func taskLikeSubject(s string) bool {
	return len(strings.Fields(s)) >= 3 && !reNotATask.MatchString(strings.TrimSpace(s))
}

// slug is "-" plus a short file-name-safe form of s ("" when nothing is left).
func slug(s string, n int) string {
	s = strings.Trim(reSlugJunk.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > n {
		s = strings.TrimRight(s[:n], "-")
	}
	if s == "" {
		return ""
	}
	return "-" + s
}
