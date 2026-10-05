package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/sparkz400/relayweft/internal/event"
)

// MCP servers: Relayweft hands the configured Model Context Protocol
// servers to every agent run of the listed roles, on both CLIs (Claude Code
// through a temporary --mcp-config file, Codex through -c mcp_servers.*
// overrides). See README "MCP servers".
//
// The section runs commands on your machine, so in a repo's .relayweft.yaml
// it applies only after `rw trust` (it is one of the commandKeys).
// String values may use ${ENV_VAR}: it is filled in from your environment
// when the agent starts, so secrets need not be written into a file.

// MCPServer is one MCP server: a local command (stdio) or a URL (HTTP/SSE).
type MCPServer struct {
	Command string            `yaml:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	// Type is the transport of a url server: http (default) or sse.
	// Codex only speaks streamable HTTP, so sse servers go to Claude only.
	Type    string            `yaml:"type,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	// Providers limits the server to codex or claude (empty = both).
	Providers []string `yaml:"providers,omitempty"`
}

// MCPCfg is the mcp section.
type MCPCfg struct {
	Servers map[string]MCPServer `yaml:"servers,omitempty"`
	// Roles get the servers; empty means DefaultMCPRoles.
	Roles []string `yaml:"roles,omitempty"`
	// AllowTools pre-approves the servers' tools for Claude (mcp__<server>),
	// also for read-only roles: without it a headless Claude cannot call
	// them. Default true. Note that MCP tools may change things outside the
	// repo even for a read-only role.
	AllowTools *bool `yaml:"allow_tools,omitempty"`
	// Strict passes --strict-mcp-config to Claude: only these servers, none
	// of the user's own Claude Code MCP config.
	Strict bool `yaml:"strict,omitempty"`
}

// DefaultMCPRoles are the roles that get MCP servers when mcp.roles is
// empty: the agents that do the work. The planner, reviewer and judge only
// read the plan or the diff, and extra servers would slow every call.
var DefaultMCPRoles = []string{event.RoleWorker, event.RoleWorkerHigh, event.RoleExplorer, event.RoleResearcher}

var mcpName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// AllowsTools reports allow_tools (default true).
func (m MCPCfg) AllowsTools() bool { return m.AllowTools == nil || *m.AllowTools }

// RoleList returns the configured roles or the default.
func (m MCPCfg) RoleList() []string {
	if len(m.Roles) == 0 {
		return DefaultMCPRoles
	}
	return m.Roles
}

// For returns the sorted names of the servers a role gets on a provider.
func (m MCPCfg) For(role, provider string) []string { return m.ForKind(role, provider, provider) }

// ForKind is For for a provider whose CLI is of the given kind.
func (m MCPCfg) ForKind(role, provider, kind string) []string {
	if len(m.Servers) == 0 || !contains(m.RoleList(), role) {
		return nil
	}
	var out []string
	for name, s := range m.Servers {
		if len(s.Providers) > 0 && !contains(s.Providers, provider) {
			continue
		}
		if kind == event.Codex && s.URL != "" && s.Transport() == "sse" {
			continue // codex has no SSE client
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Names returns every server name, sorted.
func (m MCPCfg) Names() []string {
	var out []string
	for n := range m.Servers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Transport is "stdio", "http" or "sse".
func (s MCPServer) Transport() string {
	if s.URL == "" {
		return "stdio"
	}
	if strings.EqualFold(s.Type, "sse") {
		return "sse"
	}
	return "http"
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces ${NAME} with the variable's value from lookup and
// returns the names that were not set (they become ""). A plain $ is kept.
func ExpandEnv(s string, lookup func(string) (string, bool)) (string, []string) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	return out, missing
}

// Expanded returns the server with ${VAR} filled in from lookup (nil =
// the process environment) and the sorted names of unset variables.
func (s MCPServer) Expanded(lookup func(string) (string, bool)) (MCPServer, []string) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	miss := map[string]bool{}
	ex := func(v string) string {
		out, m := ExpandEnv(v, lookup)
		for _, n := range m {
			miss[n] = true
		}
		return out
	}
	exMap := func(in map[string]string) map[string]string {
		if in == nil {
			return nil
		}
		out := make(map[string]string, len(in))
		for k, v := range in {
			out[k] = ex(v)
		}
		return out
	}
	o := s
	o.Command, o.URL = ex(s.Command), ex(s.URL)
	if s.Args != nil {
		o.Args = make([]string, len(s.Args))
		for i, a := range s.Args {
			o.Args[i] = ex(a)
		}
	}
	o.Env, o.Headers = exMap(s.Env), exMap(s.Headers)
	var names []string
	for n := range miss {
		names = append(names, n)
	}
	sort.Strings(names)
	return o, names
}

// validate checks the mcp section.
func (m MCPCfg) validate() []string {
	var errs []string
	for _, name := range m.Names() {
		s := m.Servers[name]
		if !mcpName.MatchString(name) {
			errs = append(errs, fmt.Sprintf("mcp server %q: names may use only letters, digits, _ and -", name))
		}
		switch {
		case s.Command == "" && s.URL == "":
			errs = append(errs, fmt.Sprintf("mcp server %s: needs a command or a url", name))
		case s.Command != "" && s.URL != "":
			errs = append(errs, fmt.Sprintf("mcp server %s: set a command or a url, not both", name))
		}
		switch strings.ToLower(s.Type) {
		case "", "stdio", "http", "sse":
		default:
			errs = append(errs, fmt.Sprintf("mcp server %s: type must be http or sse, got %q", name, s.Type))
		}
		// Provider names are checked by Config.validateProviders.
		for k := range s.Env {
			if !mcpName.MatchString(k) {
				errs = append(errs, fmt.Sprintf("mcp server %s: env name %q may use only letters, digits, _ and -", name, k))
			}
		}
	}
	for _, r := range m.Roles {
		if !contains(event.Roles, r) {
			errs = append(errs, fmt.Sprintf("mcp.roles: unknown role %q", r))
		}
	}
	return errs
}

// Redacted returns a copy for display (rw bugreport): env and header
// values are hidden unless they only reference ${VAR}s, and URL queries
// are dropped.
func (m MCPCfg) Redacted() MCPCfg {
	hide := func(in map[string]string) map[string]string {
		if in == nil {
			return nil
		}
		out := make(map[string]string, len(in))
		for k, v := range in {
			if strings.TrimSpace(envRef.ReplaceAllString(v, "")) == "" {
				out[k] = v
			} else {
				out[k] = "<hidden>"
			}
		}
		return out
	}
	o := m
	if m.Servers != nil {
		o.Servers = make(map[string]MCPServer, len(m.Servers))
		for n, s := range m.Servers {
			s.Env, s.Headers = hide(s.Env), hide(s.Headers)
			if i := strings.IndexAny(s.URL, "?#"); i >= 0 {
				s.URL = s.URL[:i] + "?<hidden>"
			}
			o.Servers[n] = s
		}
	}
	return o
}
