package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sparkz400/relayweft/internal/event"
)

// GenericCfg describes a CLI rw has no built-in support for (kind: generic):
// the command line, how to read its output, how to resume a session and how
// it says it is at a usage limit. The prompt always goes in on stdin.
//
// Placeholders: {model} and {effort} (the route's, checked like every route),
// {session} (in resume_args only).
type GenericCfg struct {
	Args       []string `yaml:"args,omitempty"`        // on every run, first
	ModelArgs  []string `yaml:"model_args,omitempty"`  // when the route has a model, e.g. ["-m", "{model}"]
	EffortArgs []string `yaml:"effort_args,omitempty"` // when the route has an effort
	// ReadOnlyArgs are added for read-only agents and must keep the CLI from
	// editing files or running commands (a plan mode). Without them a CLI
	// with tools takes no read-only work: rw cannot check what it does.
	ReadOnlyArgs []string `yaml:"read_only_args,omitempty"`
	// WriteArgs are added for writing agents. Without them the provider
	// takes read-only work only.
	WriteArgs []string `yaml:"write_args,omitempty"`
	// ResumeArgs continue an earlier session (a follow-up), e.g.
	// ["--resume", "{session}"]. Without them every follow-up starts a fresh
	// agent with the earlier one's context.
	ResumeArgs []string `yaml:"resume_args,omitempty"`
	// NoTools: the CLI only answers the prompt (a plain model such as
	// `ollama run`); it cannot read files or run commands, so it is
	// read-only by nature and needs no read_only_args.
	NoTools bool `yaml:"no_tools,omitempty"`
	// Output is how stdout is read: text (the whole output is the answer)
	// or jsonl (one JSON object per line, read with JSON rules).
	Output string `yaml:"output,omitempty"`
	// JSON are the rules for output: jsonl.
	JSON []JSONRule `yaml:"json,omitempty"`
	// Session (text output) is a regular expression over stdout and stderr
	// whose first group is the session id.
	Session string `yaml:"session,omitempty"`
	// Usage (text output) are regular expressions over stdout and stderr
	// whose first group is a token count.
	Usage UsagePatterns `yaml:"usage,omitempty"`
	// InputExcludesCached: the input count does not include the cached
	// part (rw counts input with the cache included).
	InputExcludesCached bool `yaml:"input_excludes_cached,omitempty"`
	// EditTools are the tool names that change files (jsonl `tool`).
	EditTools []string `yaml:"edit_tools,omitempty"`
	// LimitPatterns are added to the global limit_patterns for this CLI.
	LimitPatterns []string `yaml:"limit_patterns,omitempty"`
	// LimitExitCodes are exit codes that mean "at the usage limit".
	LimitExitCodes []int `yaml:"limit_exit_codes,omitempty"`
}

// UsagePatterns find token counts in a text CLI's output.
type UsagePatterns struct {
	Input  string `yaml:"input,omitempty"`
	Cached string `yaml:"cached,omitempty"`
	Output string `yaml:"output,omitempty"`
}

// JSONRule reads one kind of output line. A rule applies when every Match
// path has its value ("*" = present); every rule that matches applies. A
// path is dot-separated keys and array indexes (message.content.0.text).
type JSONRule struct {
	Match map[string]string `yaml:"match,omitempty"`
	// Each applies Rules to every element of the array at this path
	// (Claude-style content blocks).
	Each  string     `yaml:"each,omitempty"`
	Rules []JSONRule `yaml:"rules,omitempty"`

	Session  string `yaml:"session,omitempty"`  // the session id
	Text     string `yaml:"text,omitempty"`     // assistant text
	Delta    bool   `yaml:"delta,omitempty"`    // text is a chunk of a streamed message
	Final    string `yaml:"final,omitempty"`    // the run's answer
	Thinking string `yaml:"thinking,omitempty"` // progress shown, not kept
	Tool     string `yaml:"tool,omitempty"`     // a tool call's name
	// ToolInput is the tool call's input object: its file_path, path,
	// command... is shown, and is the edited file for edit_tools.
	ToolInput string `yaml:"tool_input,omitempty"`
	// Error is an error message. It fails the run when the run ends
	// without an answer, or always with Fatal.
	Error string `yaml:"error,omitempty"`
	Fatal bool   `yaml:"fatal,omitempty"`
	// Limit: a matching line means the usage limit was hit (the error text,
	// if any, may say when it resets).
	Limit bool `yaml:"limit,omitempty"`

	InputTokens  string `yaml:"input_tokens,omitempty"`
	CachedTokens string `yaml:"cached_tokens,omitempty"`
	OutputTokens string `yaml:"output_tokens,omitempty"`
}

// Generic output formats.
const (
	OutputText  = "text"
	OutputJSONL = "jsonl"
)

