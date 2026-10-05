package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
)

// Fuzz targets for the CLI stream parsers (ROADMAP 1.8): odd model or CLI
// output must never crash rw, never leave it stuck and never produce a
// Result that hides an error.
//
//	go test -run='^$' -fuzz=FuzzCodexParser -fuzztime=60s ./internal/runner
//	go test -run='^$' -fuzz=FuzzClaudeParser -fuzztime=60s ./internal/runner
//	go test -run='^$' -fuzz=FuzzGenericParser -fuzztime=60s ./internal/runner

// seedStreams adds every recorded fixture (whole, and line by line) plus a
// few hand-written edge cases.
func seedStreams(f *testing.F, prefix string, extra ...string) {
	files, _ := filepath.Glob(filepath.Join("testdata", prefix+"*.jsonl"))
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
		for _, line := range bytes.Split(data, []byte("\n")) {
			if len(bytes.TrimSpace(line)) > 0 {
				f.Add(append([]byte(nil), line...))
			}
		}
	}
	for _, s := range extra {
		f.Add([]byte(s))
	}
}

// parseStream feeds data line by line, as Exec does, and fails if the
// parser takes unreasonably long (a parser must never block the run).
func parseStream(t *testing.T, p lineParser, data []byte) ([]event.Event, Result) {
	t.Helper()
	type out struct {
		evs []event.Event
		res Result
	}
	done := make(chan out, 1)
	go func() {
		var o out
		for _, line := range bytes.Split(data, []byte("\n")) {
			if len(line) == 0 {
				continue // Exec skips empty lines
			}
			o.evs = append(o.evs, p.Line(line)...)
		}
		p.Finish(&o.res)
		done <- o
	}()
	select {
	case o := <-done:
		return o.evs, o.res
	case <-time.After(10 * time.Second):
		t.Fatalf("parser blocked on %d bytes of input", len(data))
		return nil, Result{}
	}
}

// checkResult asserts the invariants every parser shares.
func checkResult(t *testing.T, evs []event.Event, r Result) {
	t.Helper()
	sawErr := false
	for _, e := range evs {
		if e.Kind == event.Error {
			sawErr = true
			if e.Text == "" {
				t.Fatalf("error event without a message: %+v", e)
			}
		}
		if e.Kind == event.Quota && e.Quota == nil {
			t.Fatalf("quota event without quota info")
		}
	}
	if !sort.StringsAreSorted(r.Files) {
		t.Fatalf("files not sorted: %q", r.Files)
	}
	for i, f := range r.Files {
		if f == "" {
			t.Fatalf("empty file path in %q", r.Files)
		}
		if i > 0 && r.Files[i-1] == f {
			t.Fatalf("duplicate file %q", f)
		}
	}
	if r.Err != nil && r.Err.Error() == "" {
		t.Fatal("Result.Err with an empty message")
	}
	// An error the CLI reported must not be lost: either the run failed or
	// the agent still produced an answer after it.
	if sawErr && r.Err == nil && r.Final == "" {
		t.Fatalf("error event but Result has neither Err nor Final: %+v", r)
	}
	_ = SummaryLine(r.Final) // must not panic on whatever the agent said
}

func FuzzCodexParser(f *testing.F) {
	seedStreams(f, "codex",
		`{"type":"turn.failed"}`,
		`{"id":"1","msg":{"type":"patch_apply_begin","changes":{"":{},"a.go":{}}}}`,
		`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":""},{"path":"b"},{"path":"b"}]}}`,
		`{"type":"item.completed","item":{"type":"todo_list","items":[{"text":"x","completed":true}]}}`,
		`{"type":"item.completed","item":null}`,
		`{"rate_limits":{"primary":{"used_percent":1e308,"window_minutes":-5,"resets_in_seconds":1e300}}}`,
		`{"a":[{"b":{"c":{"d":{"rate_limits":{"secondary":{"used_percent":5}}}}}}]}`,
		"not json at all\n\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":9223372036854775807}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":9223372036854775807}}",
	)
	f.Fuzz(func(t *testing.T, data []byte) {
		evs, r := parseStream(t, &codexParser{}, data)
		checkResult(t, evs, r)
		if r.LimitHit {
			t.Fatal("the codex parser never decides a limit hit itself (Exec does)")
		}
	})
}

func FuzzClaudeParser(f *testing.F) {
	seedStreams(f, "claude",
		`{"type":"result","is_error":true}`,
		`{"type":"result","subtype":"error_max_turns","errors":["",""]}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":-1,"unifiedWindows":{"":{"utilization":-1}}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":""}},{"type":"tool_use","name":"Write","input":"x"},{"type":"text","text":"   "}]}}`,
		`{"type":"assistant","message":null}`,
		`{"type":"system","subtype":"task_summary","detail":null}`,
		`{"type":"system","subtype":"init","session_id":"s","model":""}`,
		`{"type":"system","subtype":"permission_denied","tool_use_id":"t","message":{"content":[]}}`,
		`{"type":"assistant","message":"text"}`,
		`{"type":"result","permission_denials":[{"tool_use_id":"t"},{"tool_use_id":"t"},{}]}`,
	)
	f.Fuzz(func(t *testing.T, data []byte) {
		evs, r := parseStream(t, &claudeParser{}, data)
		checkResult(t, evs, r)
		for _, e := range evs {
			if e.Kind == event.LimitHit && !r.LimitHit {
				t.Fatalf("limit event %q but Result.LimitHit is false", e.Text)
			}
		}
	})
}

// FuzzGenericParser runs the generic JSON-rules parser with the Qwen Code
// and Gemini CLI descriptions, and the text parser, over the same inputs.
func FuzzGenericParser(f *testing.F) {
	seedStreams(f, "qwen", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"write_file","input":null}]}}`)
	seedStreams(f, "gemini", `{"type":"message","role":"assistant","content":7,"delta":true}`, `{"type":"result","status":"error","error":null}`)
	f.Add([]byte("\x1b[?25lhi\r\n\nprompt eval count: 99999999999999999999\n"))
	var gs []*config.GenericCfg
	for _, file := range []string{"generic_qwen.yaml", "generic_gemini.yaml"} {
		data, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			f.Fatal(err)
		}
		var pc config.ProviderCfg
		if err := yaml.Unmarshal(data, &pc); err != nil {
			f.Fatal(err)
		}
		gs = append(gs, pc.Generic)
	}
	text := config.Default().Providers["ollama-run"].Generic
	limit := &config.GenericCfg{Output: config.OutputJSONL, JSON: []config.JSONRule{
		{Match: map[string]string{"type": "*"}, Text: "text", Error: "error", Limit: true, InputTokens: "n"}}}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, g := range append(gs, text, limit) {
			evs, r := parseStream(t, newGenericParser(g), data)
			checkResult(t, evs, r)
			for _, e := range evs {
				if e.Kind == event.LimitHit && !r.LimitHit {
					t.Fatalf("limit event %q but Result.LimitHit is false", e.Text)
				}
			}
		}
	})
}
