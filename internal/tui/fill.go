package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/dayplan"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/schedule"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// The day planner in the TUI (/fill): with fill on, typed tasks go to the
// queue and the plan decides when each runs and on which subscription's
// window (internal/dayplan). Follow-ups and scheduled tasks keep their own
// order and time.

// fillReplan is how often a holding queue is planned again: readings
// change as the logs and the tracker do.
var fillReplan = 30 * time.Second

// fillLoadTask reads a task's saved state (tests swap it).
var fillLoadTask = orchestrator.LoadTask

// fillRetries is how often a task a usage limit stopped is resumed.
const fillRetries = 1

type fillState struct {
	on      bool
	planner *dayplan.Live
	check   time.Time // plan again from then (zero = now)
	hold    string    // why the queue waits, as last logged
	holdAt  time.Time // when the plan's next task starts (zero = none)
	run     *fillRun  // the planned task running or just finished
}

// fillRun is a planned task's outcome, written by its goroutine before
// done is closed.
type fillRun struct {
	job       job
	done      chan struct{}
	hits      int // limit hits before it started
	id        string
	res       orchestrator.TaskResult
	cancelled bool
}

// plannable reports whether the day plan picks when j runs: a task (or a
// resumed one) without a start time of its own.
func (j job) plannable() bool { return !j.followUp && j.at.IsZero() }

func (m *Model) dayPlanner() *dayplan.Live {
	if m.fill.planner == nil {
		l := &dayplan.Live{Cfg: m.store.Get, Dir: m.opt.Dir, Tracker: m.orc.Tracker()}
		if m.opt.Demo {
			l.ReadLogs = func(time.Time) []sessionlog.Record { return nil }
		}
		m.fill.planner = l
	}
	return m.fill.planner
}

// fillCommand: /fill shows the plan, /fill on [until <t>] [fresh <t>],
// /fill off.
func (m *Model) fillCommand(args []string, say func(string, ...any)) {
	usage := "usage: /fill on [until 07:00] [fresh 09:00] · /fill off · /fill (the plan)"
	if len(args) == 0 {
		m.fillShow(say)
		return
	}
	switch strings.ToLower(args[0]) {
	case "off":
		m.fill.on = false
		m.fill.hold, m.fill.holdAt = "", time.Time{}
		say("fill off: queued tasks run one after another again")
		m.tickSchedule()
	case "on":
		now := time.Now()
		var until, fresh time.Time
		for i := 1; i < len(args); i += 2 {
			if i+1 >= len(args) {
				say("%s", usage)
				return
			}
			t, err := schedule.ParseAt(args[i+1], now)
			if err != nil {
				say("%s: %v - %s", args[i], err, usage)
				return
			}
			switch strings.ToLower(args[i]) {
			case "until":
				until = t
			case "fresh", "fresh-at":
				fresh = t
			default:
				say("%s", usage)
				return
			}
		}
		m.dayPlanner().SetBounds(until, fresh)
		m.fill.on, m.fill.check, m.fill.hold = true, time.Time{}, ""
		msg := "fill on: queued tasks run when a subscription's window has room, each on the window that resets first"
		if !until.IsZero() {
			msg += "; none starts after " + schedule.Clock(until, now)
		}
		if !fresh.IsZero() {
			msg += "; no window opens that would still run at " + schedule.Clock(fresh, now)
		}
		say("%s. Tasks you type now are queued and run unattended (no approvals); /fill shows the plan", msg)
		m.tickSchedule()
	default:
		say("%s", usage)
	}
}

// fillShow says whether fill is on and prints the plan for the queue.
func (m *Model) fillShow(say func(string, ...any)) {
	state := "off (/fill on to plan the queue over the usage windows)"
	if m.fill.on {
		state = "on"
		if m.fill.hold != "" {
			state += " · " + m.fill.hold
		}
	}
	say("fill is %s", state)
	_, texts := m.plannableQueue()
	if len(texts) == 0 {
		say("nothing queued to plan - with fill on, typed tasks are queued")
		return
	}
	st, err := m.dayPlanner().Plan(texts, time.Now())
	if err != nil {
		say("%v", err)
		return
	}
	for _, l := range st.Lines(texts) {
		if l != "" {
			say("%s", l)
		}
	}
}

