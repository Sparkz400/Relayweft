package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// verify runs the repo's checks (verify.commands) in the working tree and
// returns whether all passed plus a report for the reviewer and the fix
// agent (tail of each failing command's output).
func (o *Orchestrator) verify(ctx context.Context, t *task) (bool, string) {
	cmds := t.cfg.Verify.Commands
	if len(cmds) == 0 {
		return true, ""
	}
	timeout := t.cfg.Verify.Timeout.D()
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	allOK := true
	var b strings.Builder
	for _, c := range cmds {
		if ctx.Err() != nil {
			return false, "cancelled"
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := proc.Shell(cctx, c)
		cmd.Dir = o.opts.Dir
		start := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(start).Round(100 * time.Millisecond)
		timedOut := cctx.Err() == context.DeadlineExceeded
		cancel()
		ok := err == nil
		diag.Logf("verify %q in %s: ok=%v after %s err=%v", c, o.opts.Dir, ok, took, err)
		o.opts.Log.Write(sessionlog.Record{Type: "verify", TaskID: t.id, Text: c, OK: sessionlog.Bool(ok), DurationMS: took.Milliseconds()})
		if ok {
			o.emit(event.Event{Kind: event.Log, Text: fmt.Sprintf("verify ✓ %s (%s)", c, took)})
			fmt.Fprintf(&b, "PASSED: %s\n", c)
			continue
		}
		allOK = false
		why := lastLines(string(out), 40)
		if timedOut {
			why = fmt.Sprintf("timed out after %s\n%s", timeout, why)
		}
		o.emit(event.Event{Kind: event.Error, Text: fmt.Sprintf("verify ✗ %s (%s): %s", c, took, clip(lastLines(string(out), 1), 200))})
		fmt.Fprintf(&b, "FAILED: %s\n%s\n", c, why)
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
