package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/dayplan"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/schedule"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// The day planner in rw web (the Queue panel's "Fill windows"): with fill
// on, typed tasks go to the queue and the plan decides when each runs and
// on which subscription's window (internal/dayplan). Follow-ups and
// scheduled tasks keep their own order and time.

// fillReplan is how often a holding queue is planned again.
var fillReplan = 30 * time.Second

// fillLoadTask reads a task's saved state (tests swap it).
var fillLoadTask = orchestrator.LoadTask

// fillRetries is how often a task a usage limit stopped is resumed.
const fillRetries = 1

// fillState is guarded by Server.mu.
type fillState struct {
	on      bool
	planner *dayplan.Live
	check   time.Time        // plan again from then (zero = now)
	hold    string           // why the queue waits ("" = it does not)
	holdAt  time.Time        // when the plan's next task starts
	slots   map[int]slotView // the plan per queued job id
}

// slotView is a queued job's place in the day plan.
type slotView struct {
	Provider string    `json:"provider,omitempty"`
	Start    time.Time `json:"start,omitzero"`
	Notes    []string  `json:"notes,omitempty"`
	Skip     string    `json:"skip,omitempty"` // why the plan does not reach it
}

type fillView struct {
	On      bool       `json:"on"`
	Until   *time.Time `json:"until,omitempty"`
	FreshAt *time.Time `json:"fresh_at,omitempty"`
	Hold    string     `json:"hold,omitempty"`
	HoldAt  *time.Time `json:"hold_at,omitempty"`
}

// plannable reports whether the day plan picks when j runs: a task, or a
// limit-stopped one to resume, without a start time of its own.
func (j *job) plannable() bool {
	return !j.followUp && j.single == nil && j.at.IsZero() && (j.resume == nil || j.retries > 0)
}

func (s *Server) dayPlannerLocked() *dayplan.Live {
	if s.fill.planner == nil {
		l := &dayplan.Live{Cfg: s.store.Get, Dir: s.opt.Dir, Tracker: s.orc.Tracker()}
		if s.opt.Demo {
			l.ReadLogs = func(time.Time) []sessionlog.Record { return nil }
		}
		s.fill.planner = l
	}
	return s.fill.planner
}

// planQueue plans the queued tasks (not under s.mu: reading the logs may
// take a moment) and, when nothing runs, starts the one the plan runs now.
func (s *Server) planQueue(now time.Time) {
	s.mu.Lock()
	if !s.fill.on || now.Before(s.fill.check) {
		s.mu.Unlock()
		return
	}
	s.fill.check = now.Add(fillReplan)
	var ids []int
	var texts []string
	for _, j := range s.queue {
		if j.plannable() {
			ids, texts = append(ids, j.ID), append(texts, j.text)
		}
	}
	planner := s.dayPlannerLocked()
	s.mu.Unlock()

	slots := map[int]slotView{}
	var next dayplan.Next
	next.Task = -1
	hold := ""
	if len(ids) > 0 {
		st, err := planner.Plan(texts, now)
		if err != nil {
			hold = "fill: " + err.Error()
		} else {
			for _, sl := range st.Plan.Slots {
				slots[ids[sl.Task]] = slotView{Provider: sl.Provider, Start: sl.Start, Notes: sl.Notes()}
			}
			for _, sk := range st.Plan.Skipped {
				slots[ids[sk.Task]] = slotView{Skip: sk.Why}
			}
			next = st.Next(now)
			switch {
			case next.Task >= 0:
			case next.At.IsZero():
				hold = "fill: holding the queue - " + next.Why
			default:
				hold = "fill: nothing has room now; the next task " + next.Why
			}
		}
	}

	s.mu.Lock()
	s.fill.slots = slots
	var started *job
	if next.Task >= 0 && !s.running && !s.orc.Running() {
		for i, j := range s.queue {
			if j.ID == ids[next.Task] {
				s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
				j.lean, j.planned = next.Lean, true
				s.launchLocked(j)
				started = j
				break
			}
		}
	}
	if started == nil && next.Task >= 0 {
		s.fill.check = time.Time{} // something ran meanwhile: try again soon
	}
	if !next.At.IsZero() && next.At.Before(s.fill.check) {
		s.fill.check = next.At
	}
	changed := hold != s.fill.hold
	s.fill.hold, s.fill.holdAt = hold, next.At
	s.mu.Unlock()
	if started != nil {
		where := "on " + next.Lean.Provider
		if next.Lean.Strict {
			where = "only on " + next.Lean.Provider
		}
		s.notice("info", fmt.Sprintf("fill: starting a queued task %s (~%.0f%% of its window): %s", where, next.Share*100, oneLine(started.label(), 80)))
	} else if changed && hold != "" {
		s.notice("info", hold)
	}
	s.kick()
}

