package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

// NewGemini returns a runner for `gemini --output-format stream-json`
// (tested with Gemini CLI 0.62.0). The prompt is read from stdin: with a
// pipe on stdin the CLI runs headless.
func NewGemini(cfg config.ProviderCfg, det *limits.Detector) *Exec {
	return &Exec{
		Provider: event.Gemini,
		Kind:     event.Gemini,
		Cfg:      cfg,
		Detector: det,
		args:     func(s Spec) []string { return GeminiArgs(cfg, s) },
		parser:   func() lineParser { return &geminiParser{} },
		precheck: geminiPrecheck(cfg),
	}
}

// GeminiArgs builds the argument list.
//
// Read-only agents run in plan mode (Gemini's read-only mode); writers in
// auto_edit, which approves edits but no shell commands except the verify
// commands. Gemini only honours an approval mode in a folder it trusts and
// otherwise falls back to "default". --skip-trust trusts the folder for the
// run, but a trusted folder also has its own .gemini settings (which can
// run commands) and .env files (which can point the CLI, and your Google
// sign-in, at another endpoint) loaded. So it is passed only when the
// folder has neither, or allow_repo_settings says to (geminiPrecheck).
func GeminiArgs(cfg config.ProviderCfg, s Spec) []string {
	args := []string{"--output-format", "stream-json"}
	if s.Model != "" {
		args = append(args, "-m", s.Model)
	}
	if s.Resume != "" {
		args = append(args, "--resume", s.Resume)
	}
	if cfg.AllowRepoSettings || !hasGeminiRepoSettings(s.Dir) {
		args = append(args, "--skip-trust")
	}
	mode := cfg.WritePermissionMode
	if mode == "" {
		mode = "auto_edit"
	}
	if s.ReadOnly {
		mode = "plan"
	}
	args = append(args, "--approval-mode", mode)
	args = append(args, cfg.ExtraArgs...)
	if !s.ReadOnly && len(s.AllowedCommands) > 0 {
		// Variadic: keep it last so it cannot swallow other arguments.
		args = append(args, "--allowed-tools")
		for _, c := range s.AllowedCommands {
			args = append(args, "run_shell_command("+c+")")
		}
	}
	return args
}

// hasGeminiRepoSettings reports whether Gemini CLI would load settings of
// the repo's own in a trusted dir: a .gemini folder there, or a .env file
// there or in a parent folder below your home folder (Gemini looks upwards
// for one).
func hasGeminiRepoSettings(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".gemini")); err == nil {
		return true
	}
	home, _ := geminiHome()
	for d := filepath.Clean(dir); ; {
		if home != "" && strings.EqualFold(d, filepath.Clean(home)) {
			return false // ~/.env is your own
		}
		if fi, err := os.Stat(filepath.Join(d, ".env")); err == nil && !fi.IsDir() {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// geminiHome is the home folder Gemini CLI keeps its settings in (tests
// replace it).
var geminiHome = os.UserHomeDir

// geminiTrusts reports whether you trusted dir in Gemini CLI yourself
// (~/.gemini/trustedFolders.json: TRUST_FOLDER covers the folder and
// everything in it, TRUST_PARENT its parent's; DO_NOT_TRUST wins).
func geminiTrusts(dir string) bool {
	home, err := geminiHome()
	if err != nil || dir == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "trustedFolders.json"))
	if err != nil {
		return false
	}
	var rules map[string]string
	if json.Unmarshal(data, &rules) != nil {
		return false
	}
	under := func(d, root string) bool {
		rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(d))
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	trusted := false
	for path, level := range rules {
		switch level {
		case "DO_NOT_TRUST":
			if under(dir, path) {
				return false
			}
		case "TRUST_FOLDER":
			trusted = trusted || under(dir, path)
		case "TRUST_PARENT":
			trusted = trusted || under(dir, filepath.Dir(path))
		}
	}
	return trusted
}

// geminiPrecheck refuses a writing agent in a folder with settings of its
// own that you have not trusted in Gemini: Gemini would drop to "default"
// approval there and silently refuse every edit.
func geminiPrecheck(cfg config.ProviderCfg) func(Spec) error {
	return func(s Spec) error {
		if s.ReadOnly || cfg.AllowRepoSettings || !hasGeminiRepoSettings(s.Dir) || geminiTrusts(s.Dir) {
			return nil
		}
		return errors.New("this repo has settings of its own for Gemini CLI (.gemini or .env), which can run commands or redirect it, " +
			"so sy does not trust the folder for Gemini; trust it in Gemini yourself (run `gemini` there once) " +
			"or set providers.gemini.allow_repo_settings, or route writers to another provider")
	}
}

