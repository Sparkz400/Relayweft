package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

// NewCodex returns a runner for `codex exec --json` (tested with codex-cli 0.160.0).
func NewCodex(cfg config.ProviderCfg, det *limits.Detector) *Exec {
	return &Exec{
		Provider: event.Codex,
		Cfg:      cfg,
		Detector: det,
		args:     func(s Spec) []string { return CodexArgs(cfg, s) },
		parser:   func() lineParser { return &codexParser{} },
	}
}

// CodexArgs builds the argument list. The prompt is read from stdin ("-").
func CodexArgs(cfg config.ProviderCfg, s Spec) []string {
	args := []string{"exec", "--json", "--color", "never", "--skip-git-repo-check"}
	if s.Model != "" {
		args = append(args, "-m", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+s.Effort)
	}
	sandbox := cfg.WriteSandbox
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	if s.ReadOnly {
		sandbox = "read-only"
	}
	// No -C: the working directory is set on the process, and a quoted path
	// argument breaks cmd.exe quoting of npm .cmd shims on Windows.
	args = append(args, "--sandbox", sandbox)
	args = append(args, cfg.ExtraArgs...)
	return append(args, "-")
}

// codexParser understands the `codex exec --json` JSONL stream:
//
//	{"type":"thread.started","thread_id":"..."}
//	{"type":"item.started|item.updated|item.completed","item":{"type":"agent_message|reasoning|command_execution|file_change|mcp_tool_call|web_search|todo_list|error",...}}
//	{"type":"turn.completed","usage":{"input_tokens":..,"cached_input_tokens":..,"output_tokens":..,"reasoning_output_tokens":..}}
//	{"type":"turn.failed","error":{"message":"..."}}  /  {"type":"error","message":"..."}
//
// It also accepts the older {"id":..,"msg":{"type":...}} protocol.
type codexParser struct {
	final  string
	tokens event.TokenUsage
	fatal  string
	files  fileSet
}

type codexLine struct {
	Type    string                    `json:"type"`
	Item    *codexItem                `json:"item"`
	Usage   *codexUsage               `json:"usage"`
	Error   *struct{ Message string } `json:"error"`
	Message string                    `json:"message"`
	Msg     json.RawMessage           `json:"msg"` // legacy protocol
}

type codexItem struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Text     string `json:"text"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
	Status   string `json:"status"`
	Server   string `json:"server"`
	Tool     string `json:"tool"`
	Query    string `json:"query"`
	Message  string `json:"message"`
	Changes  []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
	Items []struct {
		Text      string `json:"text"`
		Completed bool   `json:"completed"`
	} `json:"items"`
}

type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

func (p *codexParser) Line(line []byte) []event.Event {
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil {
		// Codex prints a few non-JSON lines in some versions; show them as thinking.
		if s := strings.TrimSpace(string(line)); s != "" {
			return []event.Event{{Kind: event.Thinking, Text: s}}
		}
		return nil
	}
	var extra []event.Event
	if q := findRateLimits(line); q != nil {
		extra = append(extra, event.Event{Kind: event.Quota, Quota: q})
	}
	if len(l.Msg) > 0 && l.Type == "" {
		return append(extra, p.legacy(l.Msg)...)
	}
	return append(extra, p.typed(l)...)
}

func (p *codexParser) typed(l codexLine) []event.Event {
	switch l.Type {
	case "thread.started", "turn.started":
		return nil
	case "turn.completed":
		if l.Usage != nil {
			u := event.TokenUsage{
				Input: l.Usage.InputTokens, Cached: l.Usage.CachedInputTokens,
				Output: l.Usage.OutputTokens, Reasoning: l.Usage.ReasoningOutputTokens,
			}
			p.tokens = p.tokens.Add(u) // reported once by Exec at the end
		}
		return nil
	case "turn.failed", "thread.failed":
		msg := l.Message
		if l.Error != nil && l.Error.Message != "" {
			msg = l.Error.Message
		}
		if msg == "" {
			msg = l.Type
		}
		p.fatal = msg
		return []event.Event{{Kind: event.Error, Text: msg}}
	case "error":
		msg := l.Message
		if l.Error != nil && msg == "" {
			msg = l.Error.Message
		}
		if transient(msg) {
			// Codex reports its own retries as "error" events; the run goes on.
			return []event.Event{{Kind: event.Thinking, Text: "retrying: " + msg}}
		}
		p.fatal = msg
		return []event.Event{{Kind: event.Error, Text: msg}}
	case "item.started", "item.updated", "item.completed":
		if l.Item == nil {
			return nil
		}
		return p.item(l.Type, l.Item)
	}
	return nil
}

func (p *codexParser) item(phase string, it *codexItem) []event.Event {
	done := phase == "item.completed"
	switch it.Type {
	case "agent_message":
		if done && it.Text != "" {
			p.final = it.Text
			return []event.Event{{Kind: event.Message, Text: it.Text}}
		}
	case "reasoning":
		if done && it.Text != "" {
			return []event.Event{{Kind: event.Thinking, Text: it.Text}}
		}
	case "command_execution":
		if phase == "item.started" {
			return []event.Event{{Kind: event.ToolCall, Text: "$ " + it.Command}}
		}
		if done && it.ExitCode != nil && *it.ExitCode != 0 {
			return []event.Event{{Kind: event.Thinking, Text: fmt.Sprintf("exit %d: %s", *it.ExitCode, it.Command)}}
		}
	case "file_change":
		if done {
			var out []event.Event
			for _, c := range it.Changes {
				p.files.add(c.Path)
				out = append(out, event.Event{Kind: event.FileEdit, Text: c.Path})
			}
			return out
		}
	case "mcp_tool_call":
		if phase == "item.started" {
			return []event.Event{{Kind: event.ToolCall, Text: it.Server + "." + it.Tool}}
		}
	case "web_search":
		if phase == "item.started" {
			return []event.Event{{Kind: event.ToolCall, Text: strings.TrimSpace("search " + it.Query)}}
		}
	case "todo_list":
		if len(it.Items) > 0 {
			var b strings.Builder
			for _, t := range it.Items {
				mark := "[ ]"
				if t.Completed {
					mark = "[x]"
				}
				b.WriteString(mark + " " + t.Text + "  ")
			}
			return []event.Event{{Kind: event.Thinking, Text: strings.TrimSpace(b.String())}}
		}
	case "error":
		// Item-level errors are warnings (e.g. "Falling back from WebSockets
		// to HTTPS transport"); real failures arrive as turn.failed.
		msg := it.Message
		if msg == "" {
			msg = it.Text
		}
		return []event.Event{{Kind: event.Thinking, Text: "warning: " + msg}}
	}
	return nil
}

// transient reports Codex's self-healing retry messages.
func transient(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	for _, p := range []string{"reconnecting", "falling back from websockets", "stream disconnected - retrying", "stream connection failed; waiting to retry"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// legacy handles the pre-0.40 {"id":..,"msg":{...}} event protocol.
func (p *codexParser) legacy(raw json.RawMessage) []event.Event {
	var m struct {
		Type             string          `json:"type"`
		Message          string          `json:"message"`
		Text             string          `json:"text"`
		Command          []string        `json:"command"`
		Changes          map[string]any  `json:"changes"`
		LastAgentMessage string          `json:"last_agent_message"`
		Info             json.RawMessage `json:"info"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	switch m.Type {
	case "agent_message":
		p.final = m.Message
		return []event.Event{{Kind: event.Message, Text: m.Message}}
	case "agent_reasoning":
		return []event.Event{{Kind: event.Thinking, Text: m.Text}}
	case "exec_command_begin":
		return []event.Event{{Kind: event.ToolCall, Text: "$ " + strings.Join(m.Command, " ")}}
	case "patch_apply_begin":
		var out []event.Event
		for path := range m.Changes {
			p.files.add(path)
			out = append(out, event.Event{Kind: event.FileEdit, Text: path})
		}
		return out
	case "token_count":
		var info struct {
			Total codexUsage `json:"total_token_usage"`
		}
		if json.Unmarshal(m.Info, &info) == nil {
			p.tokens = event.TokenUsage{
				Input: info.Total.InputTokens, Cached: info.Total.CachedInputTokens,
				Output: info.Total.OutputTokens, Reasoning: info.Total.ReasoningOutputTokens,
			}
		}
	case "task_complete":
		if m.LastAgentMessage != "" {
			p.final = m.LastAgentMessage
		}
	case "error", "stream_error":
		p.fatal = m.Message
		return []event.Event{{Kind: event.Error, Text: m.Message}}
	}
	return nil
}

