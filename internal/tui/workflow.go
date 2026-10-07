package tui

import (
	"errors"
	"os"
	"strings"

	"github.com/sparkz400/relayweft/internal/workflow"
)

// workflowCommand: /workflow lists the saved workflows, /workflow <name>
// <task> runs a task under one (queued while another task runs).
func (m *Model) workflowCommand(args []string, rest string, say func(string, ...any)) {
	if len(args) == 0 {
		ds, err := m.workflows()
		if err != nil {
			say("workflows: %v", err)
			return
		}
		if len(ds) == 0 {
			say("no saved workflows - run rw workflow --init in a terminal for the starters")
			return
		}
		for _, d := range ds {
			say("  %-20s %s%s", d.Name, d.Description, gatesNote(d))
		}
		say("usage: /workflow <name> <task> · /schedule <when> /workflow <name> <task>")
		return
	}
	j, err := m.workflowJob(args[0], strings.TrimSpace(strings.TrimPrefix(rest, args[0])))
	if err != nil {
		say("%v", err)
		return
	}
	m.startJob(j)
}

// workflows lists the saved workflows (the starters in demo mode).
func (m *Model) workflows() ([]workflow.Definition, error) {
	if m.opt.Demo {
		return workflow.Builtins(), nil
	}
	return workflow.List()
}

// workflowJob builds a job that runs task under the saved workflow name,
// read now: a queued or scheduled job runs it as it was when queued.
func (m *Model) workflowJob(name, task string) (job, error) {
	if task == "" {
		return job{}, errors.New("usage: /workflow <name> <task>")
	}
	var d workflow.Definition
	var err error
	if m.opt.Demo {
		err = errors.New("no workflow " + name)
		for _, b := range workflow.Builtins() {
			if b.Name == name {
				d, err = b, nil
			}
		}
	} else if d, err = workflow.Load(name); errors.Is(err, os.ErrNotExist) {
		err = errors.New("no saved workflow " + name + " (/workflow lists them)")
	}
	if err == nil {
		_, err = d.Render(task)
	}
	if err == nil {
		if aerr := d.Apply(m.store.Get()); aerr != nil { // Get is a copy
			err = errors.New("workflow " + d.Name + ": " + aerr.Error())
		}
	}
	if err != nil {
		return job{}, err
	}
	return job{text: task, wf: &d}, nil
}

// approvalsNote says how a queued or scheduled job treats approvals.
func (j job) approvalsNote() string {
	if j.wf != nil && j.wf.Gated() {
		return "waits for your approval (workflow " + j.wf.Name + ")"
	}
	return "unattended (no approvals)"
}

func gatesNote(d workflow.Definition) string {
	var g []string
	if d.ApprovePlan {
		g = append(g, "plan approval")
	}
	if d.ReviewChanges {
		g = append(g, "change review")
	}
	if len(g) == 0 {
		return ""
	}
	return " (waits for " + strings.Join(g, " and ") + ")"
}
