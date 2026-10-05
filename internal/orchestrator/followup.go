package orchestrator

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/proc"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// AgentSession is a finished agent whose CLI conversation can be continued
// with a follow-up message.
type AgentSession struct {
	AgentID   string
	Provider  string
	Model     string
	Effort    string
	Role      string
	SessionID string // the CLI's own session/thread id
	Dir       string // where it ran
	// Slot is the pool worktree Dir is in ("" = the main tree): a
	// follow-up resumes the session there (followUpSlot).
	Slot string `json:",omitempty"`
	// Unlanded names the branch with what its last turn changed in Slot
	// when that turn was stopped before its changes landed: they are not
	// in the worktree any more, and the next follow-up tells the agent.
	Unlanded string `json:",omitempty"`
	Final    string // its last answer
	Title    string // the step it worked on
	Task     string // the task it was part of
	Ended    time.Time
}

// maxSessions bounds the remembered agents (oldest are forgotten).
const maxSessions = 50

// sessionsPath keeps a project's follow-up targets across rw restarts.
func sessionsPath(dir string) string {
	h := sha1.Sum([]byte(canonPath(dir)))
	return filepath.Join(stateDir(), "sessions", hex.EncodeToString(h[:6])+".json")
}

// persistSessions reports whether sessions outlive this process (not for
// demo or bench runs).
func (o *Orchestrator) persistSessions() bool {
	return o.opts.Mode != "demo" && o.opts.Bench == "" && o.opts.Dir != ""
}

// loadSessions reads the saved sessions once (o.mu held).
func (o *Orchestrator) loadSessions() {
	if o.sessions != nil {
		return
	}
	o.sessions = map[string]AgentSession{}
	if !o.persistSessions() {
		return
	}
	if data, err := os.ReadFile(sessionsPath(o.opts.Dir)); err == nil {
		_ = json.Unmarshal(data, &o.sessions) // unreadable: no sessions to resume, start afresh
	}
}

// saveSessions writes the sessions (o.mu held).
func (o *Orchestrator) saveSessions() {
	if !o.persistSessions() {
		return
	}
	p := sessionsPath(o.opts.Dir)
	_ = os.MkdirAll(filepath.Dir(p), 0o755) // WriteFile below fails then
	data, err := json.MarshalIndent(o.sessions, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

func (o *Orchestrator) rememberSession(agentID string, s AgentSession) {
	s.AgentID, s.Ended = agentID, time.Now()
	s.Final = clip(s.Final, 6000)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loadSessions()
	defer o.saveSessions()
	o.sessions[agentID] = s
	if len(o.sessions) > maxSessions {
		oldest := ""
		for id, v := range o.sessions {
			if oldest == "" || v.Ended.Before(o.sessions[oldest].Ended) {
				oldest = id
			}
		}
		delete(o.sessions, oldest)
	}
}

// Sessions lists the agents that can take a follow-up, newest first.
func (o *Orchestrator) Sessions() []AgentSession {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loadSessions()
	out := make([]AgentSession, 0, len(o.sessions))
	for _, s := range o.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ended.After(out[j].Ended) })
	return out
}

