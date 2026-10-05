package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
)

func mcpCfg() config.MCPCfg {
	return config.MCPCfg{Servers: map[string]config.MCPServer{
		"docs": {Command: `C:\Program Files\nodejs\npx.cmd`, Args: []string{"-y", "@some/mcp-server", "--root", `C:\Users\Nico Schu\repo`, "it's"},
			Env: map[string]string{"API_KEY": "${SY_TEST_SECRET}", "PLAIN": "a b", "MISSING": "x${SY_TEST_UNSET}y"}},
		"db":   {URL: "http://localhost:8080/mcp?token=${SY_TEST_SECRET}", Headers: map[string]string{"Authorization": "Bearer ${SY_TEST_SECRET}"}},
		"feed": {URL: "http://localhost:9/sse", Type: "sse"},
		"only": {Command: "uvx", Providers: []string{event.Claude}},
	}}
}

func lookup(name string) (string, bool) {
	if name == "SY_TEST_SECRET" {
		return `s3cr"et`, true
	}
	return "", false
}

func TestPrepareMCPClaudeFile(t *testing.T) {
	m, cleanup, err := PrepareMCP(event.Claude, event.RoleWorker, mcpCfg(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Names, []string{"db", "docs", "feed", "only"}) {
		t.Fatalf("names = %v", m.Names)
	}
	if !slices.Equal(m.Missing, []string{"SY_TEST_UNSET"}) {
		t.Errorf("missing = %v", m.Missing)
	}
	data, err := os.ReadFile(m.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	d := got.MCPServers["docs"]
	if d.Type != "stdio" || d.Command != `C:\Program Files\nodejs\npx.cmd` || d.Args[3] != `C:\Users\Nico Schu\repo` ||
		d.Env["API_KEY"] != `s3cr"et` || d.Env["MISSING"] != "xy" {
		t.Errorf("docs = %+v", d)
	}
	if db := got.MCPServers["db"]; db.Type != "http" || db.URL != `http://localhost:8080/mcp?token=s3cr"et` || db.Headers["Authorization"] != `Bearer s3cr"et` || db.Command != "" {
		t.Errorf("db = %+v", db)
	}
	if got.MCPServers["feed"].Type != "sse" {
		t.Errorf("feed = %+v", got.MCPServers["feed"])
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(m.ConfigFile); st.Mode().Perm() != 0o600 {
			t.Errorf("config file mode %v", st.Mode())
		}
	}
	args := strings.Join(ClaudeArgs(config.Default().Providers[event.Claude], Spec{Model: "sonnet", MCP: m}), " ")
	if !strings.Contains(args, "--mcp-config "+m.ConfigFile+" --permission-mode") {
		t.Errorf("--mcp-config must be followed by another flag: %s", args)
	}
	if !strings.Contains(args, "mcp__db,mcp__docs,mcp__feed,mcp__only") || strings.Contains(args, "--strict-mcp-config") {
		t.Errorf("args = %s", args)
	}
	dir := filepath.Dir(m.ConfigFile)
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir left behind: %v", err)
	}
}

func TestClaudeMCPReadOnlyStrictAndNoAllow(t *testing.T) {
	m, cleanup, err := PrepareMCP(event.Claude, event.RoleExplorer, config.MCPCfg{
		Servers: map[string]config.MCPServer{"docs": {Command: "npx"}}, Strict: true}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	pc := config.Default().Providers[event.Claude]
	pc.WriteAllowedTools = []string{"Edit"}
	args := ClaudeArgs(pc, Spec{ReadOnly: true, AllowedCommands: []string{"go test"}, MCP: m})
	got := strings.Join(args, " ")
	// Read-only: MCP tools only, never write tools or commands.
	if !strings.HasSuffix(got, "--allowedTools mcp__docs") || !strings.Contains(got, "--strict-mcp-config") || !strings.Contains(got, "--tools "+ReadOnlyTools) {
		t.Errorf("read-only args = %s", got)
	}
	no := false
	m2, cleanup2, _ := PrepareMCP(event.Claude, event.RoleExplorer, config.MCPCfg{
		Servers: map[string]config.MCPServer{"docs": {Command: "npx"}}, AllowTools: &no}, lookup)
	defer cleanup2()
	if got := strings.Join(ClaudeArgs(pc, Spec{ReadOnly: true, MCP: m2}), " "); strings.Contains(got, "allowedTools") || !strings.Contains(got, "--mcp-config") {
		t.Errorf("allow_tools false: %s", got)
	}
}

func TestPrepareMCPRoleFilter(t *testing.T) {
	for _, role := range []string{event.RoleReviewer, event.RoleJudge, event.RolePlanner} {
		m, cleanup, err := PrepareMCP(event.Claude, role, mcpCfg(), lookup)
		cleanup()
		if m != nil || err != nil {
			t.Errorf("%s got servers by default: %+v %v", role, m, err)
		}
	}
	cfg := mcpCfg()
	cfg.Roles = []string{event.RoleReviewer}
	if m, cleanup, _ := PrepareMCP(event.Codex, event.RoleReviewer, cfg, lookup); m == nil {
		t.Error("configured role got no servers")
	} else {
		cleanup()
	}
	if m, _, _ := PrepareMCP(event.Codex, event.RoleWorker, cfg, lookup); m != nil {
		t.Error("role outside mcp.roles got servers")
	}
	if m, _, _ := PrepareMCP(event.Codex, event.RoleWorker, config.MCPCfg{}, lookup); m != nil {
		t.Error("no servers configured, got a setup")
	}
}

func TestCodexMCPArgs(t *testing.T) {
	m, cleanup, err := PrepareMCP(event.Codex, event.RoleWorkerHigh, mcpCfg(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if m.ConfigFile != "" {
		t.Error("codex needs no config file")
	}
	// sse servers and claude-only servers are not given to codex.
	if !slices.Equal(m.Names, []string{"db", "docs"}) {
		t.Fatalf("names = %v", m.Names)
	}
	args := CodexArgs(config.Default().Providers[event.Codex], Spec{Model: "m", MCP: m})
	// ${VAR} values (env, headers) go through the environment; a ${VAR}
	// in a url cannot and stays a -c value.
	want := []string{
		"-c", `mcp_servers.db.url="http://localhost:8080/mcp?token=s3cr\u0022et"`,
		"-c", `mcp_servers.db.bearer_token_env_var='SY_MCP_DB_BEARER'`,
		"-c", `mcp_servers.docs.command='C:\Program Files\nodejs\npx.cmd'`,
		"-c", `mcp_servers.docs.args=['-y', '@some/mcp-server', '--root', 'C:\Users\Nico Schu\repo', "it's"]`,
		"-c", `mcp_servers.docs.env={PLAIN = 'a b'}`,
		"-c", `mcp_servers.docs.env_vars=['API_KEY', 'MISSING']`,
	}
	got := strings.Join(args, "\n")
	if !strings.Contains(got, strings.Join(want, "\n")) {
		t.Errorf("codex args:\n%s\nwant run:\n%s", got, strings.Join(want, "\n"))
	}
	if args[len(args)-1] != "-" {
		t.Errorf("prompt marker must stay last: %v", args)
	}
	res := CodexArgs(config.Default().Providers[event.Codex], Spec{Model: "m", Resume: "sess-1", MCP: m})
	if !strings.Contains(strings.Join(res, " "), "mcp_servers.docs.command=") || res[len(res)-2] != "sess-1" {
		t.Errorf("resume args = %v", res)
	}
	// The diag log sees names, not values.
	red := strings.Join(redactArgs(args), " ")
	if strings.Contains(red, "s3cr") || strings.Contains(red, "Program Files") || !strings.Contains(red, "mcp_servers.docs.env=<hidden>") {
		t.Errorf("redacted = %s", red)
	}
}

func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:            `'plain'`,
		`C:\a b\c`:         `'C:\a b\c'`,
		`it's "q"`:         `"it's \u0022q\u0022"`,
		"two\nlines\\":     `"two\nlines\u005C"`,
		`100%`:             `"100\u0025"`,
		`C:\%USERPROFILE%`: `"C:\u005C\u0025USERPROFILE\u0025"`,
		`hey!`:             `"hey\u0021"`,
		"tab\there":        "'tab\there'",
		"bell\x07":         `"bell\u0007"`,
		``:                 `''`,
		`back\slash'quote`: `"back\u005Cslash'quote"`,
	} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
	if got := tomlTable(map[string]string{"A-B": "1", "with space": "2", `q"k`: "3"}); got != `{A-B = '1', "q\u0022k" = '3', "with space" = '2'}` {
		t.Errorf("table = %s", got)
	}
}

// The temp config exists while the CLI runs and is removed afterwards.
func TestExecClaudeMCPConfigLifetime(t *testing.T) {
	cmd, _, argsFile := fakeCLI(t, "claude_stream.jsonl", 0, "")
	// Wrap the fake CLI: copy the --mcp-config file while it runs.
	dir := filepath.Dir(cmd)
	copyTo := filepath.Join(dir, "seen.json")
	wrapper := filepath.Join(dir, "wrap")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nprev=''\nfor a in \"$@\"; do if [ \"$prev\" = --mcp-config ]; then cp \"$a\" '"+copyTo+"'; fi; prev=\"$a\"; done\nexec '"+cmd+"' \"$@\"\n"), 0o755)
	cfg := config.Default()
	pc := cfg.Providers[event.Claude]
	pc.Command = wrapper
	r := NewClaude(pc, nil)
	r.MCP = config.MCPCfg{Servers: map[string]config.MCPServer{"docs": {Command: "npx", Env: map[string]string{"K": "${SY_TEST_SECRET}"}}}}
	r.LookupEnv = lookup
	var c collector
	res := r.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Model: "haiku", Dir: t.TempDir()}, c.emit)
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	seen, err := os.ReadFile(copyTo)
	if err != nil || !strings.Contains(string(seen), `"K": "s3cr\"et"`) {
		t.Fatalf("config during run: %s %v", seen, err)
	}
	args, _ := os.ReadFile(argsFile)
	f := strings.Fields(string(args))
	i := slices.Index(f, "--mcp-config")
	if i < 0 {
		t.Fatalf("args = %s", args)
	}
	if _, err := os.Stat(f[i+1]); !os.IsNotExist(err) {
		t.Errorf("config file left behind: %s", f[i+1])
	}
	// A reviewer gets nothing.
	res = r.Run(context.Background(), Spec{AgentID: "b", Role: event.RoleReviewer, Model: "haiku", Dir: t.TempDir()}, c.emit)
	args, _ = os.ReadFile(argsFile)
	if !res.OK() || strings.Contains(string(args), "mcp") {
		t.Errorf("reviewer args = %s", args)
	}
}

