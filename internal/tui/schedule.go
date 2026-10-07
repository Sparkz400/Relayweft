package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/schedule"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// keepAwake is proc.KeepAwake; tests replace it.
var keepAwake = proc.KeepAwake

// resetTime looks up when a provider's limit resets: this rw's tracker,
// then the session logs (not in demo mode).
func (m *Model) resetTime(provider string) (time.Time, string) {
	var recs []sessionlog.Record
	if !m.opt.Demo {
		recs, _ = sessionlog.ReadDir(m.store.Get().SessionDir())
	}
	return schedule.ResetTimeIn(provider, m.store.Get().Enabled(), m.orc.Tracker(), recs, time.Now())
}

// scheduled returns the queue positions of scheduled jobs, in start order.
func (m *Model) scheduled() []int {
	var out []int
	for i, j := range m.queue {
		if !j.at.IsZero() {
			out = append(out, i)
		}
	}
	// Stable insertion sort by start time (the queue is short).
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && m.queue[out[k]].at.Before(m.queue[out[k-1]].at); k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}
	return out
}

// scheduleCommand: /schedule lists, /schedule rm <n> removes,
// /schedule <02:30|in 2h|reset claude|2026-10-04 02:30> <task> adds.
func (m *Model) scheduleCommand(args []string, say func(string, ...any)) {
	now := time.Now()
	usage := "usage: /schedule <02:30 | in 2h | reset claude|codex|any | 2026-10-04 02:30> <task> · /schedule · /schedule rm <n>"
	switch {
	case len(args) == 0:
		idx := m.scheduled()
		if len(idx) == 0 {
			say("nothing scheduled - %s", strings.TrimPrefix(usage, "usage: "))
			return
		}
		say("scheduled (%d), run unattended when due, after any running task:", len(idx))
		for n, i := range idx {
			j := m.queue[i]
			left := "due"
			if d := j.at.Sub(now); d > 0 {
				left = "in " + schedule.Left(d)
			}
			say("  %d. %s (%s) %s", n+1, schedule.Clock(j.at, now), left, oneLine(j.label(), 90))
		}
		return
	case args[0] == "rm" || args[0] == "remove":
		idx := m.scheduled()
		n := 0
		if len(args) == 2 {
			n, _ = strconv.Atoi(args[1])
		}
		if n < 1 || n > len(idx) {
			say("usage: /schedule rm <n> (1-%d, as /schedule lists them)", len(idx))
			return
		}
		i := idx[n-1]
		j := m.queue[i]
		m.queue = append(m.queue[:i:i], m.queue[i+1:]...)
		say("removed from the schedule: %s", oneLine(j.label(), 80))
		return
	}
	at, used, note, err := schedule.ParseWords(args, now, m.resetTime, m.store.Get().ProviderNames()...)
	if err != nil {
		say("%v - %s", err, usage)
		return
	}
	task := strings.TrimSpace(strings.Join(args[used:], " "))
	if task == "" {
		say("what should run? %s", usage)
		return
	}
	j := job{text: task}
	if f := strings.Fields(task); len(f) > 0 && f[0] == "/workflow" {
		if len(f) < 3 {
			say("usage: /schedule <when> /workflow <name> <task>")
			return
		}
		var err error
		if j, err = m.workflowJob(f[1], strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(task, f[0])), f[1]))); err != nil {
			say("%v", err)
			return
		}
	} else if strings.HasPrefix(task, "@") {
		say("follow-ups cannot be scheduled; schedule a task")
		return
	}
	if note != "" {
		say("%s", note)
	}
	if at.IsZero() {
		at = now // unknown or past reset: as soon as nothing else runs
	}
	j.unattended, j.at = true, at
	m.queue = append(m.queue, j)
	when := "now"
	if d := at.Sub(now); d > 0 {
		when = fmt.Sprintf("at %s (in %s)", schedule.Clock(at, now), schedule.Left(d))
	}
	awake := "the PC is kept awake until then"
	if m.opt.AllowSleep {
		awake = "the PC may sleep (--allow-sleep)"
	}
	how := "unattended (no approvals; a budget limit stops it)"
	if j.wf != nil {
		how = j.approvalsNote() + "; a budget limit stops it"
	}
	say("scheduled %s, %s: %s - %s; /schedule lists", when, how, oneLine(j.label(), 70), awake)
	m.tickSchedule()
}

// tickSchedule starts a due scheduled job when nothing runs, and keeps the
// machine awake while scheduled work waits or runs.
func (m *Model) tickSchedule() {
	if !m.running {
		m.startNext()
	}
	want := m.running && !m.current.at.IsZero() || m.fillWaiting()
	for _, j := range m.queue {
		want = want || !j.at.IsZero()
	}
	want = want && !m.opt.AllowSleep && !m.opt.Demo
	switch {
	case want && m.awake == nil:
		m.awake = keepAwake()
	case !want && m.awake != nil:
		m.awake()
		m.awake = nil
	}
}

// nextScheduled is the earliest scheduled start (zero when none).
func (m *Model) nextScheduled() time.Time {
	var t time.Time
	for _, j := range m.queue {
		if !j.at.IsZero() && (t.IsZero() || j.at.Before(t)) {
			t = j.at
		}
	}
	return t
}
