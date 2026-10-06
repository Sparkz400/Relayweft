package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

var helpText = []string{
	"<task>                               run a task; typed while one runs, it is queued (runs unattended)",
	"@<agent> <message> · @ <message>     follow up with a finished agent (or the newest); tab completes ids",
	"                                     a running agent gets the message when its current turn ends",
	"/agents                              running agents, and finished ones that take a follow-up (kept across restarts)",
	"/models                              open the model picker (also m or ctrl+o)",
	"/route <role> <provider>:<model>[:effort]   e.g. /route worker claude:sonnet:high",
	"/prefer <role|all> <codex|claude|other|auto>",
	"/save                                write the current settings to " + config.FileName,
	"/save repo                           write routes, verify, hooks and approvals to this repo's " + config.RepoFileName,
	"/single <provider>:<model>[:effort] <task>  run one agent only (baseline for rw stats)",
	"/limit <codex|claude> [reset|set]    clear or set a provider's usage-limit state",
	"/approve on|off                      show the plan for editing before anything runs",
	"/review-changes on|off               show each agent's changes (per file or hunk) before they land",
	"/conflicts auto|resolve|ask|fail     when changes conflict: an agent resolves it (auto asks first for your own edits), ask, or keep it on a branch",
	"/verify [<command>|clear]            list, add or clear the checks run before the final review",
	"/queue · /queue clear · /queue rm <n>   tasks waiting to run",
	"/schedule <02:30|in 2h|reset claude> <task> · /schedule · /schedule rm <n>   run a task later, unattended",
	"/fill on [until 07:00] [fresh 09:00] · /fill off · /fill   day plan: queued tasks spend both subscriptions'",
	"                                     5-hour windows, each on the window that resets first; waits for resets",
	"/resume [<id>] · /history            continue an interrupted task · list the last 10 tasks",
	"/threads <n> · /parallel on|off · /review on|off (reviewer checkpoints) · /judge on|off",
	"/tiers on|off                        pick each work step's model from its difficulty and the quota left",
	"/pause · /unpause · /kill <agent> · /cancel · /clear · /usage",
	"/undo [yes] · /redo [yes]          preview, then revert (or re-apply) the last task's changes",
	"roles: " + strings.Join(event.Roles, ", "),
	"keys (agents focused): tab next · k k kill · p pause · x cancel · l log · m models · q quit",
	"anywhere: ctrl+x cancel task (the queue stays) · ctrl+o models · alt+enter new line · pasting never submits",
	"plan approval: ↑↓ · e edit prompt · x dependencies (space toggle, enter done) · d delete · k kind · r role · J/K move · enter run · esc cancel",
	"change review: ↑↓ file · space include · a all · n none · enter apply · f feedback · esc reject all",
	"  hunks: tab (or [ ]) into a modified file's hunks · ↑↓ hunk · space toggle · tab/esc back · [~] = applied in part",
	"  new, deleted, binary, one-hunk and truncated files can only be taken whole",
}

