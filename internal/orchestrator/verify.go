package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/affected"
	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// verifyScope is how much of the checks a verify run covers.
type verifyScope int

const (
	// verifyFull runs every command as configured: the run before the
	// final review.
	verifyFull verifyScope = iota
	// verifyAffected first runs only the tests the task's changes affect
	// (after a fix round). When they pass, the full checks run too, so a
	// narrowed pass never stands in for the full run.
	verifyAffected
)

// Scopes as the session log and `rw report` show them.
const (
	scopeFull     = "full"
	scopeAffected = "affected"
)

// checkSite is where a verify run happens. Best-of-N candidates and other
// repos are checked the same way, each with its own site.
type checkSite struct {
	dir   string // the folder the commands run in
	root  string // its git repo's top folder ("" = not in git: always full)
	base  string // the commit changes are measured from ("" = always full)
	label string // "repo: " before an extra repo's commands
}

// verifyRepos runs the checks of every repo of the task, each in its own
// repo, and also returns the names of the repos whose checks fail ("" is
// the primary).
func (o *Orchestrator) verifyRepos(ctx context.Context, t *task, scope verifyScope) (bool, string, map[string]bool) {
	allOK := true
	var b strings.Builder
	failing := map[string]bool{}
	for _, r := range t.allRepos() {
		ok, rep := o.verifyIn(ctx, t, r, scope)
		if !ok {
			allOK = false
			failing[r.repoName] = true
		}
		b.WriteString(rep)
		if rep == "cancelled" {
			return false, rep, failing
		}
	}
	return allOK, b.String(), failing
}

// verifyIn runs repo r's checks (verify.commands) in r (the project folder
// for the primary) and returns whether all passed plus a report for the
// reviewer and the fix agent (tail of each failing command's output).
func (o *Orchestrator) verifyIn(ctx context.Context, t, r *task, scope verifyScope) (bool, string) {
	site := checkSite{dir: o.opts.Dir}
	if r != t {
		site = checkSite{dir: r.dir, label: r.repoName + ": "}
	}
	if r.useGit {
		site.root, site.base = r.root, r.start
	}
	ok, rep, _ := o.verifyAt(ctx, t, r.cfg.Verify, site, scope)
	return ok, rep
}

// verifyAt runs the checks vc at site. In verifyAffected scope each
// command is first narrowed to the tests the changes since site.base
// affect (package affected); a command rw cannot narrow reliably runs in
// full. When every narrowed run passes, the narrowed commands run once
// more in full. failed counts the configured commands that fail in the
// run that decides (a narrowed command with several runs, one per dotnet
// test project, counts once), for ranking best-of-N candidates.
func (o *Orchestrator) verifyAt(ctx context.Context, t *task, vc config.VerifyCfg, site checkSite, scope verifyScope) (ok bool, report string, failed int) {
	cmds := vc.Commands
	if len(cmds) == 0 {
		return true, "", 0
	}
	if ctx.Err() != nil {
		return false, "cancelled", 0
	}
	if scope == verifyFull {
		return o.runChecks(ctx, t, vc, site, fullRuns(cmds, "the full checks before the final review"))
	}
	files, why := o.changedFiles(site, vc)
	if why != "" {
		o.logf("verify: %sthe full checks run (%s)", site.label, why)
		return o.runChecks(ctx, t, vc, site, fullRuns(cmds, why))
	}
	var plans []affected.Plan
	// The selection's tools read the agents' configs: in the sandbox when
	// agents write in one.
	in := affected.Input{Root: site.root, Dir: site.dir, Files: files, Exec: selectExec(o.taskCfg(t))}
	for _, c := range cmds {
		plans = append(plans, affected.Select(ctx, c, vc.AffectedCommands[c], in))
	}
	narrowed := 0
	var runs []checkRun
	for _, p := range plans {
		switch {
		case p.Full:
			runs = append(runs, checkRun{cmd: p.Command, of: p.Command, scope: scopeFull, why: p.Why})
		case len(p.Run) == 0:
			narrowed++
			runs = append(runs, checkRun{cmd: p.Command, of: p.Command, scope: scopeAffected, why: p.Why, skip: true})
		default:
			narrowed++
			for _, c := range p.Run {
				runs = append(runs, checkRun{cmd: c, of: p.Command, scope: scopeAffected, why: p.Why})
			}
		}
	}
	if narrowed > 0 {
		o.logf("verify: %sonly the tests affected by the changes first (%d changed files); the full checks run once they pass", site.label, len(files))
	}
	ok, rep, failed := o.runChecks(ctx, t, vc, site, runs)
	if rep == "cancelled" || narrowed == 0 {
		return ok, rep, failed
	}
	if !ok {
		return false, rep + "(Only the tests the changes affect ran; the full checks run once these pass.)\n", failed
	}
	// The final full run: a narrowed pass never stands in for it.
	var again []string
	for _, p := range plans {
		if !p.Full {
			again = append(again, p.Command)
		}
	}
	ok2, rep2, failed2 := o.runChecks(ctx, t, vc, site, fullRuns(again, "the affected tests pass; the full checks before the final review"))
	if rep2 == "cancelled" {
		return false, rep2, failed2
	}
	return ok2, rep + rep2, failed2
}

// checkRun is one command of a verify run.
type checkRun struct {
	cmd   string
	of    string // the configured command it stands for
	scope string // scopeFull or scopeAffected
	why   string // which tests and why, for the log and `rw report`
	skip  bool   // nothing is affected: nothing to run
}

