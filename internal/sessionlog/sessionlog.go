// Package sessionlog writes the append-only JSONL log of every routing
// decision, agent run and task outcome, and aggregates it for `sy stats`.
package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
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
	Attempt    int               `json:"attempt,omitempty"`
	Role       string            `json:"role,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Model      string            `json:"model,omitempty"`
	Effort     string            `json:"effort,omitempty"`
	Rule       string            `json:"rule,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Confidence float64           `json:"confidence,omitempty"`
	Fallback   bool              `json:"fallback,omitempty"`
	OK         *bool             `json:"ok,omitempty"`
	LimitHit   bool              `json:"limit_hit,omitempty"`
	Error      string            `json:"error,omitempty"`
	Text       string            `json:"text,omitempty"`
	Tokens     *event.TokenUsage `json:"tokens,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Files      []string          `json:"files,omitempty"`
}

// Bool returns a pointer for Record.OK.
func Bool(b bool) *bool { return &b }

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
	session := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, session+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, session: session, cwd: cwd, path: path}
	w.Write(Record{Type: TypeSession})
	return w, nil
}

// Session returns the session id.
func (w *Writer) Session() string {
	if w == nil {
		return time.Now().Format("20060102-150405")
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
	w.f.Write(append(b, '\n'))
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
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
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
	}
	return out, nil
}
