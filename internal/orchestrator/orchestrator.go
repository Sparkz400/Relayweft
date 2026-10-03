// Package orchestrator owns the task lifecycle: plan, review the plan, fan
// out to parallel agents (in git worktrees when they write), merge, review
// before done and apply one round of fixes.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
	"github.com/sparkz400/switchyard/internal/sysload"
)

// Fixed agent ids shown in the tree.
const (
	AgentMain     = "main"
	AgentReviewer = "reviewer"
	AgentJudge    = "judge"
)

// Options configure an orchestrator.
type Options struct {
	Dir     string
	Store   *config.Store
	Runners func(cfg *config.Config) runner.Set
	Tracker *limits.Tracker
	Log     *sessionlog.Writer
	Events  chan<- event.Event
	// ForceProvider pins every role to one provider (sy --provider).
	ForceProvider string
	// NoGit disables snapshots, worktrees and diffs (demo mode).
	NoGit bool
	// Mode is recorded in the session log ("routed" or "demo").
	Mode string
	// Load reports how busy the machine is (default: a live sampler).
	Load func() sysload.Sample
	// Bench labels task records with a `sy bench` task name.
	Bench string
	// TaskIDPrefix makes task ids unique when several orchestrators share
	// one session log (sy bench).
	TaskIDPrefix string
	// Approver asks a person to approve plans and changes (nil = approve).
	Approver Approver
}

// TaskOptions adjust one task run.
type TaskOptions struct {
	// Unattended tasks (queued, overnight) never wait for approvals.
	Unattended bool
	// Resume continues an interrupted task from its saved state.
	Resume *TaskState
	// Force resumes a task that is no longer marked running (it finished,
	// failed or was cancelled): its unfinished steps run again.
	Force bool
}

// busyPoll is how often a held agent re-checks the machine load.
var busyPoll = time.Second

// Orchestrator runs tasks. One task runs at a time.
type Orchestrator struct {
	opts   Options
	router *router.Router

	mu      sync.Mutex
	paused  bool
	pauseCh chan struct{}                         // closed when unpaused
	cancels map[string]map[int]context.CancelFunc // agent id -> run seq -> cancel
	runSeq  int
	active  int // agents past the load gate (guarded by mu)
	running bool
	taskSeq int
	tipped  bool // the big-repo git settings hint was shown

	sessions map[string]AgentSession // finished agents, for follow-ups
	told     map[string][]string     // messages for running agents (Tell)

	day dayCache // today's finished tasks, for the budget (budget.go)
	cur *task    // the running task (RunWith), for BudgetStatus
}

// New creates an orchestrator.
func New(o Options) *Orchestrator {
	if o.Mode == "" {
		o.Mode = "routed"
	}
	if o.Tracker == nil {
		o.Tracker = limits.NewTracker()
	}
	orc := &Orchestrator{opts: o, cancels: map[string]map[int]context.CancelFunc{}, pauseCh: make(chan struct{})}
	close(orc.pauseCh)
	orc.router = &router.Router{Cfg: o.Store.Get, State: o.Tracker, ForceProvider: o.ForceProvider}
	if orc.opts.Load == nil {
		s := sysload.NewSampler(2 * time.Second)
		orc.opts.Load = s.Get
	}
	return orc
}

// Router exposes the router (the TUI previews decisions).
func (o *Orchestrator) Router() *router.Router { return o.router }

// Tracker exposes provider state.
func (o *Orchestrator) Tracker() *limits.Tracker { return o.opts.Tracker }

// Store exposes the live config.
func (o *Orchestrator) Store() *config.Store { return o.opts.Store }

// Running reports whether a task is in progress.
func (o *Orchestrator) Running() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.running
}

// SetPaused pauses or resumes dispatching. Running agents finish their
// current run; no new agent starts while paused.
func (o *Orchestrator) SetPaused(p bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if p == o.paused {
		return
	}
	o.paused = p
	if p {
		o.pauseCh = make(chan struct{})
	} else {
		close(o.pauseCh)
	}
}

// Paused reports the pause state.
func (o *Orchestrator) Paused() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.paused
}

func (o *Orchestrator) waitUnpaused(ctx context.Context) error {
	o.mu.Lock()
	ch := o.pauseCh
	o.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Kill stops every running run of an agent id (the reviewer can run more
// than once in parallel). It returns false if nothing with that id runs.
func (o *Orchestrator) Kill(agentID string) bool {
	o.mu.Lock()
	var fns []context.CancelFunc
	for _, c := range o.cancels[agentID] {
		fns = append(fns, c)
	}
	o.mu.Unlock()
	for _, c := range fns {
		c()
	}
	return len(fns) > 0
}

// RunningAgents lists the ids of agents currently running.
func (o *Orchestrator) RunningAgents() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.cancels))
	for id, runs := range o.cancels {
		if len(runs) > 0 {
			out = append(out, id)
		}
	}
	return out
}

// emit sends an event to the UI and records the interesting ones.
func (o *Orchestrator) emit(e event.Event) {
	e = e.Stamp()
	switch e.Kind {
	case event.Quota:
		if e.Quota != nil {
			// Logged when it changes: `sy run --when-reset` reads the
			// reset time back from the logs.
			if sessionlog.QuotaChanged(o.opts.Tracker.Snapshot(e.Provider).Quota, *e.Quota) {
				q := *e.Quota
				o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeQuota, Provider: e.Provider, Quota: &q})
			}
			o.opts.Tracker.SetQuota(e.Provider, *e.Quota)
		}
	case event.Route:
		if d := e.Decision; d != nil {
			diag.Logf("route agent=%s step=%s -> %s role=%s rule=%s conf=%.2f fallback=%v: %s", e.AgentID, d.StepID, d.Label(), d.Role, d.Rule, d.Confidence, d.Fallback, d.Reason)
		}
	case event.Phase, event.Error, event.LimitHit, event.Merge, event.Checkpoint, event.TaskStart, event.TaskDone, event.ProviderState, event.Log:
		diag.Logf("%s agent=%s ok=%v: %s", e.Kind, e.AgentID, e.OK, clip(e.Text, 600))
		if e.Kind == event.TaskDone || e.Kind == event.TaskStart || e.Kind == event.Error {
			diag.Sync()
		}
	}
	if o.opts.Events != nil {
		o.opts.Events <- e
	}
}

func (o *Orchestrator) logf(format string, args ...any) {
	o.emit(event.Event{Kind: event.Log, Text: fmt.Sprintf(format, args...)})
}

// TaskResult summarizes a finished task.
type TaskResult struct {
	OK       bool
	Summary  string
	Duration time.Duration
	Tokens   event.TokenUsage
	Kept     []string // branches kept because of merge conflicts
	Cost     event.TaskCost
	UndoKey  string // for `sy undo` ("" when not in a git repo)
}

// stepResult is the outcome of one subtask.
type stepResult struct {
	ok     bool
	final  string
	err    string
	route  string
	files  []string
	tokens event.TokenUsage
}

// task is the per-run state.
type task struct {
	id       string
	text     string
	cfg      *config.Config
	runners  runner.Set
	root     string // repo root, "" when not a git repo
	useGit   bool
	snapshot string // integration commit (snapshot + merges)
	start    string // snapshot at execute start, for the final diff
	mergeMu  sync.Mutex
	mainProv string
	tokensMu sync.Mutex
	tokens   event.TokenUsage
	kept     []string
	notes    []string
	wtOK     bool          // writers may use pooled worktrees (if the plan has several)
	warm     chan struct{} // closed when the pool prewarm finished; nil if none ran
	pool     string        // pool directory when worktrees are in use
	lfs      bool          // worktrees hold LFS pointer files, not the real content
	writeSem chan struct{}
	poolSize uint64 // measured after the prewarm
	key      string // undo key: <session>-<task id>

	perProv     map[string]event.TokenUsage // guarded by tokensMu
	quotaBefore map[string]float64
	agentFiles  map[string]bool // repo-relative paths agents changed (guarded by tokensMu)

	unattended bool       // no approvals (queued task)
	state      *TaskState // persisted progress (nil in bench runs)
	resumed    bool       // continuing an interrupted task
	keepBefore bool       // resumed: undo keeps the original "before" snapshot
	repoMap    string     // context hand-off (handoff.go)
	repoNotes  string
	budget     *taskBudget // nil outside RunWith (budget.go)
}

