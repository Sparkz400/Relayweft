// Package mcpserve is `rw mcp`: rw as a stdio MCP server, so a coding
// agent (Claude Code, Codex) can hand a multi-step task to rw from inside
// its own session and follow it.
//
// The server embeds the same engine as rw run and rw web (one
// orchestrator, the worktree pool, task history). It runs one task at a
// time, in the folder it was started in (and the repos given at start-up):
// no tool takes a path. Tool calls return at once; the calling agent polls
// task_status, which can wait for a change. Every task saves its state
// after each step, so after the client reconnects (a new rw mcp) the
// history still lists it and resume_task continues it.
//
// The calling agent is untrusted input, like a typed task: the repo's
// .relayweft.yaml needs rw trust as in a normal run, the budget can only be
// lowered per task, a budget limit stops the task (nobody is asked), and
// no result carries environment values.
package mcpserve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/web"
	"github.com/sparkz400/relayweft/internal/workflow"
)

// Options configure an engine.
type Options struct {
	Orc *orchestrator.Orchestrator
	// Approver must be the one passed to the orchestrator (wrapped with
	// NewApprover).
	Approver *Approver
	// Events is the orchestrator's event channel; the engine drains it.
	Events <-chan event.Event
	Dir    string
	// SessionDir holds the session logs (task reports read them).
	SessionDir string
	// Session is the session log's id; the ids of read-only tasks and
	// follow-ups (which keep no task state) start with it.
	Session string
	// ReportDir is where task_result writes a task's Markdown report.
	ReportDir string
	Version   string
}

// Engine runs the tasks of one rw mcp.
type Engine struct {
	opt   Options
	orc   *orchestrator.Orchestrator
	ap    *Approver
	store *config.Store
	base  defaults

	mu     sync.Mutex
	jobs   []*job // this process's jobs, oldest first (at most maxJobs)
	cur    *job
	seq    int
	closed bool
}

// defaults are the config values a task's options start from.
type defaults struct {
	approvePlan, reviewChanges bool
	budgetUSD                  float64
	budgetTokens               int64
}

// maxJobs bounds the jobs kept in memory; older ones are in the history.
const maxJobs = 50

// maxPrompt bounds a task's text (a typed task is never this long).
const maxPrompt = 100 << 10

// Job kinds.
const (
	KindTask     = "task"
	KindReadOnly = "read_only"
	KindFollowUp = "follow_up"
	KindResume   = "resume"
)

// job is one task this process started.
type job struct {
	id     string
	kind   string
	prompt string
	agent  string // follow-ups: the agent it went to
	opts   RunOptions

	resume  *orchestrator.TaskState   // KindResume
	session orchestrator.AgentSession // KindFollowUp
	wf      *workflow.Definition      // KindTask under a saved workflow

	status  string // starting, running, done, failed, cancelled
	phase   string
	started time.Time
	ended   time.Time
	cancel  context.CancelFunc
	done    chan struct{}

	summary string
	answer  string // a read-only task's full answer
	undoKey string
	cost    event.TaskCost
	recent  []string // the newest activity lines
	agents  map[string]*agentView
	order   []string // agent ids in the order they appeared
}

// agentView is one agent of the running job.
type agentView struct {
	ID     string `json:"id"`
	Step   string `json:"step,omitempty"`
	Role   string `json:"role,omitempty"`
	Route  string `json:"route,omitempty"`
	State  string `json:"state"` // queued, running, ok, failed
	Detail string `json:"detail,omitempty"`
}

// maxRecent is how many activity lines task_status shows.
const maxRecent = 15

// New creates an engine and starts draining the events.
func New(o Options) (*Engine, error) {
	if o.Orc == nil || o.Approver == nil || o.Events == nil {
		return nil, errors.New("mcpserve: Orc, Approver and Events are required")
	}
	cfg := o.Orc.Store().Get()
	e := &Engine{opt: o, orc: o.Orc, ap: o.Approver, store: o.Orc.Store(),
		base: defaults{approvePlan: cfg.Orchestrator.ApprovePlan, reviewChanges: cfg.Orchestrator.ReviewChanges,
			budgetUSD: cfg.Budget.TaskUSD, budgetTokens: cfg.Budget.TaskTokens}}
	if e.opt.Session == "" {
		e.opt.Session = time.Now().Format("20060102-150405")
	}
	go e.pump()
	return e, nil
}

// Approver is the web approver, except that budget and conflict questions
// are declined: the calling agent cannot approve spending past a budget
// or resolving the person's edits, and MCP has no tool for those answers.
type Approver struct{ *web.Approver }