// Session returns the remembered session of an agent. "" or "last" picks
// the newest agent that worked on a step (not a reviewer or the judge),
// else the newest one.
func (o *Orchestrator) Session(agentID string) (AgentSession, bool) {
	if agentID == "" || agentID == "last" {
		all := o.Sessions()
		for _, s := range all {
			if s.Role != event.RoleReviewer && s.Role != event.RoleJudge && s.Role != event.RolePlanner {
				return s, true
			}
		}
		if len(all) == 0 {
			return AgentSession{}, false
		}
		return all[0], true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loadSessions()
	s, ok := o.sessions[agentID]
	return s, ok
}

// followUpContext is the prompt for a fresh agent when the old CLI session
// cannot be resumed.
func followUpContext(s AgentSession, text string) string {
	var b strings.Builder
	b.WriteString("You are continuing work another agent did in this repository.\n\n")
	fmt.Fprintf(&b, "THE ORIGINAL TASK:\n%s\n\n", s.Task)
	if s.Title != "" {
		fmt.Fprintf(&b, "THAT AGENT'S STEP: %s\n\n", s.Title)
	}
	if s.Final != "" {
		fmt.Fprintf(&b, "ITS FINAL ANSWER:\n%s\n\n", clip(s.Final, 6000))
	}
	b.WriteString("Its changes are already in the working tree.\n\nTHE USER'S FOLLOW-UP:\n")
	b.WriteString(text)
	return b.String()
}

// FollowUp sends a message to a finished agent: its CLI conversation is
// resumed so it keeps its context. When the conversation cannot be resumed
// (the CLI forgot it, or it ran in a worktree that is gone) a fresh agent
// on the same route gets the earlier task and answer as context instead.
// It runs in the main working tree, like a one-step task, or in the pool
// worktree the agent ran in (followUpSlot).
func (o *Orchestrator) FollowUp(ctx context.Context, agentID, text string) TaskResult {
	s, ok := o.Session(agentID)
	if !ok {
		return o.refuse(fmt.Sprintf("no finished agent %q to follow up (agents: %s)", agentID, strings.Join(o.sessionIDs(), ", ")))
	}
	return o.FollowUpSession(ctx, s, text)
}

// refuse ends a follow-up that cannot start like a task, so a UI waiting
// for TaskDone is never left hanging.
func (o *Orchestrator) refuse(why string) TaskResult {
	o.emit(event.Event{Kind: event.TaskStart, Text: why})
	o.emit(event.Event{Kind: event.Phase, Text: "done"})
	o.emit(event.Event{Kind: event.TaskDone, Text: why})
	return TaskResult{Summary: why}
}

// FollowUpSession is FollowUp for a session looked up earlier (a queued
// follow-up keeps the agent it was typed for, even if a later task's agent
// with the same id finished since).
func (o *Orchestrator) FollowUpSession(ctx context.Context, s AgentSession, text string) (result TaskResult) {
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
	defer func() {
		r := recover()
		o.mu.Lock()
		o.running = false
		if !finished {
			o.cur = nil
		}
		o.mu.Unlock()
		if r != nil {
			path := diag.Crash("follow-up", r, debug.Stack())
			result = TaskResult{Summary: "internal error, Relayweft bug: details in " + path + " (rw bugreport)"}
			if !finished {
				o.emit(event.Event{Kind: event.Phase, Text: "done"})
				o.emit(event.Event{Kind: event.TaskDone, Text: result.Summary})
			}
		}
	}()

	began := time.Now()
	cfg := o.opts.Store.Get()
	proc.SetLowPriority(cfg.Orchestrator.LowPriority)
	setPoolLimits(cfg)
	t := &task{id: fmt.Sprintf("%stask-%d", o.opts.TaskIDPrefix, seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg)}
	t.key = o.opts.Log.Session() + "-" + t.id
	label := "follow-up to " + s.AgentID
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: "followup"})
	o.emit(event.Event{Kind: event.TaskStart, Text: label + ": " + text})
	t.quotaBefore = o.quotaNow()
	o.snapshotBefore(t)
	// The same budget as a task: checked before each agent run.
	bctx := o.startBudget(ctx, t)

	rn, have := t.runners[s.Provider]
	var res runner.Result
	if !have {
		res.Err = fmt.Errorf("no runner for %s", s.Provider)
	} else if o.opts.Tracker.Limited(s.Provider) {
		res.Err = fmt.Errorf("%s is at its usage limit; try again after it resets", s.Provider)
		res.LimitHit = true
	} else {
		d := event.Decision{StepID: "followup", StepTitle: label, Role: s.Role, Provider: s.Provider, Model: s.Model, Effort: s.Effort,
			Rule: router.RuleForced, Reason: label, Confidence: 1}
		o.emit(event.Event{Kind: event.Route, AgentID: s.AgentID, Provider: s.Provider, Model: s.Model, Role: s.Role, Decision: &d})
		o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeDecision, TaskID: t.id, Agent: s.AgentID, Step: "followup", Role: s.Role,
			Provider: s.Provider, Model: s.Model, Effort: s.Effort, Rule: d.Rule, Reason: d.Reason, Confidence: 1})
		spec := runner.Spec{AgentID: s.AgentID, StepID: "followup", Attempt: 1, Role: s.Role, Provider: s.Provider,
			Model: s.Model, Effort: s.Effort, Prompt: text, Dir: o.opts.Dir, Timeout: cfg.Orchestrator.AgentTimeout.D(),
			AllowedCommands: verifyAllowed(cfg.Verify, o.opts.Dir)}
		resumable := s.SessionID != ""
		// An agent that ran in a pool worktree is resumed there, whatever
		// its CLI: its history names the worktree's paths, so resumed
		// anywhere else (Codex finds its sessions anywhere) it would work
		// in the wrong folder. Claude Code, Gemini CLI and Qwen Code keep
		// their sessions per folder, so for them no other folder works at
		// all. The result lands like a step's.
		var sl *slot
		var loc stepLoc
		// Only this repo's pool: an agent that ran in another repo's pool
		// (a multi-repo task) cannot land here, and Codex resumes it by id
		// as before.
		inPool := slotOf(t.root, s.Dir) != "" || (s.Slot != "" && slotOf(t.root, s.Slot) != "")
		if resumable && !samePath(s.Dir, o.opts.Dir) && (inPool || cfg.SessionPerDir(s.Provider)) {
			var why string
			if sl, loc, why = o.followUpSlot(t, s); sl == nil {
				o.logf("%s's session cannot be resumed in %s (%s); starting a fresh agent with its context", s.AgentID, s.Dir, why)
				resumable = false
			} else {
				defer sl.release()
				o.slotNotes(t, sl)
				spec.Dir = loc.dir
			}
		}
		run := func(title string) runner.Result {
			if !o.checkBudget(bctx, t, "start "+title) {
				return runner.Result{Err: errBudget, Killed: true}
			}
			o.emit(event.Event{Kind: event.AgentQueued, AgentID: s.AgentID, Provider: s.Provider, Model: s.Model, Role: s.Role, Text: title})
			r := rn.Run(bctx, spec, o.emit)
			o.opts.Tracker.AddUsage(s.Provider, r.Tokens)
			t.addTokens(s.Provider, r.Tokens)
			o.noteBudget(t, s.AgentID)
			return r
		}
		msg := text
		if s.Unlanded != "" {
			// Its last turn was stopped before its changes landed.
			msg = unlandedNote(s.Unlanded) + text
		}
		if resumable {
			spec.Resume = s.SessionID
			spec.Prompt = msg
			res = run(label)
		}
		if !resumable || (!res.OK() && !res.Killed && !res.LimitHit && ctx.Err() == nil) {
			if resumable {
				o.logf("could not resume %s's session (%s); starting a fresh agent with its context", s.AgentID, clip(errText(res.Err), 120))
			}
			// In a pool worktree the fresh agent works there too: what the
			// failed resume changed is kept, and lands with its work.
			spec.Resume = ""
			spec.Prompt = followUpContext(s, msg)
			res = run(label + " (fresh)")
		}
		unlanded := ""
		if sl != nil && res.OK() {
			r := o.landSlot(ctx, t, t, Subtask{ID: "followup", Title: label}, nil, loc, stepResult{ok: true, final: res.Final, files: res.Files}, false)
			if !r.ok {
				res.Err = errors.New(r.err)
			}
		} else if sl != nil {
			// Stopped (cancelled, a limit, the budget, a timeout) or
			// failed: what it changed in the worktree did not land, and
			// the worktree's next use resets it.
			unlanded = o.keepFollowUpWork(t, s, loc, res, ctx.Err() != nil)
		}
		if res.SessionID != "" {
			o.rememberSession(s.AgentID, AgentSession{Provider: s.Provider, Model: s.Model, Effort: s.Effort, Role: s.Role,
				SessionID: res.SessionID, Dir: spec.Dir, Slot: loc.slot, Final: res.Final, Title: label, Task: s.Task + "\n\nFollow-up: " + text,
				Unlanded: unlanded})
		} else if unlanded != "" {
			s.Unlanded = unlanded
			o.rememberSession(s.AgentID, s)
		}
		tk := res.Tokens
		o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TaskID: t.id, Agent: s.AgentID, Step: "followup", Attempt: 1,
			Role: s.Role, Provider: s.Provider, Model: s.Model, Effort: s.Effort, OK: sessionlog.Bool(res.OK()),
			LimitHit: res.LimitHit, Error: errText(res.Err), Tokens: &tk, DurationMS: res.Duration.Milliseconds(), Files: res.Files})
	}
	o.snapshotAfter(t)
	out := TaskResult{OK: res.OK(), Duration: time.Since(began), Tokens: t.tokens, Summary: clip(res.Final, 300), Cost: o.cost(t)}
	if t.useGit {
		out.UndoKey = t.key
	}
	if res.Err != nil {
		out.Summary = res.Err.Error()
	}
	if why := t.budgetStopped(); why != "" && ctx.Err() == nil {
		out.OK = false
		out.Summary = "stopped by budget: " + why
	}
	if ctx.Err() != nil {
		out.OK = false
		out.Summary = "cancelled: " + out.Summary
	}
	tk, cost := out.Tokens, out.Cost
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: "followup",
		OK: sessionlog.Bool(out.OK), Text: out.Summary, Tokens: &tk, DurationMS: out.Duration.Milliseconds(), Cost: &cost})
	o.endBudget(t, cost)
	o.mu.Lock()
	o.running = false
	o.mu.Unlock()
	finished = true
	o.emit(event.Event{Kind: event.Phase, Text: "done"})
	o.emit(event.Event{Kind: event.TaskDone, OK: out.OK, Text: out.Summary, Tokens: tk, Cost: &cost})
	return out
}

