package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/schedule"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// scheduleTick is how often due scheduled jobs are looked for.
var scheduleTick = time.Second

// popDueLocked takes the first queued job that may start now (s.mu held):
// a job without a start time, or a scheduled one whose time has come.
func (s *Server) popDueLocked(now time.Time) *job {
	for i, j := range s.queue {
		if s.fill.on && j.plannable() {
			continue // the day plan picks these (planQueue)
		}
		if j.at.IsZero() || !now.Before(j.at) {
			s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
			return j
		}
	}
	return nil
}

// scheduleLoop starts scheduled jobs when they are due and nothing runs,
// and keeps the machine awake while scheduled work is pending.
func (s *Server) scheduleLoop() {
	t := time.NewTicker(scheduleTick)
	defer t.Stop()
	defer s.setAwake(false)
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		var next *job
		if !s.running && !s.orc.Running() {
			if next = s.popDueLocked(time.Now()); next != nil {
				s.launchLocked(next)
			}
		}
		pending := s.current != nil && !s.current.at.IsZero() || s.fillWaitingLocked()
		for _, j := range s.queue {
			pending = pending || !j.at.IsZero()
		}
		s.mu.Unlock()
		s.planQueue(time.Now())
		s.setAwake(pending && !s.opt.AllowSleep && !s.opt.Demo)
		if next != nil {
			s.notice("info", "starting scheduled task: "+oneLine(next.label(), 80))
			s.kick()
		}
	}
}

func (s *Server) setAwake(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case on && s.awake == nil:
		s.awake = proc.KeepAwake()
	case !on && s.awake != nil:
		s.awake()
		s.awake = nil
	}
}

// resetTime looks up when a provider's limit resets (tracker, then logs).
func (s *Server) resetTime(provider string) (time.Time, string) {
	var recs []sessionlog.Record
	if !s.opt.Demo {
		recs, _ = sessionlog.ReadDir(s.store.Get().SessionDir())
	}
	return schedule.ResetTimeIn(provider, s.store.Get().Enabled(), s.orc.Tracker(), recs, time.Now())
}

// scheduleJob queues j to start at j.at (unattended, like every queued
// task), or as soon as nothing runs when j.at is zero.
func (s *Server) scheduleJob(j *job) submitResult {
	s.mu.Lock()
	s.jobSeq++
	j.ID = s.jobSeq
	j.unattended = true
	s.queue = append(s.queue, j)
	s.mu.Unlock()
	s.kick()
	when := "as soon as nothing else runs"
	if !j.at.IsZero() {
		when = "at " + schedule.Clock(j.at, time.Now()) + " (in " + schedule.Left(time.Until(j.at)) + ")"
	}
	return submitResult{Status: "scheduled", JobID: j.ID, Message: "scheduled " + when + ", unattended: " + oneLine(j.label(), 60)}
}

// handleSchedule: POST {"when": "02:30" | "in 2h" | "reset claude" | "2026-10-04 02:30", "text": "task"}.
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		When string `json:"when"`
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		fail(w, http.StatusBadRequest, errors.New("type a task first"))
		return
	}
	if strings.HasPrefix(text, "@") {
		fail(w, http.StatusBadRequest, errors.New("follow-ups cannot be scheduled; schedule a task"))
		return
	}
	words := strings.Fields(req.When)
	at, used, note, err := schedule.ParseWords(words, time.Now(), s.resetTime, s.store.Get().ProviderNames()...)
	if err == nil && used != len(words) {
		err = fmt.Errorf("when %q: want e.g. 02:30, in 2h, reset claude or 2026-10-04 02:30", req.When)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	res := s.scheduleJob(&job{text: text, at: at})
	if note != "" {
		res.Message = note + " - " + res.Message
	}
	s.notice("info", res.Message)
	writeJSON(w, res)
}

// handleConflict answers whether an agent may resolve a merge conflict.
func (s *Server) handleConflict(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OK bool `json:"ok"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.ap.AnswerConflict(r.PathValue("id"), req.OK); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNoRequest) {
			code = http.StatusGone
		}
		fail(w, code, err)
		return
	}
	msg := "the change is kept on a branch"
	if req.OK {
		msg = "an agent resolves the conflict"
	}
	writeJSON(w, map[string]string{"message": msg})
}

func (s *Server) handleBudget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OK bool `json:"ok"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.ap.AnswerBudget(r.PathValue("id"), req.OK); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNoRequest) {
			code = http.StatusGone
		}
		fail(w, code, err)
		return
	}
	msg := "stopping the task (budget)"
	if req.OK {
		msg = "going on past the budget until this task ends"
	}
	writeJSON(w, map[string]string{"message": msg})
}