func (p *codexParser) Finish(r *Result) {
	r.Final = p.final
	r.Tokens = p.tokens
	r.Files = p.files.list()
	if p.fatal != "" && p.final == "" {
		r.Err = errors.New(p.fatal)
	}
}

// findRateLimits looks for Codex's rate_limits object anywhere in a JSON
// line: {"rate_limits":{"primary":{"used_percent":42.0,"window_minutes":300,
// "resets_at":1791003600},"secondary":{...}}}. Codex tracks these
// internally; when a version reports them in its JSON output, Switchyard uses
// them like Claude's quota events.
func findRateLimits(line []byte) *event.QuotaInfo {
	if !bytes.Contains(line, []byte(`"rate_limits"`)) {
		return nil
	}
	var v any
	if json.Unmarshal(line, &v) != nil {
		return nil
	}
	rl := findKey(v, "rate_limits", 5)
	m, ok := rl.(map[string]any)
	if !ok {
		return nil
	}
	var q *event.QuotaInfo
	for _, name := range []string{"primary", "secondary"} {
		w, ok := m[name].(map[string]any)
		if !ok {
			continue
		}
		pct, ok := w["used_percent"].(float64)
		if !ok {
			continue
		}
		label := name
		if mins, ok := w["window_minutes"].(float64); ok && mins > 0 {
			if mins >= 1440 {
				label = fmt.Sprintf("%.0fd", mins/1440)
			} else {
				label = fmt.Sprintf("%.0fh", mins/60)
			}
		}
		if q == nil {
			q = &event.QuotaInfo{Windows: map[string]float64{}}
		}
		u := pct / 100
		q.Windows[label] = u
		if u >= q.Utilization {
			q.Utilization, q.Window = u, label
			if at, ok := w["resets_at"].(float64); ok && at > 0 {
				q.ResetsAt = time.Unix(int64(at), 0)
			} else if in, ok := w["resets_in_seconds"].(float64); ok && in > 0 {
				q.ResetsAt = time.Now().Add(time.Duration(in) * time.Second)
			}
		}
	}
	return q
}

func findKey(v any, key string, depth int) any {
	if depth < 0 {
		return nil
	}
	switch t := v.(type) {
	case map[string]any:
		if x, ok := t[key]; ok {
			return x
		}
		for _, x := range t {
			if r := findKey(x, key, depth-1); r != nil {
				return r
			}
		}
	case []any:
		for _, x := range t {
			if r := findKey(x, key, depth-1); r != nil {
				return r
			}
		}
	}
	return nil
}
