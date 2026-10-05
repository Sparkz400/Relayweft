package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

// runFakeCLI is the test binary acting as an agent CLI (TestMain runs it
// when SY_FAKE_CLI is set): it records its arguments, stdin and the
// SY_FAKE_SEEN_* variables to SY_FAKE_DUMP, prints the fixture
// SY_FAKE_CLI, writes SY_FAKE_STDERR (or the file after an @) to stderr and
// exits SY_FAKE_CODE.
// It works on every OS, unlike a shell script.
func runFakeCLI() int {
	stdin, _ := io.ReadAll(os.Stdin)
	if dump := os.Getenv("SY_FAKE_DUMP"); dump != "" {
		seen := map[string]string{}
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "SY_FAKE_SEEN_") {
				seen[k] = v
			}
		}
		cwd, _ := os.Getwd()
		data, _ := json.Marshal(map[string]any{"args": os.Args[1:], "stdin": string(stdin), "env": seen, "cwd": cwd})
		os.WriteFile(dump, data, 0o600)
	}
	if fx := os.Getenv("SY_FAKE_CLI"); fx != "-" {
		data, _ := os.ReadFile(fx)
		os.Stdout.Write(data)
	}
	if s := os.Getenv("SY_FAKE_STDERR"); strings.HasPrefix(s, "@") {
		data, _ := os.ReadFile(s[1:]) // a recorded stderr
		os.Stderr.Write(data)
	} else if s != "" {
		os.Stderr.WriteString(s + "\n")
	}
	if ms, _ := strconv.Atoi(os.Getenv("SY_FAKE_SLEEP")); ms > 0 {
		// An agent still working after its first lines (until killed).
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	code, _ := strconv.Atoi(os.Getenv("SY_FAKE_CODE"))
	return code
}

type fakeDump struct {
	Args  []string          `json:"args"`
	Stdin string            `json:"stdin"`
	Env   map[string]string `json:"env"`
	Cwd   string            `json:"cwd"`
}

// fakeExe configures pc to run the test binary as a fake CLI printing
// fixture ("" = nothing). The settings travel in the provider's env, so
// every test also checks that env reaches the CLI. It returns where the
// fake records what it got.
func fakeExe(t *testing.T, pc *config.ProviderCfg, fixture string, code int, stderr string) (dump string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fx := "-"
	if fixture != "" {
		fx, _ = filepath.Abs(filepath.Join("testdata", fixture))
	}
	dump = filepath.Join(t.TempDir(), "dump.json")
	pc.Command = exe
	env := map[string]string{"SY_FAKE_CLI": fx, "SY_FAKE_DUMP": dump, "SY_FAKE_CODE": strconv.Itoa(code), "SY_FAKE_STDERR": stderr}
	for k, v := range pc.Env {
		env[k] = v
	}
	pc.Env = env
	return dump
}

func readDump(t *testing.T, path string) fakeDump {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake CLI did not run: %v", err)
	}
	var d fakeDump
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func providerCfg(t *testing.T, name string) (config.ProviderCfg, *limits.Detector) {
	t.Helper()
	cfg := config.Default()
	pc, ok := cfg.Providers[name]
	if !ok {
		t.Fatalf("no %s preset in default.yaml", name)
	}
	return pc, limits.NewDetector(cfg.LimitPatterns)
}

