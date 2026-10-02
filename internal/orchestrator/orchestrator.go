// Package orchestrator owns the task lifecycle: plan, review the plan, fan
// out to parallel agents (in git worktrees when they write), merge, review
// before done and apply one round of fixes.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
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
}

// Orchestrator runs tasks. One task runs at a time.
type Orchestrator struct {
	opts   Options
	router *router.Router

	mu      sync.Mutex
	paused  bool
	pauseCh chan struct{} // closed when unpaused
	cancels map[string]context.CancelFunc
	running bool
	taskSeq int
}

// New creates an orchestrator.
func New(o Options) *Orchestrator {
	if o.Mode == "" {
		o.Mode = "routed"
	}
	if o.Tracker == nil {
		o.Tracker = limits.NewTracker()
	}
	orc := &Orchestrator{opts: o, cancels: map[string]context.CancelFunc{}, pauseCh: make(chan struct{})}
	close(orc.pauseCh)
	orc.router = &router.Router{Cfg: o.Store.Get, State: o.Tracker, ForceProvider: o.ForceProvider}
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

// Kill stops one running agent. It returns false if no such agent runs.
func (o *Orchestrator) Kill(agentID string) bool {
	o.mu.Lock()
	cancel, ok := o.cancels[agentID]
	o.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// RunningAgents lists the ids of agents currently running.
func (o *Orchestrator) RunningAgents() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.cancels))
	for id := range o.cancels {
		out = append(out, id)
	}
	return out
}

// emit sends an event to the UI and records the interesting ones.
func (o *Orchestrator) emit(e event.Event) {
	e = e.Stamp()
	switch e.Kind {
	case event.Quota:
		if e.Quota != nil {
			o.opts.Tracker.SetQuota(e.Provider, *e.Quota)
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
	wtBase   string
}

func (t *task) addTokens(u event.TokenUsage) {
	t.tokensMu.Lock()
	t.tokens = t.tokens.Add(u)
	t.tokensMu.Unlock()
}

// Run executes a task end to end.
func (o *Orchestrator) Run(ctx context.Context, text string) TaskResult {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return TaskResult{Summary: "a task is already running"}
	}
	o.running = true
	o.taskSeq++
	seq := o.taskSeq
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.running = false
		o.mu.Unlock()
	}()

	began := time.Now()
	cfg := o.opts.Store.Get()
	t := &task{id: fmt.Sprintf("task-%d", seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg)}
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: o.opts.Mode})
	o.emit(event.Event{Kind: event.TaskStart, Text: text})

	res := o.run(ctx, t)
	res.Duration = time.Since(began)
	res.Tokens = t.tokens
	res.Kept = t.kept
	if ctx.Err() != nil {
		res.OK = false
		res.Summary = "cancelled: " + res.Summary
	}
	tk := t.tokens
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: o.opts.Mode,
		OK: sessionlog.Bool(res.OK), Text: res.Summary, Tokens: &tk, DurationMS: res.Duration.Milliseconds()})
	o.emit(event.Event{Kind: event.Phase, Text: "done"})
	o.emit(event.Event{Kind: event.TaskDone, OK: res.OK, Text: res.Summary, Tokens: tk})
	return res
}

