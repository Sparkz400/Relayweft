package mcpserve

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/web"
)

// request finds the question of type typ that task id waits on: the one
// named reqID, or the only one.
func (e *Engine) request(id, typ, reqID string) (*web.Request, error) {
	e.mu.Lock()
	cur := e.cur
	running := cur != nil && cur.id == id && id != ""
	e.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("task %s is not running here, so it waits for nothing (task_status shows what it waits on)", oneLine(id, 80))
	}
	var match []*web.Request
	for _, r := range e.ap.Pending() {
		if r.Type == typ && (reqID == "" || r.ID == reqID) {
			match = append(match, r)
		}
	}
	what := map[string]string{"plan": "a plan approval", "changes": "a change review"}[typ]
	switch {
	case len(match) == 1:
		return match[0], nil
	case len(match) == 0 && reqID != "":
		return nil, fmt.Errorf("task %s has no %s with request_id %q (task_status lists what it waits on)", id, what, reqID)
	case len(match) == 0:
		return nil, fmt.Errorf("task %s does not wait for %s now (task_status shows what it waits on)", id, what)
	}
	var ids []string
	for _, r := range match {
		ids = append(ids, r.ID)
	}
	return nil, fmt.Errorf("task %s waits for %d of these (%s): give request_id", id, len(match), strings.Join(ids, ", "))
}

// ApprovePlan approves the waiting plan as it is, or rejects it (the task
// is cancelled before any agent runs).
func (e *Engine) ApprovePlan(id, reqID string, approve bool) (string, error) {
	r, err := e.request(id, "plan", reqID)
	if err != nil {
		return "", err
	}
	p, err := e.ap.AnswerPlan(r.ID, *r.Plan, approve)
	if err != nil {
		return "", err
	}
	if !approve {
		return "plan rejected: the task is cancelled", nil
	}
	return fmt.Sprintf("plan approved: %d step(s) run now", len(p.Subtasks)), nil
}

// PlanInput is an edited plan.
type PlanInput struct {
	Summary string      `json:"summary,omitempty" jsonschema:"what the plan does"`
	Steps   []StepInput `json:"steps" jsonschema:"the steps, in order"`
}

// StepInput is one step of an edited plan.
type StepInput struct {
	ID        string   `json:"id" jsonschema:"a short id, unique in the plan"`
	Title     string   `json:"title,omitempty"`
	Kind      string   `json:"kind" jsonschema:"edit (changes files), explore or research (read-only)"`
	Prompt    string   `json:"prompt" jsonschema:"what the step's agent is told"`
	Files     []string `json:"files,omitempty" jsonschema:"files the step will likely touch (a hint)"`
	DependsOn []string `json:"depends_on,omitempty" jsonschema:"ids of steps that must finish first"`
	Role      string   `json:"role,omitempty" jsonschema:"pin a role: worker, worker_high, explorer or researcher (empty: rw routes it)"`
	Repo      string   `json:"repo,omitempty" jsonschema:"multi-repo tasks: the repo name the step works in"`
}

// pinnable are the roles a step may be pinned to.
var pinnable = []string{event.RoleWorker, event.RoleWorkerHigh, event.RoleExplorer, event.RoleResearcher}

// EditPlan runs an edited plan instead of the waiting one.
func (e *Engine) EditPlan(id, reqID string, in PlanInput) (string, error) {
	if len(in.Steps) == 0 {
		return "", fmt.Errorf("the plan needs at least one step (approve_plan with approve false rejects it)")
	}
	p := orchestrator.Plan{Summary: in.Summary}
	for i, s := range in.Steps {
		if strings.TrimSpace(s.Prompt) == "" {
			return "", fmt.Errorf("step %d (%s) has no prompt", i+1, s.ID)
		}
		if len(s.Prompt) > maxPrompt {
			return "", fmt.Errorf("step %d (%s): the prompt is longer than %d KB", i+1, s.ID, maxPrompt>>10)
		}
		if s.Role != "" && !slices.Contains(pinnable, s.Role) {
			return "", fmt.Errorf("step %d (%s): role %q; want one of %s, or empty", i+1, s.ID, s.Role, strings.Join(pinnable, ", "))
		}
		switch router.Kind(s.Kind) {
		case router.KindEdit, router.KindExplore, router.KindResearch, router.KindFix, "":
		default:
			return "", fmt.Errorf("step %d (%s): kind %q; want edit, explore or research", i+1, s.ID, s.Kind)
		}
		p.Subtasks = append(p.Subtasks, orchestrator.Subtask{ID: s.ID, Title: s.Title, Kind: router.Kind(s.Kind), Prompt: s.Prompt,
			Files: s.Files, DependsOn: s.DependsOn, Role: s.Role, Repo: s.Repo})
	}
	r, err := e.request(id, "plan", reqID)
	if err != nil {
		return "", err
	}
	np, err := e.ap.AnswerPlan(r.ID, p, true)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("edited plan approved: %d step(s) run now", len(np.Subtasks)), nil
}