// approving reports whether this task asks a person to approve its plan.
func (o *Orchestrator) approving(t *task) bool {
	return o.opts.Approver != nil && !t.unattended && t.cfg.Orchestrator.ApprovePlan
}

// reviewing reports whether each agent's changes are shown before they land.
func (o *Orchestrator) reviewing(t *task) bool {
	return o.opts.Approver != nil && !t.unattended && t.cfg.Orchestrator.ReviewChanges
}

// noteFiles records files an agent working in dir reported changing, as
// repo-relative slash paths (agents in pool worktrees are covered by their
// merge diffs instead).
func (t *task) noteFiles(dir string, files []string) {
	if t.root == "" {
		return
	}
	t.tokensMu.Lock()
	defer t.tokensMu.Unlock()
	if t.agentFiles == nil {
		t.agentFiles = map[string]bool{}
	}
	for _, f := range files {
		f = filepath.FromSlash(f)
		if !filepath.IsAbs(f) {
			f = filepath.Join(dir, f)
		}
		rel, err := filepath.Rel(canonPath(t.root), canonPath(f))
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		t.agentFiles[filepath.ToSlash(rel)] = true
	}
}

// worktreesAllowed reports whether writers may use pooled worktrees in this
// task (whether they do also depends on the plan). It also logs a one-time
// hint about git settings that speed up snapshots in big repos.
func (o *Orchestrator) worktreesAllowed(t *task) bool {
	if !t.useGit {
		return false
	}
	oc := t.cfg.Orchestrator
	files, tips := PerfTips(t.root)
	if len(tips) > 0 && !o.tipped {
		o.tipped = true
		o.logf("big repo (%d tracked files): for faster snapshots run in it: %s", files, strings.Join(tips, " && "))
	}
	// Reviewing changes needs every writer in a worktree, even one at a time.
	if !o.reviewing(t) && (!oc.Worktrees || !oc.Parallel || oc.MaxThreads < 2) {
		return false
	}
	if !SupportsMergeTree() {
		o.logf("git < 2.38 (no merge-tree --write-tree): writing agents run one at a time in the main tree")
		return false
	}
	if oc.WorktreeMaxFiles > 0 && files > oc.WorktreeMaxFiles {
		o.logf("%d tracked files (> worktree_max_files %d): writing agents run one at a time in the main tree", files, oc.WorktreeMaxFiles)
		return false
	}
	t.lfs = (git{t.root}).usesLFS()
	return true
}

func (t *task) addTokens(provider string, u event.TokenUsage) {
	t.tokensMu.Lock()
	defer t.tokensMu.Unlock()
	t.tokens = t.tokens.Add(u)
	if t.perProv == nil {
		t.perProv = map[string]event.TokenUsage{}
	}
	t.perProv[provider] = t.perProv[provider].Add(u)
}

// quotaNow reads every provider's reported limit usage.
func (o *Orchestrator) quotaNow() map[string]float64 {
	m := map[string]float64{}
	for _, p := range event.Providers {
		if u, ok := o.opts.Tracker.Utilization(p); ok {
			m[p] = u
		}
	}
	return m
}

// cost summarizes what the task used.
func (o *Orchestrator) cost(t *task) event.TaskCost {
	t.tokensMu.Lock()
	defer t.tokensMu.Unlock()
	c := event.TaskCost{PerProvider: map[string]event.TokenUsage{}, QuotaBefore: t.quotaBefore, QuotaAfter: o.quotaNow()}
	for p, u := range t.perProv {
		c.PerProvider[p] = u
		c.CostUSD += u.CostUSD
	}
	return c
}

// snapshotBefore records the working tree before a task for `sy undo`.
func (o *Orchestrator) snapshotBefore(t *task) {
	if o.opts.NoGit || !isRepo(o.opts.Dir) {
		return
	}
	defer func() {
		if o.opts.Bench != "" {
			t.key = "" // bench runs are not undoable user tasks
		}
	}()
	root, err := repoRoot(o.opts.Dir)
	if err != nil {
		return
	}
	t.root = root
	o.logf("snapshotting the working tree (git add -A on a temporary index)")
	snap, big, err := (git{root}).snapshotSkipping(subject("switchyard before: ", t.text))
	if err != nil {
		o.logf("git snapshot failed, worktrees and undo disabled: %v", err)
		return
	}
	if len(big) > 0 {
		o.logf("left %d untracked file(s) over %d MB out of the snapshot (agents in worktrees do not see them, undo does not cover them): %s",
			len(big), snapshotMaxFile.Load()>>20, clip(strings.Join(big, ", "), 300))
	}
	t.useGit = true
	t.snapshot, t.start = snap, snap
	if o.opts.Bench == "" && !t.keepBefore {
		git{root}.recordSnapshot(t.key, "before", snap)
	}
}

// snapshotAfter records the end state (also after a cancel or failure).
func (o *Orchestrator) snapshotAfter(t *task) {
	if !t.useGit || t.start == "" || t.key == "" {
		return
	}
	g := git{t.root}
	t.tokensMu.Lock()
	msg := afterMessage(t.text, t.agentFiles)
	t.tokensMu.Unlock()
	if snap, err := g.snapshot(msg); err == nil {
		g.recordSnapshot(t.key, "after", snap)
		trimUndo(t.root)
	}
}

// Run executes a task end to end. A panic inside the task is written to a
// crash log and ends the task as failed instead of taking sy down.
func (o *Orchestrator) Run(ctx context.Context, text string) TaskResult {
	return o.RunWith(ctx, text, TaskOptions{})
}

