package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// Approver bridges the orchestrator's approval calls (made on a task
// goroutine, which must block) to the browser: each call becomes a pending
// request that the page shows and answers through the API. A request ends
// when it is answered, when its context is cancelled (task cancelled, agent
// killed) or when the approver is closed.
//
// Build it before the orchestrator and pass the same value as
// orchestrator.Options.Approver and web.Options.Approver.
type Approver struct {
	mu       sync.Mutex
	seq      int
	pending  []*Request
	onChange func()
	onAsk    func(*Request)

	done      chan struct{}
	closeOnce sync.Once
}

var (
	_ orchestrator.Approver         = (*Approver)(nil)
	_ orchestrator.EstimateApprover = (*Approver)(nil)
	_ orchestrator.ConflictApprover = (*Approver)(nil)
)

// NewApprover returns an approver that waits for the browser.
func NewApprover() *Approver { return &Approver{done: make(chan struct{})} }

// Close unblocks every waiting and future request (as "no"); used on exit.
func (a *Approver) Close() { a.closeOnce.Do(func() { close(a.done) }) }

// setNotify registers the functions called whenever the pending set
// changes and when a new request arrives.
func (a *Approver) setNotify(changed func(), asked func(*Request)) {
	a.mu.Lock()
	a.onChange, a.onAsk = changed, asked
	a.mu.Unlock()
}

func (a *Approver) changed() {
	a.mu.Lock()
	fn := a.onChange
	a.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Request is one question for the person: a plan or a change set.
type Request struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"` // "plan", "changes", "budget" or "conflict"
	Task    string             `json:"task,omitempty"`
	Plan    *orchestrator.Plan `json:"plan,omitempty"`
	Changes *ChangeView        `json:"changes,omitempty"`
	Budget  *BudgetView        `json:"budget,omitempty"`
	// Conflict asks whether an agent may resolve a merge conflict.
	Conflict *ConflictView `json:"conflict,omitempty"`
	// Estimate is the plan's dry-run estimate (plan requests whose
	// orchestrator made one); the page asks for a new one after edits.
	Estimate *orchestrator.PlanEstimate `json:"estimate,omitempty"`
	Created  time.Time                  `json:"created"`

	ctx      context.Context
	cs       *orchestrator.ChangeSet
	estimate func(orchestrator.Plan) orchestrator.PlanEstimate
	reply    chan reply
}

type reply struct {
	plan     orchestrator.Plan
	ok       bool
	decision orchestrator.ChangeDecision
}

// ChangeView is a change set as the page shows it.
type ChangeView struct {
	StepID  string     `json:"step_id"`
	Title   string     `json:"title"`
	Summary string     `json:"summary,omitempty"`
	Round   int        `json:"round"`
	Files   []FileView `json:"files"`
	// Conflict is set when the change is an agent's resolution of a merge
	// conflict: what conflicted.
	Conflict string `json:"conflict,omitempty"`
}

// FileView is one file of a change set, with its hunks split out when the
// file can be reviewed hunk by hunk.
type FileView struct {
	Path       string   `json:"path"`
	Status     string   `json:"status"`
	Added      int      `json:"added"`
	Deleted    int      `json:"deleted"`
	Binary     bool     `json:"binary,omitempty"`
	Patch      string   `json:"patch"`
	Splittable bool     `json:"splittable"`
	Header     string   `json:"header,omitempty"`
	Hunks      []string `json:"hunks,omitempty"`
}

func changeView(cs orchestrator.ChangeSet) *ChangeView {
	v := &ChangeView{StepID: cs.StepID, Title: cs.Title, Summary: cs.Summary, Round: cs.Round, Files: []FileView{}, Conflict: cs.Conflict}
	for _, f := range cs.Files {
		fv := FileView{Path: f.Path, Status: f.Status, Added: f.Added, Deleted: f.Deleted, Binary: f.Binary, Patch: f.Patch, Splittable: f.Splittable()}
		if fv.Splittable {
			fv.Header, fv.Hunks = orchestrator.SplitHunks(f.Patch)
		}
		v.Files = append(v.Files, fv)
	}
	return v
}

// Pending returns the open requests, oldest first.
func (a *Approver) Pending() []*Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Request, 0, len(a.pending))
	for _, r := range a.pending {
		if r.ctx.Err() == nil {
			out = append(out, r)
		}
	}
	return out
}