// followUpSlot prepares the pool worktree an agent ran in for a follow-up
// that resumes its session there. Claude Code keeps its sessions per
// folder, and resuming any CLI (Codex too) from the main tree would leave
// the agent's earlier absolute paths pointing into the pool worktree. The
// slot is locked and
// moved to the main tree's current state (t.snapshot, taken by the
// follow-up), so the agent sees what the person sees; its work is then
// merged into the main tree like a step's (landSlot). With a nil slot, why
// says why it cannot be used: no git, the agent ran in no pool worktree of
// this repo, or the slot is in use.
func (o *Orchestrator) followUpSlot(t *task, s AgentSession) (sl *slot, loc stepLoc, why string) {
	if !t.useGit || t.snapshot == "" {
		return nil, loc, "no git snapshot of this folder"
	}
	if !SupportsMergeTree() {
		return nil, loc, "git < 2.38"
	}
	path := s.Slot
	if path == "" {
		path = slotOf(t.root, s.Dir) // remembered before sessions named their slot
	}
	if path == "" || !within(s.Dir, path) {
		return nil, loc, "it ran in no pool worktree of this repo"
	}
	sl, err := claimSlot(t.root, path, t.snapshot, nil)
	if err != nil {
		return nil, loc, err.Error()
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		sl.release()
		return nil, loc, err.Error()
	}
	return sl, stepLoc{dir: s.Dir, slot: path, base: t.snapshot}, ""
}