// RunWith is Run with options (unattended, resume).
func (o *Orchestrator) RunWith(ctx context.Context, text string, opts TaskOptions) (result TaskResult) {
	if opts.Resume != nil {
		text = opts.Resume.Task
	}
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return TaskResult{Summary: "a task is already running"}
	}
	o.running = true
	o.taskSeq++
	seq := o.taskSeq
	o.mu.Unlock()
	finished := false
	defer func() { // panics only; the normal path clears it before TaskDone
		r := recover()
		if !finished {
			o.mu.Lock()
			o.running = false
			o.cur = nil
			o.mu.Unlock()
		}
		if r != nil {
			path := diag.Crash("task", r, debug.Stack())
			result = TaskResult{Summary: "internal error, Switchyard bug: details in " + path + " (sy bugreport)"}
			if !finished {
				o.emit(event.Event{Kind: event.Phase, Text: "done"})
				o.emit(event.Event{Kind: event.TaskDone, Text: result.Summary})
			}
		}
	}()

	began := time.Now()
	cfg := o.opts.Store.Get()
	proc.SetLowPriority(cfg.Orchestrator.LowPriority)
	minFreeDisk.Store(uint64(cfg.Orchestrator.MinFreeDiskGB * (1 << 30)))
	snapshotMaxFile.Store(int64(max(0, cfg.Orchestrator.SnapshotMaxFileMB)) << 20)
	t := &task{id: fmt.Sprintf("%stask-%d", o.opts.TaskIDPrefix, seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg)}
	t.key = o.opts.Log.Session() + "-" + t.id
	t.unattended = opts.Unattended
	refused := ""
	if o.opts.Bench == "" && o.opts.Mode != "demo" {
		t.state = opts.Resume
		if t.state == nil {
			t.state = &TaskState{ID: t.key, Task: text, Dir: o.opts.Dir, Mode: o.opts.Mode, Created: time.Now(), UndoKey: t.key}
		} else {
			t.resumed = true
		}
		// The lock comes first: two sy processes must never run one task.
		unlock, ok := t.state.lock()
		if !ok {
			refused = fmt.Sprintf("task %s is running in another sy", t.state.ID)
			t.state = nil
		} else {
			defer unlock()
			if t.resumed {
				// The state on disk is the truth: another sy may have
				// resumed and finished it since this one was loaded.
				fresh, err := LoadTask(t.state.ID)
				switch {
				case err != nil:
					refused = err.Error()
				case fresh.Status != "running" && !opts.Force:
					refused = fmt.Sprintf("task %s is %s now, not interrupted (sy resume --force runs its unfinished steps)", fresh.ID, fresh.Status)
				}
				if refused != "" {
					t.state = nil
				} else {
					t.state = fresh
					if fresh.UndoKey != "" {
						// Undo covers the whole task, not just this part.
						t.key, t.keepBefore = fresh.UndoKey, true
					}
				}
			}
		}
		if t.state != nil {
			t.state.Status = "running"
			t.state.save()
		}
	}
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: o.opts.Mode})
	o.emit(event.Event{Kind: event.TaskStart, Text: text})

	t.quotaBefore = o.quotaNow()
	// tctx is ctx plus a stop by the budget (budget.go); ctx alone tells
	// whether the person cancelled.
	tctx := o.startBudget(ctx, t)
	var res TaskResult
	if refused == "" {
		if err := o.runHooks(tctx, t, "before_task", cfg.Hooks.BeforeTask, nil); err != nil {
			refused = "not started: " + err.Error()
			if t.state != nil {
				t.state.Status = "failed"
			}
		}
	}
	if refused != "" {
		o.emit(event.Event{Kind: event.Error, Text: refused})
		res = TaskResult{Summary: refused}
	} else {
		res = o.run(tctx, t)
	}
	if why := t.budgetStopped(); why != "" && ctx.Err() == nil {
		res.OK = false
		res.Summary = "stopped by budget: " + why
	}
	o.snapshotAfter(t)
	res.Duration = time.Since(began)
	res.Tokens = t.tokens
	res.Kept = t.kept
	res.Cost = o.cost(t)
	if t.useGit {
		res.UndoKey = t.key // "" for bench runs
	}
	if ctx.Err() != nil {
		res.OK = false
		res.Summary = "cancelled: " + res.Summary
	}
	status := map[bool]string{true: "done", false: "failed"}[res.OK]
	if ctx.Err() != nil {
		status = "cancelled"
	}
	// after_task runs even after a cancel, so give it a context of its own.
	o.runHooks(context.WithoutCancel(ctx), t, "after_task", cfg.Hooks.AfterTask, map[string]string{"SY_STATUS": status, "SY_SUMMARY": res.Summary})
	if res.OK && t.useGit && o.opts.Bench == "" && t.cfg.Orchestrator.Handoff {
		addRepoNote(t.root, text, res.Summary, t.changedFiles())
	}
	if s := t.state; s != nil {
		s.Status = map[bool]string{true: "done", false: "failed"}[res.OK]
		if ctx.Err() != nil {
			s.Status = "cancelled"
		}
		s.Summary, s.UndoKey, s.CostLine = res.Summary, res.UndoKey, res.Cost.Summary()
		s.save()
		pruneStates()
	}
	tk := t.tokens
	cost := res.Cost
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: o.opts.Mode,
		OK: sessionlog.Bool(res.OK), Text: res.Summary, Tokens: &tk, DurationMS: res.Duration.Milliseconds(), Cost: &cost, Bench: o.opts.Bench})
	o.endBudget(t, cost)
	// Clear the running flag before announcing the end, so a task submitted
	// right after TaskDone is never refused.
	o.mu.Lock()
	o.running = false
	o.mu.Unlock()
	finished = true
	o.emit(event.Event{Kind: event.Phase, Text: "done"})
	o.emit(event.Event{Kind: event.TaskDone, OK: res.OK, Text: res.Summary, Tokens: tk, Cost: &cost})
	return res
}

func (o *Orchestrator) run(ctx context.Context, t *task) TaskResult {
	cfg := t.cfg
	oc := cfg.Orchestrator

	// Git setup.
	o.snapshotBefore(t)
	t.wtOK = o.worktreesAllowed(t)
	if o.reviewing(t) && !t.wtOK {
		why := "this folder is not a git repo"
		if t.useGit {
			why = "worktrees are off for this repo (git < 2.38 or more than worktree_max_files files)"
		}
		o.emit(event.Event{Kind: event.Error, Text: "change review is on, but " + why + ": agents write straight into your tree (sy undo still works in git repos)"})
	}
	if t.useGit && cfg.Orchestrator.Handoff {
		t.repoMap = repoMap(t.root)
		t.repoNotes = repoNotes(t.root)
	}

	// 1. Plan.
	o.emit(event.Event{Kind: event.Phase, Text: "plan"})
	var plan Plan
	small := false
	if t.resumed && t.state.Plan != nil {
		plan = *t.state.Plan
		o.logf("%s", t.state.resumeSummary())
		t.mainProv = o.router.Route(router.Step{ID: "plan", Kind: router.KindPlan}).Provider
	} else if words := len(strings.Fields(t.text)); oc.SmallTaskWords > 0 && words < oc.SmallTaskWords {
		small = true
		plan = Plan{Summary: "small task: one worker step", Subtasks: []Subtask{{ID: "work", Title: firstWords(t.text, 6), Kind: router.KindEdit, Prompt: t.text}}}
		if looksRead(t.text) {
			plan.Subtasks[0].Kind = router.KindExplore
			plan.Subtasks[0].ID = "explore"
		}
		o.logf("small task (%d words): skipping the planner", words)
		t.mainProv = o.router.Route(router.Step{ID: "plan", Kind: router.KindPlan}).Provider
	} else {
		if t.wtOK {
			// Create missing pool worktrees while the planner thinks.
			warm := make(chan struct{})
			t.warm = warm
			root, snap, n := t.root, t.snapshot, oc.MaxThreads
			go func() {
				defer close(warm)
				defer diag.Recover("pool prewarm", nil)
				prewarmPool(root, snap, n)
				t.poolSize = PoolSize(root) // read after <-warm only
			}()
		}
		p, ok := o.plan(ctx, t, "", nil)
		if ctx.Err() != nil {
			return TaskResult{Summary: "planning cancelled"}
		}
		if !ok {
			return TaskResult{Summary: "planning failed: " + p.Summary}
		}
		plan = p
		// 2. Review the plan. A one-step plan is skipped unless asked for:
		// on the bench every one was approved, and the final review still
		// checks the work.
		if oc.ReviewBeforePlan && (len(plan.Subtasks) > 1 || oc.ReviewSingleStepPlan) {
			for rev := 0; ; rev++ {
				o.emit(event.Event{Kind: event.Phase, Text: "review-plan"})
				v, ok := o.review(ctx, t, "plan", planReviewPrompt(t.text, plan))
				if ctx.Err() != nil {
					return TaskResult{Summary: "cancelled during plan review"}
				}
				if !ok || v.Approve || rev >= oc.MaxPlanRevisions {
					break
				}
				o.emit(event.Event{Kind: event.Phase, Text: "plan"})
				np, ok := o.plan(ctx, t, v.Advice+"\n"+strings.Join(v.Issues, "\n"), &plan)
				if !ok {
					break
				}
				plan = np
			}
		}
	}

	// 2b. The person approves (and may edit) the plan.
	if !(t.resumed && t.state.Plan != nil) && !small && o.approving(t) {
		o.emit(event.Event{Kind: event.Phase, Text: "approve-plan"})
		o.logf("waiting for you to approve the plan (%d subtasks)", len(plan.Subtasks))
		p, ok := o.opts.Approver.ApprovePlan(ctx, t.text, plan)
		if ctx.Err() != nil {
			return TaskResult{Summary: "cancelled at plan approval"}
		}
		if !ok {
			return TaskResult{Summary: "plan not approved: nothing was run"}
		}
		np, err := NormalizePlan(p)
		if err != nil {
			return TaskResult{Summary: "the edited plan is empty: nothing was run"}
		}
		plan = np
		o.logf("plan approved: %d subtasks", len(plan.Subtasks))
	}
	if t.state != nil {
		pl := plan
		t.state.Plan = &pl
		t.state.Phase = "execute"
		t.state.save()
	}

	// 3. Execute.
	o.emit(event.Event{Kind: event.Phase, Text: "execute"})
	results := o.execute(ctx, t, plan)
	if ctx.Err() != nil {
		return TaskResult{Summary: "cancelled during execution"}
	}
	allOK := true
	for _, st := range plan.Subtasks {
		if !results[st.ID].ok {
			allOK = false
		}
	}

	// 4. Verify (the repo's own checks) and review before done, with fix
	// rounds that get the reviewer's advice and the failing output.
	approved := true
	verified := true
	var lastAdvice string
	verifying := len(t.cfg.Verify.Commands) > 0
	if hasEdits(plan) && (oc.ReviewBeforeDone || verifying) {
		for round := 0; ; round++ {
			report := ""
			if verifying {
				o.emit(event.Event{Kind: event.Phase, Text: "verify"})
				verified, report = o.verify(ctx, t)
				if ctx.Err() != nil {
					return TaskResult{Summary: "cancelled during verify"}
				}
			}
			roundOK := verified
			var v Verdict
			if oc.ReviewBeforeDone {
				o.emit(event.Event{Kind: event.Phase, Text: "review"})
				stat, diff := "", ""
				if t.useGit {
					stat, diff = git{t.root}.diff(t.start, 40_000)
				}
				var ok bool
				v, ok = o.review(ctx, t, "final", finalReviewPrompt(t.text, plan, results, stat, diff, t.notes, report))
				if ctx.Err() != nil {
					return TaskResult{Summary: "cancelled during final review"}
				}
				if ok {
					roundOK = roundOK && v.Approve
					lastAdvice = v.Advice
				}
			}
			approved = roundOK
			if roundOK || round >= oc.MaxFixRounds {
				break
			}
			if !verified {
				v.Approve = false
				v.Advice = strings.TrimSpace(v.Advice + "\n\nThese checks fail; make them pass:\n" + report)
			}
			o.emit(event.Event{Kind: event.Phase, Text: "fix"})
			fix := Subtask{ID: fmt.Sprintf("fix-%d", round+1), Title: "Apply review fixes", Kind: router.KindFix, Prompt: fixPrompt(t.text, v)}
			var r stepResult
			if o.reviewing(t) && t.wtOK {
				r = o.runInWorktree(ctx, t, fix, nil, fixPrompt(t.text, v))
			} else {
				r = o.runStep(ctx, t, fix, nil, o.opts.Dir, fixPrompt(t.text, v))
			}
			results[fix.ID] = r
			if !r.ok {
				allOK = false
				break
			}
		}
	}

	ok := allOK && approved && len(t.kept) == 0
	var b strings.Builder
	done := 0
	for _, st := range plan.Subtasks {
		if results[st.ID].ok {
			done++
		}
	}
	fmt.Fprintf(&b, "%d/%d subtasks ok", done, len(plan.Subtasks))
	switch {
	case !verified:
		b.WriteString("; checks still fail (" + strings.Join(t.cfg.Verify.Commands, ", ") + ")")
	case !approved:
		b.WriteString("; reviewer still has concerns: " + clip(lastAdvice, 200))
	case oc.ReviewBeforeDone && hasEdits(plan):
		b.WriteString("; reviewer approved")
	}
	if verified && verifying && hasEdits(plan) {
		b.WriteString("; checks pass")
	}
	if len(t.kept) > 0 {
		b.WriteString("; conflicts kept on " + strings.Join(t.kept, ", "))
	}
	return TaskResult{OK: ok, Summary: b.String()}
}

