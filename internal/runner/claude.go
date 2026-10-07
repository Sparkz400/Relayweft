package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
)

// ReadOnlyTools are the Claude tools a read-only agent (explorer,
// researcher, planner, reviewer, judge) may use.
const ReadOnlyTools = "Read,Grep,Glob,WebSearch,WebFetch"

// shellTools are the Claude Code tools that run shell commands; verify
// commands are allowed in each of them.
var shellTools = []string{"Bash", "PowerShell"}

// NewClaude returns a runner for `claude -p --output-format stream-json`
// (tested with Claude Code 2.1.288).
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
	if s.Resume != "" {
		args = append(args, "--resume", s.Resume)
	}
	// extra_args go before --tools/--allowedTools, which are variadic and
	// would swallow anything after them.
	args = append(args, cfg.ExtraArgs...)
	// Variadic too, but the permission mode always follows it.
	args = append(args, claudeMCPArgs(s.MCP)...)
	if s.CheckOnly {
		args = append(args, "--permission-mode", "dontAsk", "--tools", strings.Join(shellTools, ","))
	} else if s.ReadOnly {
		// dontAsk denies anything not allowed; --tools removes the write tools entirely.
		args = append(args, "--permission-mode", "dontAsk", "--tools", ReadOnlyTools)
	} else {
		mode := cfg.WritePermissionMode
		if mode == "" {
			mode = "acceptEdits"
		}
		args = append(args, "--permission-mode", mode)
	}
	allowed := append([]string(nil), cfg.WriteAllowedTools...)
	if s.CheckOnly {
		allowed = nil
	}
	for _, c := range s.AllowedCommands {
		// Exact command and with arguments (e.g. "go test ./pkg/..."), for
		// both shell tools: Claude Code on Windows also has a PowerShell
		// tool, and Bash(...) rules do not cover a command run through it
		// (verified with Claude Code 2.1.288: denied without PowerShell(...)).
		for _, tool := range shellTools {
			allowed = append(allowed, tool+"("+c+")", tool+"("+c+" *)")
		}
	}
	if s.ReadOnly {
		// A read-only agent gets no write tools or commands, but MCP tools
		// (mcp.allow_tools) are allowed for every role that has servers:
		// dontAsk would deny them otherwise. They are not in --tools, which
		// lists built-in tools only.
		allowed = nil
	}
	allowed = append(allowed, claudeMCPTools(s.MCP)...)
	if len(allowed) > 0 {
		// Variadic flag: keep it last so it cannot swallow other arguments.
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
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
//	{"type":"system","subtype":"permission_denied","tool_name":"PowerShell","tool_use_id":"...","message":"<why>"}
//	{"type":"result","subtype":"success","is_error":false,"result":"...","usage":{...},"total_cost_usd":0.01,"permission_denials":[...]}
type claudeParser struct {
	// inputHasCache: input_tokens already include the cached part (Qwen
	// Code); Claude Code reports cache reads and writes separately.
	inputHasCache bool
	// noCost drops the API-equivalent price: Claude Code computes it for
	// Anthropic's models, which another backend is not.
	noCost bool
	// cacheGuess: another backend behind Claude Code may count the cached
	// part inside input_tokens (Ollama does). Anthropic never reports
	// input_tokens >= cache_read_input_tokens with a cache hit that large,
	// so such a reading is taken as including the cache.
	cacheGuess       bool
	session          string
	final            string
	lastMsg          string
	tokens           event.TokenUsage
	fatal            string
	limit            bool
	files            fileSet
	gotDone          bool
	calls            map[string]string // tool_use id -> "Tool arg", to name denied calls
	denied           map[string]bool   // tool_use ids already reported as denied
	messageUsage     map[string]event.TokenUsage
	finalUsage       bool
	commands         map[string]string // tool use id -> exact shell command
	executed         map[string]bool
	permissionDenied bool
}

type claudeLine struct {
	Type         string  `json:"type"`
	SessionID    string  `json:"session_id"`
	Subtype      string  `json:"subtype"`
	Model        string  `json:"model"`
	Detail       *string `json:"detail"`
	StatusDetail string  `json:"status_detail"`
	// An object on assistant/user lines, a plain string on others
	// (system permission_denied): decoded per line type.
	Message       json.RawMessage `json:"message"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	Result        string          `json:"result"`
	IsError       bool            `json:"is_error"`
	Usage         *claudeUsage    `json:"usage"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
	RateLimitInfo *claudeRateInfo `json:"rate_limit_info"`
	Errors        []string        `json:"errors"`
	// Tool calls Claude Code refused for lack of permission (in -p mode
	// nobody can approve them; the agent just reports it could not).
	PermissionDenials []struct {
		ToolName  string          `json:"tool_name"`
		ToolUseID string          `json:"tool_use_id"`
		ToolInput json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
	// Error is how Qwen Code reports a failed run.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type claudeMessage struct {
	ID      string       `json:"id"`
	Usage   *claudeUsage `json:"usage"`
	Content []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		ToolUseID string          `json:"tool_use_id"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
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
	Status         string  `json:"status"`
	ResetsAt       int64   `json:"resetsAt"`
	RateLimitType  string  `json:"rateLimitType"`
	Utilization    float64 `json:"utilization"`
	UnifiedWindows map[string]struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// editTools are the tools that change files: Claude Code's, and Qwen
// Code's (named like Gemini CLI's).
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
	"write_file": true, "edit": true, "replace": true}

func (p *claudeParser) Line(line []byte) []event.Event {
	var l claudeLine
	if err := json.Unmarshal(line, &l); err != nil {
		if s := strings.TrimSpace(string(line)); s != "" {
			return []event.Event{{Kind: event.Thinking, Text: s}}
		}
		return nil
	}
	if l.SessionID != "" {
		p.session = l.SessionID
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
		case "permission_denied":
			var why string
			_ = json.Unmarshal(l.Message, &why) // no reason given: why stays empty
			return p.deny(l.ToolUseID, l.ToolName, "", why)
		}
	case "rate_limit_event":
		if ri := l.RateLimitInfo; ri != nil {
			q := &event.QuotaInfo{Utilization: ri.Utilization, Window: ri.RateLimitType, Status: ri.Status}
			if ri.ResetsAt > 0 {
				q.ResetsAt = time.Unix(ri.ResetsAt, 0)
			}
			// Report the fullest window (5h or 7d): that is the one about to bite.
			for name, w := range ri.UnifiedWindows {
				if q.Windows == nil {
					q.Windows = map[string]float64{}
				}
				q.Windows[name] = w.Utilization
				if w.Utilization > q.Utilization || (q.Window == "" && w.Utilization >= q.Utilization) {
					q.Utilization, q.Window = w.Utilization, name
					if w.ResetsAt > 0 {
						q.ResetsAt = time.Unix(w.ResetsAt, 0)
					}
				}
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
		var m claudeMessage
		if len(l.Message) == 0 || json.Unmarshal(l.Message, &m) != nil {
			return nil
		}
		var out []event.Event
		if m.ID != "" && m.Usage != nil {
			if p.messageUsage == nil {
				p.messageUsage = map[string]event.TokenUsage{}
			}
			u, prev := p.usage(m.Usage, 0), p.messageUsage[m.ID]
			// Some snapshots omit fields already reported for this message.
			u.Input, u.Cached = max(u.Input, prev.Input), max(u.Cached, prev.Cached)
			u.Output, u.Reasoning = max(u.Output, prev.Output), max(u.Reasoning, prev.Reasoning)
			p.messageUsage[m.ID] = u
		}
		for _, c := range m.Content {
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
				if c.ID != "" {
					if p.calls == nil {
						p.calls = map[string]string{}
					}
					p.calls[c.ID] = strings.TrimSpace(c.Name + " " + arg)
					if c.Name == "Bash" || c.Name == "PowerShell" {
						var input struct {
							Command string `json:"command"`
						}
						if json.Unmarshal(c.Input, &input) == nil && input.Command != "" {
							if p.commands == nil {
								p.commands = map[string]string{}
							}
							p.commands[c.ID] = input.Command
						}
					}
				}
				if editTools[c.Name] {
					p.files.add(arg)
					out = append(out, event.Event{Kind: event.FileEdit, Text: arg})
				} else {
					out = append(out, event.Event{Kind: event.ToolCall, Text: strings.TrimSpace(c.Name + " " + arg)})
				}
			}
		}
		return out
	case "user":
		var m claudeMessage
		if json.Unmarshal(l.Message, &m) != nil {
			return nil
		}
		for _, c := range m.Content {
			if c.Type == "tool_result" && !c.IsError && p.commands[c.ToolUseID] != "" {
				if p.executed == nil {
					p.executed = map[string]bool{}
				}
				p.executed[c.ToolUseID] = true
			}
		}
	case "result":
		p.gotDone = true
		var out []event.Event
		for _, d := range l.PermissionDenials {
			// Those not already reported by a permission_denied line.
			out = append(out, p.deny(d.ToolUseID, d.ToolName, toolArg(d.ToolInput), "")...)
		}
		if l.Usage != nil {
			p.tokens, p.finalUsage = p.usage(l.Usage, l.TotalCostUSD), true
		}
		if l.IsError || strings.HasPrefix(l.Subtype, "error") {
			msg := strings.TrimSpace(l.Result)
			if msg == "" && len(l.Errors) > 0 {
				msg = strings.Join(l.Errors, "; ")
			}
			if msg == "" && l.Error != nil {
				msg = strings.TrimSpace(l.Error.Message)
			}
			if msg == "" {
				msg = "claude: " + l.Subtype
			}
			p.fatal = msg
			return append(out, event.Event{Kind: event.Error, Text: msg})
		}
		p.final = l.Result
		return out
	}
	return nil
}

// deny reports a tool call Claude Code refused for lack of permission, once
// per call. In -p mode nobody can approve it, so the agent goes on without
// it (e.g. without running a check). A warning, not an error: the run may
// still succeed, but the user should see what the agent was not allowed
// to do.
func (p *claudeParser) deny(id, tool, arg, why string) []event.Event {
	p.permissionDenied = true
	if id != "" {
		if p.denied[id] {
			return nil
		}
		if p.denied == nil {
			p.denied = map[string]bool{}
		}
		p.denied[id] = true
		if c := p.calls[id]; c != "" {
			tool, arg = c, ""
		}
	}
	text := strings.TrimSpace("warning: permission denied: " + strings.TrimSpace(tool+" "+arg))
	if why = strings.TrimSpace(why); why != "" {
		text += " (" + why + ")"
	}
	return []event.Event{{Kind: event.Thinking, Text: text}}
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
	r.SessionID = p.session
	if r.Final == "" {
		r.Final = p.lastMsg
	}
	r.Tokens = p.tokens
	if !p.finalUsage {
		for _, u := range p.messageUsage {
			r.Tokens = r.Tokens.Add(u)
		}
		r.Tokens.Incomplete = true
	}
	r.PermissionDenied = p.permissionDenied
	r.Commands = nil
	for id := range p.executed {
		if !p.denied[id] {
			r.Commands = append(r.Commands, p.commands[id])
		}
	}
	slices.Sort(r.Commands)
	r.Files = p.files.list()
	if p.limit {
		r.LimitHit = true
	}
	if p.fatal != "" {
		r.Err = errors.New(p.fatal)
	}
}

func (p *claudeParser) usage(u *claudeUsage, usd float64) event.TokenUsage {
	t := event.TokenUsage{Input: u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		Cached: u.CacheReadInputTokens, Output: u.OutputTokens, Reasoning: u.OutputTokensDetails.ThinkingTokens, CostUSD: usd}
	if p.inputHasCache || (p.cacheGuess && u.CacheReadInputTokens > 0 && u.InputTokens >= u.CacheReadInputTokens) {
		t.Input = u.InputTokens + u.CacheCreationInputTokens
	}
	if p.noCost {
		t.CostUSD = 0
	}
	return t
}

func (p *claudeParser) sessionID() string { return p.session }