// keepFollowUpWork keeps what a follow-up that did not finish changed in
// its pool worktree (loc): on a branch, like a step's half-done edits, and
// says where. If it cannot be saved, the worktree is held instead, and the
// next rw that looks at it saves it (the hold names no task state, so it
// is given up only after saving). It returns the branch, or "".
func (o *Orchestrator) keepFollowUpWork(t *task, s AgentSession, loc stepLoc, res runner.Result, cancelled bool) string {
	why := errText(res.Err)
	switch {
	case cancelled:
		why = "cancelled"
	case res.LimitHit:
		why = "usage limit"
	case why == "":
		why = "failed"
	}
	saved, err := saveSlotWork(loc.slot, loc.base, "rw/"+refPart(t.key)+"/followup-unfinished", "unfinished follow-up to "+s.AgentID)
	if err != nil {
		holdSlot(loc.slot, slotHold{Task: t.key, Step: "followup", Base: loc.base})
		msg := fmt.Sprintf("the follow-up to %s stopped (%s); what it changed in %s could not be saved on a branch (%v), so that worktree is kept as it is: copy what you need from it", s.AgentID, clip(why, 120), loc.slot, err)
		o.emit(event.Event{Kind: event.Error, AgentID: s.AgentID, Text: msg})
		o.opts.Log.Write(sessionlog.Record{Type: "pool", TaskID: t.id, Text: msg})
		return ""
	}
	if saved == nil {
		return "" // it changed nothing
	}
	saved.Why = "the follow-up to " + s.AgentID + " stopped (" + clip(why, 120) + ")"
	o.logf("%s", saved.Hint())
	o.opts.Log.Write(sessionlog.Record{Type: "pool", TaskID: t.id, Text: saved.Hint()})
	return saved.Branch
}