func hasEdits(p Plan) bool {
	for _, st := range p.Subtasks {
		if !st.Kind.ReadOnly() {
			return true
		}
	}
	return false
}

var reReadTask = regexp.MustCompile(`(?i)^\s*(where|what|how|why|explain|find|list|show|summari[sz]e|describe)\b`)

func looksRead(s string) bool { return reReadTask.MatchString(s) }

// plan runs the planner. On a parse failure the whole task becomes one edit
// step, so a chatty planner never blocks progress.
func (o *Orchestrator) plan(ctx context.Context, t *task, advice string, prev *Plan) (Plan, bool) {
	step := router.Step{ID: "plan", Title: "Plan the task", Kind: router.KindPlan, Prompt: t.text}
	d, res := o.runOnce(ctx, t, step, AgentMain, "", planPrompt(t.text, advice, prev)+t.handoff())
	t.mainProv = d.Provider
	if !res.OK() {
		msg := "planner failed"
		if res.Err != nil {
			msg = res.Err.Error()
		}
		if res.Killed {
			return Plan{Summary: msg}, false
		}
		o.logf("planner failed (%s); running the task as one worker step", clip(msg, 200))
		return Plan{Summary: "planner unavailable: single step", Subtasks: []Subtask{{ID: "work", Title: firstWords(t.text, 6), Kind: router.KindEdit, Prompt: t.text}}}, true
	}
	p, err := ParsePlan(res.Final)
	if err != nil {
		o.logf("could not read the plan (%v); running the task as one worker step", err)
		return Plan{Summary: "unparsed plan: single step", Subtasks: []Subtask{{ID: "work", Title: firstWords(t.text, 6), Kind: router.KindEdit, Prompt: t.text + "\n\nPlanner notes:\n" + clip(res.Final, 3000)}}}, true
	}
	var titles []string
	for _, st := range p.Subtasks {
		titles = append(titles, fmt.Sprintf("%s(%s)", st.ID, st.Kind))
	}
	o.logf("plan: %s -> %s", clip(p.Summary, 160), strings.Join(titles, ", "))
	return p, true
}

// runOnce runs a planner or reviewer in the main tree. If its provider hits
// its limit or turns out to be unavailable, it is retried once: the router
// then sees the provider as limited and picks the other one.
func (o *Orchestrator) runOnce(ctx context.Context, t *task, step router.Step, agentID, parent, prompt string) (event.Decision, runner.Result) {
	d, res := o.runAgent(ctx, t, step, agentID, parent, o.opts.Dir, prompt, 1)
	if res.LimitHit && !res.Killed && ctx.Err() == nil && !(o.opts.Tracker.Limited(event.Codex) && o.opts.Tracker.Limited(event.Claude)) {
		o.logf("%s: %s unavailable, retrying on %s", agentID, d.Provider, event.Other(d.Provider))
		d, res = o.runAgent(ctx, t, step, agentID, parent, o.opts.Dir, prompt, 2)
	}
	return d, res
}

// review runs the reviewer and reports its verdict.
func (o *Orchestrator) review(ctx context.Context, t *task, checkpoint, prompt string) (Verdict, bool) {
	step := router.Step{ID: "review-" + checkpoint, Title: checkpoint + " review", Kind: router.KindReview, MainProvider: t.mainProv}
	d, res := o.runOnce(ctx, t, step, AgentReviewer, AgentMain, prompt)
	if !res.OK() {
		o.logf("reviewer unavailable for %s checkpoint: %v", checkpoint, res.Err)
		return Verdict{}, false
	}
	v := ParseVerdict(res.Final)
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeReview, TaskID: t.id, Step: checkpoint, Provider: d.Provider, Model: d.Model,
		OK: sessionlog.Bool(v.Approve), Text: clip(v.Advice, 2000)})
	text := v.Advice
	if strings.TrimSpace(text) == "" {
		text = map[bool]string{true: "approved", false: "changes requested"}[v.Approve]
	}
	if len(v.Issues) > 0 {
		text += "\n- " + strings.Join(v.Issues, "\n- ")
	}
	o.emit(event.Event{Kind: event.Checkpoint, AgentID: AgentReviewer, Provider: d.Provider, Model: d.Model, Role: event.RoleReviewer,
		OK: v.Approve, Text: checkpoint + ": " + strings.TrimSpace(text)})
	return v, true
}

