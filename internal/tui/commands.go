package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

var helpText = []string{
	"/models                              open the model picker (also m or ctrl+o)",
	"/route <role> <provider>:<model>[:effort]   e.g. /route worker claude:sonnet:high",
	"/prefer <role|all> <codex|claude|other|auto>",
	"/save                                write the current models to " + config.FileName,
	"/single <provider>:<model>[:effort] <task>  run one agent only (baseline for sy stats)",
	"/limit <codex|claude> [reset|set]    clear or set a provider's usage-limit state",
	"/threads <n> · /parallel on|off · /review on|off · /judge on|off",
	"/pause · /resume · /kill <agent> · /cancel · /clear · /usage",
	"/undo [yes] · /redo [yes]          preview, then revert (or re-apply) the last task's changes",
	"roles: " + strings.Join(event.Roles, ", "),
	"keys (agents focused): tab next · k k kill · p pause · x cancel · l log · m models · q quit",
	"anywhere: ctrl+x cancel task · ctrl+o models · alt+enter new line in the prompt · pasting multi-line text never submits",
}

func (m *Model) command(line string) tea.Cmd {
	f := strings.Fields(line)
	cmd := strings.ToLower(strings.TrimPrefix(f[0], "/"))
	args := f[1:]
	say := func(format string, a ...any) { m.addLog(logLine{kind: event.Log, text: fmt.Sprintf(format, a...)}) }
	switch cmd {
	case "help", "h", "?":
		for _, h := range helpText {
			say("%s", h)
		}
	case "models", "model", "m":
		m.openPicker()
	case "route":
		if len(args) != 2 {
			say("usage: /route <role> <provider>:<model>[:effort]")
			return nil
		}
		prov, r, err := config.ParseRouteSpec(args[1])
		if err == nil {
			err = m.store.SetRoute(args[0], prov, r)
		}
		if err != nil {
			say("route not changed: %v", err)
			return nil
		}
		m.dirty = true
		say("%s on %s now uses %s (unsaved; /save to keep)", args[0], prov, args[1])
	case "prefer":
		if len(args) != 2 {
			say("usage: /prefer <role|all> <codex|claude|other|auto>")
			return nil
		}
		roles := []string{args[0]}
		if args[0] == "all" {
			roles = event.Roles
		}
		for _, r := range roles {
			v := args[1]
			if args[0] == "all" && r == event.RoleReviewer && (v == "codex" || v == "claude") {
				// keep the reviewer on the other provider unless asked by name
				continue
			}
			if err := m.store.SetPrefer(r, v); err != nil {
				say("prefer not changed: %v", err)
				return nil
			}
		}
		m.dirty = true
		say("prefer %s for %s (unsaved; /save to keep)", args[1], args[0])
	case "save":
		if err := m.store.Save(); err != nil {
			say("save failed: %v", err)
		} else {
			m.dirty = false
			say("saved to %s", m.store.Path())
		}
	case "single":
		if len(args) < 2 {
			say("usage: /single <provider>:<model>[:effort] <task>")
			return nil
		}
		prov, r, err := config.ParseRouteSpec(args[0])
		if err != nil {
			say("%v", err)
			return nil
		}
		m.startSingle(strings.Join(args[1:], " "), prov, r)
	case "limit":
		if len(args) < 1 || (args[0] != event.Codex && args[0] != event.Claude) {
			say("usage: /limit <codex|claude> [reset|set]")
			return nil
		}
		tr := m.orc.Tracker()
		if len(args) > 1 && args[1] == "set" {
			until := time.Now().Add(m.store.Get().Providers[args[0]].LimitCooldown.D())
			tr.MarkLimited(args[0], until)
			m.provs[args[0]].until = until
			say("%s marked at limit until %s", args[0], until.Format("15:04"))
		} else {
			tr.Clear(args[0])
			m.provs[args[0]].until = time.Time{}
			say("%s marked available", args[0])
		}
	case "threads":
		n, err := strconv.Atoi(strings.Join(args, ""))
		if err != nil || n < 1 {
			say("usage: /threads <n>")
			return nil
		}
		m.setOrch(func(c *config.Config) { c.Orchestrator.MaxThreads = n }, fmt.Sprintf("max_threads = %d", n))
	case "parallel", "review", "judge":
		on, ok := onOff(args)
		if !ok {
			say("usage: /%s on|off", cmd)
			return nil
		}
		m.setOrch(func(c *config.Config) {
			switch cmd {
			case "parallel":
				c.Orchestrator.Parallel = on
			case "review":
				c.Orchestrator.ReviewBeforePlan = on
				c.Orchestrator.ReviewOnRepeatError = on
				c.Orchestrator.ReviewBeforeDone = on
			case "judge":
				c.Routing.Judge = on
			}
		}, fmt.Sprintf("%s = %v", cmd, on))
	case "pause":
		m.orc.SetPaused(true)
		say("paused")
	case "resume":
		m.orc.SetPaused(false)
		say("resumed")
	case "kill":
		if len(args) != 1 || !m.orc.Kill(args[0]) {
			say("usage: /kill <agent> (running: %s)", strings.Join(m.orc.RunningAgents(), ", "))
		}
	case "cancel":
		m.cancelRunning()
	case "clear":
		if !m.running {
			m.resetTree()
		}
		m.logs = nil
	case "usage":
		for _, p := range event.Providers {
			s := m.orc.Tracker().Snapshot(p)
			q := ""
			if s.Quota != nil {
				q = fmt.Sprintf(" · quota %.0f%% (%s)", s.Quota.Utilization*100, s.Quota.Window)
			}
			lim := ""
			if s.Limited(time.Now()) {
				lim = " · LIMITED until " + s.LimitedUntil.Format("15:04")
			}
			say("%s: %d calls · %s in / %s out%s%s", p, s.Calls, sessionlog.Human(s.Tokens.Input), sessionlog.Human(s.Tokens.Output), q, lim)
		}
	case "undo", "redo":
		if m.running {
			say("a task is running - cancel it first (ctrl+x), then /%s", cmd)
			return nil
		}
		redo := cmd == "redo"
		if len(args) == 1 && args[0] == "yes" {
			plan, err := orchestrator.Undo(m.opt.Dir, "", redo)
			if err != nil {
				say("%s failed, nothing was changed: %v", cmd, err)
				return nil
			}
			say("%s done: %d file(s) restored for task %q (%s)", cmd, len(plan.Changes), oneLine(plan.Task.Task, 60), plan.Task.Key)
			if !redo {
				say("changed your mind? /redo yes puts the task's changes back")
			}
			return nil
		}
		plan, err := orchestrator.PreviewUndo(m.opt.Dir, "", redo)
		if err != nil {
			say("%v", err)
			return nil
		}
		say("%s task %q from %s would change %d file(s):", cmd, oneLine(plan.Task.Task, 60), plan.Task.When.Format("Jan 2 15:04"), len(plan.Changes))
		for i, c := range plan.Changes {
			if i == 15 {
				say("  ... and %d more", len(plan.Changes)-15)
				break
			}
			say("  %s", c)
		}
		if len(plan.Edited) > 0 {
			say("you edited %d of these since; your edits are kept (3-way merge, nothing is written on a conflict)", len(plan.Edited))
		}
		say("type /%s yes to apply", cmd)
	case "quit", "exit", "q":
		_, c := m.tryQuit()
		return c
	default:
		say("unknown command %q - /help lists commands", f[0])
	}
	return nil
}

func (m *Model) setOrch(fn func(c *config.Config), what string) {
	err := m.store.Update(func(c *config.Config) error { fn(c); return nil })
	if err != nil {
		m.addLog(logLine{kind: event.Log, text: "not changed: " + err.Error()})
		return
	}
	m.dirty = true
	m.addLog(logLine{kind: event.Log, text: what + " (next task; /save to keep)"})
}

func onOff(args []string) (bool, bool) {
	if len(args) != 1 {
		return false, false
	}
	switch strings.ToLower(args[0]) {
	case "on", "true", "yes", "1":
		return true, true
	case "off", "false", "no", "0":
		return false, true
	}
	return false, false
}
