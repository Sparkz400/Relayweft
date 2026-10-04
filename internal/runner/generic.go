package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

// NewGeneric returns a runner for a CLI described in the config
// (kind: generic): its arguments, output format, resume and limits all come
// from providers.<name>.generic. Its limit patterns add to the global ones.
func NewGeneric(name string, cfg config.ProviderCfg, patterns []string) *Exec {
	g := cfg.Generic
	if g == nil {
		g = &config.GenericCfg{} // refused by validation; never panic here
	}
	return &Exec{
		Provider:       name,
		Kind:           event.Generic,
		Cfg:            cfg,
		Detector:       limits.NewDetector(append(append([]string(nil), patterns...), g.LimitPatterns...)),
		args:           func(s Spec) []string { return GenericArgs(cfg, s) },
		parser:         func() lineParser { return newGenericParser(g) },
		precheck:       genericPrecheck(name, cfg),
		limitExitCodes: g.LimitExitCodes,
	}
}

// GenericArgs builds the argument list: args, model_args, effort_args, then
// read_only_args or write_args, resume_args and extra_args.
func GenericArgs(cfg config.ProviderCfg, s Spec) []string {
	g := cfg.Generic
	if g == nil {
		return nil
	}
	fill := func(list []string) []string {
		out := make([]string, len(list))
		for i, a := range list {
			out[i] = strings.NewReplacer("{model}", s.Model, "{effort}", s.Effort, "{session}", s.Resume).Replace(a)
		}
		return out
	}
	args := fill(g.Args)
	if s.Model != "" {
		args = append(args, fill(g.ModelArgs)...)
	}
	if s.Effort != "" {
		args = append(args, fill(g.EffortArgs)...)
	}
	if s.ReadOnly {
		args = append(args, fill(g.ReadOnlyArgs)...)
	} else {
		args = append(args, fill(g.WriteArgs)...)
	}
	if s.Resume != "" {
		args = append(args, fill(g.ResumeArgs)...)
	}
	return append(args, cfg.ExtraArgs...)
}

// sessionID is what a session id taken from a CLI's output may look like
// before it goes back on a command line (cmd.exe parses npm .cmd shims).
var sessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,127}$`)

// genericPrecheck refuses work the described CLI cannot do safely: writing
// without write_args, read-only work without a read-only mode, and a resume
// it has no arguments for.
func genericPrecheck(name string, cfg config.ProviderCfg) func(Spec) error {
	return func(s Spec) error {
		if cfg.Generic == nil {
			return fmt.Errorf("providers.%s has no generic: section", name)
		}
		switch {
		case s.ReadOnly && !cfg.CanReadOnly(name):
			return fmt.Errorf("providers.%s.generic has no read_only_args and is not no_tools, so sy cannot keep it read-only; route read-only work elsewhere", name)
		case !s.ReadOnly && !cfg.CanWrite(name):
			return fmt.Errorf("providers.%s takes read-only work only (no generic.write_args); route writing work elsewhere", name)
		}
		if s.Resume != "" {
			if len(cfg.Generic.ResumeArgs) == 0 {
				return fmt.Errorf("providers.%s cannot resume a session (no generic.resume_args)", name)
			}
			if !sessionID.MatchString(s.Resume) {
				return fmt.Errorf("session id %q is not safe to pass on a command line", clipStr(s.Resume, 40))
			}
		}
		return nil
	}
}

func newGenericParser(g *config.GenericCfg) lineParser {
	if g.Output == config.OutputJSONL {
		return &jsonRuleParser{g: g}
	}
	return &textParser{g: g}
}

// ansi matches terminal escape sequences (spinners, colours, cursor moves).
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[@-Z\\-_]`)

// stripANSI removes escape sequences and the carriage returns spinners use.
func stripANSI(s string) string {
	s = ansi.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "\r"); i >= 0 {
		s = s[i+1:] // what a terminal would show last on this line
	}
	return s
}

// textParser reads a CLI whose stdout is the answer. Each line is shown as
// progress; the session id and token counts come from regular expressions
// over stdout and stderr.
type textParser struct {
	g      *config.GenericCfg
	out    strings.Builder
	stderr string
}

func (p *textParser) keepBlankLines() {}

func (p *textParser) Line(line []byte) []event.Event {
	s := stripANSI(strings.TrimRight(string(line), "\r\n"))
	p.out.WriteString(s + "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return []event.Event{{Kind: event.Thinking, Text: s}}
}

func (p *textParser) Stderr(s string) { p.stderr = s }

func (p *textParser) Finish(r *Result) {
	r.Final = strings.TrimSpace(p.out.String())
	all := p.out.String() + "\n" + stripLinesANSI(p.stderr)
	if len(p.g.ResumeArgs) > 0 {
		if id := firstGroup(p.g.Session, all); sessionID.MatchString(id) {
			r.SessionID = id
		}
	}
	r.Tokens = event.TokenUsage{
		Input:  firstInt(p.g.Usage.Input, all),
		Cached: firstInt(p.g.Usage.Cached, all),
		Output: firstInt(p.g.Usage.Output, all),
	}
	if p.g.InputExcludesCached {
		r.Tokens.Input += r.Tokens.Cached
	}
}

func stripLinesANSI(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		// A CRLF line end is not a spinner's carriage return: without the
		// trim, stripANSI would keep only the empty rest after it.
		lines[i] = stripANSI(strings.TrimSuffix(l, "\r"))
	}
	return strings.Join(lines, "\n")
}