// execute runs all subtasks respecting dependencies and max_threads.
func (o *Orchestrator) execute(ctx context.Context, t *task, p Plan) map[string]stepResult {
	oc := t.cfg.Orchestrator
	threads := oc.MaxThreads
	if !oc.Parallel || threads < 1 {
		threads = 1
	}
	edits := 0
	for _, st := range p.Subtasks {
		if !st.Kind.ReadOnly() {
			edits++
		}
	}
	useWT := t.wtOK && ((threads > 1 && edits > 1) || (o.reviewing(t) && edits > 0))
	if useWT {
		if t.lfs {
			o.logf("git lfs repo: worktrees keep LFS files as pointers")
		}
		t.pool = poolDir(t.root)
		if t.warm != nil {
			select {
			case <-t.warm:
				if warn := uint64(oc.PoolWarnGB * (1 << 30)); warn > 0 && t.poolSize > warn {
					o.logf("worktree pool for this repo uses %s (> pool_warn_gb %.0f GB): `sy clean` frees it; unused slots are pruned after pool_max_idle",
						humanBytes(t.poolSize), oc.PoolWarnGB)
				}
			case <-ctx.Done():
			}
		}
	}

	for _, st := range p.Subtasks {
		o.emit(event.Event{Kind: event.AgentQueued, AgentID: st.ID, ParentID: AgentMain, Role: string(st.Kind), Text: st.Title})
	}

	results := map[string]stepResult{}
	var mu sync.Mutex
	done := map[string]bool{}
	started := map[string]bool{}
	if t.resumed && t.state != nil {
		// Subtasks that already succeeded before the interruption: their
		// changes are in the working tree, their results feed dependents.
		for i, st := range p.Subtasks {
			if r, ok := t.state.Results[st.ID]; ok && r.OK {
				results[st.ID] = stepResult{ok: true, final: r.Final}
				done[st.ID], started[st.ID] = true, true
				o.emit(event.Event{Kind: event.Done, AgentID: st.ID, ParentID: AgentMain, OK: true, Text: "done before the interruption"})
			} else if !st.Kind.ReadOnly() {
				// It may have been running when sy stopped: its agent may
				// have left half-done edits in the tree.
				p.Subtasks[i].Prompt += "\n\nNOTE: an earlier attempt at this subtask was interrupted (sy stopped). Files it was editing may be partly changed: check the current state of the files before you edit, and finish or redo the work."
			}
		}
	}
	sem := make(chan struct{}, threads)
	t.writeSem = make(chan struct{}, 1) // writers in the main tree run one at a time
	writeSem := t.writeSem
	inflight := 0
	var wg sync.WaitGroup
	wake := make(chan struct{}, len(p.Subtasks)+1)

	for {
		mu.Lock()
		if len(done) == len(p.Subtasks) {
			mu.Unlock()
			break
		}
		var ready []Subtask
		for _, st := range p.Subtasks {
			if started[st.ID] {
				continue
			}
			ok := true
			for _, d := range st.DependsOn {
				if !done[d] {
					ok = false
				}
			}
			if ok {
				ready = append(ready, st)
				started[st.ID] = true
			}
		}
		if len(ready) == 0 && inflight == 0 {
			// Nothing can ever start (should not happen after ParsePlan's
			// normalization, but never hang on a bad plan).
			for _, st := range p.Subtasks {
				if !done[st.ID] {
					results[st.ID] = stepResult{err: "could not be scheduled"}
					done[st.ID] = true
				}
			}
			mu.Unlock()
			break
		}
		inflight += len(ready)
		mu.Unlock()
		if ctx.Err() != nil {
			break
		}
		for _, st := range ready {
			st := st
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					inflight--
					mu.Unlock()
					wake <- struct{}{}
				}()
				defer diag.Recover("subtask "+st.ID, func(crashLog string) {
					mu.Lock()
					results[st.ID] = stepResult{err: "internal error, details in " + crashLog}
					done[st.ID] = true
					mu.Unlock()
				})
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					mu.Lock()
					results[st.ID] = stepResult{err: "cancelled"}
					done[st.ID] = true
					mu.Unlock()
					return
				}
				defer func() { <-sem }()
				mu.Lock()
				var deps []string
				declared := map[string]bool{}
				for _, d := range st.DependsOn {
					declared[d] = true
					if r := results[d]; r.final != "" {
						deps = append(deps, fmt.Sprintf("[%s] %s", d, clip(r.final, 3000)))
					}
				}
				if !st.Kind.ReadOnly() && t.cfg.Orchestrator.Handoff {
					// What finished read-only steps found helps every writer.
					for _, ro := range p.Subtasks {
						if r := results[ro.ID]; ro.Kind.ReadOnly() && !declared[ro.ID] && r.ok && r.final != "" {
							deps = append(deps, fmt.Sprintf("[%s, read-only finding] %s", ro.ID, clip(r.final, 1500)))
						}
					}
				}
				mu.Unlock()
				var r stepResult
				switch {
				case st.Kind.ReadOnly():
					r = o.runStep(ctx, t, st, deps, o.opts.Dir, "")
				case useWT:
					r = o.runInWorktree(ctx, t, st, deps, "")
				default:
					select {
					case writeSem <- struct{}{}:
					case <-ctx.Done():
						r = stepResult{err: "cancelled"}
					}
					if r.err == "" {
						func() {
							defer func() { <-writeSem }() // released even if the step panics
							r = o.runStep(ctx, t, st, deps, o.opts.Dir, "")
							if r.ok {
								o.afterMerge(ctx, t, st.ID, r.files) // it wrote straight into the tree
							}
						}()
					}
				}
				mu.Lock()
				results[st.ID] = r
				done[st.ID] = true
				mu.Unlock()
				t.state.setResult(st.ID, r)
			}()
		}
		select {
		case <-wake:
		case <-ctx.Done():
		}
	}
	wg.Wait()
	return results
}