func TestNewPassesMCP(t *testing.T) {
	cfg := config.Default()
	cfg.MCP = mcpCfg()
	for p, r := range New(cfg) {
		if len(r.(*Exec).MCP.Servers) != 4 {
			t.Errorf("%s runner has no MCP config", p)
		}
	}
}

// A Codex -c value passes cmd.exe and the CLI's argv parser on Windows
// (npm .cmd shim): no " inside the TOML string, no backslash right before
// a quote, no % or ! for cmd to expand, and the value decodes back.
func TestTOMLStringSafeForCmdShim(t *testing.T) {
	for _, in := range []string{
		`' " -c sandbox_mode=danger-full-access "`,
		`a\" -c sandbox_mode=danger-full-access \"b`,
		`C:\dir\`, `trailing\\`, `"`, `\`, `%PATH%`, `100%`, `!x!`, "x'y\nz\t\x01",
		`C:\Program Files\x`, `a & b | c < d > e ^ f`,
	} {
		out := tomlString(in)
		inner := out[1 : len(out)-1]
		if strings.ContainsAny(inner, `"%!`) || strings.Contains(out, `\"`) {
			t.Errorf("tomlString(%q) = %s: quote, backslash-quote, %% or ! reaches the command line", in, out)
		}
		got := inner
		if out[0] == '"' {
			// TOML basic-string escapes (\uXXXX \n \r \t) are a subset of Go's.
			var err error
			if got, err = strconv.Unquote(out); err != nil {
				t.Errorf("tomlString(%q) = %s: %v", in, out, err)
			}
		}
		if got != in {
			t.Errorf("tomlString(%q) = %s decodes to %q", in, out, got)
		}
	}
}