// CanWrite reports whether the provider may run writing agents: every
// built-in CLI can; a generic one only with write_args.
func (p ProviderCfg) CanWrite(name string) bool {
	if p.KindOf(name) != event.Generic {
		return true
	}
	return p.Generic != nil && len(p.Generic.WriteArgs) > 0 && !p.Generic.NoTools
}

// CanReadOnly reports whether the provider may run read-only agents: a
// generic CLI only when it has no tools or a read-only mode.
func (p ProviderCfg) CanReadOnly(name string) bool {
	if p.KindOf(name) != event.Generic {
		return true
	}
	return p.Generic != nil && (p.Generic.NoTools || len(p.Generic.ReadOnlyArgs) > 0)
}

// placeholder finds {name} in an argument.
var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

// validateGeneric checks a provider's generic: section.
func validateGeneric(name string, pc ProviderCfg) []string {
	var errs []string
	add := func(f string, a ...any) {
		errs = append(errs, fmt.Sprintf("provider %s: generic: "+f, append([]any{name}, a...)...))
	}
	g := pc.Generic
	if pc.KindOf(name) != event.Generic {
		if g != nil {
			errs = append(errs, fmt.Sprintf("provider %s: a generic: section needs kind: generic", name))
		}
		return errs
	}
	if g == nil {
		return []string{fmt.Sprintf("provider %s: kind generic needs a generic: section (args, output...)", name)}
	}
	args := func(field string, list []string, allowed ...string) {
		for _, a := range list {
			for _, m := range placeholder.FindAllStringSubmatch(a, -1) {
				if !contains(allowed, m[1]) {
					add("%s: {%s} is not known here (use %s)", field, m[1], braced(allowed))
				}
			}
		}
	}
	args("args", g.Args, "model", "effort")
	args("model_args", g.ModelArgs, "model")
	args("effort_args", g.EffortArgs, "effort")
	args("read_only_args", g.ReadOnlyArgs, "model", "effort")
	args("write_args", g.WriteArgs, "model", "effort")
	args("resume_args", g.ResumeArgs, "session")
	if len(g.ResumeArgs) > 0 && !strings.Contains(strings.Join(g.ResumeArgs, " "), "{session}") {
		add("resume_args must contain {session}")
	}
	if g.NoTools && len(g.WriteArgs) > 0 {
		add("no_tools and write_args together: a CLI without tools cannot edit files")
	}
	if !g.NoTools && len(g.ReadOnlyArgs) == 0 && len(g.WriteArgs) == 0 {
		add("set no_tools (a plain model), read_only_args (its read-only mode) or write_args, or it can run nothing")
	}
	re := func(field, pat string, groups bool) {
		if pat == "" {
			return
		}
		r, err := regexp.Compile("(?im)" + pat)
		if err != nil {
			add("%s: %v", field, err)
		} else if groups && r.NumSubexp() < 1 {
			add("%s %q needs a (group) around the value", field, pat)
		}
	}
	for _, p := range g.LimitPatterns {
		re("limit_patterns", p, false)
	}
	switch g.Output {
	case "", OutputText:
		if len(g.JSON) > 0 {
			add("json rules need output: jsonl")
		}
		re("session", g.Session, true)
		re("usage.input", g.Usage.Input, true)
		re("usage.cached", g.Usage.Cached, true)
		re("usage.output", g.Usage.Output, true)
		if g.Session != "" && len(g.ResumeArgs) == 0 {
			add("session is read but there are no resume_args to use it")
		}
	case OutputJSONL:
		if g.Session != "" || g.Usage != (UsagePatterns{}) {
			add("session and usage patterns are for output: text; use json rules")
		}
		answers := false
		var walk func(rs []JSONRule, depth int)
		walk = func(rs []JSONRule, depth int) {
			for _, r := range rs {
				if r.Text != "" || r.Final != "" {
					answers = true
				}
				if depth > 2 {
					add("json rules nest at most 3 deep")
					return
				}
				if (r.Each == "") != (len(r.Rules) == 0) {
					add("json rule: each and rules go together")
				}
				walk(r.Rules, depth+1)
			}
		}
		walk(g.JSON, 0)
		if !answers {
			add("output: jsonl needs a json rule with text or final (the answer)")
		}
	default:
		add("output must be text or jsonl, got %q", g.Output)
	}
	return errs
}

func braced(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "{" + n + "}"
	}
	return strings.Join(out, ", ")
}

// StandsBy reports whether the provider stands by for a role: it takes
// that role's work when every other provider is at or near its limit.
func (p ProviderCfg) StandsBy(role string) bool { return contains(p.Standby, role) }

// MayStandIn reports whether a provider may take a role's work for another
// provider: as a fallback (not only_preferred) or on standby for the role.
func (p ProviderCfg) MayStandIn(role string) bool { return !p.OnlyPreferred || p.StandsBy(role) }