func fullRuns(cmds []string, why string) []checkRun {
	out := make([]checkRun, len(cmds))
	for i, c := range cmds {
		out[i] = checkRun{cmd: c, of: c, scope: scopeFull, why: why}
	}
	return out
}

// changedFiles lists the files changed at site since its base, or says
// why the narrowing is off.
func (o *Orchestrator) changedFiles(site checkSite, vc config.VerifyCfg) ([]string, string) {
	switch {
	case vc.Affected == affected.Off:
		return nil, "verify.affected is off"
	case site.root == "" || site.base == "":
		return nil, "not in a git repo, so rw cannot tell what changed"
	}
	files, skipped, err := git{site.root}.changedSince(site.base)
	switch {
	case err != nil:
		return nil, "could not list the changed files: " + clip(err.Error(), 200)
	case len(skipped) > 0:
		return nil, fmt.Sprintf("%d untracked file(s) over the snapshot size limit, which rw cannot compare", len(skipped))
	}
	return files, ""
}

// runChecks runs commands in order and reports each. All run, also after
// a failure, so the fix agent sees every failing check at once. failed
// counts the configured commands with a failing run.
func (o *Orchestrator) runChecks(ctx context.Context, t *task, vc config.VerifyCfg, site checkSite, runs []checkRun) (bool, string, int) {
	timeout := vc.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	label := site.label
	allOK := true
	var b strings.Builder
	failed := map[string]bool{}
	for _, r := range runs {
		if ctx.Err() != nil {
			return false, "cancelled", len(failed)
		}
		what := r.scope
		if r.why != "" {
			what += ": " + r.why
		}
		if r.skip {
			o.opts.Log.Write(sessionlog.Record{Type: "verify", TaskID: t.id, Text: label + r.cmd, OK: sessionlog.Bool(true), Kind: r.scope, Reason: "nothing to run: " + r.why})
			o.emit(event.Event{Kind: event.Log, Text: fmt.Sprintf("verify – %s%s: nothing to run (%s)", label, r.cmd, clip(r.why, 300))})
			fmt.Fprintf(&b, "SKIPPED: %s%s (no affected tests)\n", label, r.cmd)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		var out []byte
		// Full and narrowed runs alike: in the sandbox when agents write in one.
		cmd, done, err := checkCmd(cctx, o.taskCfg(t), site.dir, r.cmd)
		if err == nil {
			out, err = cmd.CombinedOutput()
			if why := done(err, out); why != "" {
				out = append(out, "\n"+why...)
				if err == nil {
					err = errors.New(why) // it changed a submodule's .git
				}
			}
		} else {
			out = []byte(err.Error())
		}
		took := time.Since(start).Round(100 * time.Millisecond)
		timedOut := cctx.Err() == context.DeadlineExceeded
		cancel()
		ok := err == nil
		diag.Logf("verify %q (%s) in %s: ok=%v after %s err=%v", r.cmd, what, site.dir, ok, took, err)
		o.opts.Log.Write(sessionlog.Record{Type: "verify", TaskID: t.id, Text: label + r.cmd, OK: sessionlog.Bool(ok), DurationMS: took.Milliseconds(),
			Kind: r.scope, Reason: r.why})
		note := ""
		if r.scope == scopeAffected {
			note = " (affected tests only)"
		}
		if ok {
			o.emit(event.Event{Kind: event.Log, Text: fmt.Sprintf("verify ✓ %s%s (%s; %s)", label, r.cmd, took, clip(what, 300))})
			fmt.Fprintf(&b, "PASSED: %s%s%s\n", label, r.cmd, note)
			continue
		}
		allOK = false
		failed[r.of] = true
		why := lastLines(string(out), 40)
		if timedOut {
			why = fmt.Sprintf("timed out after %s\n%s", timeout, why)
		}
		o.emit(event.Event{Kind: event.Error, Text: fmt.Sprintf("verify ✗ %s%s (%s; %s): %s", label, r.cmd, took, clip(what, 200), clip(lastLines(string(out), 1), 200))})
		fmt.Fprintf(&b, "FAILED: %s%s%s\n%s\n", label, r.cmd, note, why)
	}
	return allOK, b.String(), len(failed)
}

// verifyAllowed is what a writing agent in dir may run without asking: the
// checks, and their narrowed forms (package affected), so an agent can
// run the tests its change affects like rw does.
func verifyAllowed(vc config.VerifyCfg, dir string) []string {
	out := append([]string(nil), vc.Commands...)
	if vc.Affected == affected.Off {
		return out
	}
	for _, c := range vc.Commands {
		pre, _ := affected.Allowed(dir, c, vc.AffectedCommands[c])
		for _, p := range pre {
			// Claude's --allowedTools is comma-joined: a comma would
			// split the rule into others.
			if !strings.Contains(p, ",") && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// verifyHint is the step prompt's line about the checks.
func verifyHint(vc config.VerifyCfg, dir string) string {
	s := "\nBefore you finish, run the repo's checks (" + strings.Join(vc.Commands, "; ") + ") and fix what your change broke."
	if vc.Affected != affected.Off {
		var forms []string
		for _, c := range vc.Commands {
			if _, h := affected.Allowed(dir, c, vc.AffectedCommands[c]); h != "" {
				forms = append(forms, h)
			}
		}
		if len(forms) > 0 {
			s += " While you work, you may run just the tests your change affects (" + strings.Join(forms, "; ") + ")."
		}
	}
	return s + "\n"
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
