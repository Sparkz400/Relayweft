package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/proc"
)

// mcpEntry is an MCP server registration found in a client's config.
type mcpEntry struct {
	client, where, name string
	command             string
	args                []string
}

// isRWMCP reports whether the entry starts `rw mcp`, directly or through a
// wrapper (cmd /c rw mcp, npx-style launchers): an rw program followed by
// the word mcp.
func (e mcpEntry) isRWMCP() bool {
	words := append([]string{e.command}, e.args...)
	for i, w := range words[:len(words)-1] {
		if proc.ProgramName(w) == "rw" && words[i+1] == "mcp" {
			return true
		}
	}
	return false
}

// rwProgram is the rw the entry starts.
func (e mcpEntry) rwProgram() string {
	for _, w := range append([]string{e.command}, e.args...) {
		if proc.ProgramName(w) == "rw" {
			return w
		}
	}
	return e.command
}

// doctorMCPServe checks where `rw mcp` is set up as an MCP server: Claude
// Code (the project's .mcp.json, ~/.claude.json for user and local scope)
// and Codex (~/.codex/config.toml, the project's .codex/config.toml). It
// shows commands only, never env values or headers.
func doctorMCPServe(w io.Writer, cfg *config.Config, dir string, ok func(bool) string, warn string) (problems int) {
	home, _ := os.UserHomeDir()
	var found []mcpEntry
	add := func(es []mcpEntry, err error, path string) {
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(w, "%s rw mcp      could not read %s: %v\n", warn, path, err)
		}
		for _, e := range es {
			if e.isRWMCP() {
				found = append(found, e)
			}
		}
	}
	p := filepath.Join(dir, ".mcp.json")
	es, err := claudeMCPFile(p, "", "Claude Code", "project .mcp.json")
	add(es, err, p)
	claudeDir := home
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		claudeDir = d
	}
	p = filepath.Join(claudeDir, ".claude.json")
	es, err = claudeMCPFile(p, dir, "Claude Code", p)
	add(es, err, p)
	codexDir := filepath.Join(home, ".codex")
	if d := os.Getenv("CODEX_HOME"); d != "" {
		codexDir = d
	}
	for _, p := range []string{filepath.Join(codexDir, "config.toml"), filepath.Join(dir, ".codex", "config.toml")} {
		es, err := codexMCPFile(p)
		add(es, err, p)
	}
	self, _ := os.Executable()
	for _, e := range found {
		prog := e.rwProgram()
		bin, err := proc.Resolve(prog)
		switch {
		case err != nil:
			problems++
			fmt.Fprintf(w, "%s rw mcp      %s (%s, %q): %q not found on PATH\n", ok(false), e.client, e.where, e.name, prog)
		case self != "" && !sameFile(bin, self):
			fmt.Fprintf(w, "%s rw mcp      %s (%s, %q) starts %s, not this rw (%s)\n", warn, e.client, e.where, e.name, bin, self)
		default:
			fmt.Fprintf(w, "%s rw mcp      %s (%s, %q): %s\n", ok(true), e.client, e.where, e.name, bin)
		}
	}
	if len(found) == 0 {
		fmt.Fprintf(w, "%s rw mcp      not set up for Claude Code or Codex here (optional; see docs/mcp.md)\n", stMuted.Render("info"))
	}
	// rw's own agents get the servers of the mcp: section. rw mcp there
	// would only refuse (recursion guard) and cost each agent a start.
	for _, name := range cfg.MCP.Names() {
		s := cfg.MCP.Servers[name]
		if (mcpEntry{command: s.Command, args: s.Args}).isRWMCP() {
			fmt.Fprintf(w, "%s rw mcp      your mcp: section gives rw's own agents rw mcp (%s); it refuses tasks under them, so remove it\n", warn, name)
		}
	}
	return problems
}

