package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/notify"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// job is something the TUI runs as a task: a new task, a resumed one or a
// follow-up message to a finished agent.
type job struct {
	text       string
	followUp   bool
	agent      string // follow-up target; "" = the newest agent
	resume     *orchestrator.TaskState
	unattended bool // queued: never waits for approvals
}

func (j job) label() string {
	switch {
	case j.followUp && j.agent == "":
		return "@ " + j.text
	case j.followUp:
		return "@" + j.agent + " " + j.text
	case j.resume != nil:
		return "resume: " + j.resume.Task
	}
	return j.text
}

// parseFollowUp reads "@<agent> message" or "@ message" (the newest agent).
// ok is false when text is not a follow-up at all.
func parseFollowUp(text string) (agent, msg string, ok bool) {
	if !strings.HasPrefix(text, "@") {
		return "", "", false
	}
	rest := text[1:]
	if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' {
		return "", strings.TrimSpace(rest), true
	}
	i := strings.IndexAny(rest, " \t\n")
	if i < 0 {
		return rest, "", true
	}
	agent = rest[:i]
	if agent == "last" {
		agent = ""
	}
	return agent, strings.TrimSpace(rest[i:]), true
}

// submit starts typed text: a follow-up when it starts with @, else a task.
func (m *Model) submit(text string) {
	if agent, msg, ok := parseFollowUp(text); ok {
		if msg == "" {
			m.flashNotice("usage: @<agent> <message> or @ <message> for the last agent (/agents lists them)")
			return
		}
		if _, have := m.orc.Session(agent); !have {
			m.flashNotice(fmt.Sprintf("no finished agent %q to follow up - /agents lists them", orLast(agent)))
			return
		}
		m.startJob(job{text: msg, followUp: true, agent: agent})
		return
	}
	m.startTask(text)
}

func orLast(agent string) string {
	if agent == "" {
		return "last"
	}
	return agent
}

// startTask runs a new task, or queues it while another one runs.
func (m *Model) startTask(text string) { m.startJob(job{text: text}) }

// startJob runs j now, or queues it (unattended) when a task is running.
func (m *Model) startJob(j job) {
	if m.running {
		if j.resume != nil {
			m.flashNotice("a task is running - /resume once it has finished")
			return
		}
		j.unattended = true
		m.queue = append(m.queue, j)
		m.flashNotice(fmt.Sprintf("queued (%d): %s - runs unattended (no approvals) after the current task; /queue lists", len(m.queue), oneLine(j.label(), 60)))
		return
	}
	if m.orc.Running() {
		// TaskDone always comes after the orchestrator is free; this is
		// only reachable when a task was started outside the TUI.
		m.flashNotice("the orchestrator is still busy - try again in a moment")
		return
	}
	m.resetTree()
	m.running = true
	m.taskText = j.label()
	m.taskStart = time.Now()
	m.result = ""
	m.nodes[orchestrator.AgentMain].title = m.taskText
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTask = cancel
	done := make(chan struct{})
	m.taskDone = done
	if j.resume != nil {
		m.interrupted = nil
	}
	orc := m.orc
	go func() {
		defer close(done)
		if j.followUp {
			orc.FollowUp(ctx, j.agent, j.text)
			return
		}
		orc.RunWith(ctx, j.text, orchestrator.TaskOptions{Unattended: j.unattended, Resume: j.resume})
	}()
	if m.overlay == nil {
		m.focus = focusTree
		m.input.Blur()
	}
}

// startNext runs the next queued job, if any.
func (m *Model) startNext() {
	if m.running || len(m.queue) == 0 {
		return
	}
	j := m.queue[0]
	m.queue = m.queue[1:]
	m.addLog(logLine{kind: event.Log, text: fmt.Sprintf("starting queued task (%d left): %s", len(m.queue), oneLine(j.label(), 80))})
	m.startJob(j)
}

// sendNotify is notify.Send; tests replace it.
var sendNotify = notify.Send

