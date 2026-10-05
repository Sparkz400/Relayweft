package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// verify runs the repo's checks (verify.commands) in the working tree and
// returns whether all passed plus a report for the reviewer and the fix
// agent (tail of each failing command's output).
func (o *Orchestrator) verify(ctx context.Context, t *task) (bool, string) {
	return o.verifyIn(ctx, t, t)
}

// verifyRepos runs the checks of every repo of the task, each in its own
// repo, and also returns the names of the repos whose checks fail ("" is
// the primary).
func (o *Orchestrator) verifyRepos(ctx context.Context, t *task) (bool, string, map[string]bool) {
	allOK := true
	var b strings.Builder
	failing := map[string]bool{}
	for _, r := range t.allRepos() {
		ok, rep := o.verifyIn(ctx, t, r)
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

// verifyIn runs repo r's checks in r (the project folder for the primary).
func (o *Orchestrator) verifyIn(ctx context.Context, t, r *task) (bool, string) {
	cmds := r.cfg.Verify.Commands
	if len(cmds) == 0 {
		return true, ""
	}
	timeout := r.cfg.Verify.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	dir, label := o.opts.Dir, ""
	if r != t {
		dir, label = r.dir, r.repoName+": "
	}
	allOK := true
	var b strings.Builder
	for _, c := range cmds {
		if ctx.Err() != nil {
			return false, "cancelled"
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		var out []byte
		cmd, done, err := checkCmd(cctx, r.cfg, dir, c) // in the sandbox when it is on
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
		diag.Logf("verify %q in %s: ok=%v after %s err=%v", c, dir, ok, took, err)
		o.opts.Log.Write(sessionlog.Record{Type: "verify", TaskID: t.id, Text: label + c, OK: sessionlog.Bool(ok), DurationMS: took.Milliseconds()})
		if ok {
			o.emit(event.Event{Kind: event.Log, Text: fmt.Sprintf("verify ✓ %s%s (%s)", label, c, took)})
			fmt.Fprintf(&b, "PASSED: %s%s\n", label, c)
			continue
		}
		allOK = false
		why := lastLines(string(out), 40)
		if timedOut {
			why = fmt.Sprintf("timed out after %s\n%s", timeout, why)
		}
		o.emit(event.Event{Kind: event.Error, Text: fmt.Sprintf("verify ✗ %s%s (%s): %s", label, c, took, clip(lastLines(string(out), 1), 200))})
		fmt.Fprintf(&b, "FAILED: %s%s\n%s\n", label, c, why)
	}
	return allOK, b.String()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