// sameFile is samePath after symlinks (Homebrew, winget links).
func sameFile(a, b string) bool {
	if samePath(a, b) {
		return true
	}
	// Resolving touches the disk (a network share can be slow): only for
	// paths that could be the same.
	if !strings.EqualFold(filepath.Base(a), filepath.Base(b)) {
		return false
	}
	if ea, err := filepath.EvalSymlinks(a); err == nil {
		a = ea
	}
	if eb, err := filepath.EvalSymlinks(b); err == nil {
		b = eb
	}
	return samePath(a, b)
}

// claudeMCPFile reads the mcpServers of a Claude Code config: a project's
// .mcp.json, or ~/.claude.json (user scope, and with project set the local
// scope of that folder).
func claudeMCPFile(path, project, client, where string) ([]mcpEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	type servers map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	var f struct {
		MCPServers servers `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers servers `json:"mcpServers"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	var out []mcpEntry
	list := func(s servers, where string) {
		for name, v := range s {
			out = append(out, mcpEntry{client: client, where: where, name: name, command: v.Command, args: v.Args})
		}
	}
	if project == "" {
		list(f.MCPServers, where)
		return out, nil
	}
	list(f.MCPServers, "user scope, "+where)
	for p, v := range f.Projects {
		if sameFile(p, project) {
			list(v.MCPServers, "local scope, "+where)
		}
	}
	return out, nil
}

// codexMCPFile reads the [mcp_servers.<name>] tables of a Codex
// config.toml: their command and args (strings and one array, the forms
// `codex mcp add` writes). Other keys are skipped.
func codexMCPFile(path string) ([]mcpEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []mcpEntry
	var cur *mcpEntry
	sc := bufio.NewScanner(f)
	var pending string // an args array spread over lines
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if pending != "" {
			pending += " " + line
			if !strings.Contains(line, "]") {
				continue
			}
			cur.args = tomlStrings(pending)
			pending = ""
			continue
		}
		if strings.HasPrefix(line, "[") {
			cur = nil
			// [mcp_servers.rw] or [mcp_servers."rw"], not a sub-table
			// such as [mcp_servers.rw.env].
			name, isServer := strings.CutPrefix(strings.Trim(line, "[] "), "mcp_servers.")
			quoted := len(name) > 1 && name[0] == '"' && strings.Index(name[1:], `"`) == len(name)-2
			if isServer && (quoted || !strings.Contains(name, ".")) {
				out = append(out, mcpEntry{client: "Codex", where: path, name: strings.Trim(name, `"`)})
				cur = &out[len(out)-1]
			}
			continue
		}
		if cur == nil {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "command":
			cur.command = tomlString(strings.TrimSpace(val))
		case "args":
			val = strings.TrimSpace(val)
			if strings.HasPrefix(val, "[") && !strings.Contains(val, "]") {
				pending = val
				continue
			}
			cur.args = tomlStrings(val)
		}
	}
	return out, sc.Err()
}

// tomlString reads a TOML basic ("...") or literal ('...') string.
func tomlString(s string) string {
	if strings.HasPrefix(s, "'") {
		if i := strings.Index(s[1:], "'"); i >= 0 {
			return s[1 : 1+i]
		}
		return ""
	}
	if v, err := strconv.Unquote(firstQuoted(s)); err == nil {
		return v
	}
	return ""
}

// firstQuoted is the first "..." string at the start of s.
func firstQuoted(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return ""
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return s[:i+1]
		}
	}
	return ""
}

// tomlStrings reads a one-line array of strings.
func tomlStrings(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	if i := strings.LastIndex(s, "]"); i >= 0 {
		s = s[:i]
	}
	var out []string
	for {
		s = strings.TrimLeft(s, " ,\t")
		if s == "" {
			return out
		}
		var item string
		if s[0] == '\'' {
			end := strings.Index(s[1:], "'")
			if end < 0 {
				return out
			}
			item, s = s[1:1+end], s[2+end:]
		} else {
			q := firstQuoted(s)
			if q == "" {
				return out
			}
			v, err := strconv.Unquote(q)
			if err != nil {
				return out
			}
			item, s = v, s[len(q):]
		}
		out = append(out, item)
	}
}