// runInWorktree runs a writing subtask in its own worktree and merges it.
func (o *Orchestrator) runInWorktree(ctx context.Context, t *task, st Subtask, deps []string, prompt string) stepResult {
	g := git{t.root}
	t.mergeMu.Lock()
	base := t.snapshot
	t.mergeMu.Unlock()
	s, err := acquireSlot(t.root, base)
	if err != nil {
		o.logf("worktree for %s failed (%v); running in the main tree", st.ID, err)
		if o.reviewing(t) {
			o.emit(event.Event{Kind: event.Error, AgentID: st.ID, Text: "change review is not possible for " + st.ID + " (no worktree): its changes go straight into your tree; sy undo reverts the task"})
		}
		select { // one writer at a time in the main tree
		case t.writeSem <- struct{}{}:
			defer func() { <-t.writeSem }()
		case <-ctx.Done():
			return stepResult{err: "cancelled"}
		}
		return o.runStep(ctx, t, st, deps, o.opts.Dir, prompt)
	}
	defer s.release()
	path := s.path
	// Agents should work in the same relative directory they would use in the main tree.
	dir := path
	if rel, err := filepath.Rel(t.root, o.opts.Dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		dir = filepath.Join(path, rel)
	}
	r := o.runStep(ctx, t, st, deps, dir, prompt)
	if !r.ok {
		return r
	}
	wg := git{path}
	var commit, agentHead string
	for round := 1; ; round++ {
		sc, err := wg.commitWork(base, "switchyard: "+st.Title)
		if err != nil {
			o.mergeEvent(t, st.ID, false, "commit failed: "+err.Error())
			r.ok, r.err = false, "commit failed: "+err.Error()
			return r
		}
		o.slotWarnings(t, st.ID, sc, &agentHead)
		if !sc.Changed && round == 1 {
			o.mergeEvent(t, st.ID, true, "no file changes")
			return r
		}
		commit = sc.Commit
		if !o.reviewing(t) {
			break
		}
		// The person reviews the agent's changes before they land.
		files, err := g.changeSet(base, commit)
		if err != nil || len(files) == 0 {
			break
		}
		o.logf("%s: waiting for you to review %d changed file(s)", st.ID, len(files))
		dec := o.opts.Approver.ReviewChanges(ctx, ChangeSet{StepID: st.ID, Title: st.Title, Summary: r.final, Round: round, Files: files})
		if ctx.Err() != nil {
			r.ok, r.err = false, "cancelled during your review"
			return r
		}
		if dec.Feedback != "" && round >= 3 {
			// Out of feedback rounds: asking for changes must never apply
			// the work it objected to.
			o.logf("%s: no more feedback rounds; the changes are not applied", st.ID)
			dec = ChangeDecision{}
		}
		if dec.Feedback != "" {
			o.logf("%s: you asked for changes: %s", st.ID, clip(dec.Feedback, 200))
			again := stepPrompt(t.text, st, deps, "", "", false) + "\nYou already changed files in this directory for this subtask. The user reviewed your changes and asks:\n" +
				dec.Feedback + "\nUpdate your changes accordingly, then reply with a short summary.\n"
			r = o.runStep(ctx, t, st, deps, dir, again)
			if !r.ok {
				// Keep the last reviewed version; the slot is reset for the
				// next agent.
				branch := o.saveBranch(t, st.ID, commit)
				o.logf("%s: the rerun failed; your previous version is kept on %s", st.ID, branch)
				return r
			}
			continue
		}
		if len(dec.Apply) == 0 {
			branch := o.saveBranch(t, st.ID, commit)
			o.mergeEvent(t, st.ID, false, "rejected by you; the changes are kept on "+branch)
			t.mergeMu.Lock() // t.notes is shared by parallel steps
			t.notes = append(t.notes, fmt.Sprintf("the user rejected the changes of %s (kept on branch %s)", st.ID, branch))
			t.mergeMu.Unlock()
			r.ok, r.err = false, "changes rejected by you"
			return r
		}
		if len(dec.Apply) < len(files) || len(dec.Hunks) > 0 {
			full := commit
			pc, err := g.selectionCommit(base, commit, files, dec, "switchyard: "+st.Title+" (what you accepted)")
			if err != nil {
				branch := o.saveBranch(t, st.ID+"-full", full)
				r.ok, r.err = false, "could not apply the selected files ("+err.Error()+"); the full change is on "+branch
				return r
			}
			commit = pc
			branch := o.saveBranch(t, st.ID+"-full", full)
			o.logf("%s: applying %d of %d files (%d split by hunk); the full change is kept on %s", st.ID, len(dec.Apply), len(files), len(dec.Hunks), branch)
		}
		break
	}
	t.mergeMu.Lock()
	defer t.mergeMu.Unlock()
	tree, clean, info, err := g.mergeTree(t.snapshot, commit)
	if err != nil || !clean {
		reason := info
		if err != nil {
			reason = err.Error()
		}
		branch := o.keepBranch(t, st.ID, commit)
		o.mergeEvent(t, st.ID, false, fmt.Sprintf("conflict in %s; kept on %s", reason, branch))
		t.notes = append(t.notes, fmt.Sprintf("%s conflicted (%s) and was NOT applied; its changes are on branch %s", st.ID, reason, branch))
		r.ok, r.err = false, "merge conflict: "+reason
		return r
	}
	merged, err := g.commitTree("commit-tree", tree, "-p", t.snapshot, "-p", commit, "-m", "switchyard: merge "+st.ID)
	if err != nil {
		o.mergeEvent(t, st.ID, false, err.Error())
		r.ok, r.err = false, err.Error()
		return r
	}
	skipped, err := g.applyDiffReport(t.snapshot, merged)
	if len(skipped) > 0 {
		o.logf("%s: submodule changes are not applied to your tree: %s", st.ID, strings.Join(skipped, ", "))
		t.notes = append(t.notes, fmt.Sprintf("%s changed submodule(s) %s; Switchyard does not apply submodule changes", st.ID, strings.Join(skipped, ", ")))
	}
	if err != nil {
		branch := o.keepBranch(t, st.ID, commit)
		o.mergeEvent(t, st.ID, false, fmt.Sprintf("could not apply to working tree (%v); kept on %s", err, branch))
		t.notes = append(t.notes, fmt.Sprintf("%s could not be applied to the working tree; its changes are on branch %s", st.ID, branch))
		r.ok, r.err = false, "apply failed"
		return r
	}
	var landed []string
	if names, err := g.out("diff", "--name-only", "-z", t.snapshot, merged); err == nil {
		var paths []string
		for _, p := range strings.Split(names, "\x00") {
			if p != "" {
				paths = append(paths, filepath.Join(t.root, filepath.FromSlash(p)))
			}
		}
		t.noteFiles(t.root, paths)
		landed = paths
	}
	t.snapshot = merged
	o.mergeEvent(t, st.ID, true, fmt.Sprintf("merged %d file(s)", len(r.files)))
	o.afterMerge(ctx, t, st.ID, landed)
	return r
}

// slotWarnings reports what an agent did in its worktree that Switchyard
// cannot carry over as is. agentHead remembers the HEAD already saved.
func (o *Orchestrator) slotWarnings(t *task, stepID string, sc slotCommit, agentHead *string) {
	if sc.Head != "" && sc.Head != *agentHead {
		*agentHead = sc.Head
		branch := o.saveBranch(t, stepID+"-agent-head", sc.Head)
		o.logf("%s: the agent moved git HEAD in its worktree (committed or switched branches); its files are merged as usual, and its own commits are kept on %s", stepID, branch)
		o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeMerge, TaskID: t.id, Step: stepID, Text: "agent moved HEAD; kept on " + branch})
	}
	if len(sc.Nested) > 0 {
		o.logf("%s: left out nested git repositories the agent created (their files are not merged): %s", stepID, strings.Join(sc.Nested, ", "))
		t.mergeMu.Lock()
		t.notes = append(t.notes, fmt.Sprintf("%s created nested git repositories (%s); they were not merged", stepID, strings.Join(sc.Nested, ", ")))
		t.mergeMu.Unlock()
	}
	if len(sc.Sparse) > 0 {
		o.logf("%s: files written outside your sparse checkout are dropped: %s", stepID, clip(strings.Join(sc.Sparse, ", "), 300))
		t.mergeMu.Lock()
		t.notes = append(t.notes, fmt.Sprintf("%s wrote files outside the sparse checkout (%s); they were dropped", stepID, clip(strings.Join(sc.Sparse, ", "), 300)))
		t.mergeMu.Unlock()
	}
}

// saveBranch keeps a commit on a branch for the person to look at later
// without counting it as a conflict. Branches are only ever created, never
// moved: an existing name gets a numbered suffix.
func (o *Orchestrator) saveBranch(t *task, stepID, commit string) string {
	g := git{t.root}
	base := "sy/" + refPart(o.opts.Log.Session()) + "/" + refPart(t.id) + "/" + refPart(stepID)
	for i := 1; i <= 20; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		if created, exists := createBranch(g, name, commit); created {
			return name
		} else if !exists {
			break
		}
	}
	// Last resort; the commit must stay referenced either way.
	name := "sy/kept-" + commit[:min(12, len(commit))]
	if created, _ := createBranch(g, name, commit); created {
		return name
	}
	g.out("update-ref", "refs/switchyard/kept/"+commit, commit)
	return commit[:min(12, len(commit))]
}

// createBranch creates refs/heads/name at commit unless it exists; a branch
// already at commit counts as created.
func createBranch(g git, name, commit string) (created, exists bool) {
	ref := "refs/heads/" + name
	if _, err := g.out("update-ref", "--create-reflog", ref, commit, ""); err == nil {
		return true, false
	}
	cur, err := g.out("rev-parse", "-q", "--verify", ref+"^{commit}")
	if err != nil {
		return false, false
	}
	return cur == commit, true
}

// refPart makes s usable as one component of a branch name.
func refPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	p := b.String()
	for strings.Contains(p, "..") {
		p = strings.ReplaceAll(p, "..", ".")
	}
	p = strings.Trim(p, ".-")
	p = strings.TrimSuffix(p, ".lock")
	if len(p) > 60 {
		p = strings.TrimRight(p[:60], ".-")
	}
	if p == "" {
		p = "x"
	}
	return p
}

