package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// runHooks runs one hook list (config hooks.*) in the project folder. It
// stops at the first failure and returns it.
func (o *Orchestrator) runHooks(ctx context.Context, t *task, which string, cmds []string, env map[string]string) error {
	if len(cmds) == 0 || o.opts.Bench != "" || o.opts.Mode == "demo" {
		return nil
	}
	timeout := t.cfg.Hooks.Timeout.D()
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	base := map[string]string{"SY_TASK": t.text, "SY_TASK_ID": t.key, "SY_DIR": o.opts.Dir, "SY_HOOK": which}
	for k, v := range env {
		base[k] = v
	}
	vars := proc.WithoutSecrets(os.Environ())
	for k, v := range base {
		vars = append(vars, k+"="+v)
	}
	for _, c := range cmds {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := proc.Shell(cctx, c)
		cmd.Dir = o.opts.Dir
		cmd.Env = vars
		start := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(start).Round(100 * time.Millisecond)
		if cctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		cancel()
		diag.Logf("hook %s %q: err=%v after %s", which, c, err, took)
		o.opts.Log.Write(sessionlog.Record{Type: "hook", TaskID: t.id, Text: which + ": " + c, OK: sessionlog.Bool(err == nil), DurationMS: took.Milliseconds()})
		if err != nil {
			msg := fmt.Sprintf("hook %s failed: %s: %v %s", which, c, err, clip(lastLines(string(out), 5), 400))
			o.emit(event.Event{Kind: event.Error, Text: msg})
			return fmt.Errorf("%s", msg)
		}
		o.emit(event.Event{Kind: event.Log, Text: fmt.Sprintf("hook %s ✓ %s (%s)", which, c, took)})
	}
	return nil
}

// afterMerge runs the after_merge hooks for a step's landed files.
func (o *Orchestrator) afterMerge(ctx context.Context, t *task, stepID string, files []string) {
	o.runHooks(ctx, t, "after_merge", t.cfg.Hooks.AfterMerge, map[string]string{"SY_STEP": stepID, "SY_FILES": strings.Join(files, "\n")})
}
