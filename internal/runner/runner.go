// Package runner drives the official codex and claude CLIs as subprocesses
// and turns their JSON lines into normalized events.
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/proc"
)

// Spec describes one agent run.
type Spec struct {
	AgentID  string
	ParentID string
	StepID   string
	Attempt  int
	Role     string
	Provider string
	Model    string
	Effort   string
	Prompt   string
	Dir      string
	ReadOnly bool
	Timeout  time.Duration
	// Resume continues an earlier session of the same CLI (a follow-up
	// message) instead of starting a new one.
	Resume string
	// AllowedCommands are shell command prefixes a writing agent may run
	// without asking (verify/test commands); Claude needs them listed.
	AllowedCommands []string
	// MCP is the run's MCP servers. Exec fills it from its MCP config for
	// the spec's role; callers leave it nil.
	MCP *MCPRun
	// OnSession, when set, is called with the CLI's session id as soon as
	// the CLI reports it (Claude's init line, Codex's thread.started), long
	// before the run ends: a step whose sy dies mid-run can resume that
	// session. It runs on the runner's goroutine.
	OnSession func(id string)
}

// Result is what an agent run produced.
type Result struct {
	Final    string // the agent's last message
	Tokens   event.TokenUsage
	Err      error // nil on success
	LimitHit bool
	ResetAt  time.Time // when the limit resets, if the CLI said so
	Files    []string  // files the agent reported editing
	Duration time.Duration
	Killed   bool // cancelled by the user or the orchestrator
	// SessionID identifies the CLI session so a follow-up can resume it.
	SessionID string
}

// OK reports whether the run succeeded.
func (r Result) OK() bool { return r.Err == nil && !r.LimitHit && !r.Killed }

// Runner runs one agent to completion. emit is called from the runner's
// goroutine for every event; Run blocks until the process exits. Cancelling
// ctx kills the process tree.
type Runner interface {
	Run(ctx context.Context, s Spec, emit func(event.Event)) Result
}

// Set picks a runner per provider.
type Set map[string]Runner

// lineParser turns one CLI's stdout lines into events and remembers the
// pieces needed for the final Result.
type lineParser interface {
	Line(line []byte) []event.Event
	Finish(r *Result)
}

// Exec is the shared subprocess driver used by every CLI runner.
type Exec struct {
	Provider string // the provider's name in the config
	// Kind is the CLI protocol (event.Codex, event.Claude...); "" = Provider.
	Kind     string
	Cfg      config.ProviderCfg
	Detector *limits.Detector
	// MCP servers handed to the CLI (config.MCPCfg.For picks per role).
	MCP config.MCPCfg
	// LookupEnv reads ${VAR}s in MCP values (nil = os.LookupEnv).
	LookupEnv func(string) (string, bool)
	args      func(s Spec) []string
	parser    func() lineParser
	// precheck refuses a run the CLI could not do properly (nil = none).
	precheck func(s Spec) error
	// limitExitCodes are exit codes that mean "at the usage limit".
	limitExitCodes []int
}

// Optional lineParser extras.
type (
	// blankKeeper wants empty lines too (text output keeps paragraphs).
	blankKeeper interface{ keepBlankLines() }
	// flusher has events left once stdout ends (a streamed message).
	flusher interface{ Flush() []event.Event }
	// stderrReader reads the CLI's stderr tail before Finish (token counts
	// some CLIs print there).
	stderrReader interface{ Stderr(string) }
	// sessionReporter knows the CLI's session id before the run ends.
	sessionReporter interface{ sessionID() string }
)

func (x *Exec) kind() string {
	if x.Kind != "" {
		return x.Kind
	}
	return x.Provider
}