// ask registers a request and waits for its answer. ok is false when the
// context was cancelled or the approver closed first.
func (a *Approver) ask(r *Request) (reply, bool) {
	r.reply = make(chan reply, 1)
	r.Created = time.Now()
	select {
	case <-a.done:
		return reply{}, false
	default:
	}
	a.mu.Lock()
	a.seq++
	r.ID = fmt.Sprintf("a%d", a.seq)
	a.pending = append(a.pending, r)
	asked := a.onAsk
	a.mu.Unlock()
	a.changed()
	if asked != nil {
		asked(r)
	}
	defer func() {
		if a.remove(r.ID) != nil {
			a.changed()
		}
	}()
	select {
	case rep := <-r.reply:
		return rep, true
	case <-r.ctx.Done():
		return reply{}, false
	case <-a.done:
		return reply{}, false
	}
}

// remove takes a request out of the pending list (nil if it was not there).
func (a *Approver) remove(id string) *Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, r := range a.pending {
		if r.ID == id {
			a.pending = append(a.pending[:i:i], a.pending[i+1:]...)
			return r
		}
	}
	return nil
}

func (a *Approver) find(id, typ string) (*Request, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.pending {
		if r.ID == id {
			if r.Type != typ {
				return nil, fmt.Errorf("request %s is a %s request, not %s", id, r.Type, typ)
			}
			return r, nil
		}
	}
	return nil, errNoRequest
}

var errNoRequest = errors.New("that approval is no longer waiting (answered, or the task ended)")

// answer delivers a reply exactly once.
func (a *Approver) answer(id string, rep reply) error {
	r := a.remove(id)
	if r == nil {
		return errNoRequest
	}
	r.reply <- rep
	a.changed()
	return nil
}

// ApprovePlan implements orchestrator.Approver.
func (a *Approver) ApprovePlan(ctx context.Context, task string, p orchestrator.Plan) (orchestrator.Plan, bool) {
	cp := clonePlan(p)
	rep, ok := a.ask(&Request{ctx: ctx, Type: "plan", Task: task, Plan: &cp})
	if !ok {
		return p, false
	}
	return rep.plan, rep.ok
}

// ApprovePlanEstimate implements orchestrator.EstimateApprover: the page
// shows each step's estimate and the total (EstimatePlan re-estimates an
// edited plan).
func (a *Approver) ApprovePlanEstimate(ctx context.Context, task string, p orchestrator.Plan, est func(orchestrator.Plan) orchestrator.PlanEstimate) (orchestrator.Plan, bool) {
	cp := clonePlan(p)
	e := est(clonePlan(p))
	rep, ok := a.ask(&Request{ctx: ctx, Type: "plan", Task: task, Plan: &cp, Estimate: &e, estimate: est})
	if !ok {
		return p, false
	}
	return rep.plan, rep.ok
}

// EstimatePlan estimates a plan request's plan as edited on the page.
func (a *Approver) EstimatePlan(id string, p orchestrator.Plan) (orchestrator.PlanEstimate, error) {
	r, err := a.find(id, "plan")
	if err != nil {
		return orchestrator.PlanEstimate{}, err
	}
	if r.estimate == nil {
		return orchestrator.PlanEstimate{}, errors.New("this plan has no estimate")
	}
	if r.Plan != nil {
		p.Repos = r.Plan.Repos
	}
	return r.estimate(clonePlan(p)), nil
}

// ReviewChanges implements orchestrator.Approver. Parallel agents may call
// it at the same time; each gets its own request.
func (a *Approver) ReviewChanges(ctx context.Context, cs orchestrator.ChangeSet) orchestrator.ChangeDecision {
	rep, ok := a.ask(&Request{ctx: ctx, Type: "changes", Task: cs.Title, Changes: changeView(cs), cs: &cs})
	if !ok {
		return orchestrator.ChangeDecision{}
	}
	return rep.decision
}

// BudgetView is a budget question as the page shows it.
type BudgetView struct {
	orchestrator.BudgetRequest
	Text string `json:"text"` // e.g. "this task's cost $2.04 reached the budget of $2.00"
	Hint string `json:"hint"` // how to raise the limit for good
}

// ApproveBudget implements orchestrator.Approver: true lets the task go on
// past the limit until it ends.
func (a *Approver) ApproveBudget(ctx context.Context, r orchestrator.BudgetRequest) bool {
	rep, ok := a.ask(&Request{ctx: ctx, Type: "budget", Task: r.Task, Budget: &BudgetView{BudgetRequest: r, Text: r.String(), Hint: r.RaiseHint()}})
	return ok && rep.ok
}

