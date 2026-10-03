package runner

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
)

func feed(t *testing.T, p lineParser, file string) []event.Event {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []event.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, p.Line(sc.Bytes())...)
	}
	return out
}

func kinds(evs []event.Event) map[event.Kind]int {
	m := map[event.Kind]int{}
	for _, e := range evs {
		m[e.Kind]++
	}
	return m
}

func TestClaudeParserRealFixture(t *testing.T) {
	p := &claudeParser{}
	evs := feed(t, p, "claude_stream.jsonl")
	k := kinds(evs)
	if k[event.Quota] != 1 {
		t.Errorf("want 1 quota event, got %d", k[event.Quota])
	}
	if k[event.ToolCall] != 1 {
		t.Errorf("want 1 tool call (Read), got %d: %+v", k[event.ToolCall], evs)
	}
	var r Result
	p.Finish(&r)
	if r.Final != "OK" {
		t.Errorf("final = %q", r.Final)
	}
	if r.Err != nil || r.LimitHit {
		t.Errorf("unexpected error %v limit %v", r.Err, r.LimitHit)
	}
	if r.Tokens.Output != 140 || r.Tokens.Input != 17+5500+5353 || r.Tokens.CostUSD == 0 {
		t.Errorf("tokens = %+v", r.Tokens)
	}
	for _, e := range evs {
		if e.Kind == event.Quota && (e.Quota.Utilization != 0.76 || e.Quota.Window != "seven_day") {
			t.Errorf("quota = %+v", e.Quota)
		}
		if e.Kind == event.ToolCall && e.Text != "Read /work/a.txt" {
			t.Errorf("tool text = %q", e.Text)
		}
	}
}

func TestClaudeParserLimit(t *testing.T) {
	p := &claudeParser{}
	evs := feed(t, p, "claude_limit.jsonl")
	if kinds(evs)[event.LimitHit] != 1 {
		t.Fatalf("want a LimitHit event, got %+v", evs)
	}
	var r Result
	p.Finish(&r)
	if !r.LimitHit || r.Err == nil {
		t.Fatalf("want limit + error, got %+v", r)
	}
	at, ok := limits.ParseReset(r.Err.Error(), time.Now())
	if !ok || at.Unix() != 1791003600 {
		t.Errorf("reset = %v %v", at, ok)
	}
}

func TestCodexParser(t *testing.T) {
	p := &codexParser{}
	evs := feed(t, p, "codex_exec.jsonl")
	k := kinds(evs)
	if k[event.FileEdit] != 2 {
		t.Errorf("want 2 file edits, got %d", k[event.FileEdit])
	}
	if k[event.ToolCall] != 4 { // 2 commands, 1 mcp, 1 search
		t.Errorf("want 4 tool calls, got %d", k[event.ToolCall])
	}
	if k[event.Usage] != 0 {
		t.Errorf("usage must only be reported once, by Exec: got %d", k[event.Usage])
	}
	var r Result
	p.Finish(&r)
	if !strings.HasPrefix(r.Final, "Kept the trailing field") {
		t.Errorf("final = %q", r.Final)
	}
	if r.Tokens.Input != 24763 || r.Tokens.Output != 122 || r.Tokens.Reasoning != 64 {
		t.Errorf("tokens = %+v", r.Tokens)
	}
	if strings.Join(r.Files, ",") != "internal/parse.go,internal/parse_test.go" {
		t.Errorf("files = %v", r.Files)
	}
}

func TestCodexParserLegacy(t *testing.T) {
	p := &codexParser{}
	evs := feed(t, p, "codex_legacy.jsonl")
	k := kinds(evs)
	if k[event.ToolCall] != 1 || k[event.FileEdit] != 1 || k[event.Message] != 1 {
		t.Errorf("kinds = %v", k)
	}
	var r Result
	p.Finish(&r)
	if r.Final != "Done." || r.Tokens.Input != 1000 {
		t.Errorf("result = %+v", r)
	}
}

func TestCodexParserLimitMessage(t *testing.T) {
	p := &codexParser{}
	evs := feed(t, p, "codex_limit.jsonl")
	det := limits.NewDetector(config.Default().LimitPatterns)
	hits := 0
	for _, e := range evs {
		if e.Kind == event.Error && det.Match(e.Text) {
			hits++
		}
	}
	if hits == 0 {
		t.Fatal("limit text not detected")
	}
	var r Result
	p.Finish(&r)
	if r.Err == nil {
		t.Fatal("want error")
	}
}