// geminiParser understands Gemini CLI's stream-json output:
//
//	{"type":"init","session_id":"...","model":"..."}
//	{"type":"message","role":"user|assistant","content":"...","delta":true}
//	{"type":"tool_use","tool_name":"write_file","tool_id":"...","parameters":{...}}
//	{"type":"tool_result","tool_id":"...","status":"success|error","output":"...","error":{"type":"...","message":"..."}}
//	{"type":"error","severity":"warning|error","message":"..."}
//	{"type":"result","status":"success|error","error":{...},"stats":{"input_tokens":..,"output_tokens":..,"cached":..}}
//
// Assistant text arrives in chunks; a message is complete when anything
// else arrives. A sign-in problem never reaches stdout: the CLI exits with
// code 41 and says so on stderr.
type geminiParser struct {
	session string
	text    strings.Builder // the assistant message being streamed
	lastMsg string
	tokens  event.TokenUsage
	fatal   string
	lastErr string // the last error event: the run's error if it ends without an answer
	files   fileSet
	gotDone bool
}

type geminiLine struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	Model      string          `json:"model"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolName   string          `json:"tool_name"`
	Parameters json.RawMessage `json:"parameters"`
	Status     string          `json:"status"`
	Severity   string          `json:"severity"`
	Message    string          `json:"message"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Stats *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		Cached       int64 `json:"cached"`
	} `json:"stats"`
}

// geminiEditTools are the tools that change files (Gemini CLI and the
// CLIs forked from it).
var geminiEditTools = map[string]bool{"write_file": true, "replace": true, "edit": true}

// flush ends the streamed assistant message.
func (p *geminiParser) flush() []event.Event {
	t := p.text.String()
	p.text.Reset()
	if strings.TrimSpace(t) == "" {
		return nil
	}
	p.lastMsg = t
	return []event.Event{{Kind: event.Message, Text: t}}
}

func (p *geminiParser) Line(line []byte) []event.Event {
	var l geminiLine
	if err := json.Unmarshal(line, &l); err != nil {
		if s := strings.TrimSpace(string(line)); s != "" {
			return []event.Event{{Kind: event.Thinking, Text: s}}
		}
		return nil
	}
	if l.Type == "message" && l.Role == "assistant" {
		p.text.WriteString(l.Content)
		return nil
	}
	out := p.flush()
	switch l.Type {
	case "init":
		if l.SessionID != "" {
			p.session = l.SessionID
		}
		out = append(out, event.Event{Kind: event.Thinking, Model: l.Model, Text: "session ready (" + l.Model + ")"})
	case "tool_use":
		arg := toolArg(l.Parameters)
		if geminiEditTools[l.ToolName] {
			p.files.add(arg)
			out = append(out, event.Event{Kind: event.FileEdit, Text: arg})
		} else {
			out = append(out, event.Event{Kind: event.ToolCall, Text: strings.TrimSpace(l.ToolName + " " + arg)})
		}
	case "tool_result":
		if l.Status == "error" && l.Error != nil && l.Error.Message != "" {
			out = append(out, event.Event{Kind: event.Thinking, Text: "tool failed: " + l.Error.Message})
		}
	case "error":
		if l.Severity == "error" {
			msg := strings.TrimSpace(l.Message)
			if msg == "" {
				msg = "gemini: error"
			}
			p.lastErr = msg
			out = append(out, event.Event{Kind: event.Error, Text: msg})
		} else if strings.TrimSpace(l.Message) != "" {
			out = append(out, event.Event{Kind: event.Thinking, Text: l.Message})
		}
	case "result":
		p.gotDone = true
		if st := l.Stats; st != nil {
			// input_tokens is the whole prompt, cached part included.
			p.tokens = event.TokenUsage{Input: st.InputTokens, Cached: st.Cached, Output: st.OutputTokens}
		}
		if l.Status == "error" {
			msg := "gemini: error"
			if l.Error != nil && strings.TrimSpace(l.Error.Message) != "" {
				msg = strings.TrimSpace(l.Error.Message)
			}
			p.fatal = msg
			out = append(out, event.Event{Kind: event.Error, Text: msg})
		}
	}
	return out
}

func (p *geminiParser) Finish(r *Result) {
	p.flush()
	r.Final = p.lastMsg
	r.SessionID = p.session
	r.Tokens = p.tokens
	r.Files = p.files.list()
	switch {
	case p.fatal != "":
		r.Err = errors.New(p.fatal)
	case r.Final == "" && p.lastErr != "":
		r.Err = errors.New(p.lastErr)
	}
}
