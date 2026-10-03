package orchestrator

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sessionlog"
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
	Final     string // its last answer
	Title     string // the step it worked on
	Task      string // the task it was part of
	Ended     time.Time
}

// maxSessions bounds the remembered agents (oldest are forgotten).
const maxSessions = 50

// sessionsPath keeps a project's follow-up targets across sy restarts.
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
		json.Unmarshal(data, &o.sessions)
	}
}

// saveSessions writes the sessions (o.mu held).
func (o *Orchestrator) saveSessions() {
	if !o.persistSessions() {
		return
	}
	p := sessionsPath(o.opts.Dir)
	os.MkdirAll(filepath.Dir(p), 0o755)
	data, err := json.MarshalIndent(o.sessions, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, p)
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
// It runs in the main working tree, like a one-step task.
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
		o.mu.Unlock()
		if r != nil {
			path := diag.Crash("follow-up", r, debug.Stack())
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
	t := &task{id: fmt.Sprintf("%stask-%d", o.opts.TaskIDPrefix, seq), text: text, cfg: cfg, runners: o.opts.Runners(cfg)}
	t.key = o.opts.Log.Session() + "-" + t.id
	label := "follow-up to " + s.AgentID
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTask, TaskID: t.id, Task: text, Mode: "followup"})
	o.emit(event.Event{Kind: event.TaskStart, Text: label + ": " + text})
	t.quotaBefore = o.quotaNow()
	o.snapshotBefore(t)

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
			AllowedCommands: cfg.Verify.Commands}
		// Claude keys its sessions by working directory: one that ran in a
		// pool worktree cannot be resumed from the main tree.
		resumable := s.SessionID != "" && (s.Provider != event.Claude || canonPath(s.Dir) == canonPath(o.opts.Dir))
		if resumable {
			spec.Resume = s.SessionID
			o.emit(event.Event{Kind: event.AgentQueued, AgentID: s.AgentID, Provider: s.Provider, Model: s.Model, Role: s.Role, Text: label})
			res = rn.Run(ctx, spec, o.emit)
			o.opts.Tracker.AddUsage(s.Provider, res.Tokens)
			t.addTokens(s.Provider, res.Tokens)
		}
		if !resumable || (!res.OK() && !res.Killed && !res.LimitHit && ctx.Err() == nil) {
			if resumable {
				o.logf("could not resume %s's session (%s); starting a fresh agent with its context", s.AgentID, clip(errText(res.Err), 120))
			}
			spec.Resume = ""
			spec.Prompt = followUpContext(s, text)
			o.emit(event.Event{Kind: event.AgentQueued, AgentID: s.AgentID, Provider: s.Provider, Model: s.Model, Role: s.Role, Text: label + " (fresh)"})
			res = rn.Run(ctx, spec, o.emit)
			o.opts.Tracker.AddUsage(s.Provider, res.Tokens)
			t.addTokens(s.Provider, res.Tokens)
		}
		if res.SessionID != "" {
			o.rememberSession(s.AgentID, AgentSession{Provider: s.Provider, Model: s.Model, Effort: s.Effort, Role: s.Role,
				SessionID: res.SessionID, Dir: o.opts.Dir, Final: res.Final, Title: label, Task: s.Task + "\n\nFollow-up: " + text})
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
	if ctx.Err() != nil {
		out.OK = false
		out.Summary = "cancelled: " + out.Summary
	}
	tk, cost := out.Tokens, out.Cost
	o.opts.Log.Write(sessionlog.Record{Type: sessionlog.TypeTaskEnd, TaskID: t.id, Task: text, Mode: "followup",
		OK: sessionlog.Bool(out.OK), Text: out.Summary, Tokens: &tk, DurationMS: out.Duration.Milliseconds(), Cost: &cost})
	o.mu.Lock()
	o.running = false
	o.mu.Unlock()
	finished = true
	o.emit(event.Event{Kind: event.Phase, Text: "done"})
	o.emit(event.Event{Kind: event.TaskDone, OK: out.OK, Text: out.Summary, Tokens: tk, Cost: &cost})
	return out
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
