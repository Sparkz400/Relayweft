package web

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/sparkz400/relayweft/internal/workflow"
	"gopkg.in/yaml.v3"
)

// workflowView is a saved workflow as the page shows it in its pickers.
type workflowView struct {
	workflow.Definition
	// Problem says why the workflow cannot run in this project now
	// (it requires checks and none are configured).
	Problem string `json:"problem,omitempty"`
}

type workflowsView struct {
	Workflows []workflowView `json:"workflows"`
	Dir       string         `json:"dir,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// listWorkflows is workflow.List; demo mode shows the starters without
// reading or writing the user's files.
func (s *Server) listWorkflows() ([]workflow.Definition, error) {
	if s.opt.Demo {
		return workflow.Builtins(), nil
	}
	return workflow.List()
}

// loadWorkflow is workflow.Load (the starters in demo mode).
func (s *Server) loadWorkflow(name string) (workflow.Definition, error) {
	if s.opt.Demo {
		for _, d := range workflow.Builtins() {
			if d.Name == name {
				return d, nil
			}
		}
		return workflow.Definition{}, errors.New("no workflow " + name)
	}
	d, err := workflow.Load(name)
	if errors.Is(err, os.ErrNotExist) {
		return d, errors.New("no saved workflow " + name + " (rw workflow lists them)")
	}
	return d, err
}

func (s *Server) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	v := workflowsView{Workflows: []workflowView{}}
	if !s.opt.Demo {
		v.Dir, _ = workflow.Dir()
	}
	ds, err := s.listWorkflows()
	if err != nil {
		v.Error = err.Error()
	}
	for _, d := range ds {
		wv := workflowView{Definition: d}
		if err := d.Apply(s.store.Get()); err != nil { // Get is a copy
			wv.Problem = err.Error()
		}
		v.Workflows = append(v.Workflows, wv)
	}
	writeJSON(w, v)
}

// handleWorkflowsInit installs the starter workflows; saved ones are kept.
func (s *Server) handleWorkflowsInit(w http.ResponseWriter, r *http.Request) {
	if !readJSON(w, r, &struct{}{}) {
		return
	}
	if s.opt.Demo {
		writeJSON(w, map[string]string{"message": "demo mode: the starters are shown, nothing is saved"})
		return
	}
	added := 0
	for _, d := range workflow.Builtins() {
		data, err := yaml.Marshal(d)
		if err == nil {
			err = workflow.Save(data, false)
		}
		switch {
		case err == nil:
			added++
		case !os.IsExist(err):
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	msg := "installed the starter workflows"
	if added == 0 {
		msg = "the starter workflows are already installed (yours were kept)"
	}
	writeJSON(w, map[string]string{"message": msg})
}

// workflowJob builds a job that runs text under the saved workflow name.
// The workflow is read now: a queued or scheduled job runs the workflow
// as it was when it was queued, even if its file changes meanwhile.
func (s *Server) workflowJob(name, text string) (*job, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("describe the workflow's task")
	}
	d, err := s.loadWorkflow(name)
	if err != nil {
		return nil, err
	}
	if _, err := d.Render(text); err != nil {
		return nil, err
	}
	if err := d.Apply(s.store.Get()); err != nil {
		return nil, errors.New("workflow " + d.Name + ": " + err.Error())
	}
	return &job{text: text, wf: &d}, nil
}

// approvalsNote says how a queued or scheduled job treats approvals.
func (j *job) approvalsNote() string {
	if j.wf != nil && j.wf.Gated() {
		return "waits for your approval (workflow " + j.wf.Name + ")"
	}
	return "unattended (no approvals)"
}