func (m *Model) command(line string) tea.Cmd {
	f := strings.Fields(line)
	cmd := strings.ToLower(strings.TrimPrefix(f[0], "/"))
	args := f[1:]
	say := func(format string, a ...any) { m.addLog(logLine{kind: event.Log, text: fmt.Sprintf(format, a...)}) }
	rest := strings.TrimSpace(strings.TrimPrefix(line, f[0]))
	if m.phase2Command(cmd, args, rest, say) {
		return nil
	}
	if cmd == "schedule" {
		m.scheduleCommand(args, say)
		return nil
	}
	if cmd == "fill" {
		m.fillCommand(args, say)
		return nil
	}
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
			if args[0] == "all" && r == event.RoleReviewer && m.store.Get().IsProvider(v) {
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
		if len(args) == 1 && args[0] == "repo" {
			p, err := m.store.SaveRepo(m.opt.Dir)
			if err != nil {
				say("save failed: %v", err)
			} else {
				say("saved this repo's settings (routes, verify, hooks, approvals) to %s - commit it to share", p)
			}
			return nil
		}
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
		prov, r, err := m.store.Get().ParseRouteFor(args[0])
		if err != nil {
			say("%v", err)
			return nil
		}
		m.startSingle(strings.Join(args[1:], " "), prov, r)
	case "limit":
		if len(args) < 1 || !m.store.Get().IsProvider(args[0]) {
			say("usage: /limit <%s> [reset|set]", strings.Join(m.store.Get().ProviderNames(), "|"))
			return nil
		}
		tr := m.orc.Tracker()
		if len(args) > 1 && args[1] == "set" {
			until := time.Now().Add(m.store.Get().Providers[args[0]].LimitCooldown.D())
			tr.MarkLimited(args[0], until)
			m.prov(args[0]).until = until
			say("%s marked at limit until %s", args[0], until.Format("15:04"))
		} else {
			tr.Clear(args[0])
			m.prov(args[0]).until = time.Time{}
			say("%s marked available", args[0])
		}
	case "threads":
		n, err := strconv.Atoi(strings.Join(args, ""))
		if err != nil || n < 1 {
			say("usage: /threads <n>")
			return nil
		}
		m.setOrch(func(c *config.Config) { c.Orchestrator.MaxThreads = n }, fmt.Sprintf("max_threads = %d", n))
	case "parallel", "review", "reviewer", "judge", "tiers":
		on, ok := onOff(args)
		if !ok {
			say("usage: /%s on|off", cmd)
			return nil
		}
		m.setOrch(func(c *config.Config) {
			switch cmd {
			case "parallel":
				c.Orchestrator.Parallel = on
			case "review", "reviewer":
				c.Orchestrator.ReviewBeforePlan = on
				c.Orchestrator.ReviewOnRepeatError = on
				c.Orchestrator.ReviewBeforeDone = on
			case "judge":
				c.Routing.Judge = on
			case "tiers":
				c.Routing.Tiers = config.TiersOff
				if on {
					c.Routing.Tiers = config.TiersAuto
				}
			}
		}, fmt.Sprintf("%s = %v", cmd, on))
	case "pause":
		m.orc.SetPaused(true)
		say("paused")
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
		for _, p := range m.shownProviders() {
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
		say("%s: checking the working tree...", cmd)
		return undoCmd(m.opt.Dir, cmd == "redo", len(args) == 1 && args[0] == "yes")
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

// undoMsg carries the lines an undo or redo produced.
type undoMsg []string

// undoCmd previews or applies an undo/redo off the UI thread: snapshots and
// restores in a big repo can take a while.
func undoCmd(dir string, redo, apply bool) tea.Cmd {
	return func() tea.Msg {
		verb := map[bool]string{true: "redo", false: "undo"}[redo]
		var out []string
		say := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
		if apply {
			plan, err := orchestrator.Undo(dir, "", redo, false)
			if err != nil {
				say("%s failed, nothing was changed: %v", verb, err)
				return undoMsg(out)
			}
			say("%s done: %d file(s) for task %q (%s)", verb, plan.TotalChanges(), oneLine(plan.Task.Task, 60), plan.Task.Key)
			if !redo {
				say("changed your mind? /redo yes puts the task's changes back")
			}
			return undoMsg(out)
		}
		plan, err := orchestrator.PreviewUndo(dir, "", redo)
		if err != nil {
			say("%v", err)
			return undoMsg(out)
		}
		say("%s task %q from %s would change %d file(s):", verb, oneLine(plan.Task.Task, 60), plan.Task.When.Format("Jan 2 15:04"), len(plan.Changes))
		for i, c := range plan.Changes {
			if i == 15 {
				say("  ... and %d more", len(plan.Changes)-15)
				break
			}
			say("  %s", c)
		}
		for _, o := range plan.Others { // multi-repo task: undone together
			say("  and in repo %s: %d file(s)", o.Repo, len(o.Changes))
		}
		if len(plan.Edited) > 0 {
			say("you edited %d of these after the task; your edits are kept (3-way merge, nothing is written on a conflict)", len(plan.Edited))
		}
		if len(plan.Unreported) > 0 {
			say("no agent reported changing %s - possibly your own edits during the task; `rw undo --agent-files-only` leaves them alone",
				strings.Join(plan.Unreported, ", "))
		}
		say("type /%s yes to apply", verb)
		return undoMsg(out)
	}
}
