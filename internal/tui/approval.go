package tui

import (
	"context"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sparkz400/switchyard/internal/notify"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// Approver bridges the orchestrator's approval calls (made on its task
// goroutine, which must block) to the Bubble Tea model (which must never
// block): each call becomes a request on a channel, the model shows an
// overlay and answers on the request's own reply channel.
//
// Build it before the orchestrator, pass it as orchestrator.Options.Approver
// and as tui.Options.Approver.
type Approver struct {
	reqs      chan *approvalReq
	done      chan struct{}
	closeOnce sync.Once
}

var (
	_ orchestrator.Approver         = (*Approver)(nil)
	_ orchestrator.EstimateApprover = (*Approver)(nil)
)

// NewApprover returns an approver that waits for the TUI.
func NewApprover() *Approver {
	return &Approver{reqs: make(chan *approvalReq), done: make(chan struct{})}
}

// Close unblocks every waiting and future approval (as "no"); used on exit.
func (a *Approver) Close() {
	a.closeOnce.Do(func() { close(a.done) })
}

// approvalReq is one question for the person: a plan or a change set.
type approvalReq struct {
	ctx     context.Context
	task    string
	plan    *orchestrator.Plan      // set for plan approval
	changes *orchestrator.ChangeSet // set for change review
	budget  *orchestrator.BudgetRequest
	// estimate re-estimates the plan (nil: the plan has no estimate).
	estimate func(orchestrator.Plan) orchestrator.PlanEstimate
	reply    chan approvalReply // buffered: answering never blocks
}

type approvalReply struct {
	plan     orchestrator.Plan
	ok       bool
	decision orchestrator.ChangeDecision
}

// ask hands a request to the model and waits for the answer. ok=false when
// ctx was cancelled or the approver closed first.
func (a *Approver) ask(r *approvalReq) (approvalReply, bool) {
	r.reply = make(chan approvalReply, 1)
	select {
	case a.reqs <- r:
	case <-r.ctx.Done():
		return approvalReply{}, false
	case <-a.done:
		return approvalReply{}, false
	}
	select {
	case rep := <-r.reply:
		return rep, true
	case <-r.ctx.Done():
		return approvalReply{}, false
	case <-a.done:
		return approvalReply{}, false
	}
}

// ApprovePlan implements orchestrator.Approver.
func (a *Approver) ApprovePlan(ctx context.Context, task string, p orchestrator.Plan) (orchestrator.Plan, bool) {
	cp := clonePlan(p)
	rep, ok := a.ask(&approvalReq{ctx: ctx, task: task, plan: &cp})
	if !ok {
		return p, false
	}
	return rep.plan, rep.ok
}

// ApprovePlanEstimate implements orchestrator.EstimateApprover: the plan
// overlay shows each step's estimate and the total, updated as it is edited.
func (a *Approver) ApprovePlanEstimate(ctx context.Context, task string, p orchestrator.Plan, est func(orchestrator.Plan) orchestrator.PlanEstimate) (orchestrator.Plan, bool) {
	cp := clonePlan(p)
	rep, ok := a.ask(&approvalReq{ctx: ctx, task: task, plan: &cp, estimate: est})
	if !ok {
		return p, false
	}
	return rep.plan, rep.ok
}

// ReviewChanges implements orchestrator.Approver. Parallel agents may call
// it at the same time; the model queues the requests and shows one at a time.
func (a *Approver) ReviewChanges(ctx context.Context, cs orchestrator.ChangeSet) orchestrator.ChangeDecision {
	rep, ok := a.ask(&approvalReq{ctx: ctx, changes: &cs})
	if !ok {
		return orchestrator.ChangeDecision{}
	}
	return rep.decision
}

// ApproveBudget implements orchestrator.Approver: the person decides
// whether the task goes on past a budget limit.
func (a *Approver) ApproveBudget(ctx context.Context, r orchestrator.BudgetRequest) bool {
	rep, ok := a.ask(&approvalReq{ctx: ctx, task: r.Task, budget: &r})
	return ok && rep.ok
}

// approvalMsg delivers a request to the model.
type approvalMsg struct{ req *approvalReq }

// waitApproval listens for the next request (like waitEvents).
func waitApproval(a *Approver) tea.Cmd {
	return func() tea.Msg {
		select {
		case r := <-a.reqs:
			return approvalMsg{r}
		case <-a.done:
			return nil
		}
	}
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

// overlay is the approval currently on screen.
type overlay interface {
	req() *approvalReq
	update(m *Model, k tea.KeyMsg) tea.Cmd
	view(m *Model, W, H int) string
	keys() string
}

// onApproval queues a request and opens it when nothing else is open.
func (m *Model) onApproval(r *approvalReq) {
	m.approvals = append(m.approvals, r)
	if len(m.approvals) == 1 {
		m.openApproval()
	}
}

// openApproval shows the head of the approval queue.
func (m *Model) openApproval() {
	m.overlay = nil
	if len(m.approvals) == 0 {
		if m.running {
			m.focus = focusTree
		} else {
			m.focus = focusPrompt
			m.input.Focus()
		}
		return
	}
	r := m.approvals[0]
	switch {
	case r.plan != nil:
		m.overlay = newPlanOverlay(r, m.th.ASCII)
		m.alert(notify.EventWaiting, "Switchyard needs you", "approve the plan: "+oneLine(r.task, 120))
	case r.budget != nil:
		m.overlay = &budgetOverlay{r: r}
		m.alert(notify.EventWaiting, "Switchyard needs you", "budget reached: "+r.budget.String())
	default:
		m.overlay = newReviewOverlay(r)
		m.alert(notify.EventWaiting, "Switchyard needs you", "review changes of "+r.changes.StepID)
	}
	m.input.Blur()
	m.focus = focusTree
	m.overlayArm = time.Now().Add(overlayGrace)
}

// overlayGrace keeps an overlay that just opened from taking keys: someone
// typing in the prompt when it pops up must not delete steps or approve by
// accident. Every key in the grace period extends it, so the overlay starts
// listening only after a short pause in typing.
var overlayGrace = 700 * time.Millisecond

// answer replies to the open request and moves on to the next one.
func (m *Model) answer(rep approvalReply) {
	if len(m.approvals) == 0 {
		return
	}
	m.approvals[0].reply <- rep
	m.approvals = m.approvals[1:]
	m.openApproval()
}

// pruneApprovals drops requests nobody waits for any more (task cancelled,
// agent killed).
func (m *Model) pruneApprovals() {
	if len(m.approvals) == 0 {
		return
	}
	head := m.approvals[0]
	kept := m.approvals[:0]
	for _, r := range m.approvals {
		if r.ctx.Err() == nil {
			kept = append(kept, r)
		}
	}
	m.approvals = kept
	if len(kept) == 0 || kept[0] != head {
		m.openApproval()
	}
}

// dropApprovals forgets every request (the task ended).
func (m *Model) dropApprovals() {
	if len(m.approvals) == 0 && m.overlay == nil {
		return
	}
	m.approvals = nil
	m.overlay = nil
}