// Apply lands an agent's changes: every file, or only the files named.
func (e *Engine) Apply(id, reqID string, files []string) (string, error) {
	r, err := e.request(id, "changes", reqID)
	if err != nil {
		return "", err
	}
	var all []string
	for _, f := range r.Changes.Files {
		all = append(all, f.Path)
	}
	if len(files) == 0 {
		files = all
	}
	// A path that is not in the change set would be dropped, and an
	// empty set rejects everything: refuse instead of guessing.
	for _, f := range files {
		if !slices.Contains(all, f) {
			return "", fmt.Errorf("%q is not one of the changed files (%s)", oneLine(f, 120), oneLine(strings.Join(all, ", "), 600))
		}
	}
	d, err := e.ap.AnswerChanges(r.ID, orchestrator.ChangeDecision{Apply: files})
	if err != nil {
		return "", err
	}
	if len(d.Apply) < len(all) {
		return fmt.Sprintf("applying %d of %d file(s); the rest is kept on a branch", len(d.Apply), len(all)), nil
	}
	return fmt.Sprintf("applying %d file(s)", len(d.Apply)), nil
}

// Reject sends the changes back with feedback (the agent tries again in
// its worktree), or without feedback rejects them (kept on a branch).
func (e *Engine) Reject(id, reqID, feedback string) (string, error) {
	r, err := e.request(id, "changes", reqID)
	if err != nil {
		return "", err
	}
	feedback = strings.TrimSpace(feedback)
	if len(feedback) > maxPrompt {
		return "", fmt.Errorf("feedback is longer than %d KB", maxPrompt>>10)
	}
	if _, err := e.ap.AnswerChanges(r.ID, orchestrator.ChangeDecision{Feedback: feedback}); err != nil {
		return "", err
	}
	if feedback != "" {
		return "sent back to the agent with your feedback; its next changes are shown again", nil
	}
	return "all changes rejected: nothing lands, the work is kept on a branch", nil
}

// Undo reverts a finished task's changes in the working tree (redo puts
// them back). Files no agent reported changing are left alone unless
// allFiles: they may be the calling agent's own edits made meanwhile.
func (e *Engine) Undo(id string, redo, allFiles bool) (string, error) {
	key := ""
	if j := e.find(id); j != nil {
		e.mu.Lock()
		ended, k, kind := !j.ended.IsZero(), j.undoKey, j.kind
		e.mu.Unlock()
		if !ended {
			return "", fmt.Errorf("task %s is still running: cancel_task first", id)
		}
		if kind == KindReadOnly {
			return "", fmt.Errorf("task %s was read-only: there is nothing to undo", id)
		}
		key = k
	}
	if key == "" {
		st, err := e.loadHere(id)
		if err != nil {
			return "", err
		}
		if st.Status == "running" {
			return "", fmt.Errorf("task %s has not finished: undo it once it ends", id)
		}
		key = st.UndoKey
	}
	if key == "" {
		return "", fmt.Errorf("task %s has no undo record (it changed nothing, or ran without git)", id)
	}
	plan, err := orchestrator.Undo(e.opt.Dir, key, redo, !allFiles)
	if err != nil {
		return "", err
	}
	verb := "undone"
	if redo {
		verb = "redone"
	}
	if plan.TotalChanges() == 0 {
		return "the task changed no files: nothing to do", nil
	}
	changes := plan.Changes
	if !allFiles {
		changes = nil
		for _, c := range plan.Changes {
			if _, path, _ := strings.Cut(c, " "); !slices.Contains(plan.Unreported, path) {
				changes = append(changes, c)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d file(s) here", verb, len(changes))
	if n := plan.TotalChanges() - len(plan.Changes); n > 0 {
		fmt.Fprintf(&b, ", %d in the task's other repos", n)
	}
	for i, c := range changes {
		if i == 20 {
			fmt.Fprintf(&b, "\n  ... and %d more", len(changes)-20)
			break
		}
		b.WriteString("\n  " + c)
	}
	if len(plan.Edited) > 0 {
		fmt.Fprintf(&b, "\nedits made after the task were kept (3-way merge): %s", strings.Join(plan.Edited, ", "))
	}
	if len(plan.Unreported) > 0 && !allFiles {
		fmt.Fprintf(&b, "\nleft alone (no agent reported changing them; maybe your own edits): %s. all_files: true reverts them too",
			strings.Join(plan.Unreported, ", "))
	}
	return b.String(), nil
}