func TestArgs(t *testing.T) {
	cfg := config.Default()
	cx := CodexArgs(cfg.Providers[event.Codex], Spec{Model: "gpt-6.1-sol", Effort: "high", Dir: "/w"})
	want := "exec --json --color never --skip-git-repo-check -m gpt-6.1-sol -c model_reasoning_effort=high --sandbox workspace-write -"
	if got := strings.Join(cx, " "); got != want {
		t.Errorf("codex args\n got %s\nwant %s", got, want)
	}
	ro := strings.Join(CodexArgs(cfg.Providers[event.Codex], Spec{Model: "m", ReadOnly: true}), " ")
	if !strings.Contains(ro, "--sandbox read-only") {
		t.Errorf("read-only codex args: %s", ro)
	}
	cl := strings.Join(ClaudeArgs(cfg.Providers[event.Claude], Spec{Model: "opus", Effort: "high"}), " ")
	if cl != "-p --output-format stream-json --verbose --model opus --effort high --permission-mode acceptEdits" {
		t.Errorf("claude args: %s", cl)
	}
	clro := strings.Join(ClaudeArgs(cfg.Providers[event.Claude], Spec{Model: "haiku", ReadOnly: true}), " ")
	if !strings.Contains(clro, "--permission-mode dontAsk --tools "+ReadOnlyTools) {
		t.Errorf("claude read-only args: %s", clro)
	}
	pc := cfg.Providers[event.Claude]
	pc.WriteAllowedTools = []string{"Bash(go test *)"}
	cla := ClaudeArgs(pc, Spec{Model: "sonnet"})
	if cla[len(cla)-2] != "--allowedTools" || cla[len(cla)-1] != "Bash(go test *)" {
		t.Errorf("allowed tools must come last: %v", cla)
	}
}

func TestSummaryLine(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"summary\": \"do it\", \"subtasks\": []}\n```": "do it",
		"```json\n{\"approve\": true, \"advice\": \"fine\"}\n```":  "fine",
		"\n\nhello\nworld": "hello",
	}
	for in, want := range cases {
		if got := SummaryLine(in); got != want {
			t.Errorf("SummaryLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeCLI writes a shell script that records its stdin and args, prints a
// fixture and exits with a code.
func fakeCLI(t *testing.T, fixture string, code int, stderr string) (cmd, promptFile, argsFile string) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script CLI stub")
	}
	dir := t.TempDir()
	promptFile = filepath.Join(dir, "prompt")
	argsFile = filepath.Join(dir, "args")
	fix, _ := filepath.Abs(filepath.Join("testdata", fixture))
	script := "#!/bin/sh\ncat > '" + promptFile + "'\necho \"$@\" > '" + argsFile + "'\ncat '" + fix + "'\n"
	if stderr != "" {
		script += "echo '" + stderr + "' >&2\n"
	}
	script += "exit " + string(rune('0'+code)) + "\n"
	cmd = filepath.Join(dir, "cli")
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, promptFile, argsFile
}

type collector struct {
	mu  sync.Mutex
	evs []event.Event
}

func (c *collector) emit(e event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, e)
}

func TestExecClaudeEndToEnd(t *testing.T) {
	cmd, promptFile, argsFile := fakeCLI(t, "claude_stream.jsonl", 0, "")
	cfg := config.Default()
	pc := cfg.Providers[event.Claude]
	pc.Command = cmd
	r := NewClaude(pc, limits.NewDetector(cfg.LimitPatterns))
	var c collector
	res := r.Run(context.Background(), Spec{AgentID: "a1", Role: "worker", Model: "haiku", Prompt: "multi\nline \"prompt\"", Dir: t.TempDir()}, c.emit)
	if !res.OK() || res.Final != "OK" {
		t.Fatalf("result = %+v", res)
	}
	got, _ := os.ReadFile(promptFile)
	if string(got) != "multi\nline \"prompt\"" {
		t.Errorf("prompt via stdin = %q", got)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--model haiku") {
		t.Errorf("args = %s", args)
	}
	k := kinds(c.evs)
	if k[event.Started] != 1 || k[event.Done] != 1 {
		t.Errorf("kinds = %v", k)
	}
	for _, e := range c.evs {
		if e.AgentID != "a1" || e.Provider != event.Claude || e.Role != "worker" {
			t.Fatalf("event not stamped: %+v", e)
		}
	}
}