// ${VAR} values reach Codex through its environment: never on the command
// line, which other processes can read.
func TestCodexMCPSecretsStayOffCommandLine(t *testing.T) {
	cfg := config.MCPCfg{Servers: map[string]config.MCPServer{
		"docs": {Command: "npx", Env: map[string]string{
			"API_KEY": "${SY_TEST_SECRET}", "LITERAL": "visible",
			// The environment has OPENAI_API_KEY with another value:
			// setting it for Codex would change Codex's own login, so this
			// one stays a -c value (documented).
			"OPENAI_API_KEY": "${SY_TEST_SECRET}",
		}},
		"other": {Command: "uvx", Env: map[string]string{"API_KEY": "${SY_TEST_OTHER}"}},
		"web": {URL: "https://mcp.example.com/mcp", Headers: map[string]string{
			"Authorization": "Bearer ${SY_TEST_SECRET}", "X-Api-Key": "${SY_TEST_SECRET}", "X-Plain": "p",
		}},
	}}
	look := func(name string) (string, bool) {
		switch name {
		case "SY_TEST_SECRET":
			return "s3cret", true
		case "SY_TEST_OTHER":
			return "0ther", true
		case "OPENAI_API_KEY":
			return "sk-own", true
		}
		return "", false
	}
	m, cleanup, err := PrepareMCP(event.Codex, event.RoleWorker, cfg, look)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	args := strings.Join(codexMCPArgs(m), "\n")
	for _, want := range []string{
		`mcp_servers.docs.env={LITERAL = 'visible', OPENAI_API_KEY = 's3cret'}`,
		`mcp_servers.docs.env_vars=['API_KEY']`,
		// Same name, other value: one Codex environment cannot hold both.
		`mcp_servers.other.env={API_KEY = '0ther'}`,
		`mcp_servers.web.http_headers={X-Plain = 'p'}`,
		`mcp_servers.web.bearer_token_env_var='SY_MCP_WEB_BEARER'`,
		`mcp_servers.web.env_http_headers={X-Api-Key = 'SY_MCP_WEB_HEADER_1'}`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args miss %s:\n%s", want, args)
		}
	}
	if n := strings.Count(args, "s3cret"); n != 1 {
		t.Errorf("secret on the command line %d times (want only the documented OPENAI_API_KEY case):\n%s", n, args)
	}
	want := []string{"API_KEY=s3cret", "SY_MCP_WEB_BEARER=s3cret", "SY_MCP_WEB_HEADER_1=s3cret"}
	got := append([]string(nil), m.ChildEnv...)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("child env = %q, want %q", got, want)
	}
	// Claude reads its config file: nothing moves.
	cm, cleanup, err := PrepareMCP(event.Claude, event.RoleWorker, cfg, look)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(cm.ChildEnv) != 0 || cm.Servers["docs"].Env["API_KEY"] != "s3cret" {
		t.Errorf("claude setup changed: %+v", cm)
	}
}

