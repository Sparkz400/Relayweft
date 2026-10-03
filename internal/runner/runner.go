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

// Exec is the shared subprocess driver used by the Codex and Claude runners.
type Exec struct {
	Provider string
	Cfg      config.ProviderCfg
	Detector *limits.Detector
	args     func(s Spec) []string
	parser   func() lineParser
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
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, path, x.args(s)...)
	proc.Prepare(cmd)
	cmd.Dir = s.Dir
	// The prompt goes in on stdin: multi-line prompts as arguments get
	// mangled by cmd.exe when the CLI is an npm .cmd shim on Windows.
	cmd.Stdin = strings.NewReader(s.Prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr := &tail{max: 8 << 10}
	cmd.Stderr = stderr
	diag.Logf("spawn agent=%s role=%s step=%s attempt=%d model=%s effort=%s readonly=%v dir=%s: %s %s",
		s.AgentID, s.Role, s.StepID, s.Attempt, s.Model, s.Effort, s.ReadOnly, s.Dir, path, strings.Join(x.args(s), " "))
	if err := cmd.Start(); err != nil {
		diag.Logf("spawn agent=%s failed: %v", s.AgentID, err)
		return fail(fmt.Errorf("start %s: %w", x.Provider, err))
	}
	proc.Started(cmd)
	emit(stamp(event.Event{Kind: event.Started, Text: "started " + s.Model}))

	p := x.parser()
	var res Result
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		for _, e := range p.Line(line) {
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
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, os.ErrClosed) {
		stderr.Write([]byte("\nread stdout: " + err.Error()))
		// Keep draining so the CLI never blocks on a full pipe.
		io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
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
	if res.Err != nil && !res.LimitHit {
		if x.Detector.Match(res.Err.Error()) || (waitErr != nil && x.Detector.Match(stderr.String())) {
			res.LimitHit = true
			if t, ok := limits.ParseReset(res.Err.Error()+"\n"+stderr.String(), time.Now()); ok {
				res.ResetAt = t
			}
			emit(stamp(event.Event{Kind: event.LimitHit, Text: res.Err.Error()}))
		}
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
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

// New builds the real runners from config.
func New(cfg *config.Config) Set {
	det := limits.NewDetector(cfg.LimitPatterns)
	return Set{
		event.Codex:  NewCodex(cfg.Providers[event.Codex], det),
		event.Claude: NewClaude(cfg.Providers[event.Claude], det),
	}
}
