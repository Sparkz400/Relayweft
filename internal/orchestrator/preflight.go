package orchestrator

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// A host-shell check cannot prove Claude's permission rules allow the same
// command. Probe through its runner and require successful tool results.
func (o *Orchestrator) preflight(ctx context.Context, t *task, only string) error {
	commands := t.cfg.Verify.Preflight
	if len(commands) == 0 {
		return nil
	}
	for _, command := range commands {
		if strings.TrimSpace(command) == "" || strings.ContainsAny(command, ",\r\n") {
			return fmt.Errorf("preflight: commands must be nonempty single lines without commas (Claude permission-rule separator)")
		}
	}
	for _, p := range t.cfg.Enabled() {
		if (only != "" && only != p) || t.cfg.Kind(p) != event.Claude {
			continue
		}
		rn := t.runners[p]
		if rn == nil {
			return fmt.Errorf("preflight: no runner for %s", p)
		}
		route := t.cfg.Roles[event.RoleWorker].For(p)
		id := "preflight-" + p
		account, admitted := o.admitAgent(ctx, t, router.Step{ID: id, Kind: router.KindExplore, Title: "check preflight"}, event.Decision{Provider: p, Role: event.RoleWorker, Model: route.Model, Effort: route.Effort}, id)
		if !admitted {
			return errBudget
		}
		spec := runner.Spec{AgentID: id, StepID: id, Attempt: 1, Provider: p, Role: event.RoleWorker,
			Model: route.Model, Effort: route.Effort, Dir: o.opts.Dir, Base: t.start,
			Timeout: 2 * time.Minute, CheckOnly: true, AllowedCommands: commands,
			Prompt: "Permission preflight only. Execute EACH of these exact shell commands once, separately, and stop. Do not inspect or implement the task. Do not edit files, substitute commands, or claim execution without a tool call. Report a failure immediately.\n\n" + strings.Join(commands, "\n")}
		o.logf("preflight: %s must execute %d configured checks before implementation", p, len(commands))
		res := rn.Run(ctx, spec, o.emit)
		account(res.Tokens)
		o.opts.Tracker.AddUsage(p, res.Tokens)
		err := res.Err
		if !res.OK() || res.PermissionDenied {
			err = fmt.Errorf("command execution failed or permission was denied: %v", res.Err)
		} else {
			for _, command := range commands {
				if !slices.Contains(res.Commands, command) {
					err = fmt.Errorf("no successful tool result for %q", command)
					break
				}
			}
		}
		o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TaskID: t.id, Agent: id, Step: id,
			Kind: "preflight", Provider: p, Role: event.RoleWorker, Model: route.Model, Effort: route.Effort,
			Tokens: &res.Tokens, DurationMS: res.Duration.Milliseconds(), OK: sessionlog.Bool(err == nil), Error: errText(err)})
		o.noteBudget(t, id)
		if err != nil {
			return fmt.Errorf("preflight %s: %w; implementation did not start; check verify.preflight, CLI permissions and tool availability", p, err)
		}
	}
	return nil
}
