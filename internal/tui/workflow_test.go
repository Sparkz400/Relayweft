package tui

import (
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/workflow"
)

func TestWorkflowCommandQueuesAndSchedules(t *testing.T) {
	stubAwake(t)
	m, _, _ := newModel(t, false)
	data := []byte("name: fixture\ndescription: test workflow\nprompt: 'Investigate {{task}}'\napprove_plan: true\n")
	if err := workflow.Save(data, false); err != nil {
		t.Fatal(err)
	}
	logged := func(s string) bool {
		for _, l := range m.logs {
			if strings.Contains(l.text, s) {
				return true
			}
		}
		return false
	}
	m.command("/workflow")
	if !logged("fixture") || !logged("waits for plan approval") {
		t.Fatal("/workflow does not list the saved workflow and its approvals")
	}
	for _, bad := range []string{"/workflow nope fix it", "/workflow fixture", "/schedule in 2h /workflow fixture", "/schedule in 2h /workflow nope fix"} {
		m.command(bad)
		if len(m.queue) != 0 || m.running {
			t.Fatalf("%q queued or started something", bad)
		}
	}
	m.command("/schedule in 2h /workflow fixture the CSV parser")
	if len(m.queue) != 1 || m.queue[0].wf == nil || m.queue[0].wf.Name != "fixture" || m.queue[0].text != "the CSV parser" || !m.queue[0].unattended {
		t.Fatalf("queue = %+v", m.queue)
	}
	if !logged("waits for your approval (workflow fixture)") || m.queue[0].label() != "fixture: the CSV parser" {
		t.Error("the schedule message does not say the workflow's approvals hold")
	}
	m.Shutdown()
}