// firstGroup returns the first group of pat's first match in s ("" = none).
func firstGroup(pat, s string) string {
	if pat == "" {
		return ""
	}
	re, err := regexp.Compile("(?im)" + pat)
	if err != nil {
		return "" // refused by validation
	}
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func firstInt(pat, s string) int64 {
	n, _ := strconv.ParseInt(strings.ReplaceAll(firstGroup(pat, s), ",", ""), 10, 64)
	return n
}

// jsonRuleParser reads a JSON-lines CLI with the config's rules.
type jsonRuleParser struct {
	g       *config.GenericCfg
	session string
	text    strings.Builder // a streamed (delta) message being assembled
	lastMsg string
	final   string
	tokens  event.TokenUsage
	fatal   string
	lastErr string
	limit   bool
	files   fileSet
}

func (p *jsonRuleParser) Line(line []byte) []event.Event {
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		if s := strings.TrimSpace(stripANSI(string(line))); s != "" {
			return append(p.flush(), event.Event{Kind: event.Thinking, Text: s})
		}
		return nil
	}
	var out []event.Event
	streamed := false
	p.apply(p.g.JSON, v, &out, &streamed)
	if !streamed {
		// Anything but a chunk ends a streamed message.
		out = append(p.flush(), out...)
	}
	return out
}

func (p *jsonRuleParser) flush() []event.Event {
	t := p.text.String()
	p.text.Reset()
	if strings.TrimSpace(t) == "" {
		return nil
	}
	p.lastMsg = t
	return []event.Event{{Kind: event.Message, Text: t}}
}

func (p *jsonRuleParser) apply(rules []config.JSONRule, v any, out *[]event.Event, streamed *bool) {
	for _, r := range rules {
		if !matches(r.Match, v) {
			continue
		}
		if r.Each != "" {
			if arr, ok := jsonPath(v, r.Each).([]any); ok {
				for _, el := range arr {
					p.apply(r.Rules, el, out, streamed)
				}
			}
		}
		if s := str(jsonPath(v, r.Session)); r.Session != "" && s != "" {
			p.session = s
		}
		if r.Text != "" {
			if s := str(jsonPath(v, r.Text)); s != "" {
				if r.Delta {
					p.text.WriteString(s)
					*streamed = true
				} else {
					*out = append(*out, p.flush()...)
					p.lastMsg = s
					*out = append(*out, event.Event{Kind: event.Message, Text: s})
				}
			}
		}
		if s := str(jsonPath(v, r.Thinking)); r.Thinking != "" && strings.TrimSpace(s) != "" {
			*out = append(*out, event.Event{Kind: event.Thinking, Text: s})
		}
		if r.Final != "" {
			if s := str(jsonPath(v, r.Final)); s != "" {
				p.final = s
			}
		}
		if r.Tool != "" {
			if name := str(jsonPath(v, r.Tool)); name != "" {
				arg := ""
				if r.ToolInput != "" {
					raw, _ := json.Marshal(jsonPath(v, r.ToolInput))
					arg = toolArg(raw)
				}
				if contains(p.g.EditTools, name) {
					p.files.add(arg)
					*out = append(*out, event.Event{Kind: event.FileEdit, Text: arg})
				} else {
					*out = append(*out, event.Event{Kind: event.ToolCall, Text: strings.TrimSpace(name + " " + arg)})
				}
			}
		}
		if r.Error != "" || r.Limit {
			msg := strings.TrimSpace(str(jsonPath(v, r.Error)))
			if msg == "" {
				msg = "error"
				if r.Limit {
					msg = "usage limit reached"
				}
			}
			kind := event.Error
			if r.Limit {
				kind, p.limit = event.LimitHit, true
			}
			p.lastErr = msg
			if r.Fatal {
				p.fatal = msg
			}
			*out = append(*out, event.Event{Kind: kind, Text: msg})
		}
		if n, ok := num(jsonPath(v, r.InputTokens)); r.InputTokens != "" && ok {
			p.tokens.Input = n
		}
		if n, ok := num(jsonPath(v, r.CachedTokens)); r.CachedTokens != "" && ok {
			p.tokens.Cached = n
		}
		if n, ok := num(jsonPath(v, r.OutputTokens)); r.OutputTokens != "" && ok {
			p.tokens.Output = n
		}
	}
}

func (p *jsonRuleParser) Flush() []event.Event { return p.flush() }

func (p *jsonRuleParser) Finish(r *Result) {
	p.flush()
	r.Final = p.final
	if r.Final == "" {
		r.Final = p.lastMsg
	}
	if len(p.g.ResumeArgs) > 0 && sessionID.MatchString(p.session) {
		r.SessionID = p.session
	}
	r.Tokens = p.tokens
	if p.g.InputExcludesCached {
		r.Tokens.Input += r.Tokens.Cached
	}
	r.Files = p.files.list()
	// Exec clears a limit the CLI recovered from (it still answered).
	r.LimitHit = p.limit
	switch {
	case p.fatal != "":
		r.Err = errors.New(p.fatal)
	case strings.TrimSpace(r.Final) == "" && p.lastErr != "":
		r.Err = errors.New(p.lastErr)
	}
}

// jsonPath follows a dot path (keys and array indexes) into decoded JSON.
func jsonPath(v any, path string) any {
	if path == "" {
		return nil
	}
	for _, k := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			v = t[k]
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(t) {
				return nil
			}
			v = t[i]
		default:
			return nil
		}
	}
	return v
}

// matches reports whether every path has its value ("*" = present).
func matches(m map[string]string, v any) bool {
	for path, want := range m {
		got := jsonPath(v, path)
		if got == nil {
			return false
		}
		if want != "*" && str(got) != want {
			return false
		}
	}
	return true
}

// str renders a JSON scalar as text (objects and arrays: "").
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func num(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