func (o *Orchestrator) keepBranch(t *task, stepID, commit string) string {
	branch := o.saveBranch(t, stepID, commit)
	t.kept = append(t.kept, branch)
	return branch
}

func (o *Orchestrator) mergeEvent(t *task, stepID string, ok bool, text string) {
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeMerge, TaskID: t.id, Step: stepID, OK: sessionlog.Bool(ok), Text: text})
	o.emit(event.Event{Kind: event.Merge, AgentID: stepID, ParentID: AgentMain, OK: ok, Text: text})
}

var reNumbers = regexp.MustCompile(`\d+`)

// reAuth matches CLI errors that mean "not logged in".
var reAuth = regexp.MustCompile(`(?i)(not logged in|please (log|sign) ?in|log ?in required|unauthori[sz]ed|\b401\b|authentication (failed|required)|token (has )?expired|codex login)`)

// errorSignature normalizes an error so "the same error twice" ignores
// timestamps, line numbers and durations.
func errorSignature(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	s = reNumbers.ReplaceAllString(s, "#")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// runStep runs one subtask with retries, limit fallback and the
// "error repeats" escalation. prompt overrides the generated step prompt.
func (o *Orchestrator) runStep(ctx context.Context, t *task, st Subtask, deps []string, dir, prompt string) stepResult {
	oc := t.cfg.Orchestrator
	step := router.Step{ID: st.ID, Title: st.Title, Kind: st.Kind, Prompt: st.Prompt, Files: st.Files, MainProvider: t.mainProv, UserRole: st.Role}
	var prevErr, advice, lastSig string
	failures, limitRetries := 0, 0
	for attempt := 1; ; attempt++ {
		p := prompt
		if p == "" {
			p = stepPrompt(t.text, st, deps, prevErr, advice, st.Kind.ReadOnly()) + t.handoff()
			if t.lfs && t.pool != "" && strings.HasPrefix(dir, t.pool) {
				p += lfsNote
			}
			if cmds := t.cfg.Verify.Commands; len(cmds) > 0 && !st.Kind.ReadOnly() {
				p += "\nBefore you finish, run the repo's checks (" + strings.Join(cmds, "; ") + ") and fix what your change broke.\n"
			}
		} else if advice != "" || prevErr != "" {
			p += "\n\nPREVIOUS ATTEMPT FAILED WITH:\n" + clip(prevErr, 2000) + "\n\nREVIEWER ADVICE:\n" + advice
		}
		d, res := o.runAgent(ctx, t, step, st.ID, AgentMain, dir, p, attempt)
		r := stepResult{ok: res.OK(), final: res.Final, route: d.Label(), files: res.Files, tokens: res.Tokens}
		if res.Err != nil {
			r.err = res.Err.Error()
		}
		if r.ok || res.Killed || ctx.Err() != nil {
			return r
		}
		if res.LimitHit {
			limitRetries++
			if limitRetries > 2 || (o.opts.Tracker.Limited(event.Codex) && o.opts.Tracker.Limited(event.Claude)) {
				r.err = "both providers are at their usage limit"
				return r
			}
			continue // rerouted by rule 1 on the next attempt
		}
		failures++
		if failures >= oc.MaxAttempts {
			return r
		}
		sig := errorSignature(r.err)
		if sig == lastSig {
			step.RepeatError = true
			step.Escalations++
			o.logf("%s: same error twice -> escalating", st.ID)
			if oc.ReviewOnRepeatError {
				if v, ok := o.review(ctx, t, "error:"+st.ID, errorReviewPrompt(t.text, st, r.err, failures)); ok {
					advice = strings.TrimSpace(v.Advice + "\n" + strings.Join(v.Issues, "\n"))
				}
			}
		}
		lastSig = sig
		prevErr = r.err
	}
}

// runAgent routes a step, runs one agent and records everything.
func (o *Orchestrator) runAgent(ctx context.Context, t *task, step router.Step, agentID, parent, dir, prompt string, attempt int) (event.Decision, runner.Result) {
	if err := o.waitUnpaused(ctx); err != nil {
		return event.Decision{}, runner.Result{Err: err, Killed: true}
	}
	if ctx.Err() != nil {
		return event.Decision{}, runner.Result{Err: ctx.Err(), Killed: true}
	}
	if !o.checkBudget(ctx, t, fmt.Sprintf("start %s (%s)", agentID, step.Title)) {
		return event.Decision{}, runner.Result{Err: errBudget, Killed: true}
	}
	if step.Kind != router.KindJudge {
		// The judge runs inside its parent's slot; holding it would only
		// make the parent wait for itself.
		o.waitForRoom(ctx, t, agentID)
		defer o.releaseRoom()
	}
	if ctx.Err() != nil {
		return event.Decision{}, runner.Result{Err: ctx.Err(), Killed: true}
	}
	d := o.router.Route(step)
	if o.router.NeedsJudge(d) && step.Kind != router.KindJudge {
		jstep := router.Step{ID: step.ID + "-judge", Title: "judge " + step.Title, Kind: router.KindJudge}
		_, jres := o.runAgent(ctx, t, jstep, AgentJudge, AgentMain, dir, router.JudgePrompt(step), 1)
		if role, ok := router.ParseJudge(jres.Final); ok {
			step.ForceRole = role
			d = o.router.Route(step)
			d.Judged = true
		}
	}
	if o.opts.Tracker.Limited(d.Provider) {
		err := fmt.Errorf("%s is at its usage limit and %s cannot take this role", d.Provider, event.Other(d.Provider))
		o.emit(event.Event{Kind: event.Error, AgentID: agentID, Text: err.Error()})
		return d, runner.Result{Err: err, LimitHit: true}
	}
	rn, ok := t.runners[d.Provider]
	if !ok {
		return d, runner.Result{Err: errors.New("no runner for " + d.Provider)}
	}

	dc := d
	o.emit(event.Event{Kind: event.Route, AgentID: agentID, ParentID: parent, Provider: d.Provider, Model: d.Model, Role: d.Role, Decision: &dc})
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeDecision, TaskID: t.id, Agent: agentID, Step: step.ID, Attempt: attempt,
		Role: d.Role, Provider: d.Provider, Model: d.Model, Effort: d.Effort, Rule: d.Rule, Reason: d.Reason,
		Confidence: d.Confidence, Fallback: d.Fallback, Judged: d.Judged})
	title := step.Title
	if attempt > 1 {
		title = fmt.Sprintf("%s (attempt %d)", step.Title, attempt)
	}
	o.emit(event.Event{Kind: event.AgentQueued, AgentID: agentID, ParentID: parent, Provider: d.Provider, Model: d.Model, Role: d.Role, Text: title})

	actx, cancel := context.WithCancel(ctx)
	o.mu.Lock()
	o.runSeq++
	run := o.runSeq
	if o.cancels[agentID] == nil {
		o.cancels[agentID] = map[int]context.CancelFunc{}
	}
	o.cancels[agentID][run] = cancel
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.cancels[agentID], run)
		if len(o.cancels[agentID]) == 0 {
			delete(o.cancels, agentID)
		}
		o.mu.Unlock()
		cancel()
	}()

	spec := runner.Spec{
		AgentID: agentID, ParentID: parent, StepID: step.ID, Attempt: attempt, Role: d.Role,
		Provider: d.Provider, Model: d.Model, Effort: d.Effort, Prompt: prompt, Dir: dir,
		ReadOnly: step.Kind.ReadOnly(), Timeout: t.cfg.Orchestrator.AgentTimeout.D(),
	}
	if !spec.ReadOnly {
		spec.AllowedCommands = t.cfg.Verify.Commands
	}
	res := rn.Run(actx, spec, o.emit)
	res = o.deliverTold(actx, rn, spec, agentID, res)
	if res.SessionID != "" {
		o.rememberSession(agentID, AgentSession{Provider: d.Provider, Model: d.Model, Effort: d.Effort, Role: d.Role,
			SessionID: res.SessionID, Dir: dir, Final: res.Final, Title: step.Title, Task: t.text})
	}
	o.opts.Tracker.AddUsage(d.Provider, res.Tokens)
	t.addTokens(d.Provider, res.Tokens)
	if !step.Kind.ReadOnly() {
		t.noteFiles(dir, res.Files)
	}
	why := "at usage limit"
	if !res.OK() && !res.LimitHit && !res.Killed && res.Err != nil && (reAuth.MatchString(res.Err.Error()) || strings.Contains(res.Err.Error(), "not found on PATH")) {
		// A CLI that is logged out or missing is as unusable as one at its
		// limit: route around it for the rest of the session.
		res.LimitHit = true
		res.ResetAt = time.Now().Add(12 * time.Hour)
		why = "unavailable (" + clip(res.Err.Error(), 120) + "; run `sy doctor`)"
	}
	if res.LimitHit {
		until := res.ResetAt
		if until.IsZero() || until.Before(time.Now()) {
			until = time.Now().Add(t.cfg.Providers[d.Provider].LimitCooldown.D())
		}
		o.opts.Tracker.MarkLimited(d.Provider, until)
		o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeLimit, TaskID: t.id, Agent: agentID, Provider: d.Provider, Model: d.Model, Text: errText(res.Err), Until: &until})
		o.emit(event.Event{Kind: event.ProviderState, Provider: d.Provider, Until: until, Text: fmt.Sprintf("%s %s until %s; /limit %s reset to retry", d.Provider, why, until.Format("15:04"), d.Provider)})
	}
	tk := res.Tokens
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TaskID: t.id, Agent: agentID, Step: step.ID, Attempt: attempt,
		Role: d.Role, Provider: d.Provider, Model: d.Model, Effort: d.Effort, OK: sessionlog.Bool(res.OK()), LimitHit: res.LimitHit,
		Error: errText(res.Err), Tokens: &tk, DurationMS: res.Duration.Milliseconds(), Files: res.Files, Text: clip(res.Final, 500)})
	// Over budget now? Stopping cancels the task; this agent's result
	// still counts.
	o.checkBudget(ctx, t, "go on after "+agentID+" finished")
	return d, res
}

