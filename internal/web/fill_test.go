package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func TestFillAPIQueuesWaitsAndLeans(t *testing.T) {
	env := newEnv(t, nil)
	tr := env.srv.orc.Tracker()
	now := time.Now()
	// Codex is nearly out for an hour; Claude is at its limit briefly.
	tr.SetQuota(event.Codex, event.QuotaInfo{Utilization: 0.95, Window: "5h", ResetsAt: now.Add(time.Hour)})
	tr.MarkLimited(event.Claude, now.Add(1500*time.Millisecond))

	for _, bad := range []map[string]any{
		{"on": true, "until": "soonish"},
		{"on": true, "fresh_at": "later"},
		{"on": false, "until": "07:00"},
	} {
		if res, data := env.do("POST", "/api/fill", bad, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, res.StatusCode, data)
		}
	}
	var out map[string]string
	env.call("POST", "/api/fill", map[string]any{"on": true, "until": now.Add(2 * time.Hour).Format("15:04")}, &out)
	if !strings.Contains(out["message"], "fill on") || !strings.Contains(out["message"], "none starts after") {
		t.Fatalf("message = %v", out)
	}
	if st := env.state(); !st.Fill.On || st.Fill.Until == nil {
		t.Fatalf("fill = %+v", st.Fill)
	}

	var r submitResult
	env.call("POST", "/api/task", map[string]string{"text": "fix the parser"}, &r)
	if r.Status != "queued" || !strings.Contains(r.Message, "day plan") {
		t.Fatalf("submit = %+v", r)
	}
	waitFor(t, "the plan in the state", func() bool {
		st := env.state()
		return len(st.Queue) == 1 && st.Queue[0].Plan != nil && strings.Contains(st.Fill.Hold, "nothing has room now")
	})
	st := env.state()
	if p := st.Queue[0].Plan; p.Provider != event.Claude || !strings.Contains(strings.Join(p.Notes, " "), "waits for claude's limit resets") || st.Running {
		t.Fatalf("plan = %+v running=%v", p, st.Running)
	}
	var plan struct {
		Lines []string `json:"lines"`
	}
	env.call("GET", "/api/fill", nil, &plan)
	if len(plan.Lines) == 0 || !strings.HasPrefix(plan.Lines[0], "Day plan · 1 queued task(s)") {
		t.Fatalf("plan lines = %v", plan.Lines)
	}
	env.srv.mu.Lock()
	awake := env.srv.fillWaitingLocked()
	env.srv.mu.Unlock()
	if !awake {
		t.Error("planned work pending, but the PC may sleep")
	}

	// Claude's limit resets: the task runs there, review included.
	waitFor(t, "the planned task", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	env.mu.Lock()
	routes := 0
	for _, e := range env.events {
		if e.Kind == event.Route && e.Decision != nil {
			routes++
			if e.Decision.Provider != event.Claude {
				t.Errorf("%s ran on %s", e.Decision.StepID, e.Decision.Provider)
			}
		}
	}
	env.mu.Unlock()
	if routes == 0 {
		t.Fatal("no routing decisions")
	}
	if st := env.state(); len(st.Queue) != 0 || st.Fill.Hold != "" {
		t.Fatalf("after: queue=%+v fill=%+v", st.Queue, st.Fill)
	}

	// Off: a typed task runs at once again.
	env.call("POST", "/api/fill", map[string]any{"on": false}, &out)
	env.call("POST", "/api/task", map[string]string{"text": "now"}, &r)
	if r.Status != "started" {
		t.Fatalf("with fill off: %+v", r)
	}
	waitFor(t, "the second task", func() bool { return env.count(event.TaskDone) == 2 })
}

func TestFillResumesALimitStoppedTask(t *testing.T) {
	env := newEnv(t, nil)
	old := fillLoadTask
	fillLoadTask = func(id string) (*orchestrator.TaskState, error) {
		if id != "t1" {
			return nil, errors.New("no such task")
		}
		return &orchestrator.TaskState{ID: id, Task: "big refactor"}, nil
	}
	defer func() { fillLoadTask = old }()
	s := env.srv
	s.orc.Tracker().MarkLimited(event.Claude, time.Now().Add(time.Hour))
	s.mu.Lock()
	defer s.mu.Unlock()
	note := s.fillAfterLocked(&job{text: "big refactor", planned: true}, orchestrator.TaskResult{}, "t1", false, 0)
	if !strings.Contains(note, "resumes when a window has room") || len(s.queue) != 1 {
		t.Fatalf("note=%q queue=%v", note, s.queue)
	}
	if j := s.queue[0]; j.resume == nil || !j.force || j.retries != 1 || !j.plannable() {
		t.Fatalf("requeued = %+v", j)
	}
	// Once only; not after a cancel or without a limit hit.
	s.queue = nil
	for _, c := range []struct {
		j         *job
		cancelled bool
		hits      int
	}{
		{&job{planned: true, retries: 1}, false, 0},
		{&job{planned: true}, true, 0},
		{&job{planned: true}, false, 1},
	} {
		if note := s.fillAfterLocked(c.j, orchestrator.TaskResult{}, "t1", c.cancelled, c.hits); note != "" || len(s.queue) != 0 {
			t.Fatalf("%+v: note=%q queue=%d", c, note, len(s.queue))
		}
	}
	// A resume the person asked for is not the plan's to schedule.
	if (&job{resume: &orchestrator.TaskState{}}).plannable() {
		t.Error("a resume job is plannable")
	}
}