// NewApprover returns the approver to pass to the orchestrator.
func NewApprover() *Approver { return &Approver{Approver: web.NewApprover()} }

// ApproveBudget implements orchestrator.Approver: never go past a budget.
func (a *Approver) ApproveBudget(context.Context, orchestrator.BudgetRequest) bool { return false }

// ApproveResolve keeps the work on a branch when a conflict needs consent.
// Automatic resolutions allowed by the person's config do not ask here.
func (a *Approver) ApproveResolve(context.Context, orchestrator.ConflictQuestion) bool { return false }

// RunOptions are what the calling agent may set for one task.
type RunOptions struct {
	Prompt string
	// ApprovePlan and ReviewChanges override the config for this task
	// (nil = the config's setting).
	ApprovePlan   *bool
	ReviewChanges *bool
	// ReadOnly runs one read-only agent that answers the prompt.
	ReadOnly bool
	// BudgetUSD and BudgetTokens lower this task's budget (0 = config).
	BudgetUSD    float64
	BudgetTokens int64
	// Workflow runs Prompt as the task of this saved workflow: its checks,
	// budget caps and approvals apply (ApprovePlan false cannot drop them).
	Workflow string
}

// errBusy is returned while a task runs: one task at a time.
type errBusy struct{ id, status string }

func (e errBusy) Error() string {
	if e.id == "" {
		return "a task is still starting: wait a moment, then call list_tasks"
	}
	return fmt.Sprintf("task %s is still %s: one task at a time. Wait for it (task_status with wait_seconds), or stop it (cancel_task)", e.id, e.status)
}

// Run starts a task, a read-only question or a resume, and returns its id.
func (e *Engine) Run(ctx context.Context, o RunOptions) (string, error) {
	o.Prompt = strings.TrimSpace(o.Prompt)
	if o.Prompt == "" {
		return "", errors.New("prompt is empty: describe the task")
	}
	if len(o.Prompt) > maxPrompt {
		return "", fmt.Errorf("prompt is %d KB; at most %d KB", len(o.Prompt)>>10, maxPrompt>>10)
	}
	if o.BudgetUSD < 0 || o.BudgetTokens < 0 {
		return "", errors.New("a budget cannot be negative")
	}
	kind := KindTask
	if o.ReadOnly {
		kind = KindReadOnly
	}
	j := &job{kind: kind, prompt: o.Prompt, opts: o}
	if o.Workflow != "" {
		if o.ReadOnly {
			return "", errors.New("a workflow runs a planned task; it does not combine with read_only")
		}
		d, err := workflow.Load(o.Workflow)
		if err != nil {
			return "", fmt.Errorf("workflow %s: %w", o.Workflow, err)
		}
		if j.prompt, err = d.Render(o.Prompt); err != nil {
			return "", err
		}
		if err = d.Apply(e.store.Get()); err != nil { // Get is a copy
			return "", fmt.Errorf("workflow %s: %w", d.Name, err)
		}
		j.wf = &d
	}
	return e.launch(ctx, j)
}

// Resume continues an interrupted, failed or cancelled task of this
// folder: finished steps are skipped, an interrupted step's agent continues
// its own session.
func (e *Engine) Resume(ctx context.Context, id string) (string, error) {
	st, err := e.loadHere(id)
	if err != nil {
		return "", err
	}
	if st.Status == "running" && !st.Interrupted() {
		return "", fmt.Errorf("task %s is running in another rw: resume it there, or wait until it ends", st.ID)
	}
	// The id comes from the orchestrator once it has the task's lock: a
	// resume refused there (another rw took it meanwhile) gets none.
	return e.launch(ctx, &job{kind: KindResume, prompt: st.Task, resume: st})
}

// FollowUp sends a message to an agent: a running one gets it when its
// current turn ends; a finished one continues its session as a new task.
// agent "" is the newest finished agent.
func (e *Engine) FollowUp(ctx context.Context, agent, msg string) (id, told string, err error) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", "", errors.New("message is empty")
	}
	if len(msg) > maxPrompt {
		return "", "", fmt.Errorf("message is %d KB; at most %d KB", len(msg)>>10, maxPrompt>>10)
	}
	if agent != "" {
		for _, a := range e.orc.RunningAgents() {
			if a == agent {
				if err := e.orc.Tell(agent, msg); err != nil {
					return "", "", err
				}
				return "", fmt.Sprintf("message for %s queued: delivered when its current turn ends, before its work is merged", agent), nil
			}
		}
	}
	sess, ok := e.orc.Session(agent)
	if !ok {
		name := agent
		if name == "" {
			name = "last"
		}
		return "", "", fmt.Errorf("no finished agent %q to follow up (task_result lists them)", name)
	}
	id, err = e.launch(ctx, &job{kind: KindFollowUp, prompt: msg, agent: sess.AgentID, session: sess})
	return id, "", err
}