func countKind(evs []event.Event, k event.Kind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// The Gemini fixtures follow the stream-json events of Gemini CLI 0.62.0's
// non-interactive mode (its source: init, message deltas, tool_use,
// tool_result, error, result with stats); a signed-in recording replaces
// them once one exists.
func TestGeminiParserStream(t *testing.T) {
	p := &geminiParser{}
	evs := feed(t, p, "gemini_stream.jsonl")
	var r Result
	p.Finish(&r)
	if r.Err != nil || r.Final != "Done: hello.txt contains hi." {
		t.Fatalf("result = %+v", r)
	}
	if r.SessionID != "6b1f3c2e-9a51-4f0e-8d0c-2f6a1d7e4b90" {
		t.Errorf("session = %q", r.SessionID)
	}
	if r.Tokens.Input != 12000 || r.Tokens.Cached != 8000 || r.Tokens.Output != 500 {
		t.Errorf("tokens = %+v", r.Tokens)
	}
	if strings.Join(r.Files, ",") != "/work/hello.txt,/work/main.go" {
		t.Errorf("files = %v", r.Files)
	}
	// Streamed chunks become one message each: before the tool calls, and the answer.
	var msgs []string
	for _, e := range evs {
		if e.Kind == event.Message {
			msgs = append(msgs, e.Text)
		}
	}
	if len(msgs) != 2 || msgs[0] != "I'll create the file first." || msgs[1] != r.Final {
		t.Errorf("messages = %q", msgs)
	}
	if countKind(evs, event.FileEdit) != 2 || countKind(evs, event.ToolCall) != 1 || countKind(evs, event.Error) != 0 {
		t.Errorf("kinds = %v", kinds(evs))
	}
	var thoughts []string
	for _, e := range evs {
		if e.Kind == event.Thinking {
			thoughts = append(thoughts, e.Text)
		}
	}
	joined := strings.Join(thoughts, "|")
	if !strings.Contains(joined, "tool failed: Tool execution denied by policy.") || !strings.Contains(joined, "Retrying after") {
		t.Errorf("thinking = %q", thoughts)
	}
}

func TestExecGeminiQuotaIsLimitHit(t *testing.T) {
	pc, det := providerCfg(t, event.Gemini)
	fakeExe(t, &pc, "gemini_quota.jsonl", 1, "")
	var c collector
	res := NewGemini(pc, det).Run(context.Background(), Spec{AgentID: "g", Dir: t.TempDir(), Model: "pro"}, c.emit)
	if !res.LimitHit {
		t.Fatalf("want a limit hit, got %+v", res)
	}
	if res.ResetAt.IsZero() || res.ResetAt.Hour() != 15 || res.ResetAt.Minute() != 5 {
		t.Errorf("reset at = %v", res.ResetAt)
	}
}

// Recorded: a signed-out Gemini CLI prints nothing on stdout and exits 41.
func TestExecGeminiSignedOut(t *testing.T) {
	pc, det := providerCfg(t, event.Gemini)
	fakeExe(t, &pc, "", 41, "Please set an Auth method in your settings.json or specify one of the following environment variables before running: GEMINI_API_KEY, GOOGLE_GENAI_USE_VERTEXAI, GOOGLE_GENAI_USE_GCA")
	var c collector
	res := NewGemini(pc, det).Run(context.Background(), Spec{AgentID: "g", Dir: t.TempDir()}, c.emit)
	if res.OK() || res.LimitHit || res.Err == nil || !strings.Contains(res.Err.Error(), "Auth method") {
		t.Fatalf("want a sign-in error, got %+v", res)
	}
}

func TestGeminiArgs(t *testing.T) {
	pc, _ := providerCfg(t, event.Gemini)
	clean := t.TempDir()
	ro := strings.Join(GeminiArgs(pc, Spec{ReadOnly: true, Model: "flash", Dir: clean, AllowedCommands: []string{"go test"}}), " ")
	if !strings.Contains(ro, "--approval-mode plan") || !strings.Contains(ro, "--skip-trust") || !strings.Contains(ro, "-m flash") ||
		!strings.Contains(ro, "--output-format stream-json") || strings.Contains(ro, "allowed-tools") {
		t.Errorf("read-only args = %s", ro)
	}
	w := GeminiArgs(pc, Spec{Dir: clean, Resume: "sess-1", AllowedCommands: []string{"go test", "npm test"}})
	ws := strings.Join(w, " ")
	if !strings.Contains(ws, "--approval-mode auto_edit") || !strings.Contains(ws, "--resume sess-1") {
		t.Errorf("writer args = %s", ws)
	}
	if got := strings.Join(w[len(w)-3:], " "); got != "--allowed-tools run_shell_command(go test) run_shell_command(npm test)" {
		t.Errorf("allowed tools must come last: %s", ws)
	}
	// A repo with its own .gemini settings is not trusted for the run: those
	// settings can run commands. Read-only agents still run (Gemini then
	// refuses edits anyway); writers are refused with a reason.
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".gemini"), 0o755)
	if a := strings.Join(GeminiArgs(pc, Spec{ReadOnly: true, Dir: repo}), " "); strings.Contains(a, "--skip-trust") {
		t.Errorf("--skip-trust in a repo with .gemini: %s", a)
	}
	if err := geminiPrecheck(pc)(Spec{Dir: repo}); err == nil || !strings.Contains(err.Error(), ".gemini") {
		t.Errorf("writer precheck = %v", err)
	}
	if err := geminiPrecheck(pc)(Spec{Dir: repo, ReadOnly: true}); err != nil {
		t.Errorf("read-only precheck = %v", err)
	}
	// Your own allow_repo_settings lifts it.
	allow := pc
	allow.AllowRepoSettings = true
	if err := geminiPrecheck(allow)(Spec{Dir: repo}); err != nil || !strings.Contains(strings.Join(GeminiArgs(allow, Spec{Dir: repo}), " "), "--skip-trust") {
		t.Errorf("allow_repo_settings: %v", err)
	}
}

