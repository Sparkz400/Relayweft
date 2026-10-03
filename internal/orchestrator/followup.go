package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

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

func (o *Orchestrator) rememberSession(agentID string, s AgentSession) {
	s.AgentID, s.Ended = agentID, time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sessions == nil {
		o.sessions = map[string]AgentSession{}
	}
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
		return TaskResult{Summary: fmt.Sprintf("no finished agent %q to follow up (agents: %s)", agentID, strings.Join(o.sessionIDs(), ", "))}
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
	defer func() {
		o.mu.Lock()
		o.running = false
		o.mu.Unlock()
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
	o.emit(event.Event{Kind: event.Phase, Text: "done"})
	o.emit(event.Event{Kind: event.TaskDone, OK: out.OK, Text: out.Summary, Tokens: tk, Cost: &cost})
	return out
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
