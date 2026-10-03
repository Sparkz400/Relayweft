package sessionlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/switchyard/internal/event"
)

func TestWriteReadAggregate(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, "/proj")
	if err != nil {
		t.Fatal(err)
	}
	tk := event.TokenUsage{Input: 1000, Output: 200}
	w.Write(Record{Type: TypeTask, TaskID: "task-1", Mode: "routed"})
	w.Write(Record{Type: TypeDecision, TaskID: "task-1", Rule: "default", Provider: "codex", Model: "gpt-6.1-sol"})
	w.Write(Record{Type: TypeDecision, TaskID: "task-1", Rule: "limit-fallback", Fallback: true})
	w.Write(Record{Type: TypeAgentEnd, TaskID: "task-1", Role: "worker", Provider: "codex", Model: "gpt-6.1-sol", OK: Bool(true), Tokens: &tk, DurationMS: 4000})
	w.Write(Record{Type: TypeAgentEnd, TaskID: "task-1", Role: "worker", Provider: "codex", Model: "gpt-6.1-sol", OK: Bool(false), LimitHit: true})
	w.Write(Record{Type: TypeReview, TaskID: "task-1", OK: Bool(true)})
	w.Write(Record{Type: TypeTaskEnd, TaskID: "task-1", Mode: "routed", OK: Bool(true), Tokens: &tk, DurationMS: 9000})
	w.Close()
	// A broken line must not hide the rest.
	f, _ := os.OpenFile(w.Path(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{broken\n")
	f.Close()

	recs, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 8 { // session + 7
		t.Fatalf("records = %d", len(recs))
	}
	s := Aggregate(recs, Filter{})
	if len(s.Routes) != 1 || s.Routes[0].Calls != 2 || s.Routes[0].OK != 1 || s.Routes[0].LimitHits != 1 {
		t.Errorf("routes = %+v", s.Routes[0])
	}
	if s.Rules["default"] != 1 || s.Fallbacks != 1 || s.Reviews != 1 {
		t.Errorf("rules = %v fallbacks = %d", s.Rules, s.Fallbacks)
	}
	if len(s.Modes) != 1 || s.Modes[0].Tasks != 1 || s.Modes[0].PerProv["codex"] != 1200 {
		t.Errorf("modes = %+v", s.Modes[0])
	}
	if got := Aggregate(recs, Filter{Cwd: filepath.FromSlash("/elsewhere")}); got.Sessions != 0 {
		t.Errorf("cwd filter kept %d sessions", got.Sessions)
	}
	var b bytes.Buffer
	s.Print(&b)
	for _, want := range []string{"codex:gpt-6.1-sol", "limit-fallback", "routed"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report misses %q:\n%s", want, b.String())
		}
	}
}

func TestNilWriterIsSafe(t *testing.T) {
	var w *Writer
	w.Write(Record{Type: TypeTask})
	if w.Path() != "" || w.Close() != nil || w.Session() == "" {
		t.Fatal("nil writer misbehaves")
	}
}
