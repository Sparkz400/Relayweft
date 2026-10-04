package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

const historyHeader = `# sy bench tasks from this repo's history (written by sy bench --from-history).
#
# Each task is a past commit. The run starts from the commit's parent, the
# prompt is its commit message, and the check is the repo's tests with the
# commit's own test files in place. The test files are restored before every
# check, so an agent cannot pass by editing or deleting them.
# Validated tasks failed the check on the parent and passed it on the commit.
#
# Read the prompts before running: commit messages are often terser than a
# real request, and you can reword them. Delete the tasks you do not want.
# The runs share this repo's git objects, so an agent that searches the
# history could find the original commit; every mode has the same chance.
#
# learn: true feeds the results into this repo's learned routes when the
# bench ends (like sy tune --apply), so the next tasks use what clearly
# won here. A mode like routed:worker=claude:sonnet:medium runs the routed
# pipeline with that role on another route: that is what gives the learner
# an alternative to the current route. Single-agent modes do not count.
#
# Run it with: sy bench --file %s
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
	timeout            time.Duration
}

// benchFromHistory writes a bench file whose tasks are past multi-file
// commits of the repo, each checked by the repo's tests with the commit's
// test files in place.
func benchFromHistory(c common, o historyOpts) error {
	if _, err := os.Stat(o.out); err == nil {
		return fmt.Errorf("%s exists (choose another with --file)", o.out)
	}
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
	if o.check == "" {
		return fmt.Errorf("no test command found in verify.commands or the build files: pass --check \"<command>\"")
	}
	ws, err := orchestrator.NewBenchWorkspace(dir)
	if err != nil {
		return err
	}
	commits, err := ws.History(o.scan)
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
	} else {
		unlock, err := ws.Lock()
		if err != nil {
			return err
		}
		defer unlock()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		fmt.Printf("validating with %q: the check must pass on the commit and fail on its parent (in %s)\n", o.check, ws.Path)
		for _, h := range cands {
			if len(picked) == o.count || ctx.Err() != nil {
				break
			}
			fmt.Printf("  %s %s ... ", h.short(), oneLine(h.subject(), 60))
			why := validateHistory(ctx, ws, h, o)
			if why != "" {
				fmt.Println("skip:", why)
				continue
			}
			fmt.Println("ok")
			picked = append(picked, h)
		}
		if ctx.Err() != nil {
			fmt.Println("cancelled; writing the tasks validated so far")
		}
	}
	if len(picked) == 0 {
		return fmt.Errorf("no commit passed validation (does the check need a --setup command, like \"npm ci\"?)")
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
	fmt.Printf("wrote %d task(s) to %s\nnext: read the prompts, then run `sy bench --file %s`\n", len(picked), o.out, o.out)
	diag.Logf("bench from history: %d candidates, %d picked", len(cands), len(picked))
	return nil
}

// validateHistory checks that a commit makes a meaningful task: its check
// passes on the commit and fails on the parent with the commit's tests in
// place. It returns why not, or "".
func validateHistory(ctx context.Context, ws *orchestrator.BenchWorkspace, h historyCandidate, o historyOpts) string {
	run := func(base string, tests *benchTests) (bool, string) {
		rctx, cancel := context.WithTimeout(ctx, o.timeout)
		defer cancel()
		t := benchTask{Check: o.check, Base: base, Tests: tests}
		if note := prepareBenchRun(rctx, ws, base, o.setup, t); note != "" {
			return false, note
		}
		ok, out := benchCheck(rctx, ws, t)
		switch {
		case ctx.Err() != nil:
			return false, "cancelled"
		case rctx.Err() != nil:
			return false, "timed out"
		}
		return ok, lastLine(out)
	}
	if ok, why := run(h.SHA, nil); !ok {
		return "the check fails on the commit itself: " + why
	}
	switch ok, why := run(h.Parent, &benchTests{From: h.SHA, Files: h.tests}); {
	case why == "cancelled" || why == "timed out":
		return "on the parent: " + why
	case ok:
		return "the check already passes before the change"
	}
	return ""
}

// historyTask turns a candidate into a bench task.
func historyTask(h historyCandidate, o historyOpts) benchTask {
	prompt := h.prompt
	if !o.hidden {
		prompt += "\n\nThe tests for this change are already in place: " + strings.Join(h.tests, ", ") +
			". Make them pass without changing them."
	}
	return benchTask{
		Name:   "h-" + h.short() + slug(h.subject(), 32),
		Prompt: prompt + "\n",
		Check:  o.check,
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
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf(historyHeader, o.out) + "\n")
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
		h := historyCandidate{HistoryCommit: c, prompt: cleanCommitMessage(c.Message)}
		why := ""
		for _, f := range c.Files {
			switch {
			case isTestPath(f.Path):
				h.tests = append(h.tests, f.Path)
			case isLockPath(f.Path):
			case f.Binary:
				why = "binary files outside the tests"
			default:
				h.lines += f.Lines
				if !isDocPath(f.Path) {
					h.code = append(h.code, f.Path)
				}
			}
		}
		switch {
		case why != "":
		case !taskLikeSubject(h.subject()):
			why = "message is not a task (merge, revert, bump, wip or too short)"
		case len(h.tests) == 0:
			why = "no test changes"
		case len(h.code) < lim.minFiles:
			why = fmt.Sprintf("fewer than %d code files", lim.minFiles)
		case len(c.Files) > lim.maxFiles:
			why = fmt.Sprintf("more than %d files", lim.maxFiles)
		case h.lines > lim.maxLines:
			why = fmt.Sprintf("more than %d changed lines", lim.maxLines)
		}
		if why != "" {
			skipped[why]++
			continue
		}
		out = append(out, h)
	}
	return out, skipped
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
// conventions of Go, Python, JS/TS, Rust, Java/Kotlin, C# and Ruby.
func isTestPath(p string) bool {
	l := strings.ToLower(p)
	for _, seg := range strings.Split(path.Dir(l), "/") {
		switch seg {
		case "test", "tests", "__tests__", "spec", "specs", "testdata", "fixtures", "__snapshots__", "__fixtures__":
			return true
		}
	}
	base := path.Base(l)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	switch {
	case strings.Contains(base, ".test.") || strings.Contains(base, ".spec."):
		return true
	case ext == ".py" && (strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test") || name == "conftest"):
		return true
	case (ext == ".go" || ext == ".rb" || ext == ".exs") && (strings.HasSuffix(name, "_test") || strings.HasSuffix(name, "_spec")):
		return true
	case (ext == ".java" || ext == ".kt" || ext == ".cs" || ext == ".scala") && (strings.HasSuffix(name, "test") || strings.HasSuffix(name, "tests")):
		return true
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
	reNotATask  = regexp.MustCompile(`(?i)^(merge|revert|bump|release|fixup!|squash!|amend!|wip\b|chore\(deps|chore\(release|v?\d+\.\d+)`)
	reSlugJunk  = regexp.MustCompile(`[^a-z0-9]+`)
	reGenerated = regexp.MustCompile(`(?i)generated with \[?claude code`)
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
