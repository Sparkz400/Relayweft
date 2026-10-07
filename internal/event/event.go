// Package event defines the normalized event stream that every runner
// adapter produces and that the TUI, session log and headless printer
// consume. Nothing outside the runner package ever sees raw CLI output.
package event

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Built-in providers. They are also the provider kinds: the CLI protocol a
// configured provider speaks (providers.<name>.kind; a provider named after
// a kind needs none). Any other provider name comes from the config.
const (
	Codex  = "codex"
	Claude = "claude"
	Gemini = "gemini"
	Qwen   = "qwen"
	// Generic is a CLI described entirely in the config (providers.<name>.generic).
	Generic = "generic"
)

// Kinds lists every CLI protocol rw can drive.
var Kinds = []string{Codex, Claude, Gemini, Qwen, Generic}

// ProvidersOf returns the provider keys of a per-provider map in display
// order: codex, claude, then the rest by name.
func ProvidersOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for _, p := range []string{Codex, Claude} {
		if _, ok := m[p]; ok {
			out = append(out, p)
		}
	}
	rest := make([]string, 0, len(m))
	for p := range m {
		if p != Codex && p != Claude {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
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
	// Incomplete marks a lower bound: the CLI ended without final accounting.
	Incomplete bool    `json:"incomplete,omitempty"`
	Input      int64   `json:"input,omitempty"`
	Cached     int64   `json:"cached,omitempty"`
	Output     int64   `json:"output,omitempty"`
	Reasoning  int64   `json:"reasoning,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"` // only Claude reports this
}

// Total returns fresh tokens: uncached input + output. Input includes cache
// reads (both CLIs report it that way); those are cheap and would otherwise
// dwarf everything else in the usage bars and stats.
func (t TokenUsage) Total() int64 { return t.Input - t.Cached + t.Output }

// Add returns the sum of two usages.
func (t TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{
		Incomplete: t.Incomplete || o.Incomplete,
		Input:      t.Input + o.Input, Cached: t.Cached + o.Cached, Output: t.Output + o.Output,
		Reasoning: t.Reasoning + o.Reasoning, CostUSD: t.CostUSD + o.CostUSD,
	}
}

// QuotaInfo is provider-reported usage of the subscription window.
type QuotaInfo struct {
	Utilization float64            `json:"utilization"` // 0..1, the fullest window
	Window      string             `json:"window,omitempty"`
	ResetsAt    time.Time          `json:"resets_at,omitempty"`
	Status      string             `json:"status,omitempty"`
	Windows     map[string]float64 `json:"windows,omitempty"` // every reported window, 0..1
}

// TaskCost is what one task used.
type TaskCost struct {
	PerProvider map[string]TokenUsage `json:"per_provider,omitempty"`
	CostUSD     float64               `json:"cost_usd,omitempty"` // Claude's API-equivalent price (not billed on a subscription)
	QuotaBefore map[string]float64    `json:"quota_before,omitempty"`
	QuotaAfter  map[string]float64    `json:"quota_after,omitempty"`
}

// Summary is a one-line description, e.g.
// "codex 12k · claude 40k fresh tokens · ≈$0.31 API-equivalent · claude limit 61%→64%".
func (c TaskCost) Summary() string {
	var parts []string
	incomplete := false
	for _, p := range ProvidersOf(c.PerProvider) {
		incomplete = incomplete || c.PerProvider[p].Incomplete
		if u := c.PerProvider[p]; u.Total() > 0 {
			parts = append(parts, p+" "+HumanTokens(u.Total()))
		}
	}
	if len(parts) == 0 {
		if incomplete {
			return "usage incomplete (no final accounting)"
		}
		return "no tokens used"
	}
	s := strings.Join(parts, " · ") + " fresh tokens"
	if incomplete {
		s = "at least " + s + " (usage incomplete)"
	}
	if c.CostUSD > 0 {
		s += fmt.Sprintf(" · ≈$%.2f API-equivalent", c.CostUSD)
	}
	for _, p := range ProvidersOf(c.QuotaAfter) {
		b, okB := c.QuotaBefore[p]
		a, okA := c.QuotaAfter[p]
		if okA {
			if okB {
				s += fmt.Sprintf(" · %s limit %.0f%%→%.0f%%", p, b*100, a*100)
			} else {
				s += fmt.Sprintf(" · %s limit %.0f%%", p, a*100)
			}
		}
	}
	return s
}

// HumanTokens formats a token count: 950, 1.2k, 45k, 1.3M.
func HumanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
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
	// From is the provider the route preferred before a fallback moved it.
	From   string `json:"from,omitempty"`
	Judged bool   `json:"judged,omitempty"`
	Tier   string `json:"tier,omitempty"` // routing.tiers: auto picked this tier (fast|standard|strong)
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
	Cost     *TaskCost  `json:"cost,omitempty"`     // TaskDone
	Until    time.Time  `json:"until,omitempty"`    // ProviderState: limited until
}

// Stamp sets the timestamp if it is empty and returns the event.
func (e Event) Stamp() Event {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	return e
}
