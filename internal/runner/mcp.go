package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

// MCPRun is the MCP setup of one agent run. Exec fills Spec.MCP from its
// config (callers leave it nil); ClaudeArgs and CodexArgs turn it into
// flags.
type MCPRun struct {
	Names   []string                    // sorted
	Servers map[string]config.MCPServer // ${VAR} already expanded
	// ConfigFile is Claude's temporary {"mcpServers": ...} file.
	ConfigFile string
	Strict     bool
	AllowTools bool
	Missing    []string // ${VAR}s that were not set (names only)
}

// PrepareMCP builds the MCP setup for a role on a provider; nil when it
// gets no servers. For Claude it writes the config file into a fresh temp
// dir; cleanup removes it (always safe to call). lookup reads environment
// variables (nil = the process environment).
func PrepareMCP(provider, role string, m config.MCPCfg, lookup func(string) (string, bool)) (run *MCPRun, cleanup func(), err error) {
	cleanup = func() {}
	names := m.For(role, provider)
	if len(names) == 0 {
		return nil, cleanup, nil
	}
	run = &MCPRun{Names: names, Servers: map[string]config.MCPServer{}, Strict: m.Strict, AllowTools: m.AllowsTools()}
	miss := map[string]bool{}
	for _, n := range names {
		s, missing := m.Servers[n].Expanded(lookup)
		run.Servers[n] = s
		for _, v := range missing {
			miss[v] = true
		}
	}
	for v := range miss {
		run.Missing = append(run.Missing, v)
	}
	sort.Strings(run.Missing)
	if provider != event.Claude {
		return run, cleanup, nil
	}
	dir, err := os.MkdirTemp("", "sy-mcp-")
	if err != nil {
		return nil, cleanup, fmt.Errorf("mcp config: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }
	data, err := ClaudeMCPJSON(run.Servers)
	if err == nil {
		run.ConfigFile = filepath.Join(dir, "mcp.json")
		// 0600: env values and headers may hold secrets.
		err = os.WriteFile(run.ConfigFile, data, 0o600)
	}
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("mcp config: %w", err)
	}
	return run, cleanup, nil
}

// ClaudeMCPJSON is the content of Claude Code's --mcp-config file:
//
//	{"mcpServers": {"docs": {"type": "stdio", "command": "npx", "args": [...], "env": {...}},
//	                "db": {"type": "http", "url": "...", "headers": {...}}}}
func ClaudeMCPJSON(servers map[string]config.MCPServer) ([]byte, error) {
	type entry struct {
		Type    string            `json:"type"`
		Command string            `json:"command,omitempty"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		URL     string            `json:"url,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	out := map[string]entry{}
	for n, s := range servers {
		e := entry{Type: s.Transport()}
		if e.Type == "stdio" {
			e.Command, e.Args, e.Env = s.Command, s.Args, s.Env
		} else {
			e.URL, e.Headers = s.URL, s.Headers
		}
		out[n] = e
	}
	return json.MarshalIndent(map[string]any{"mcpServers": out}, "", "  ")
}

// codexMCPArgs are Codex's -c overrides for the servers (config.toml's
// [mcp_servers.<name>] table, given inline). Values are TOML.
//
// Checked against the Codex config docs (mcp_servers.<name>.command, args,
// env, url, http_headers); url servers need a Codex with streamable HTTP
// MCP support (older versions also needed experimental_use_rmcp_client).
// Check with `codex --help` / the Codex config reference.
func codexMCPArgs(m *MCPRun) []string {
	if m == nil {
		return nil
	}
	var args []string
	for _, n := range m.Names {
		s := m.Servers[n]
		key := "mcp_servers." + n + "."
		if s.URL != "" {
			args = append(args, "-c", key+"url="+tomlString(s.URL))
			if len(s.Headers) > 0 {
				args = append(args, "-c", key+"http_headers="+tomlTable(s.Headers))
			}
			continue
		}
		args = append(args, "-c", key+"command="+tomlString(s.Command))
		if len(s.Args) > 0 {
			args = append(args, "-c", key+"args="+tomlArray(s.Args))
		}
		if len(s.Env) > 0 {
			args = append(args, "-c", key+"env="+tomlTable(s.Env))
		}
	}
	return args
}

// claudeMCPArgs are Claude's flags before the permission flags. Both
// --mcp-config and --allowedTools are variadic, so --mcp-config is never
// last (the permission mode always follows it).
func claudeMCPArgs(m *MCPRun) []string {
	if m == nil || m.ConfigFile == "" {
		return nil
	}
	args := []string{"--mcp-config", m.ConfigFile}
	if m.Strict {
		args = append(args, "--strict-mcp-config")
	}
	return args
}

// claudeMCPTools are the allowedTools entries: "mcp__<server>" allows every
// tool of that server.
func claudeMCPTools(m *MCPRun) []string {
	if m == nil || !m.AllowTools || m.ConfigFile == "" {
		return nil
	}
	out := make([]string, len(m.Names))
	for i, n := range m.Names {
		out[i] = "mcp__" + n
	}
	return out
}

// tomlString writes a TOML string. A literal string ('...') is preferred:
// it needs no escapes, so Windows paths keep their backslashes and no
// double quotes reach cmd.exe when the CLI is an npm .cmd shim.
func tomlString(s string) string {
	literal := !strings.ContainsAny(s, "'\r\n")
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f {
			literal = false
		}
	}
	if literal {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlArray(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = tomlString(x)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlTable(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		kk := k
		if !bareKey.MatchString(k) {
			kk = strconv.Quote(k)
		}
		parts[i] = kk + " = " + tomlString(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// redactArgs hides MCP values for the diag log: server names stay, values
// (commands, env, headers, URLs with tokens) do not.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "mcp_servers.") {
			if k, _, ok := strings.Cut(a, "="); ok {
				a = k + "=<hidden>"
			}
		}
		out[i] = a
	}
	return out
}
