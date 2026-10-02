// Package event defines the normalized event stream that every runner
// adapter produces and that the TUI, session log and headless printer
// consume. Nothing outside the runner package ever sees raw CLI output.
package event

import (
	"fmt"
	"time"
)

// Providers.
const (
	Codex  = "codex"
	Claude = "claude"
)

// Providers lists every supported provider in display order.
var Providers = []string{Codex, Claude}

// Other returns the opposite provider.
func Other(p string) string {
	if p == Codex {
		return Claude
	}
	return Codex
}

// Roles.
const (
	RolePlanner    = "planner"
	RoleWorker     = "worker"
	RoleWorkerHigh = "worker_high"
	RoleExplorer   = "explorer"
	RoleResearcher = "researcher"
	RoleReviewer   = "reviewer"
	RoleJudge      = "judge"
)

// Roles lists every role in display order.
var Roles = []string{RolePlanner, RoleWorker, RoleWorkerHigh, RoleExplorer, RoleResearcher, RoleReviewer, RoleJudge}

// Kind is the type of an event.
type Kind int

const (
	// Agent-level events, produced by runners.
	Started Kind = iota
	Thinking
	ToolCall
	FileEdit
	Message
	Usage
	Quota // provider-reported quota utilization (Claude rate_limit_event)
	Error
	LimitHit
	Done

	// Orchestration-level events, produced by the orchestrator.
	AgentQueued // a node was added to the tree
	Route       // the router made a decision
	Checkpoint  // the reviewer finished a checkpoint
	Phase       // the task moved to a new phase
	Merge       // a worktree was merged back (or failed to)
	Log         // free-form log line
	TaskStart
	TaskDone
	ProviderState // a provider was marked limited / available
)

var kindNames = map[Kind]string{
	Started: "started", Thinking: "thinking", ToolCall: "tool", FileEdit: "edit",
	Message: "message", Usage: "usage", Quota: "quota", Error: "error",
	LimitHit: "limit", Done: "done", AgentQueued: "queued", Route: "route",
	Checkpoint: "checkpoint", Phase: "phase", Merge: "merge", Log: "log",
	TaskStart: "task_start", TaskDone: "task_done", ProviderState: "provider",
}

func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// MarshalText makes kinds readable in JSON.
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// TokenUsage is a token count as reported by a CLI.
type TokenUsage struct {
	Input     int64   `json:"input,omitempty"`
	Cached    int64   `json:"cached,omitempty"`
	Output    int64   `json:"output,omitempty"`
	Reasoning int64   `json:"reasoning,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"` // only Claude reports this
}

// Total returns fresh tokens: uncached input + output. Input includes cache
// reads (both CLIs report it that way); those are cheap and would otherwise
// dwarf everything else in the usage bars and stats.
func (t TokenUsage) Total() int64 { return t.Input - t.Cached + t.Output }

// Add returns the sum of two usages.
func (t TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{
		Input: t.Input + o.Input, Cached: t.Cached + o.Cached, Output: t.Output + o.Output,
		Reasoning: t.Reasoning + o.Reasoning, CostUSD: t.CostUSD + o.CostUSD,
	}
}

// QuotaInfo is provider-reported usage of the subscription window.
type QuotaInfo struct {
	Utilization float64   `json:"utilization"` // 0..1
	Window      string    `json:"window,omitempty"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
	Status      string    `json:"status,omitempty"`
}

// Decision is one routing decision.
type Decision struct {
	StepID     string  `json:"step_id"`
	StepTitle  string  `json:"step_title,omitempty"`
	Role       string  `json:"role"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Effort     string  `json:"effort,omitempty"`
	Rule       string  `json:"rule"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	Fallback   bool    `json:"fallback,omitempty"`
	Judged     bool    `json:"judged,omitempty"`
}

// Label is a short "provider:model@effort" description.
func (d Decision) Label() string {
	s := d.Provider + ":" + d.Model
	if d.Effort != "" {
		s += "@" + d.Effort
	}
	return s
}

// Event is the single normalized message type.
type Event struct {
	AgentID   string     `json:"agent_id,omitempty"`
	ParentID  string     `json:"parent_id,omitempty"`
	Provider  string     `json:"provider,omitempty"`
	Model     string     `json:"model,omitempty"`
	Role      string     `json:"role,omitempty"`
	Kind      Kind       `json:"kind"`
	Text      string     `json:"text,omitempty"`
	Tokens    TokenUsage `json:"tokens,omitempty"`
	Timestamp time.Time  `json:"ts"`

	OK       bool       `json:"ok,omitempty"`       // Done, Checkpoint, Merge, TaskDone
	Decision *Decision  `json:"decision,omitempty"` // Route
	Quota    *QuotaInfo `json:"quota,omitempty"`    // Quota
	Until    time.Time  `json:"until,omitempty"`    // ProviderState: limited until
}

// Stamp sets the timestamp if it is empty and returns the event.
func (e Event) Stamp() Event {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	return e
}