// unlandedNote tells a resumed agent that its last turn's changes are not
// in its folder.
func unlandedNote(branch string) string {
	return "NOTE: your previous turn was stopped before its changes were applied. They are NOT in this folder any more (they are kept on git branch " +
		branch + "). Check the current state of the files before you continue.\n\n"
}

// Tell queues a message for a running agent. It is delivered when the
// agent's current turn ends: its CLI session is resumed with the message
// before its work is merged or reviewed, so it can still change course.
func (o *Orchestrator) Tell(agentID, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.cancels[agentID]) == 0 {
		return fmt.Errorf("%s is not running", agentID)
	}
	if o.told == nil {
		o.told = map[string][]string{}
	}
	o.told[agentID] = append(o.told[agentID], text)
	return nil
}

func (o *Orchestrator) takeTold(agentID string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	msgs := o.told[agentID]
	delete(o.told, agentID)
	return msgs
}

// deliverTold resumes a finished turn with the messages told to the agent
// meanwhile (repeatedly, while more arrive).
func (o *Orchestrator) deliverTold(ctx context.Context, rn runner.Runner, spec runner.Spec, agentID string, res runner.Result) runner.Result {
	for {
		msgs := o.takeTold(agentID)
		if len(msgs) == 0 || ctx.Err() != nil {
			return res
		}
		if !res.OK() || res.SessionID == "" {
			o.emit(event.Event{Kind: event.Error, AgentID: agentID, Text: "your message could not be delivered (the agent ended without a resumable session): " + clip(strings.Join(msgs, " / "), 200)})
			return res
		}
		o.emit(event.Event{Kind: event.Log, AgentID: agentID, Text: "delivering your message: " + clip(strings.Join(msgs, " / "), 200)})
		next := spec
		next.Resume = res.SessionID
		next.Prompt = "The user sent you this while you were working. Take it into account, adjust your work if needed, then reply with an updated short summary:\n\n" + strings.Join(msgs, "\n\n")
		nr := rn.Run(ctx, next, o.emit)
		nr.Tokens = addUsage(res.Tokens, nr.Tokens)
		nr.Files = append(res.Files, nr.Files...)
		nr.Duration += res.Duration
		if nr.SessionID == "" {
			nr.SessionID = res.SessionID
		}
		res = nr
	}
}

func addUsage(a, b event.TokenUsage) event.TokenUsage {
	a.Input += b.Input
	a.Output += b.Output
	a.Reasoning += b.Reasoning
	a.Cached += b.Cached
	a.CostUSD += b.CostUSD
	return a
}

func (o *Orchestrator) sessionIDs() []string {
	var ids []string
	for _, s := range o.Sessions() {
		ids = append(ids, s.AgentID)
	}
	if len(ids) == 0 {
		return []string{"none yet"}
	}
	return ids
}
