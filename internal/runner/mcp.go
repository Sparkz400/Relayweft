package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

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
	// Codex only: values that came from ${VAR} expansion are not put on
	// the command line (other processes can read it); ChildEnv holds them
	// as NAME=value for the CLI's environment and Codex forwards them by
	// name (codexSecrets).
	ChildEnv []string
	Secrets  map[string]codexSecrets
}

// codexSecrets are one server's values that reach Codex through its
// environment instead of -c:
//
//   - EnvVars: mcp_servers.<name>.env_vars, names Codex copies from its own
//     environment into the stdio server's.
//   - BearerEnv: mcp_servers.<name>.bearer_token_env_var (an
//     "Authorization: Bearer ..." header of a url server).
//   - HeaderEnv: mcp_servers.<name>.env_http_headers, header -> variable.
//
// Keys checked against the Codex config reference (MCP servers section).
type codexSecrets struct {
	EnvVars   []string
	BearerEnv string
	HeaderEnv map[string]string
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
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, n := range names {
		s, missing := m.Servers[n].Expanded(lookup)
		if provider == event.Codex {
			s = run.moveSecrets(n, m.Servers[n], s, lookup)
		}
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
	dir, err := mcpTempDir()
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

// mcpRoot is sy's own directory for Claude's MCP config files: the user's
// cache dir (not a shared /tmp another user could prepare), falling back
// to the temp dir.
var mcpRoot = func() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "switchyard", "mcp")
	}
	return filepath.Join(os.TempDir(), "switchyard-mcp")
}

var sweepOnce sync.Once

// mcpStale is how old a left-over sy-mcp-* dir must be to be removed.
const mcpStale = 24 * time.Hour

// mcpTempDir makes a fresh sy-mcp-* dir under mcpRoot. The first call of a
// process removes sy-mcp-* dirs older than a day: a hard kill (power loss,
// taskkill of sy) skips the cleanup and would leave secrets on disk.
func mcpTempDir() (string, error) {
	root := mcpRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	sweepOnce.Do(func() {
		sweepMCP(root, time.Now())
		sweepMCP(os.TempDir(), time.Now()) // where older versions put them
	})
	return os.MkdirTemp(root, "sy-mcp-")
}

// sweepMCP removes sy-mcp-* entries of dir last changed before now-mcpStale.
func sweepMCP(dir string, now time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "sy-mcp-") {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > mcpStale {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// moveSecrets takes the values of a Codex server that came from ${VAR}
// expansion off the command line: they go into ChildEnv and the server
// config names them (codexSecrets). raw is the configured server, s the
// expanded one; the returned server keeps only what may go into -c.
//
// Not covered (they stay -c values, visible in the process list): ${VAR}
// in command, args and url, and an env value whose name the environment
// already has with another value (setting it for Codex would change Codex's
// own environment, e.g. OPENAI_API_KEY). Literal values in the config are
// not treated as secrets: use ${VAR} for secrets.
func (run *MCPRun) moveSecrets(name string, raw, s config.MCPServer, lookup func(string) (string, bool)) config.MCPServer {
	var sec codexSecrets
	if len(raw.Env) > 0 {
		env := map[string]string{}
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := s.Env[k]
			env[k] = v
			if !strings.Contains(raw.Env[k], "${") || !envName.MatchString(k) {
				continue
			}
			if cur, ok := lookup(k); ok && cur != v {
				continue // would change Codex's own variable
			}
			if cur, ok := run.childEnv(k); ok && cur != v {
				continue // another server already uses the name with another value
			}
			sec.EnvVars = append(sec.EnvVars, k)
			run.ChildEnv = append(run.ChildEnv, k+"="+v)
			delete(env, k)
		}
		s.Env = env
	}
	if s.URL != "" && len(raw.Headers) > 0 {
		hdr := map[string]string{}
		keys := make([]string, 0, len(s.Headers))
		for k := range s.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		prefix := "SY_MCP_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
		for i, k := range keys {
			v := s.Headers[k]
			if !strings.Contains(raw.Headers[k], "${") {
				hdr[k] = v
				continue
			}
			if tok, ok := strings.CutPrefix(v, "Bearer "); ok && strings.EqualFold(k, "Authorization") {
				sec.BearerEnv = prefix + "BEARER"
				run.ChildEnv = append(run.ChildEnv, sec.BearerEnv+"="+tok)
				continue
			}
			if sec.HeaderEnv == nil {
				sec.HeaderEnv = map[string]string{}
			}
			ev := fmt.Sprintf("%sHEADER_%d", prefix, i)
			sec.HeaderEnv[k] = ev
			run.ChildEnv = append(run.ChildEnv, ev+"="+v)
		}
		s.Headers = hdr
	}
	if len(sec.EnvVars) > 0 || sec.BearerEnv != "" || len(sec.HeaderEnv) > 0 {
		if run.Secrets == nil {
			run.Secrets = map[string]codexSecrets{}
		}
		run.Secrets[name] = sec
	}
	return s
}

// childEnv looks a name up in ChildEnv.
func (run *MCPRun) childEnv(name string) (string, bool) {
	for _, kv := range run.ChildEnv {
		if k, v, _ := strings.Cut(kv, "="); strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
// Check with `codex --help` / the Codex config reference. Values that came
// from ${VAR} go through the environment instead (moveSecrets): env_vars,
// bearer_token_env_var and env_http_headers.
func codexMCPArgs(m *MCPRun) []string {
	if m == nil {
		return nil
	}
	var args []string
	for _, n := range m.Names {
		s := m.Servers[n]
		sec := m.Secrets[n]
		key := "mcp_servers." + n + "."
		if s.URL != "" {
			args = append(args, "-c", key+"url="+tomlString(s.URL))
			if len(s.Headers) > 0 {
				args = append(args, "-c", key+"http_headers="+tomlTable(s.Headers))
			}
			if sec.BearerEnv != "" {
				args = append(args, "-c", key+"bearer_token_env_var="+tomlString(sec.BearerEnv))
			}
			if len(sec.HeaderEnv) > 0 {
				args = append(args, "-c", key+"env_http_headers="+tomlTable(sec.HeaderEnv))
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
		if len(sec.EnvVars) > 0 {
			args = append(args, "-c", key+"env_vars="+tomlArray(sec.EnvVars))
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

// tomlString writes a TOML string for a Codex -c value. On Windows the
// value passes through cmd.exe (npm .cmd shim) and the CLI's argv parser,
// so it must not carry what either one interprets:
//
//   - A literal string ('...') needs no escapes, so Windows paths keep
//     their backslashes; it is used when s has no ', ", %, ! or control
//     characters.
//   - Otherwise a basic string with \uXXXX escapes for " and \ (so no
//     quote or backslash-quote can end the argv entry early; proc.CmdQuote
//     handles them too, this is defence in depth) and for % and ! (cmd.exe
//     expands %NAME% even inside quotes, and !NAME! with delayed
//     expansion).
func tomlString(s string) string {
	literal := !strings.ContainsAny(s, "'\"%!\r\n")
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f {
			literal = false
		}
	}
	if literal {
		return "'" + s + "'"
	}
	return tomlBasic(s)
}

// tomlBasic is a TOML basic string ("...") whose only " are its delimiters
// and whose only backslashes start \uXXXX, \n, \r or \t escapes.
func tomlBasic(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\', '%', '!':
			fmt.Fprintf(&b, `\u%04X`, r)
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
			kk = tomlBasic(k)
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