func (o *Orchestrator) run(ctx context.Context, t *task) TaskResult {
	cfg := t.cfg
	oc := cfg.Orchestrator

	// Git setup.
	if !o.opts.NoGit && isRepo(o.opts.Dir) {
		if root, err := repoRoot(o.opts.Dir); err == nil {
			t.root = root
			t.useGit = true
			if snap, err := (git{root}).snapshot("switchyard start snapshot"); err == nil {
				t.snapshot, t.start = snap, snap
			} else {
				o.logf("git snapshot failed, worktrees disabled: %v", err)
				t.useGit = false
			}
		}
	}

	// 1. Plan.
	o.emit(event.Event{Kind: event.Phase, Text: "plan"})
	var plan Plan
	if words := len(strings.Fields(t.text)); oc.SmallTaskWords > 0 && words < oc.SmallTaskWords {
		plan = Plan{Summary: "small task: one worker step", Subtasks: []Subtask{{ID: "work", Title: firstWords(t.text, 6), Kind: router.KindEdit, Prompt: t.text}}}
		if looksRead(t.text) {
			plan.Subtasks[0].Kind = router.KindExplore
			plan.Subtasks[0].ID = "explore"
		}
		o.logf("small task (%d words): skipping the planner", words)
		t.mainProv = o.router.Route(router.Step{ID: "plan", Kind: router.KindPlan}).Provider
	} else {
		p, ok := o.plan(ctx, t, "", nil)
		if ctx.Err() != nil {
			return TaskResult{Summary: "planning cancelled"}
		}
		if !ok {
			return TaskResult{Summary: "planning failed: " + p.Summary}
		}
		plan = p
		// 2. Review the plan.
		if oc.ReviewBeforePlan {
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

	// 4. Review before done, with fix rounds.
	approved := true
	var lastAdvice string
	if oc.ReviewBeforeDone && hasEdits(plan) {
		for round := 0; ; round++ {
			o.emit(event.Event{Kind: event.Phase, Text: "review"})
			stat, diff := "", ""
			if t.useGit {
				stat, diff = git{t.root}.diff(t.start, 40_000)
			}
			v, ok := o.review(ctx, t, "final", finalReviewPrompt(t.text, plan, results, stat, diff, t.notes))
			if ctx.Err() != nil {
				return TaskResult{Summary: "cancelled during final review"}
			}
			if !ok {
				break // reviewer unavailable: do not block
			}
			approved = v.Approve
			lastAdvice = v.Advice
			if v.Approve || round >= oc.MaxFixRounds {
				break
			}
			o.emit(event.Event{Kind: event.Phase, Text: "fix"})
			fix := Subtask{ID: fmt.Sprintf("fix-%d", round+1), Title: "Apply review fixes", Kind: router.KindFix, Prompt: fixPrompt(t.text, v)}
			r := o.runStep(ctx, t, fix, nil, o.opts.Dir, fixPrompt(t.text, v))
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
	if !approved {
		b.WriteString("; reviewer still has concerns: " + clip(lastAdvice, 200))
	} else if oc.ReviewBeforeDone && hasEdits(plan) {
		b.WriteString("; reviewer approved")
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
	d, res := o.runAgent(ctx, t, step, AgentMain, "", o.opts.Dir, planPrompt(t.text, advice, prev), 1)
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

// review runs the reviewer and reports its verdict.
func (o *Orchestrator) review(ctx context.Context, t *task, checkpoint, prompt string) (Verdict, bool) {
	step := router.Step{ID: "review-" + checkpoint, Title: checkpoint + " review", Kind: router.KindReview, MainProvider: t.mainProv}
	d, res := o.runAgent(ctx, t, step, AgentReviewer, AgentMain, o.opts.Dir, prompt, 1)
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
	useWT := t.useGit && oc.Worktrees && threads > 1 && edits > 1
	if useWT && !SupportsMergeTree() {
		o.logf("git < 2.38 (no merge-tree --write-tree): writing agents run one at a time in the main tree")
		useWT = false
	}
	if useWT {
		t.wtBase = worktreeBase(t.root, fmt.Sprintf("%s-%s", o.opts.Log.Session(), t.id))
		defer func() {
			os.RemoveAll(t.wtBase)
			os.Remove(filepath.Dir(t.wtBase)) // only succeeds when empty
		}()
	}

	for _, st := range p.Subtasks {
		o.emit(event.Event{Kind: event.AgentQueued, AgentID: st.ID, ParentID: AgentMain, Role: string(st.Kind), Text: st.Title})
	}

	results := map[string]stepResult{}
	var mu sync.Mutex
	done := map[string]bool{}
	started := map[string]bool{}
	sem := make(chan struct{}, threads)
	writeSem := make(chan struct{}, 1) // writers in the main tree run one at a time
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
		mu.Unlock()
		if ctx.Err() != nil {
			break
		}
		for _, st := range ready {
			st := st
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { wake <- struct{}{} }()
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
				for _, d := range st.DependsOn {
					if r := results[d]; r.final != "" {
						deps = append(deps, fmt.Sprintf("[%s] %s", d, clip(r.final, 3000)))
					}
				}
				mu.Unlock()
				var r stepResult
				switch {
				case st.Kind.ReadOnly():
					r = o.runStep(ctx, t, st, deps, o.opts.Dir, "")
				case useWT:
					r = o.runInWorktree(ctx, t, st, deps)
				default:
					select {
					case writeSem <- struct{}{}:
					case <-ctx.Done():
						r = stepResult{err: "cancelled"}
					}
					if r.err == "" {
						r = o.runStep(ctx, t, st, deps, o.opts.Dir, "")
						<-writeSem
					}
				}
				mu.Lock()
				results[st.ID] = r
				done[st.ID] = true
				mu.Unlock()
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
func (o *Orchestrator) runInWorktree(ctx context.Context, t *task, st Subtask, deps []string) stepResult {
	g := git{t.root}
	t.mergeMu.Lock()
	base := t.snapshot
	t.mergeMu.Unlock()
	path := filepath.Join(t.wtBase, st.ID)
	if err := g.addWorktree(path, base); err != nil {
		o.logf("worktree for %s failed (%v); running in the main tree", st.ID, err)
		return o.runStep(ctx, t, st, deps, o.opts.Dir, "")
	}
	defer g.removeWorktree(path)
	// Agents should work in the same relative directory they would use in the main tree.
	dir := path
	if rel, err := filepath.Rel(t.root, o.opts.Dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		dir = filepath.Join(path, rel)
	}
	r := o.runStep(ctx, t, st, deps, dir, "")
	if !r.ok {
		return r
	}
	wg := git{path}
	commit, changed, err := wg.commitAll("switchyard: " + st.Title)
	if err != nil {
		o.mergeEvent(t, st.ID, false, "commit failed: "+err.Error())
		r.ok, r.err = false, "commit failed: "+err.Error()
		return r
	}
	if !changed {
		o.mergeEvent(t, st.ID, true, "no file changes")
		return r
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
	if err := g.applyDiff(t.snapshot, merged); err != nil {
		branch := o.keepBranch(t, st.ID, commit)
		o.mergeEvent(t, st.ID, false, fmt.Sprintf("could not apply to working tree (%v); kept on %s", err, branch))
		t.notes = append(t.notes, fmt.Sprintf("%s could not be applied to the working tree; its changes are on branch %s", st.ID, branch))
		r.ok, r.err = false, "apply failed"
		return r
	}
	t.snapshot = merged
	o.mergeEvent(t, st.ID, true, fmt.Sprintf("merged %d file(s)", len(r.files)))
	return r
}

func (o *Orchestrator) keepBranch(t *task, stepID, commit string) string {
	branch := fmt.Sprintf("sy/%s/%s", o.opts.Log.Session(), stepID)
	if _, err := (git{t.root}).out("branch", "-f", branch, commit); err != nil {
		return commit[:min(12, len(commit))]
	}
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
	step := router.Step{ID: st.ID, Title: st.Title, Kind: st.Kind, Prompt: st.Prompt, Files: st.Files, MainProvider: t.mainProv}
	var prevErr, advice, lastSig string
	failures, limitRetries := 0, 0
	for attempt := 1; ; attempt++ {
		p := prompt
		if p == "" {
			p = stepPrompt(t.text, st, deps, prevErr, advice, st.Kind.ReadOnly())
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
		Confidence: d.Confidence, Fallback: d.Fallback})
	title := step.Title
	if attempt > 1 {
		title = fmt.Sprintf("%s (attempt %d)", step.Title, attempt)
	}
	o.emit(event.Event{Kind: event.AgentQueued, AgentID: agentID, ParentID: parent, Provider: d.Provider, Model: d.Model, Role: d.Role, Text: title})

	actx, cancel := context.WithCancel(ctx)
	o.mu.Lock()
	o.cancels[agentID] = cancel
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.cancels, agentID)
		o.mu.Unlock()
		cancel()
	}()

	spec := runner.Spec{
		AgentID: agentID, ParentID: parent, StepID: step.ID, Attempt: attempt, Role: d.Role,
		Provider: d.Provider, Model: d.Model, Effort: d.Effort, Prompt: prompt, Dir: dir,
		ReadOnly: step.Kind.ReadOnly(), Timeout: t.cfg.Orchestrator.AgentTimeout.D(),
	}
	res := rn.Run(actx, spec, o.emit)
	o.opts.Tracker.AddUsage(d.Provider, res.Tokens)
	t.addTokens(res.Tokens)
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
		o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeLimit, TaskID: t.id, Agent: agentID, Provider: d.Provider, Model: d.Model, Text: errText(res.Err)})
		o.emit(event.Event{Kind: event.ProviderState, Provider: d.Provider, Until: until, Text: fmt.Sprintf("%s %s until %s; /limit %s reset to retry", d.Provider, why, until.Format("15:04"), d.Provider)})
	}
	tk := res.Tokens
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TaskID: t.id, Agent: agentID, Step: step.ID, Attempt: attempt,
		Role: d.Role, Provider: d.Provider, Model: d.Model, Effort: d.Effort, OK: sessionlog.Bool(res.OK()), LimitHit: res.LimitHit,
		Error: errText(res.Err), Tokens: &tk, DurationMS: res.Duration.Milliseconds(), Files: res.Files, Text: clip(res.Final, 500)})
	return d, res
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
	t := &task{id: fmt.Sprintf("task-%d", seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg)}
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: "single"})
	o.emit(event.Event{Kind: event.TaskStart, Text: text})
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
	tk := res.Tokens
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TaskID: t.id, Agent: AgentMain, Step: "single", Attempt: 1,
		Role: event.RoleWorker, Provider: provider, Model: route.Model, Effort: route.Effort, OK: sessionlog.Bool(res.OK()),
		LimitHit: res.LimitHit, Error: errText(res.Err), Tokens: &tk, DurationMS: res.Duration.Milliseconds(), Files: res.Files})
	out := TaskResult{OK: res.OK(), Duration: time.Since(began), Tokens: tk, Summary: clip(res.Final, 300)}
	if res.Err != nil {
		out.Summary = res.Err.Error()
	}
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: "single",
		OK: sessionlog.Bool(out.OK), Text: out.Summary, Tokens: &tk, DurationMS: out.Duration.Milliseconds()})
	o.emit(event.Event{Kind: event.TaskDone, OK: out.OK, Text: out.Summary, Tokens: tk})
	return out
}