// alert shows a desktop notification when notifications are on. It never
// blocks the UI: Send can take seconds.
func (m *Model) alert(title, body string) {
	if m.opt.Demo || !m.store.Get().Notify.Enabled {
		return
	}
	send := sendNotify
	go func() {
		if send(title, body) != nil {
			// No desktop notifications here: ring the terminal bell.
			// stderr, because Bubble Tea owns stdout.
			os.Stderr.WriteString(notify.Bell())
		}
	}()
}

// notifyDone tells the person a long task has ended.
func (m *Model) notifyDone(ok bool, summary string, took time.Duration) {
	if took < m.store.Get().Notify.MinTask.D() {
		return
	}
	title := "Switchyard: done"
	if !ok {
		title = "Switchyard: failed"
	}
	m.alert(title, oneLine(summary, 200))
}

// checkInterrupted looks for a task a crash or a closed window cut short.
func (m *Model) checkInterrupted() {
	if m.opt.Demo {
		return
	}
	s := orchestrator.LastInterrupted(m.opt.Dir)
	if s == nil {
		return
	}
	m.interrupted = s
	m.addLog(logLine{kind: event.Log, text: fmt.Sprintf("task interrupted at %s: %s - /resume to continue", s.Updated.Format("Jan 2 15:04"), oneLine(s.Task, 80))})
}

// phase2Command handles the commands for approvals, verify, follow-ups,
// the queue and task history. handled is false for other commands.
func (m *Model) phase2Command(cmd string, args []string, rest string, say func(string, ...any)) (handled bool) {
	switch cmd {
	case "approve":
		on, ok := onOff(args)
		if !ok {
			say("usage: /approve on|off  (now %s: show the plan before anything runs)", onWord(m.store.Get().Orchestrator.ApprovePlan))
			return true
		}
		m.setOrch(func(c *config.Config) { c.Orchestrator.ApprovePlan = on }, fmt.Sprintf("approve plan = %v", on))
	case "review-changes":
		on, ok := onOff(args)
		if !ok {
			say("usage: /review-changes on|off  (now %s: show each agent's changes before they land)", onWord(m.store.Get().Orchestrator.ReviewChanges))
			return true
		}
		m.setOrch(func(c *config.Config) { c.Orchestrator.ReviewChanges = on }, fmt.Sprintf("review changes = %v", on))
	case "verify":
		m.verifyCommand(args, rest, say)
	case "agents":
		ss := m.orc.Sessions()
		if len(ss) == 0 {
			say("no finished agents yet - follow-ups need an agent that finished in this session")
			return true
		}
		say("agents that take a follow-up (@<agent> message · @ message = the newest):")
		for _, s := range ss {
			say("  %-12s %-10s %s:%s · %s · %s", s.AgentID, s.Role, s.Provider, s.Model, s.Ended.Format("15:04"), oneLine(s.Title, 60))
		}
	case "queue":
		m.queueCommand(args, say)
	case "resume":
		if len(args) == 0 && m.orc.Paused() {
			m.orc.SetPaused(false)
			say("resumed")
			return true
		}
		m.resumeCommand(args, say)
	case "unpause":
		m.orc.SetPaused(false)
		say("resumed")
	case "history":
		hs := orchestrator.History(m.opt.Dir, 10)
		if len(hs) == 0 {
			say("no tasks recorded for this folder yet")
			return true
		}
		for _, s := range hs {
			status := s.Status
			if s.Interrupted() {
				status = "interrupted"
			}
			line := fmt.Sprintf("%-11s %s  %s  %s", status, s.Created.Format("Jan 2 15:04"), s.ID, oneLine(s.Task, 60))
			if s.CostLine != "" {
				line += " · " + s.CostLine
			}
			say("%s", line)
		}
		say("/resume <id> continues one (finished subtasks are skipped)")
	default:
		return false
	}
	return true
}

func onWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (m *Model) verifyCommand(args []string, line string, say func(string, ...any)) {
	if len(args) == 0 {
		cmds := m.store.Get().Verify.Commands
		if len(cmds) == 0 {
			say("no verify commands: /verify <command> adds one (e.g. /verify go test ./...)")
			return
		}
		say("verify commands (run before the final review; agents may run them too):")
		for i, c := range cmds {
			say("  %d. %s", i+1, c)
		}
		return
	}
	if len(args) == 1 && args[0] == "clear" {
		m.setOrch(func(c *config.Config) { c.Verify.Commands = nil }, "verify commands cleared")
		return
	}
	m.setOrch(func(c *config.Config) {
		c.Verify.Commands = append(append([]string(nil), c.Verify.Commands...), line)
	}, "verify command added: "+line)
}

func (m *Model) queueCommand(args []string, say func(string, ...any)) {
	switch {
	case len(args) == 0:
		if len(m.queue) == 0 {
			say("the queue is empty - a task typed while one runs is queued")
			return
		}
		say("queued (%d), run one after another, unattended:", len(m.queue))
		for i, j := range m.queue {
			say("  %d. %s", i+1, oneLine(j.label(), 100))
		}
	case args[0] == "clear":
		n := len(m.queue)
		m.queue = nil
		say("queue cleared (%d dropped)", n)
	case args[0] == "rm" && len(args) == 2:
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 || n > len(m.queue) {
			say("usage: /queue rm <n> (1-%d)", len(m.queue))
			return
		}
		j := m.queue[n-1]
		m.queue = append(m.queue[:n-1:n-1], m.queue[n:]...)
		say("removed from the queue: %s", oneLine(j.label(), 80))
	default:
		say("usage: /queue · /queue clear · /queue rm <n>")
	}
}

func (m *Model) resumeCommand(args []string, say func(string, ...any)) {
	if m.running {
		say("a task is running - /resume once it has finished")
		return
	}
	var s *orchestrator.TaskState
	if len(args) > 0 {
		var err error
		if s, err = orchestrator.LoadTask(args[0]); err != nil {
			say("%v", err)
			return
		}
		if s.Status == "running" && !s.Interrupted() {
			say("task %s is still running in another sy", s.ID)
			return
		}
		if s.Status == "done" {
			say("task %s already finished; resuming runs the steps that did not succeed", s.ID)
		}
		if s.Dir != "" && !sameDir(s.Dir, m.opt.Dir) {
			say("note: task %s ran in %s, this sy works in %s", s.ID, s.Dir, m.opt.Dir)
		}
	} else {
		s = m.interrupted
		if s == nil {
			s = orchestrator.LastInterrupted(m.opt.Dir)
		}
		if s == nil {
			say("nothing to resume here - /history lists past tasks, /resume <id> picks one")
			return
		}
	}
	say("resuming task %s: %s", s.ID, oneLine(s.Task, 80))
	m.startJob(job{text: s.Task, resume: s})
}

func sameDir(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, `/\`), strings.TrimRight(b, `/\`))
}

// completeAgent completes "@<prefix>" in the prompt to the next finished
// agent id that matches, cycling on repeated tabs.
func (m *Model) completeAgent() bool {
	v := m.input.Value()
	prefix := ""
	if m.complete.active && v == "@"+m.complete.last+" " {
		prefix = m.complete.prefix // tab again: the next match
	} else {
		m.complete.active = false
		if !strings.HasPrefix(v, "@") || strings.ContainsAny(v, " \t\n") {
			return false
		}
		prefix = v[1:]
	}
	var ids []string
	for _, s := range m.orc.Sessions() {
		ids = append(ids, s.AgentID)
	}
	sort.Strings(ids)
	var match []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			match = append(match, id)
		}
	}
	if len(match) == 0 {
		m.flashNotice("no finished agent matches @" + prefix + " (/agents lists them)")
		return true
	}
	next := match[0]
	if m.complete.active {
		for i, id := range match {
			if id == m.complete.last {
				next = match[(i+1)%len(match)]
				break
			}
		}
	}
	m.complete.active, m.complete.prefix, m.complete.last = true, prefix, next
	m.input.SetValue("@" + next + " ")
	m.input.CursorEnd()
	return true
}
