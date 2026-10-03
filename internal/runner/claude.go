package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

// ReadOnlyTools are the Claude tools a read-only agent (explorer,
// researcher, planner, reviewer, judge) may use.
const ReadOnlyTools = "Read,Grep,Glob,WebSearch,WebFetch"

// NewClaude returns a runner for `claude -p --output-format stream-json`
// (tested with Claude Code 2.1.287).
func NewClaude(cfg config.ProviderCfg, det *limits.Detector) *Exec {
	return &Exec{
		Provider: event.Claude,
		Cfg:      cfg,
		Detector: det,
		args:     func(s Spec) []string { return ClaudeArgs(cfg, s) },
		parser:   func() lineParser { return &claudeParser{} },
	}
}

// ClaudeArgs builds the argument list. The prompt is read from stdin.
func ClaudeArgs(cfg config.ProviderCfg, s Spec) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
	}
	// extra_args go before --tools/--allowedTools, which are variadic and
	// would swallow anything after them.
	args = append(args, cfg.ExtraArgs...)
	if s.ReadOnly {
		// dontAsk denies anything not allowed; --tools removes the write tools entirely.
		args = append(args, "--permission-mode", "dontAsk", "--tools", ReadOnlyTools)
	} else {
		mode := cfg.WritePermissionMode
		if mode == "" {
			mode = "acceptEdits"
		}
		args = append(args, "--permission-mode", mode)
	}
	if !s.ReadOnly && len(cfg.WriteAllowedTools) > 0 {
		// Variadic flag: keep it last so it cannot swallow other arguments.
		args = append(args, "--allowedTools", strings.Join(cfg.WriteAllowedTools, ","))
	}
	return args
}

// claudeParser understands Claude Code's stream-json output:
//
//	{"type":"system","subtype":"init","model":"..."}
//	{"type":"rate_limit_event","rate_limit_info":{"status":"allowed|allowed_warning|rejected","utilization":0.76,"resetsAt":1791003600,...}}
//	{"type":"assistant","message":{"content":[{"type":"text|thinking|tool_use",...}]}}
//	{"type":"user","message":{"content":[{"type":"tool_result",...}]}}
//	{"type":"system","subtype":"task_summary","detail":"Reading a.txt"}
//	{"type":"result","subtype":"success","is_error":false,"result":"...","usage":{...},"total_cost_usd":0.01}
type claudeParser struct {
	final   string
	lastMsg string
	tokens  event.TokenUsage
	fatal   string
	limit   bool
	files   fileSet
	gotDone bool
}

type claudeLine struct {
	Type          string          `json:"type"`
	Subtype       string          `json:"subtype"`
	Model         string          `json:"model"`
	Detail        *string         `json:"detail"`
	StatusDetail  string          `json:"status_detail"`
	Message       *claudeMessage  `json:"message"`
	Result        string          `json:"result"`
	IsError       bool            `json:"is_error"`
	Usage         *claudeUsage    `json:"usage"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
	RateLimitInfo *claudeRateInfo `json:"rate_limit_info"`
	Errors        []string        `json:"errors"`
}

type claudeMessage struct {
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		IsError  bool            `json:"is_error"`
		Content  json.RawMessage `json:"content"`
	} `json:"content"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type claudeRateInfo struct {
	Status        string  `json:"status"`
	ResetsAt      int64   `json:"resetsAt"`
	RateLimitType string  `json:"rateLimitType"`
	Utilization   float64 `json:"utilization"`
}

var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

func (p *claudeParser) Line(line []byte) []event.Event {
	var l claudeLine
	if err := json.Unmarshal(line, &l); err != nil {
		if s := strings.TrimSpace(string(line)); s != "" {
			return []event.Event{{Kind: event.Thinking, Text: s}}
		}
		return nil
	}
	switch l.Type {
	case "system":
		switch l.Subtype {
		case "init":
			return []event.Event{{Kind: event.Thinking, Model: l.Model, Text: "session ready (" + l.Model + ")"}}
		case "task_summary":
			if l.Detail != nil && *l.Detail != "" {
				return []event.Event{{Kind: event.Thinking, Text: *l.Detail}}
			}
		}
	case "rate_limit_event":
		if ri := l.RateLimitInfo; ri != nil {
			q := &event.QuotaInfo{Utilization: ri.Utilization, Window: ri.RateLimitType, Status: ri.Status}
			if ri.ResetsAt > 0 {
				q.ResetsAt = time.Unix(ri.ResetsAt, 0)
			}
			out := []event.Event{{Kind: event.Quota, Quota: q}}
			if ri.Status == "rejected" {
				p.limit = true
				msg := "Claude usage limit reached"
				if !q.ResetsAt.IsZero() {
					msg = fmt.Sprintf("%s|%d", msg, ri.ResetsAt)
				}
				out = append(out, event.Event{Kind: event.LimitHit, Text: msg})
			}
			return out
		}
	case "assistant":
		if l.Message == nil {
			return nil
		}
		var out []event.Event
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					p.lastMsg = c.Text
					out = append(out, event.Event{Kind: event.Message, Text: c.Text})
				}
			case "thinking":
				if t := strings.TrimSpace(c.Thinking); t != "" {
					out = append(out, event.Event{Kind: event.Thinking, Text: t})
				}
			case "tool_use":
				arg := toolArg(c.Input)
				if editTools[c.Name] {
					p.files.add(arg)
					out = append(out, event.Event{Kind: event.FileEdit, Text: arg})
				} else {
					out = append(out, event.Event{Kind: event.ToolCall, Text: strings.TrimSpace(c.Name + " " + arg)})
				}
			}
		}
		return out
	case "result":
		p.gotDone = true
		if l.Usage != nil {
			u := l.Usage
			p.tokens = event.TokenUsage{
				Input:     u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
				Cached:    u.CacheReadInputTokens,
				Output:    u.OutputTokens,
				Reasoning: u.OutputTokensDetails.ThinkingTokens,
				CostUSD:   l.TotalCostUSD,
			}
		}
		if l.IsError || strings.HasPrefix(l.Subtype, "error") {
			msg := strings.TrimSpace(l.Result)
			if msg == "" && len(l.Errors) > 0 {
				msg = strings.Join(l.Errors, "; ")
			}
			if msg == "" {
				msg = "claude: " + l.Subtype
			}
			p.fatal = msg
			return []event.Event{{Kind: event.Error, Text: msg}}
		}
		p.final = l.Result
	}
	return nil
}

// toolArg extracts the most telling argument of a tool call for display.
func toolArg(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "notebook_path", "path", "pattern", "command", "url", "query", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			if k == "file_path" || k == "notebook_path" {
				return filepath.ToSlash(v)
			}
			return v
		}
	}
	return ""
}

func (p *claudeParser) Finish(r *Result) {
	r.Final = p.final
	if r.Final == "" {
		r.Final = p.lastMsg
	}
	r.Tokens = p.tokens
	r.Files = p.files.list()
	if p.limit {
		r.LimitHit = true
	}
	if p.fatal != "" {
		r.Err = errors.New(p.fatal)
	}
}