// startWait bounds how long Run waits for a task to get its id.
var startWait = 30 * time.Second

func (e *Engine) launch(ctx context.Context, j *job) (string, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return "", errors.New("rw mcp is shutting down")
	}
	if c := e.cur; c != nil {
		e.mu.Unlock()
		return "", errBusy{c.id, c.status}
	}
	if e.orc.Running() {
		e.mu.Unlock()
		return "", errBusy{}
	}
	if err := e.applyOptions(j); err != nil {
		e.mu.Unlock()
		return "", fmt.Errorf("options: %w", err)
	}
	e.seq++
	if j.kind == KindReadOnly || j.kind == KindFollowUp {
		// No task state: an id of this session.
		j.id = fmt.Sprintf("%s-%s-%d", e.opt.Session, strings.ReplaceAll(j.kind, "_", ""), e.seq)
	}
	j.status, j.started, j.agents = "starting", time.Now(), map[string]*agentView{}
	jctx, cancel := context.WithCancel(context.Background())
	j.cancel, j.done = cancel, make(chan struct{})
	e.cur = j
	e.jobs = append(e.jobs, j)
	if len(e.jobs) > maxJobs {
		e.jobs = e.jobs[len(e.jobs)-maxJobs:]
	}
	e.mu.Unlock()

	gotID := make(chan struct{})
	var once sync.Once
	go func() {
		defer cancel()
		var res orchestrator.TaskResult
		switch j.kind {
		case KindReadOnly:
			e.setStatus(j, "running")
			once.Do(func() { close(gotID) })
			rr := e.orc.RunRead(jctx, oneLine(j.prompt, 80), readPrompt(j.prompt), router.KindExplore)
			res = rr.TaskResult
			e.mu.Lock()
			j.answer = rr.Reply
			e.mu.Unlock()
		case KindFollowUp:
			e.setStatus(j, "running")
			once.Do(func() { close(gotID) })
			res = e.orc.FollowUpSession(jctx, j.session, j.prompt)
		default:
			opts := orchestrator.TaskOptions{Started: func(id string) {
				e.mu.Lock()
				j.id = id
				e.mu.Unlock()
				e.setStatus(j, "running")
				once.Do(func() { close(gotID) })
			}}
			if j.resume != nil {
				opts.Resume, opts.Force = j.resume, j.resume.Status != "running"
			}
			opts.Workflow = j.wf
			res = e.orc.RunWith(jctx, j.prompt, opts)
		}
		once.Do(func() { close(gotID) }) // ended before it started (refused)
		e.finish(j, res, jctx.Err() != nil)
	}()
	// Not on ctx: a caller that gave up must not be told "not started"
	// about a task that runs.
	select {
	case <-gotID:
	case <-time.After(startWait):
	}
	e.mu.Lock()
	id, ended, summary := j.id, !j.ended.IsZero(), j.summary
	e.mu.Unlock()
	switch {
	case id == "" && ended:
		return "", fmt.Errorf("not started: %s", summary)
	case id == "":
		return "", fmt.Errorf("the task is still starting after %s (it runs; call list_tasks to find its id)", startWait)
	}
	return id, nil
}

// applyOptions sets the config for the job about to start (e.mu held).
// Only this engine starts tasks on its orchestrator, one at a time, so the
// live config is the task's.
func (e *Engine) applyOptions(j *job) error {
	o := j.opts
	return e.store.Update(func(c *config.Config) error {
		c.Orchestrator.ApprovePlan = pick(o.ApprovePlan, e.base.approvePlan)
		c.Orchestrator.ReviewChanges = pick(o.ReviewChanges, e.base.reviewChanges)
		c.Budget.TaskUSD = lower(e.base.budgetUSD, o.BudgetUSD)
		c.Budget.TaskTokens = lower(e.base.budgetTokens, o.BudgetTokens)
		return nil
	})
}