// plannableQueue is the queue positions and texts the plan picks from.
func (m *Model) plannableQueue() (idx []int, texts []string) {
	for i, j := range m.queue {
		if j.plannable() {
			idx = append(idx, i)
			texts = append(texts, j.text)
		}
	}
	return idx, texts
}

// fillSettled handles the last planned task once its goroutine has ended:
// a task a usage limit stopped goes back to the front of the queue, to be
// resumed when a window has room. It reports false while that task's
// outcome is not in yet.
func (m *Model) fillSettled() bool {
	r := m.fill.run
	if r == nil {
		return true
	}
	select {
	case <-r.done:
	default:
		return false
	}
	m.fill.run = nil
	m.fill.check = time.Time{} // plan the next one now
	if r.res.OK || r.cancelled || r.id == "" || r.job.retries >= fillRetries || dayplan.LimitHits(m.orc.Tracker(), m.store.Get()) <= r.hits {
		return true
	}
	st, err := fillLoadTask(r.id)
	if err != nil {
		return true
	}
	j := job{text: st.Task, resume: st, force: true, unattended: true, retries: r.job.retries + 1}
	m.queue = append([]job{j}, m.queue...)
	m.fill.check = time.Time{}
	m.addLog(logLine{kind: event.Log, text: fmt.Sprintf("fill: a usage limit stopped %s; it resumes when a window has room", oneLine(st.Task, 60))})
	return true
}

// startPlanned starts the queued task the day plan runs now, if any, and
// otherwise notes what the queue waits for.
func (m *Model) startPlanned(now time.Time) {
	idx, texts := m.plannableQueue()
	if len(idx) == 0 {
		m.fill.hold, m.fill.holdAt = "", time.Time{}
		return
	}
	if now.Before(m.fill.check) {
		return
	}
	m.fill.check = now.Add(fillReplan)
	st, err := m.dayPlanner().Plan(texts, now)
	if err != nil {
		m.fillHold("fill: "+err.Error(), time.Time{})
		return
	}
	next := st.Next(now)
	if next.Task < 0 {
		hold := "fill: nothing has room now; the next task " + next.Why
		if next.At.IsZero() {
			hold = "fill: holding the queue - " + next.Why
		} else if next.At.Before(m.fill.check) {
			m.fill.check = next.At
		}
		m.fillHold(hold, next.At)
		return
	}
	i := idx[next.Task]
	j := m.queue[i]
	m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
	j.lean, j.planned = next.Lean, true
	m.fill.hold, m.fill.holdAt = "", time.Time{}
	where := "on " + next.Lean.Provider
	if next.Lean.Strict {
		where = "only on " + next.Lean.Provider
	}
	m.addLog(logLine{kind: event.Log, text: fmt.Sprintf("fill: starting a queued task %s (~%.0f%% of its window, %d left): %s", where, next.Share*100, len(m.queue), oneLine(j.label(), 80))})
	m.startJob(j)
}

// fillHold logs why the queue waits, once per reason.
func (m *Model) fillHold(why string, at time.Time) {
	m.fill.holdAt = at
	if why != m.fill.hold {
		m.fill.hold = why
		m.addLog(logLine{kind: event.Log, text: why})
	}
}

// fillWaiting reports whether planned work is pending (keep the PC awake).
func (m *Model) fillWaiting() bool {
	if !m.fill.on {
		return false
	}
	if m.running && m.current.planned {
		return true
	}
	idx, _ := m.plannableQueue()
	return len(idx) > 0
}

// fillHeader is the header's note while fill is on.
func (m *Model) fillHeader(now time.Time) string {
	if !m.fill.on {
		return ""
	}
	s := " · fill"
	if !m.fill.holdAt.IsZero() {
		s += " waits to " + schedule.Clock(m.fill.holdAt, now)
	} else if m.fill.hold != "" {
		s += " holding"
	}
	return s
}