// fillAfter handles a planned job's end: a task a usage limit stopped goes
// back to the front of the queue, to be resumed when a window has room.
// s.mu is held.
func (s *Server) fillAfterLocked(j *job, res orchestrator.TaskResult, id string, cancelled bool, hitsBefore int) string {
	s.fill.check = time.Time{} // plan the next one now
	if !j.planned || res.OK || cancelled || id == "" || j.retries >= fillRetries ||
		dayplan.LimitHits(s.orc.Tracker(), s.store.Get()) <= hitsBefore {
		return ""
	}
	st, err := fillLoadTask(id)
	if err != nil {
		return ""
	}
	s.jobSeq++
	r := &job{ID: s.jobSeq, text: st.Task, resume: st, force: true, unattended: true, retries: j.retries + 1}
	s.queue = append([]*job{r}, s.queue...)
	return "fill: a usage limit stopped " + oneLine(st.Task, 60) + "; it resumes when a window has room"
}

// fillWaitingLocked reports whether planned work is pending (keep the PC
// awake). s.mu is held.
func (s *Server) fillWaitingLocked() bool {
	if !s.fill.on {
		return false
	}
	if s.current != nil && s.current.planned {
		return true
	}
	for _, j := range s.queue {
		if j.plannable() {
			return true
		}
	}
	return false
}

func (s *Server) fillViewLocked() fillView {
	v := fillView{On: s.fill.on, Hold: s.fill.hold}
	if s.fill.planner != nil {
		u, f := s.fill.planner.Bounds()
		if !u.IsZero() {
			v.Until = &u
		}
		if !f.IsZero() {
			v.FreshAt = &f
		}
	}
	if !s.fill.holdAt.IsZero() {
		t := s.fill.holdAt
		v.HoldAt = &t
	}
	return v
}

// handleFill: POST {"on": true, "until": "07:00", "fresh_at": "09:00"}
// switches fill on or off; GET returns the plan as text lines.
func (s *Server) handleFill(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mu.Lock()
		var texts []string
		for _, j := range s.queue {
			if j.plannable() {
				texts = append(texts, j.text)
			}
		}
		planner := s.dayPlannerLocked()
		s.mu.Unlock()
		if len(texts) == 0 {
			writeJSON(w, map[string][]string{"lines": {"Nothing queued to plan."}})
			return
		}
		st, err := planner.Plan(texts, time.Now())
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string][]string{"lines": st.Lines(texts)})
		return
	}
	var req struct {
		On      bool   `json:"on"`
		Until   string `json:"until"`
		FreshAt string `json:"fresh_at"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	now := time.Now()
	var until, fresh time.Time
	var err error
	if strings.TrimSpace(req.Until) != "" {
		if until, err = schedule.ParseAt(req.Until, now); err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("until: %w", err))
			return
		}
	}
	if strings.TrimSpace(req.FreshAt) != "" {
		if fresh, err = schedule.ParseAt(req.FreshAt, now); err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("full windows at: %w", err))
			return
		}
	}
	if !req.On && (!until.IsZero() || !fresh.IsZero()) {
		fail(w, http.StatusBadRequest, errors.New("until and full windows at go with fill on"))
		return
	}
	s.mu.Lock()
	s.dayPlannerLocked().SetBounds(until, fresh)
	s.fill.on, s.fill.check, s.fill.hold, s.fill.holdAt, s.fill.slots = req.On, time.Time{}, "", time.Time{}, nil
	s.mu.Unlock()
	msg := "fill off: queued tasks run one after another again"
	if req.On {
		msg = "fill on: queued tasks run when a subscription's window has room, each on the window that resets first"
		if !until.IsZero() {
			msg += "; none starts after " + schedule.Clock(until, now)
		}
		if !fresh.IsZero() {
			msg += "; no window opens that would still run at " + schedule.Clock(fresh, now)
		}
		msg += ". Tasks you type now are queued and run unattended."
	}
	s.notice("info", msg)
	s.kick()
	writeJSON(w, map[string]string{"message": msg})
}
