package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

func TestScheduleAPIRunsWhenDue(t *testing.T) {
	env := newEnv(t, nil)
	var r submitResult
	env.call("POST", "/api/schedule", map[string]string{"when": "in 2h", "text": "later task"}, &r)
	if r.Status != "scheduled" || !strings.Contains(r.Message, "in 2h") {
		t.Fatalf("result = %+v", r)
	}
	for _, bad := range []map[string]string{
		{"when": "in 2h extra", "text": "x"},
		{"when": "sometime", "text": "x"},
		{"when": "in 2h", "text": "  "},
		{"when": "in 2h", "text": "@w follow up"},
	} {
		if res, data := env.do("POST", "/api/schedule", bad, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, res.StatusCode, data)
		}
	}
	st := env.state()
	if len(st.Queue) != 1 || st.Queue[0].At == nil || time.Until(*st.Queue[0].At) < 119*time.Minute || st.Running {
		t.Fatalf("queue = %+v running=%v", st.Queue, st.Running)
	}
	// Not due: nothing starts.
	time.Sleep(1200 * time.Millisecond)
	if env.count(event.TaskStart) != 0 {
		t.Fatal("a task scheduled in 2h started")
	}
	// Due: it starts on its own, unattended.
	env.srv.mu.Lock()
	env.srv.queue[0].at = time.Now().Add(-time.Second)
	env.srv.mu.Unlock()
	waitFor(t, "the scheduled task", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	if q := env.state().Queue; len(q) != 0 {
		t.Errorf("queue after = %+v", q)
	}
	// An unknown reset time means now.
	env.call("POST", "/api/schedule", map[string]string{"when": "reset codex", "text": "short one"}, &r)
	if !strings.Contains(r.Message, "starting now") {
		t.Errorf("reset message = %q", r.Message)
	}
	waitFor(t, "the reset task", func() bool { return env.count(event.TaskDone) == 2 })
}

func TestBudgetQuestionInBrowser(t *testing.T) {
	env := newEnv(t, func(c *config.Config) { c.Budget.TaskTokens = 1 })
	if st := env.state(); st.Budget.Limits.TaskTokens != 1 {
		t.Fatalf("budget in state = %+v", st.Budget)
	}
	env.call("POST", "/api/task", map[string]string{"text": longTask}, nil)
	var id string
	waitFor(t, "the budget question", func() bool {
		for _, a := range env.state().Approvals {
			if a.Type == "budget" && a.Budget != nil && strings.Contains(a.Budget.Text, "this task's tokens") && strings.Contains(a.Budget.Hint, "--budget-task-tokens") {
				id = a.ID
			}
		}
		return id != ""
	})
	if res, _ := env.do("POST", "/api/approvals/"+id+"/plan", map[string]any{"ok": true}, nil); res.StatusCode/100 == 2 {
		t.Error("a budget question was answered as a plan")
	}
	var out map[string]string
	env.call("POST", "/api/approvals/"+id+"/budget", map[string]bool{"ok": false}, &out)
	if !strings.Contains(out["message"], "stopping") {
		t.Errorf("answer = %v", out)
	}
	if res, _ := env.do("POST", "/api/approvals/"+id+"/budget", map[string]bool{"ok": true}, nil); res.StatusCode != http.StatusGone {
		t.Errorf("second answer: %d", res.StatusCode)
	}
	waitFor(t, "the task to stop", func() bool { return env.count(event.TaskDone) == 1 && !env.state().Running })
	if last := env.state().Last; last == nil || !strings.HasPrefix(last.Text, "stopped by budget") {
		t.Errorf("last = %+v", last)
	}
}