// The Codex process gets the secrets in its environment, not its argv.
func TestExecCodexMCPSecretsInChildEnv(t *testing.T) {
	cmd, _, argsFile := fakeCLI(t, "codex_exec.jsonl", 0, "")
	dir := filepath.Dir(cmd)
	envFile := filepath.Join(dir, "env")
	wrapper := filepath.Join(dir, "wrap")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nenv > '"+envFile+"'\nexec '"+cmd+"' \"$@\"\n"), 0o755)
	cfg := config.Default()
	pc := cfg.Providers[event.Codex]
	pc.Command = wrapper
	r := NewCodex(pc, nil)
	r.MCP = config.MCPCfg{Servers: map[string]config.MCPServer{"docs": {Command: "npx", Env: map[string]string{"SY_MCP_TEST_KEY": "${SY_TEST_SECRET}"}}}}
	r.LookupEnv = lookup
	var c collector
	res := r.Run(context.Background(), Spec{AgentID: "a", Role: event.RoleWorker, Model: "m", Dir: t.TempDir()}, c.emit)
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	args, _ := os.ReadFile(argsFile)
	env, _ := os.ReadFile(envFile)
	if strings.Contains(string(args), "s3cr") || !strings.Contains(string(args), "mcp_servers.docs.env_vars=['SY_MCP_TEST_KEY']") {
		t.Errorf("args = %s", args)
	}
	if !strings.Contains(string(env), "SY_MCP_TEST_KEY=s3cr\"et\n") || !strings.Contains(string(env), "PATH=") {
		t.Errorf("child env lacks the secret or the inherited environment:\n%s", env)
	}
}

// Claude's config files live in sy's own dir; dirs a hard kill left behind
// are removed after a day.
func TestMCPTempDirSweepsStale(t *testing.T) {
	root := t.TempDir()
	old := mcpRoot
	mcpRoot = func() string { return root }
	sweepOnce = sync.Once{}
	t.Cleanup(func() { mcpRoot = old; sweepOnce = sync.Once{} })
	stale := filepath.Join(root, "sy-mcp-stale")
	fresh := filepath.Join(root, "sy-mcp-fresh")
	mine := filepath.Join(root, "keep-me")
	for _, d := range []string{stale, fresh, mine} {
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "mcp.json"), []byte("{}"), 0o600)
	}
	two := time.Now().Add(-48 * time.Hour)
	os.Chtimes(stale, two, two)
	os.Chtimes(mine, two, two)
	m, cleanup, err := PrepareMCP(event.Claude, event.RoleWorker, mcpCfg(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasPrefix(m.ConfigFile, filepath.Join(root, "sy-mcp-")) {
		t.Errorf("config file %s is not in sy's dir %s", m.ConfigFile, root)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale sy-mcp-* dir not removed")
	}
	for _, d := range []string{fresh, mine} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s removed: %v", d, err)
		}
	}
}

// TestMain keeps Claude's MCP config files out of the real cache dir. Run
// with SY_FAKE_CLI set, the test binary is a fake agent CLI (fakeExe).
func TestMain(m *testing.M) {
	if os.Getenv("SY_FAKE_CLI") != "" {
		os.Exit(runFakeCLI())
	}
	if os.Getenv("SY_FAKE_DOCKER") != "" {
		os.Exit(runFakeDocker())
	}
	dir, err := os.MkdirTemp("", "sy-runner-test-")
	if err != nil {
		panic(err)
	}
	mcpRoot = func() string { return dir }
	// Never read your real ~/.gemini (trustedFolders.json there may trust
	// the temp folders the tests use).
	geminiHome = func() (string, error) { return dir, nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