// ConflictView is a conflict question as the page shows it.
type ConflictView struct {
	orchestrator.ConflictQuestion
	Text string `json:"text"` // e.g. "b conflicts with step a (Add the flag) in shared.txt"
	Hint string `json:"hint"` // what yes and no do
}

// ApproveResolve implements orchestrator.ConflictApprover: true lets an
// agent resolve the conflict, false keeps the change on a branch.
func (a *Approver) ApproveResolve(ctx context.Context, q orchestrator.ConflictQuestion) bool {
	rep, ok := a.ask(&Request{ctx: ctx, Type: "conflict", Task: q.Task, Conflict: &ConflictView{ConflictQuestion: q, Text: q.String(), Hint: q.Hint()}})
	return ok && rep.ok
}

// AnswerConflict answers a conflict question: let an agent resolve it (ok)
// or keep the change on a branch.
func (a *Approver) AnswerConflict(id string, ok bool) error {
	if _, err := a.find(id, "conflict"); err != nil {
		return err
	}
	return a.answer(id, reply{ok: ok})
}

// AnswerBudget answers a budget question: go on (ok) or stop the task.
func (a *Approver) AnswerBudget(id string, ok bool) error {
	if _, err := a.find(id, "budget"); err != nil {
		return err
	}
	return a.answer(id, reply{ok: ok})
}

// AnswerPlan approves (ok, with the possibly edited plan) or rejects a plan.
// An approved plan is normalized first; a plan that cannot run is an error
// and the request stays open.
func (a *Approver) AnswerPlan(id string, p orchestrator.Plan, ok bool) (orchestrator.Plan, error) {
	r, err := a.find(id, "plan")
	if err != nil {
		return p, err
	}
	if r.Plan != nil {
		p.Repos = r.Plan.Repos // a multi-repo task's repos are not the page's to change
	}
	if ok {
		np, err := orchestrator.NormalizePlan(clonePlan(p))
		if err != nil {
			return p, fmt.Errorf("cannot run this plan: %w", err)
		}
		p = np
	}
	// The orchestrator edits its plan in place: hand it a copy of the one
	// returned to the caller.
	return p, a.answer(id, reply{plan: clonePlan(p), ok: ok})
}

// AnswerChanges answers a change review. Unknown paths are dropped; hunk
// selections are kept only for files that can be split, and a selection
// of every hunk applies the file whole.
func (a *Approver) AnswerChanges(id string, d orchestrator.ChangeDecision) (orchestrator.ChangeDecision, error) {
	r, err := a.find(id, "changes")
	if err != nil {
		return d, err
	}
	d = cleanDecision(*r.cs, d)
	return d, a.answer(id, reply{decision: d})
}

func cleanDecision(cs orchestrator.ChangeSet, d orchestrator.ChangeDecision) orchestrator.ChangeDecision {
	files := map[string]orchestrator.FileChange{}
	for _, f := range cs.Files {
		files[f.Path] = f
	}
	out := orchestrator.ChangeDecision{Feedback: d.Feedback}
	if d.Feedback != "" {
		return out // feedback never applies anything
	}
	seen := map[string]bool{}
	for _, p := range d.Apply {
		f, ok := files[p]
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		keep, split := d.Hunks[p]
		if !split || !f.Splittable() {
			out.Apply = append(out.Apply, p)
			continue
		}
		_, hunks := orchestrator.SplitHunks(f.Patch)
		set := map[int]bool{}
		for _, i := range keep {
			if i >= 0 && i < len(hunks) {
				set[i] = true
			}
		}
		if len(set) == 0 {
			continue // no hunk kept: the file is not applied
		}
		out.Apply = append(out.Apply, p)
		if len(set) == len(hunks) {
			continue // every hunk: apply whole
		}
		idx := make([]int, 0, len(set))
		for i := range set {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		if out.Hunks == nil {
			out.Hunks = map[string][]int{}
		}
		out.Hunks[p] = idx
	}
	return out
}

func clonePlan(p orchestrator.Plan) orchestrator.Plan {
	out := p
	out.Subtasks = make([]orchestrator.Subtask, len(p.Subtasks))
	for i, st := range p.Subtasks {
		st.Files = append([]string(nil), st.Files...)
		st.DependsOn = append([]string(nil), st.DependsOn...)
		out.Subtasks[i] = st
	}
	return out
}