func TestExecCodexLimitFromExitAndStderr(t *testing.T) {
	cmd, _, _ := fakeCLI(t, "codex_limit.jsonl", 1, "ERROR: usage limit")
	cfg := config.Default()
	pc := cfg.Providers[event.Codex]
	pc.Command = cmd
	r := NewCodex(pc, limits.NewDetector(cfg.LimitPatterns))
	var c collector
	res := r.Run(context.Background(), Spec{AgentID: "a2", Model: "gpt-6-luna"}, c.emit)
	if !res.LimitHit {
		t.Fatalf("want limit hit, got %+v", res)
	}
	if res.ResetAt.IsZero() || res.ResetAt.Hour() != 15 || res.ResetAt.Minute() != 5 {
		t.Errorf("reset at = %v", res.ResetAt)
	}
}

func TestExecMissingCommand(t *testing.T) {
	cfg := config.Default()
	pc := cfg.Providers[event.Codex]
	pc.Command = "definitely-not-a-real-cli-xyz"
	var c collector
	res := NewCodex(pc, nil).Run(context.Background(), Spec{AgentID: "a"}, c.emit)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "not found") {
		t.Fatalf("want not found error, got %v", res.Err)
	}
}

func TestExecKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script CLI stub")
	}
	dir := t.TempDir()
	cmd := filepath.Join(dir, "slow")
	os.WriteFile(cmd, []byte("#!/bin/sh\ncat >/dev/null\nsleep 30 &\nsleep 30\n"), 0o755)
	cfg := config.Default()
	pc := cfg.Providers[event.Claude]
	pc.Command = cmd
	ctx, cancel := context.WithCancel(context.Background())
	var c collector
	done := make(chan Result)
	go func() { done <- NewClaude(pc, nil).Run(ctx, Spec{AgentID: "k"}, c.emit) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if !res.Killed {
			t.Fatalf("want killed, got %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("kill did not stop the process tree")
	}
}

func TestFakeScenario(t *testing.T) {
	set := NewFakeSet(0)
	var c collector
	res := set[event.Codex].Run(context.Background(), Spec{AgentID: "p", StepID: "plan", Prompt: MarkerPlan + " plan it"}, c.emit)
	if !res.OK() || !strings.Contains(res.Final, "subtasks") {
		t.Fatalf("fake plan = %+v", res)
	}
}

// Recorded from codex-cli 0.160.0 behind a failing proxy (retries are
// reported as "error" events), with a successful ending appended.
func TestCodexReconnectsAreNotErrors(t *testing.T) {
	p := &codexParser{}
	evs := feed(t, p, "codex_reconnect.jsonl")
	if n := kinds(evs)[event.Error]; n != 0 {
		t.Fatalf("retries reported as %d errors", n)
	}
	var r Result
	p.Finish(&r)
	if r.Err != nil || r.Final != "hi" {
		t.Fatalf("result = %+v", r)
	}
}

func TestMidStreamLimitThenSuccessIsNotALimit(t *testing.T) {
	cmd, _, _ := fakeCLI(t, "codex_midstream_429.jsonl", 0, "")
	cfg := config.Default()
	pc := cfg.Providers[event.Codex]
	pc.Command = cmd
	var c collector
	res := NewCodex(pc, limits.NewDetector(cfg.LimitPatterns)).Run(context.Background(), Spec{AgentID: "m"}, c.emit)
	if res.LimitHit || !res.OK() || res.Final != "Finished anyway." {
		t.Fatalf("recovered run must succeed without a limit: %+v", res)
	}
	usage := 0
	for _, e := range c.evs {
		if e.Kind == event.Usage {
			usage++
		}
	}
	if usage != 1 {
		t.Errorf("usage events = %d, want exactly 1", usage)
	}
}

// An agent that leaves a detached process holding stdout must not keep the
// run (and so the whole task) alive after cancel. exec.Cmd closes our end
// of the pipe WaitDelay (set by proc.Prepare) after the cancel.
func TestCancelWithOrphanHoldingStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script CLI stub")
	}
	if _, err := os.Stat("/usr/bin/setsid"); err != nil {
		t.Skip("setsid not available")
	}
	dir := t.TempDir()
	cmd := filepath.Join(dir, "orphaning")
	// setsid puts the sleeper outside our process group, so the group kill
	// misses it, and it inherits stdout.
	os.WriteFile(cmd, []byte("#!/bin/sh\ncat >/dev/null\nsetsid sleep 30 &\necho '{\"type\":\"system\",\"subtype\":\"init\",\"model\":\"m\"}'\nsleep 30\n"), 0o755)
	cfg := config.Default()
	pc := cfg.Providers[event.Claude]
	pc.Command = cmd
	ctx, cancel := context.WithCancel(context.Background())
	var c collector
	done := make(chan Result)
	go func() { done <- NewClaude(pc, nil).Run(ctx, Spec{AgentID: "o"}, c.emit) }()
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case res := <-done:
		if !res.Killed {
			t.Errorf("want killed, got %+v", res)
		}
		if d := time.Since(start); d > 8*time.Second {
			t.Errorf("cancel took %s", d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancel hung on an orphan holding stdout")
	}
}