func pick(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// lower is the stricter of a configured limit and a requested one (0 = no
// limit): a caller may lower the budget, never raise it.
func lower[T int64 | float64](conf, req T) T {
	switch {
	case req <= 0:
		return conf
	case conf <= 0:
		return req
	}
	return min(conf, req)
}

func (e *Engine) setStatus(j *job, s string) {
	e.mu.Lock()
	if j.status == "starting" || j.status == "running" {
		j.status = s
	}
	e.mu.Unlock()
}

// finish records a job's end.
func (e *Engine) finish(j *job, res orchestrator.TaskResult, cancelled bool) {
	e.mu.Lock()
	switch {
	case cancelled:
		j.status = "cancelled"
	case res.OK:
		j.status = "done"
	default:
		j.status = "failed"
	}
	j.summary, j.undoKey, j.cost, j.ended = res.Summary, res.UndoKey, res.Cost, time.Now()
	if e.cur == j {
		e.cur = nil
	}
	if j.id == "" {
		// Refused before it got an id: nothing to look up later.
		for i, x := range e.jobs {
			if x == j {
				e.jobs = append(e.jobs[:i:i], e.jobs[i+1:]...)
				break
			}
		}
	}
	close(j.done)
	e.mu.Unlock()
}

// Cancel stops the running task (its finished steps stay; resume_task
// continues it).
func (e *Engine) Cancel(id string) (string, error) {
	e.mu.Lock()
	j := e.cur
	if j == nil || j.id != id {
		e.mu.Unlock()
		if st, err := e.loadHere(id); err == nil && st.Status == "running" && !st.Interrupted() {
			return "", fmt.Errorf("task %s runs in another rw process: stop it there", id)
		}
		if e.find(id) != nil {
			return "", fmt.Errorf("task %s is not running", id)
		}
		return "", fmt.Errorf("no running task %q here (list_tasks lists them)", id)
	}
	j.cancel()
	e.mu.Unlock()
	return "cancelling: stopping its agents. Finished steps stay; resume_task continues it", nil
}

// Close cancels the running task and waits (bounded) for its agents to
// stop. Pending approvals end as "no".
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	j := e.cur
	e.mu.Unlock()
	// Cancel first: a closed approver answers a waiting review "reject"
	// and a waiting plan "no", which a task not yet cancelled would take
	// for the person's answer.
	if j != nil {
		j.cancel()
	}
	e.ap.Close()
	if j != nil {
		select {
		case <-j.done:
		case <-time.After(8 * time.Second):
		}
	}
}

// find returns this process's job with that id (nil if none).
func (e *Engine) find(id string) *job {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.jobs) - 1; i >= 0; i-- {
		if e.jobs[i].id == id {
			return e.jobs[i]
		}
	}
	return nil
}

// pump drains the orchestrator's events into the running job's view.
func (e *Engine) pump() {
	for ev := range e.opt.Events {
		e.observe(ev)
	}
}

// observe folds one event into the running job.
func (e *Engine) observe(ev event.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.cur
	if j == nil {
		return
	}
	agent := func() *agentView {
		if ev.AgentID == "" {
			return nil
		}
		a := j.agents[ev.AgentID]
		if a == nil {
			a = &agentView{ID: ev.AgentID, State: "queued"}
			j.agents[ev.AgentID] = a
			j.order = append(j.order, ev.AgentID)
		}
		if ev.Role != "" {
			a.Role = ev.Role
		}
		return a
	}
	switch ev.Kind {
	case event.Phase:
		j.phase = ev.Text
	case event.AgentQueued:
		if a := agent(); a != nil {
			a.State, a.Detail = "queued", oneLine(ev.Text, 120)
		}
	case event.Route:
		if a := agent(); a != nil && ev.Decision != nil {
			a.Step, a.Route = ev.Decision.StepID, ev.Decision.Label()
		}
	case event.Started:
		if a := agent(); a != nil {
			a.State = "running"
		}
	case event.Done:
		if a := agent(); a != nil {
			a.State = map[bool]string{true: "ok", false: "failed"}[ev.OK]
			a.Detail = oneLine(ev.Text, 200)
		}
	case event.Error, event.LimitHit, event.Log, event.Checkpoint, event.Merge:
		who := ev.AgentID
		if who == "" {
			who = "rw"
		}
		line := ev.Timestamp.Format("15:04:05") + " " + who + ": "
		switch ev.Kind {
		case event.Error:
			line += "error: "
		case event.LimitHit:
			line += "usage limit: "
		case event.Checkpoint:
			line += map[bool]string{true: "review approved: ", false: "review asked for changes: "}[ev.OK]
		case event.Merge:
			line += map[bool]string{true: "merged: ", false: "merge failed: "}[ev.OK]
		}
		j.recent = append(j.recent, line+oneLine(ev.Text, 240))
		if len(j.recent) > maxRecent {
			j.recent = j.recent[len(j.recent)-maxRecent:]
		}
	}
}

// readPrompt is what the read-only agent gets: the question, and that it
// must not change anything.
func readPrompt(q string) string {
	return "Answer this question about the repository in your working directory. This is read-only: DO NOT modify any files.\n\nQuestion:\n" + q
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if n > 0 && len(r) > n {
		return string(r[:max(0, n-1)]) + "…"
	}
	return s
}