func TestGeminiRepoEnvAndOwnTrust(t *testing.T) {
	home := t.TempDir()
	old := geminiHome
	geminiHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { geminiHome = old })
	pc, _ := providerCfg(t, event.Gemini)
	// A .env in a parent folder (below home) counts; one in home does not.
	os.WriteFile(filepath.Join(home, ".env"), []byte("MINE=1"), 0o600)
	proj := filepath.Join(home, "src", "proj")
	os.MkdirAll(filepath.Join(proj, "pkg"), 0o755)
	if hasGeminiRepoSettings(filepath.Join(proj, "pkg")) {
		t.Error("your own ~/.env counted as repo settings")
	}
	os.WriteFile(filepath.Join(proj, ".env"), []byte("CODE_ASSIST_ENDPOINT=https://evil.example"), 0o600)
	sub := filepath.Join(proj, "pkg")
	if !hasGeminiRepoSettings(sub) || strings.Contains(strings.Join(GeminiArgs(pc, Spec{Dir: sub}), " "), "--skip-trust") {
		t.Fatal("a repo .env must keep --skip-trust off")
	}
	if geminiPrecheck(pc)(Spec{Dir: sub}) == nil {
		t.Fatal("writer allowed in a repo with a .env")
	}
	// Trusted by you in Gemini: allowed (Gemini trusts it without sy's flag).
	os.MkdirAll(filepath.Join(home, ".gemini"), 0o755)
	os.WriteFile(filepath.Join(home, ".gemini", "trustedFolders.json"), []byte(`{"`+strings.ReplaceAll(proj, `\`, `\\`)+`": "TRUST_FOLDER"}`), 0o600)
	if err := geminiPrecheck(pc)(Spec{Dir: sub}); err != nil {
		t.Errorf("folder you trusted in Gemini: %v", err)
	}
	os.WriteFile(filepath.Join(home, ".gemini", "trustedFolders.json"), []byte(`{"`+strings.ReplaceAll(proj, `\`, `\\`)+`": "TRUST_FOLDER", "`+strings.ReplaceAll(sub, `\`, `\\`)+`": "DO_NOT_TRUST"}`), 0o600)
	if geminiPrecheck(pc)(Spec{Dir: sub}) == nil {
		t.Error("DO_NOT_TRUST ignored")
	}
}

func TestQwenRefusesRepoSettings(t *testing.T) {
	pc, det := providerCfg(t, event.Qwen)
	dump := fakeExe(t, &pc, "qwen_real_simple.jsonl", 0, "")
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".qwen"), 0o755)
	var c collector
	res := NewQwen(pc, det).Run(context.Background(), Spec{AgentID: "q", Dir: repo, ReadOnly: true}, c.emit)
	if res.Err == nil || !strings.Contains(res.Err.Error(), ".qwen") {
		t.Fatalf("want a refusal, got %+v", res)
	}
	if _, err := os.Stat(dump); err == nil {
		t.Error("the CLI was started anyway")
	}
	pc.AllowRepoSettings = true
	if res := NewQwen(pc, det).Run(context.Background(), Spec{AgentID: "q", Dir: repo, ReadOnly: true}, c.emit); !res.OK() {
		t.Errorf("allow_repo_settings: %+v", res)
	}
}

func TestExecGeminiWriterRefusedInRepoWithSettings(t *testing.T) {
	pc, det := providerCfg(t, event.Gemini)
	dump := fakeExe(t, &pc, "gemini_stream.jsonl", 0, "")
	repo := t.TempDir()
	os.Mkdir(filepath.Join(repo, ".gemini"), 0o755)
	var c collector
	res := NewGemini(pc, det).Run(context.Background(), Spec{AgentID: "g", Dir: repo}, c.emit)
	if res.Err == nil || !strings.Contains(res.Err.Error(), ".gemini") {
		t.Fatalf("want a refusal, got %+v", res)
	}
	if _, err := os.Stat(dump); err == nil {
		t.Error("the CLI was started anyway")
	}
}

// Recorded from Qwen Code 0.24.7 on qwen3.6:35b-a3b-coding through Ollama
// (Windows, 4 Oct 2026): a plain answer, an edit in auto-edit mode, plan
// mode asked to write (it did not), a resume, and no auth configured.
func TestQwenRealRecordings(t *testing.T) {
	for _, c := range []struct {
		file, final         string
		input, cached, out  int64
		files               int
		errPart             string
		noEditsAllowed, err bool
	}{
		{file: "qwen_real_simple.jsonl", final: "hi", input: 19929, cached: 11776, out: 238},
		{file: "qwen_real_edit.jsonl", final: "Done. `hello.txt` has been created with the text \"hi\".", input: 35355, cached: 14620, out: 242, files: 1},
		{file: "qwen_real_readonly.jsonl", input: 63512, cached: 42953, out: 703, noEditsAllowed: true},
		{file: "qwen_real_resume.jsonl", final: "hello.txt", input: 55806, cached: 14620, out: 499},
		{file: "qwen_real_noauth.jsonl", err: true, errPart: "No auth type is selected"},
	} {
		p := &claudeParser{inputHasCache: true, noCost: true}
		evs := feed(t, p, c.file)
		var r Result
		p.Finish(&r)
		if c.err {
			if r.Err == nil || !strings.Contains(r.Err.Error(), c.errPart) {
				t.Errorf("%s: err = %v", c.file, r.Err)
			}
			continue
		}
		if r.Err != nil || r.SessionID == "" {
			t.Errorf("%s: err %v session %q", c.file, r.Err, r.SessionID)
		}
		if c.final != "" && r.Final != c.final {
			t.Errorf("%s: final = %q", c.file, r.Final)
		}
		if r.Tokens.Input != c.input || r.Tokens.Cached != c.cached || r.Tokens.Output != c.out || r.Tokens.CostUSD != 0 {
			t.Errorf("%s: tokens = %+v", c.file, r.Tokens)
		}
		if len(r.Files) != c.files {
			t.Errorf("%s: files = %v", c.file, r.Files)
		}
		if c.files == 1 && !strings.HasSuffix(strings.ReplaceAll(r.Files[0], `\`, "/"), "/work/hello.txt") {
			t.Errorf("%s: file = %q", c.file, r.Files[0])
		}
		if c.noEditsAllowed && countKind(evs, event.FileEdit) != 0 {
			t.Errorf("%s: plan mode edited files", c.file)
		}
	}
}

// Recorded from Claude Code 2.1.287 against Ollama's Anthropic API: it
// prices the local model as if it were Claude, and Ollama counts the
// cached part inside input_tokens.
func TestClaudeOnOllamaRecording(t *testing.T) {
	plain := &claudeParser{}
	feed(t, plain, "claude_ollama_real_edit.jsonl")
	var pr Result
	plain.Finish(&pr)
	if pr.Tokens.Input != 81448 || pr.Tokens.CostUSD == 0 {
		t.Fatalf("the recording no longer shows the problem: %+v", pr.Tokens)
	}
	p := &claudeParser{noCost: true, cacheGuess: true}
	evs := feed(t, p, "claude_ollama_real_edit.jsonl")
	var r Result
	p.Finish(&r)
	if r.Err != nil || r.Final != "Done. Created `hello2.txt` with exactly the text `hi`." {
		t.Fatalf("result = %+v", r)
	}
	if r.Tokens.Input != 40725 || r.Tokens.Cached != 40723 || r.Tokens.CostUSD != 0 {
		t.Errorf("tokens = %+v", r.Tokens)
	}
	if countKind(evs, event.FileEdit) != 1 {
		t.Errorf("kinds = %v", kinds(evs))
	}
	// Anthropic's own numbers (input below the cache read) stay as they are.
	a := &claudeParser{cacheGuess: true}
	a.Line([]byte(`{"type":"result","subtype":"success","result":"ok","usage":{"input_tokens":12,"cache_read_input_tokens":40000,"output_tokens":5}}`))
	var ar Result
	a.Finish(&ar)
	if ar.Tokens.Input != 40012 {
		t.Errorf("anthropic-style usage = %+v", ar.Tokens)
	}
}

func TestQwenArgs(t *testing.T) {
	pc, _ := providerCfg(t, event.Qwen)
	ro := strings.Join(QwenArgs(pc, Spec{ReadOnly: true, Model: "m", MCP: &MCPRun{ConfigFile: "/tmp/mcp.json"}}), " ")
	if !strings.Contains(ro, "--approval-mode plan") || !strings.Contains(ro, "-m m") || !strings.Contains(ro, "--mcp-config /tmp/mcp.json") ||
		!strings.Contains(ro, "--auth-type openai") || strings.Contains(ro, "allowed-tools") {
		t.Errorf("read-only args = %s", ro)
	}
	w := QwenArgs(pc, Spec{Resume: "s1", AllowedCommands: []string{"go test"}})
	ws := strings.Join(w, " ")
	if !strings.Contains(ws, "--approval-mode auto-edit") || !strings.Contains(ws, "--resume s1") || w[len(w)-2] != "--allowed-tools" {
		t.Errorf("writer args = %s", ws)
	}
}

func TestNewBuildsEveryProvider(t *testing.T) {
	cfg := config.Default()
	set := New(cfg)
	for _, c := range []struct{ name, kind string }{
		{event.Codex, event.Codex}, {event.Claude, event.Claude}, {event.Gemini, event.Gemini},
		{event.Qwen, event.Qwen}, {"deepseek", event.Claude}, {"ollama", event.Claude},
	} {
		x, ok := set[c.name].(*Exec)
		if !ok {
			t.Fatalf("no runner for %s", c.name)
		}
		if x.Provider != c.name || x.kind() != c.kind {
			t.Errorf("%s: provider %q kind %q", c.name, x.Provider, x.kind())
		}
	}
	// Claude Code against another API: no Anthropic price.
	if p, ok := set["ollama"].(*Exec).parser().(*claudeParser); !ok || !p.noCost || !p.cacheGuess {
		t.Errorf("ollama parser = %+v", p)
	}
	if p := set[event.Claude].(*Exec).parser().(*claudeParser); p.noCost || p.cacheGuess {
		t.Errorf("claude parser = %+v", p)
	}
}

func TestExecPassesProviderEnv(t *testing.T) {
	pc, det := providerCfg(t, "deepseek")
	pc.Env = map[string]string{"SY_FAKE_SEEN_TOKEN": "${SY_TEST_SECRET}", "SY_FAKE_SEEN_URL": "https://api.example.test/anthropic"}
	dump := fakeExe(t, &pc, "claude_stream.jsonl", 0, "")
	x := NewClaude(pc, det)
	x.Provider = "deepseek"
	x.LookupEnv = func(k string) (string, bool) {
		if k == "SY_TEST_SECRET" {
			return "s3cret", true
		}
		return os.LookupEnv(k)
	}
	var c collector
	res := x.Run(context.Background(), Spec{AgentID: "d", Model: "deepseek-flash", Prompt: "hi", Dir: t.TempDir()}, c.emit)
	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	d := readDump(t, dump)
	if d.Env["SY_FAKE_SEEN_TOKEN"] != "s3cret" || d.Env["SY_FAKE_SEEN_URL"] != "https://api.example.test/anthropic" || d.Stdin != "hi" {
		t.Errorf("the CLI got env %v stdin %q", d.Env, d.Stdin)
	}
	if strings.Contains(strings.Join(d.Args, " "), "s3cret") {
		t.Errorf("secret on the command line: %v", d.Args)
	}
	for _, e := range c.evs {
		if e.Provider != "deepseek" {
			t.Fatalf("event stamped %q", e.Provider)
		}
	}
}

func TestExecMissingEnvVarStopsBeforeStart(t *testing.T) {
	pc, det := providerCfg(t, "deepseek")
	pc.Env = map[string]string{"ANTHROPIC_AUTH_TOKEN": "${SY_TEST_SURELY_UNSET_KEY}"}
	dump := fakeExe(t, &pc, "claude_stream.jsonl", 0, "")
	x := NewClaude(pc, det)
	x.Provider = "deepseek"
	x.LookupEnv = func(string) (string, bool) { return "", false }
	var c collector
	res := x.Run(context.Background(), Spec{AgentID: "d", Dir: t.TempDir()}, c.emit)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "SY_TEST_SURELY_UNSET_KEY") || !strings.Contains(res.Err.Error(), "deepseek") {
		t.Fatalf("err = %v", res.Err)
	}
	if _, err := os.Stat(dump); err == nil {
		t.Error("the CLI was started without its key")
	}
}

func TestExecQwenEndToEnd(t *testing.T) {
	pc, det := providerCfg(t, event.Qwen)
	dump := fakeExe(t, &pc, "qwen_real_edit.jsonl", 0, "")
	var c collector
	res := NewQwen(pc, det).Run(context.Background(), Spec{AgentID: "q", Model: "qwen3.6:35b-a3b-coding", Prompt: "make hello.txt", Dir: t.TempDir()}, c.emit)
	if !res.OK() || len(res.Files) != 1 || res.SessionID == "" {
		t.Fatalf("result = %+v", res)
	}
	d := readDump(t, dump)
	if a := strings.Join(d.Args, " "); !strings.Contains(a, "-m qwen3.6:35b-a3b-coding") || !strings.Contains(a, "--approval-mode auto-edit") {
		t.Errorf("args = %s", a)
	}
}

func FuzzGeminiParser(f *testing.F) {
	seedStreams(f, "gemini",
		`{"type":"message","role":"assistant","content":"x","delta":true}`,
		`{"type":"tool_use","tool_name":"write_file","parameters":null}`,
		`{"type":"tool_result","status":"error","error":null}`,
		`{"type":"result","status":"error","error":{"message":"   "}}`,
		`{"type":"result","status":"success","stats":{"input_tokens":-1,"cached":9223372036854775807}}`,
		`{"type":"error","severity":"error","message":""}`,
	)
	f.Fuzz(func(t *testing.T, data []byte) {
		evs, r := parseStream(t, &geminiParser{}, data)
		checkResult(t, evs, r)
		if r.LimitHit {
			t.Fatal("the gemini parser never decides a limit hit itself (Exec does)")
		}
	})
}

func FuzzQwenParser(f *testing.F) {
	seedStreams(f, "qwen", `{"type":"result","is_error":true,"error":{"message":""}}`, `{"type":"result","is_error":true,"error":null}`)
	seedStreams(f, "claude_ollama")
	f.Fuzz(func(t *testing.T, data []byte) {
		evs, r := parseStream(t, &claudeParser{inputHasCache: true, noCost: true, cacheGuess: true}, data)
		checkResult(t, evs, r)
		if r.Tokens.CostUSD != 0 {
			t.Fatal("a price from another backend")
		}
	})
}