func TestClaudeQuotaUsesFullestWindow(t *testing.T) {
	p := &claudeParser{}
	evs := p.Line([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":100,"rateLimitType":"five_hour","utilization":0.2,"unifiedWindows":{"five_hour":{"utilization":0.2,"resetsAt":100},"seven_day":{"utilization":0.91,"resetsAt":2000000000}}}}`))
	if len(evs) != 1 || evs[0].Quota == nil {
		t.Fatalf("events = %+v", evs)
	}
	q := evs[0].Quota
	if q.Utilization != 0.91 || q.Window != "seven_day" || q.ResetsAt.Unix() != 2000000000 || q.Windows["five_hour"] != 0.2 {
		t.Errorf("quota = %+v", q)
	}
}

func TestCodexRateLimitsBecomeQuota(t *testing.T) {
	p := &codexParser{}
	evs := p.Line([]byte(`{"type":"turn.completed","usage":{"input_tokens":5,"output_tokens":1},"rate_limits":{"primary":{"used_percent":42.5,"window_minutes":300,"resets_at":2000000000},"secondary":{"used_percent":88,"window_minutes":10080,"resets_in_seconds":3600}}}`))
	var q *event.QuotaInfo
	for _, e := range evs {
		if e.Kind == event.Quota {
			q = e.Quota
		}
	}
	if q == nil {
		t.Fatalf("no quota event in %+v", evs)
	}
	if q.Utilization != 0.88 || q.Window != "7d" || q.Windows["5h"] != 0.425 {
		t.Errorf("quota = %+v", q)
	}
	// Lines without rate limits stay cheap and quota-free.
	if evs := p.Line([]byte(`{"type":"turn.started"}`)); len(evs) != 0 {
		t.Errorf("unexpected events %+v", evs)
	}
}

func TestSessionIDsAndResumeArgs(t *testing.T) {
	var r Result
	cp := &codexParser{}
	feed(t, cp, "codex_exec.jsonl")
	cp.Finish(&r)
	if r.SessionID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Errorf("codex session = %q", r.SessionID)
	}
	cfg := config.Default()
	cx := strings.Join(CodexArgs(cfg.Providers[event.Codex], Spec{Model: "m", Effort: "high", Dir: "/w", Resume: "T1"}), " ")
	if !strings.HasPrefix(cx, "exec resume --json") || !strings.HasSuffix(cx, "T1 -") || strings.Contains(cx, "--sandbox") || strings.Contains(cx, "-C ") {
		t.Errorf("codex resume args: %s", cx)
	}
	cl := ClaudeArgs(cfg.Providers[event.Claude], Spec{Model: "opus", Resume: "S1", AllowedCommands: []string{"go test ./..."}})
	j := strings.Join(cl, " ")
	if !strings.Contains(j, "--resume S1") || !strings.Contains(j, "Bash(go test ./...)") || !strings.Contains(j, "Bash(go test ./... *)") {
		t.Errorf("claude args: %q", cl)
	}
}

func TestClaudeSessionID(t *testing.T) {
	p := &claudeParser{}
	p.Line([]byte(`{"type":"system","subtype":"init","session_id":"abc-123","model":"opus"}`))
	p.Line([]byte(`{"type":"result","subtype":"success","result":"OK","session_id":"abc-123","usage":{"input_tokens":1,"output_tokens":1}}`))
	var r Result
	p.Finish(&r)
	if r.SessionID != "abc-123" {
		t.Errorf("claude session = %q", r.SessionID)
	}
}