// Run implements Runner.
func (x *Exec) Run(ctx context.Context, s Spec, emit func(event.Event)) Result {
	start := time.Now()
	stamp := func(e event.Event) event.Event {
		e.AgentID, e.ParentID, e.Provider, e.Role = s.AgentID, s.ParentID, x.Provider, s.Role
		if e.Model == "" {
			e.Model = s.Model
		}
		return e.Stamp()
	}
	fail := func(err error) Result {
		emit(stamp(event.Event{Kind: event.Error, Text: err.Error()}))
		emit(stamp(event.Event{Kind: event.Done, Text: err.Error()}))
		return Result{Err: err, Duration: time.Since(start)}
	}

	path, err := proc.Resolve(x.Cfg.Command)
	if err != nil {
		return fail(fmt.Errorf("%s CLI %q not found on PATH: %w", x.Provider, x.Cfg.Command, err))
	}
	if x.precheck != nil {
		if err := x.precheck(s); err != nil {
			return fail(fmt.Errorf("%s: %w", x.Provider, err))
		}
	}
	provEnv, missing := x.Cfg.EnvFor(x.LookupEnv)
	if len(missing) > 0 {
		return fail(fmt.Errorf("%s needs %s set in your environment (providers.%s.env)", x.Provider, strings.Join(missing, ", "), x.Provider))
	}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	if s.MCP == nil {
		m, cleanup, err := prepareMCP(x.Provider, x.kind(), s.Role, x.MCP, x.LookupEnv)
		if err != nil {
			return fail(err)
		}
		// The temp config file lives exactly as long as the agent.
		defer cleanup()
		s.MCP = m
	}
	argv := x.args(s)
	cmd := exec.CommandContext(ctx, path, argv...)
	proc.Prepare(cmd)
	cmd.Dir = s.Dir
	// Without sy's forge and CI tokens (proc.WithoutSecrets); what the
	// MCP servers and the provider name explicitly is added back below.
	cmd.Env = proc.WithoutSecrets(os.Environ())
	if s.MCP != nil {
		// MCP secrets from ${VAR}: in the environment, not on the command
		// line (codexMCPArgs names them).
		cmd.Env = append(cmd.Env, s.MCP.ChildEnv...)
	}
	// The provider's env (an API endpoint and key) comes last and wins.
	cmd.Env = append(cmd.Env, provEnv...)
	// The prompt goes in on stdin: multi-line prompts as arguments get
	// mangled by cmd.exe when the CLI is an npm .cmd shim on Windows.
	cmd.Stdin = strings.NewReader(s.Prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr := &tail{max: 8 << 10}
	cmd.Stderr = stderr
	diag.Logf("spawn agent=%s provider=%s role=%s step=%s attempt=%d model=%s effort=%s readonly=%v dir=%s: %s %s",
		s.AgentID, x.Provider, s.Role, s.StepID, s.Attempt, s.Model, s.Effort, s.ReadOnly, s.Dir, path, strings.Join(redactArgs(argv), " "))
	if len(provEnv) > 0 {
		// Names only: values may be API keys.
		diag.Logf("env agent=%s %s", s.AgentID, strings.Join(envNames(provEnv), ","))
	}
	if s.MCP != nil {
		// Names only: env values and headers may be secrets.
		diag.Logf("mcp agent=%s servers=%s unset=%s", s.AgentID, strings.Join(s.MCP.Names, ","), strings.Join(s.MCP.Missing, ","))
	}
	if err := cmd.Start(); err != nil {
		diag.Logf("spawn agent=%s failed: %v", s.AgentID, err)
		return fail(fmt.Errorf("start %s: %w", x.Provider, err))
	}
	proc.Started(cmd)
	emit(stamp(event.Event{Kind: event.Started, Text: "started " + s.Model}))

	p := x.parser()
	var res Result
	handle := func(evs []event.Event) {
		for _, e := range evs {
			if e.Kind == event.Error && x.Detector.Match(e.Text) {
				e.Kind = event.LimitHit
			}
			if e.Kind == event.LimitHit {
				res.LimitHit = true
				if t, ok := limits.ParseReset(e.Text, time.Now()); ok && res.ResetAt.IsZero() {
					res.ResetAt = t
				}
			}
			emit(stamp(e))
		}
	}
	_, keepBlank := p.(blankKeeper)
	sr, _ := p.(sessionReporter)
	reported := ""
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 && !keepBlank {
			continue
		}
		handle(p.Line(line))
		if sr != nil && s.OnSession != nil {
			if id := sr.sessionID(); id != "" && id != reported {
				reported = id
				s.OnSession(id)
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, os.ErrClosed) {
		stderr.Write([]byte("\nread stdout: " + err.Error()))
		// Keep draining so the CLI never blocks on a full pipe.
		io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if f, ok := p.(flusher); ok {
		handle(f.Flush())
	}
	if sr, ok := p.(stderrReader); ok {
		sr.Stderr(stderr.String())
	}
	p.Finish(&res)
	res.Duration = time.Since(start)
	if res.LimitHit && res.Err == nil && waitErr == nil && strings.TrimSpace(res.Final) != "" {
		// A limit message mid-stream that the CLI recovered from (it still
		// finished with an answer) is not a limit hit.
		res.LimitHit, res.ResetAt = false, time.Time{}
	}

	switch {
	case ctx.Err() != nil:
		res.Killed = true
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.Killed = false
			res.Err = fmt.Errorf("timed out after %s", s.Timeout)
			diag.Health("agent-timeout", "agent", s.AgentID, "after", s.Timeout)
		} else {
			res.Err = errors.New("killed")
		}
	case waitErr != nil && res.Err == nil:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		res.Err = fmt.Errorf("%s exited: %s", x.Provider, lastLines(msg, 6))
	}
	if code := exitCode(cmd); waitErr != nil && !res.LimitHit && ctx.Err() == nil && containsInt(x.limitExitCodes, code) {
		res.LimitHit = true
		if res.Err == nil {
			res.Err = fmt.Errorf("%s exited with code %d (a usage limit): %s", x.Provider, code, lastLines(stderr.String(), 3))
		}
		if t, ok := limits.ParseReset(stderr.String(), time.Now()); ok {
			res.ResetAt = t
		}
		emit(stamp(event.Event{Kind: event.LimitHit, Text: res.Err.Error()}))
	}
	if res.Err != nil && !res.LimitHit {
		if x.Detector.Match(res.Err.Error()) || (waitErr != nil && x.Detector.Match(stderr.String())) {
			res.LimitHit = true
			if t, ok := limits.ParseReset(res.Err.Error()+"\n"+stderr.String(), time.Now()); ok {
				res.ResetAt = t
			}
			emit(stamp(event.Event{Kind: event.LimitHit, Text: res.Err.Error()}))
		}
	}
	code := exitCode(cmd)
	diag.Logf("exit agent=%s pid=%d code=%d after %s ok=%v killed=%v limit=%v tokens=%d err=%v stderr=%q",
		s.AgentID, cmd.Process.Pid, code, res.Duration.Round(time.Millisecond), res.OK(), res.Killed, res.LimitHit,
		res.Tokens.Total(), res.Err, lastLines(stderr.String(), 4))
	if res.Err != nil && !res.LimitHit && !res.Killed {
		emit(stamp(event.Event{Kind: event.Error, Text: res.Err.Error()}))
	}
	if res.Tokens.Total() > 0 {
		emit(stamp(event.Event{Kind: event.Usage, Tokens: res.Tokens}))
	}
	done := event.Event{Kind: event.Done, OK: res.OK(), Tokens: res.Tokens, Text: SummaryLine(res.Final)}
	if !res.OK() && res.Err != nil {
		done.Text = res.Err.Error()
	}
	emit(stamp(done))
	return res
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

func containsInt(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// SummaryLine returns the first meaningful line of an agent reply for
// one-line displays: code fences and blank lines are skipped and a JSON
// reply is shown by its "summary" or "advice" field when it has one.
func SummaryLine(s string) string {
	s = strings.TrimSpace(s)
	for _, key := range []string{`"summary"`, `"advice"`} {
		if i := strings.Index(s, key); i >= 0 {
			rest := strings.TrimLeft(s[i+len(key):], " :")
			if strings.HasPrefix(rest, `"`) {
				if j := strings.Index(rest[1:], `"`); j > 0 {
					return rest[1 : j+1]
				}
			}
		}
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#*>- "))
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		if strings.HasPrefix(line, "{") {
			return "" // bare JSON without a summary field: nothing readable
		}
		return line
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// fileSet collects edited paths in first-seen order.
type fileSet struct {
	seen  map[string]bool
	order []string
}

func (f *fileSet) add(p string) {
	if p == "" {
		return
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if !f.seen[p] {
		f.seen[p] = true
		f.order = append(f.order, p)
	}
}

func (f *fileSet) list() []string {
	out := append([]string(nil), f.order...)
	sort.Strings(out)
	return out
}

// New builds the real runners from config: one per configured provider,
// driven by its kind.
func New(cfg *config.Config) Set {
	det := limits.NewDetector(cfg.LimitPatterns)
	set := Set{}
	for _, name := range cfg.ProviderNames() {
		pc := cfg.Providers[name]
		var x *Exec
		switch kind := pc.KindOf(name); kind {
		case event.Codex:
			x = NewCodex(pc, det)
		case event.Claude:
			x = NewClaude(pc, det)
			if pc.Env["ANTHROPIC_BASE_URL"] != "" {
				// Claude Code against another API (Ollama, DeepSeek): its
				// price estimate is for Anthropic's models, not these.
				x.parser = func() lineParser { return &claudeParser{noCost: true, cacheGuess: true} }
			}
		case event.Gemini:
			x = NewGemini(pc, det)
		case event.Qwen:
			x = NewQwen(pc, det)
		case event.Generic:
			x = NewGeneric(name, pc, cfg.LimitPatterns)
		default:
			continue // rejected by config validation
		}
		x.Provider, x.Kind, x.MCP = name, pc.KindOf(name), cfg.MCP
		set[name] = x
	}
	return set
}

// envNames returns the NAME part of NAME=value pairs.
func envNames(env []string) []string {
	out := make([]string, len(env))
	for i, kv := range env {
		out[i], _, _ = strings.Cut(kv, "=")
	}
	return out
}
