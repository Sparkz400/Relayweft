// Package sessionlog writes the append-only JSONL log of every routing
// decision, agent run and task outcome, and aggregates it for `rw stats`.
package sessionlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
)

// Record types.
const (
	TypeSession  = "session"
	TypeTask     = "task_start"
	TypeTaskEnd  = "task_end"
	TypeDecision = "decision"
	TypeAgentEnd = "agent_end"
	TypeLimit    = "limit"
	TypeReview   = "review"
	TypeMerge    = "merge"
	TypeConfig   = "config_change"
	TypeQuota    = "quota" // provider-reported quota (resets_at), logged when it changes
	// TypeBestOf is one candidate of a best-of step: OK says whether it
	// was kept, Reason how the winner was picked (BestOfBy*), Passed
	// whether its checks passed (nil: none ran).
	TypeBestOf = "best_of"
)

// How a best-of step's winner was picked (Record.Reason of TypeBestOf).
// Only a pick by checks or by the reviewer says something about the
// losers' routes.
const (
	BestOfByChecks   = "checks"   // its checks passed and the others' failed
	BestOfByReviewer = "reviewer" // the reviewer compared the diffs
	BestOfByOrder    = "order"    // the fixed order: fewer failing checks, a change, the smaller diff, the cheaper run
	BestOfOnly       = "only"     // the only candidate whose agent finished
	BestOfNone       = "none"     // every candidate failed
)

// Record is one JSONL line.
type Record struct {
	Type       string            `json:"type"`
	TS         time.Time         `json:"ts"`
	Session    string            `json:"session"`
	Cwd        string            `json:"cwd,omitempty"`
	Task       string            `json:"task,omitempty"`
	TaskID     string            `json:"task_id,omitempty"`
	Mode       string            `json:"mode,omitempty"` // routed | single | demo
	Agent      string            `json:"agent,omitempty"`
	Step       string            `json:"step,omitempty"`
	Kind       string            `json:"kind,omitempty"` // decision, agent_end: the step's kind (edit, explore, ...); verify: full | affected
	Attempt    int               `json:"attempt,omitempty"`
	Role       string            `json:"role,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Model      string            `json:"model,omitempty"`
	Effort     string            `json:"effort,omitempty"`
	Rule       string            `json:"rule,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Confidence float64           `json:"confidence,omitempty"`
	Judged     bool              `json:"judged,omitempty"` // decision: the judge model picked or confirmed the role
	Tier       string            `json:"tier,omitempty"`   // decision: the model tier routing.tiers picked
	Fallback   bool              `json:"fallback,omitempty"`
	From       string            `json:"from,omitempty"` // decision: the provider a fallback moved away from
	OK         *bool             `json:"ok,omitempty"`
	LimitHit   bool              `json:"limit_hit,omitempty"`
	Error      string            `json:"error,omitempty"`
	Text       string            `json:"text,omitempty"`
	Tokens     *event.TokenUsage `json:"tokens,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Files      []string          `json:"files,omitempty"`
	Cost       *event.TaskCost   `json:"cost,omitempty"`  // task_end
	Bench      string            `json:"bench,omitempty"` // task name in a `rw bench` run
	Passed     *bool             `json:"passed,omitempty"`
	Quota      *event.QuotaInfo  `json:"quota,omitempty"` // quota
	Until      *time.Time        `json:"until,omitempty"` // limit: limited until (the reset time when known)

	// Unavailable says whether a limit record (and its agent_end) was no
	// usage limit: the CLI was logged out or missing, and rw routed around
	// it the same way. Always set on those records, so nil means a log from
	// before the field (see IsUnavailable).
	Unavailable *bool `json:"unavailable,omitempty"`
}

// Bool returns a pointer for Record.OK.
func Bool(b bool) *bool { return &b }

// reUnavailable matches CLI errors that mean "not logged in" or "not
// installed".
var reUnavailable = regexp.MustCompile(`(?i)(not logged in|please (log|sign) ?in|log ?in required|unauthori[sz]ed|\b401\b|authentication (failed|required)|token (has )?expired|codex login|not found on PATH)`)

// UnavailableError reports whether an agent's error means its CLI is
// logged out or missing: rw routes around it like a provider at its limit.
func UnavailableError(msg string) bool { return reUnavailable.MatchString(msg) }

// IsUnavailable reports whether a limit record, or an agent_end with a
// limit hit, was a logged-out or missing CLI rather than a usage limit.
// Logs written before Record.Unavailable (sy, rw 0.3) are told by the
// error text.
func IsUnavailable(r Record) bool {
	if r.Unavailable != nil {
		return *r.Unavailable
	}
	switch {
	case r.Type == TypeLimit:
		return UnavailableError(r.Text)
	case r.Type == TypeAgentEnd && r.LimitHit:
		return UnavailableError(r.Error)
	}
	return false
}

// Writer appends records to one session file. A nil *Writer discards.
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	session string
	cwd     string
	path    string
}

// Open creates <dir>/<timestamp>-<session>.jsonl.
func Open(dir, cwd string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	session := newSessionID()
	path := filepath.Join(dir, session+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, session: session, cwd: cwd, path: path}
	w.Write(Record{Type: TypeSession})
	return w, nil
}

// newSessionID is the start time plus a random suffix: two rw started in
// the same second must not share kept branches or undo refs.
// Format: 20060102-150405-1a2b.
func newSessionID() string {
	var b [2]byte
	rand.Read(b[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// nilSession is the session id of a nil Writer, fixed for the process so
// every caller in one rw agrees on it.
var nilSession = sync.OnceValue(newSessionID)

// Session returns the session id.
func (w *Writer) Session() string {
	if w == nil {
		return nilSession()
	}
	return w.session
}

// Path returns the log file path.
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// Write appends one record, filling in time, session and cwd.
func (w *Writer) Write(r Record) {
	if w == nil {
		return
	}
	if r.TS.IsZero() {
		r.TS = time.Now()
	}
	r.Session = w.session
	if r.Cwd == "" {
		r.Cwd = w.cwd
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.f.Write(append(b, '\n')) // a failed log write has nowhere to go
}

// Close closes the file.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// ReadDir loads every record from every .jsonl file in dir. Broken lines
// are skipped so one crashed session does not hide the rest.
func ReadDir(dir string) ([]Record, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, f := range files {
		recs, err := readFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}

// readFile loads one .jsonl file, skipping broken lines.
func readFile(f string) ([]Record, error) {
	data, err := os.ReadFile(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	var out []Record
	start := 0
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			line := data[start:i]
			start = i + 1
			if len(line) == 0 {
				continue
			}
			var r Record
			if json.Unmarshal(line, &r) == nil {
				out = append(out, r)
			}
		}
	}
	return out, nil
}