// busy reports whether the machine is too loaded to start another agent.
func (o *Orchestrator) busy(oc config.OrchestratorCfg) (bool, string) {
	s := o.opts.Load()
	if oc.MaxCPUPercent > 0 && s.CPUOK && s.CPU*100 >= float64(oc.MaxCPUPercent) {
		return true, fmt.Sprintf("CPU %.0f%% >= %d%%", s.CPU*100, oc.MaxCPUPercent)
	}
	if oc.MinFreeMemoryMB > 0 && s.MemOK && s.MemFree < uint64(oc.MinFreeMemoryMB)<<20 {
		return true, fmt.Sprintf("only %d MB RAM free < %d MB", s.MemFree>>20, oc.MinFreeMemoryMB)
	}
	return false, ""
}

// waitForRoom holds a new agent while the machine is maxed out, then
// reserves a slot for it (released with releaseRoom). The first agent always
// starts (a task must make progress), and after busy_max_wait the agent
// starts anyway, so a machine that is busy for other reasons only slows sy
// down, never stops it. Checking and reserving happen under one lock, so
// agents starting at the same moment cannot all slip through.
func (o *Orchestrator) waitForRoom(ctx context.Context, t *task, agentID string) {
	oc := t.cfg.Orchestrator
	limited := oc.MaxCPUPercent > 0 || oc.MinFreeMemoryMB > 0
	deadline := time.Now().Add(oc.BusyMaxWait.D())
	held := false
	for {
		o.mu.Lock()
		busy, why := false, ""
		if limited && o.active > 0 {
			busy, why = o.busy(oc)
		}
		late := !time.Now().Before(deadline)
		if !busy || late || ctx.Err() != nil {
			o.active++
			o.mu.Unlock()
			switch {
			case busy && late:
				o.logf("machine still busy (%s) after %s: starting %s anyway", why, oc.BusyMaxWait.D(), agentID)
			case held && !busy:
				o.logf("machine has room again: starting %s", agentID)
			}
			return
		}
		o.mu.Unlock()
		if !held {
			held = true
			o.logf("machine busy (%s): holding %s until it calms down (at most %s)", why, agentID, oc.BusyMaxWait.D())
		}
		select {
		case <-ctx.Done():
		case <-time.After(busyPoll):
		}
	}
}

func (o *Orchestrator) releaseRoom() {
	o.mu.Lock()
	o.active--
	o.mu.Unlock()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RunSingle runs the whole task with one agent on a fixed route, without
// planning or review: the single-agent baseline for `sy stats`.
func (o *Orchestrator) RunSingle(ctx context.Context, text, provider string, route config.Route) TaskResult {
	o.mu.Lock()
	o.taskSeq++
	seq := o.taskSeq
	o.mu.Unlock()
	began := time.Now()
	cfg := o.opts.Store.Get()
	proc.SetLowPriority(cfg.Orchestrator.LowPriority)
	t := &task{id: fmt.Sprintf("%stask-%d", o.opts.TaskIDPrefix, seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg)}
	t.key = o.opts.Log.Session() + "-" + t.id
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: "single"})
	o.emit(event.Event{Kind: event.TaskStart, Text: text})
	t.quotaBefore = o.quotaNow()
	o.snapshotBefore(t)
	d := event.Decision{StepID: "single", StepTitle: "single agent", Role: event.RoleWorker, Provider: provider, Model: route.Model, Effort: route.Effort,
		Rule: router.RuleForced, Reason: "single-agent baseline", Confidence: 1}
	o.emit(event.Event{Kind: event.Route, AgentID: AgentMain, Provider: provider, Model: route.Model, Role: event.RoleWorker, Decision: &d})
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeDecision, TaskID: t.id, Agent: AgentMain, Step: "single", Role: d.Role,
		Provider: provider, Model: route.Model, Effort: route.Effort, Rule: d.Rule, Reason: d.Reason, Confidence: 1})
	o.emit(event.Event{Kind: event.AgentQueued, AgentID: AgentMain, Provider: provider, Model: route.Model, Role: event.RoleWorker, Text: "single agent"})
	rn := t.runners[provider]
	spec := runner.Spec{AgentID: AgentMain, StepID: "single", Attempt: 1, Role: event.RoleWorker, Provider: provider,
		Model: route.Model, Effort: route.Effort, Prompt: text, Dir: o.opts.Dir, Timeout: cfg.Orchestrator.AgentTimeout.D()}
	res := rn.Run(ctx, spec, o.emit)
	o.opts.Tracker.AddUsage(provider, res.Tokens)
	t.addTokens(provider, res.Tokens)
	o.snapshotAfter(t)
	tk := res.Tokens
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TaskID: t.id, Agent: AgentMain, Step: "single", Attempt: 1,
		Role: event.RoleWorker, Provider: provider, Model: route.Model, Effort: route.Effort, OK: sessionlog.Bool(res.OK()),
		LimitHit: res.LimitHit, Error: errText(res.Err), Tokens: &tk, DurationMS: res.Duration.Milliseconds(), Files: res.Files})
	out := TaskResult{OK: res.OK(), Duration: time.Since(began), Tokens: tk, Summary: clip(res.Final, 300), Cost: o.cost(t)}
	if t.useGit {
		out.UndoKey = t.key
	}
	if res.Err != nil {
		out.Summary = res.Err.Error()
	}
	cost := out.Cost
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: "single",
		OK: sessionlog.Bool(out.OK), Text: out.Summary, Tokens: &tk, DurationMS: out.Duration.Milliseconds(), Cost: &cost, Bench: o.opts.Bench})
	o.emit(event.Event{Kind: event.TaskDone, OK: out.OK, Text: out.Summary, Tokens: tk, Cost: &cost})
	return out
}
