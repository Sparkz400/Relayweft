package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNoteFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pin(v bool) *bool { return &v }

func TestNoteFormatRoundTrip(t *testing.T) {
	n := note{body: "- 2026-10-06: fix the parser\n  result: ok", refs: []noteRef{{"a.go", "abc"}, {"dir/b#1.go", "def"}}, more: 3, pinned: true}
	got := parseNote(n.String())
	if got.String() != n.String() || !got.pinned || got.more != 3 || len(got.refs) != 2 || got.refs[1].path != "dir/b#1.go" {
		t.Fatalf("%+v", got)
	}
	if b := parseNote("only text\r\n  pinned: yes").body; b != "only text" {
		t.Fatalf("CRLF note body %q", b)
	}
}

func TestTerms(t *testing.T) {
	got := terms("Fix ProjectMemory in internal/web/static/app.js for notes_include and the 2026 HTTPServer")
	for _, w := range []string{"projectmemory", "project", "memory", "internal", "web", "static", "app", "notes_include", "notes", "include", "httpserver"} {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	for _, w := range []string{"fix", "the", "2026", "js", "for"} {
		if got[w] {
			t.Errorf("unexpected %q", w)
		}
	}
}

// A task gets its pinned conventions, the newest notes and the notes that
// match it; unrelated notes stay out.
func TestSelectNotesByTask(t *testing.T) {
	root := gitRepo(t)
	topics := []struct{ task, file string }{
		{"speed up the tokenizer", "lex/tokenizer.go"},
		{"retry webhooks on 502", "hooks/webhook.go"},
		{"handle quoted strings in the parser", "parse/parser.go"},
		{"rename billing exports", "billing/export.go"},
		{"tune the cache eviction", "cache/lru.go"},
		{"translate the settings page", "ui/settings.js"},
		{"log slow queries", "db/slowlog.go"},
	}
	for _, c := range topics {
		writeNoteFile(t, root, c.file, c.task)
		if err := addRepoNote(root, c.task, "done", []string{c.file}); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := ProjectMemory(root, "")
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, Text: "Errors are wrapped with %w, never logged and returned.", Pin: pin(true)}); err != nil {
		t.Fatal(err)
	}
	const task = "The parser drops escaped quotes; fix parser.go"
	v, err := ProjectMemory(root, task)
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]bool{}
	for _, e := range v.Entries {
		if e.Reason == "" {
			t.Errorf("no reason for %q", e.Text)
		}
		if !e.Pinned {
			in[strings.SplitN(e.Text, "\n", 2)[0][len("- 2026-10-06: "):]] = e.Included
		}
	}
	want := map[string]bool{
		"handle quoted strings in the parser": true, // related
		"translate the settings page":         true, // newest
		"log slow queries":                    true, // newest
		"speed up the tokenizer":              false,
		"retry webhooks on 502":               false,
		"rename billing exports":              false,
		"tune the cache eviction":             false,
	}
	for k, w := range want {
		if in[k] != w {
			t.Errorf("%q included = %v, want %v", k, in[k], w)
		}
	}
	if last := v.Entries[len(v.Entries)-1]; !last.Pinned || !last.Included {
		t.Fatalf("pinned convention left out: %+v", last)
	}
	if !strings.Contains(v.Prompt, "PROJECT CONVENTIONS PINNED") || strings.Index(v.Prompt, "%w") > strings.Index(v.Prompt, "quoted strings") {
		t.Fatalf("conventions not first:\n%s", v.Prompt)
	}
	if !strings.Contains(v.Prompt, "files: parse/parser.go") || strings.Contains(v.Prompt, "#") {
		t.Fatalf("prompt must name sources without fingerprints:\n%s", v.Prompt)
	}
	if v.Prompt != repoNotes(root, task) {
		t.Fatal("preview differs from actual prompt context")
	}
	// Without a task the newest notes fill the slots, as before.
	if v, _ = ProjectMemory(root, ""); strings.Contains(v.Prompt, "speed up the tokenizer") || !strings.Contains(v.Prompt, "retry webhooks") {
		t.Fatalf("no-task selection:\n%s", v.Prompt)
	}
}

// A note retires when the code it describes changes, and comes back when the
// user confirms it; a pinned convention stays but is marked.
func TestNoteInvalidation(t *testing.T) {
	root := gitRepo(t)
	writeNoteFile(t, root, "a.go", "package a")
	writeNoteFile(t, root, "b.go", "package b")
	if err := addRepoNote(root, "add helpers", "ok", []string{"a.go", "b.go", "deleted.go"}); err != nil {
		t.Fatal(err)
	}
	v, _ := ProjectMemory(root, "")
	e := v.Entries[0]
	if !e.Included || len(e.Sources) != 2 || e.Sources[0].State != "current" {
		t.Fatalf("%+v", e)
	}
	writeNoteFile(t, root, "a.go", "package a // changed")
	v, _ = ProjectMemory(root, "")
	if e = v.Entries[0]; !e.Included || e.Sources[0].State != "changed" || !strings.Contains(v.Prompt, "a.go (changed since)") {
		t.Fatalf("partly changed note: %+v\n%s", e, v.Prompt)
	}
	os.Remove(filepath.Join(root, "b.go"))
	v, _ = ProjectMemory(root, "")
	if e = v.Entries[0]; e.Included || !strings.Contains(e.Reason, "changed since") || v.Prompt != "" {
		t.Fatalf("note on changed code still included: %+v", e)
	}
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, ID: e.ID, Refresh: true}); err != nil {
		t.Fatal(err)
	}
	v, _ = ProjectMemory(root, "")
	if e = v.Entries[0]; !e.Included || len(e.Sources) != 1 || e.Sources[0].State != "current" {
		t.Fatalf("refreshed note: %+v", e)
	}
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, ID: e.ID, Pin: pin(true)}); err != nil {
		t.Fatal(err)
	}
	writeNoteFile(t, root, "a.go", "package a // changed again")
	v, _ = ProjectMemory(root, "")
	if e = v.Entries[0]; !e.Included || !e.Pinned || !strings.Contains(e.Reason, "re-confirm") || !strings.Contains(v.Prompt, "check it still holds") {
		t.Fatalf("pinned note on changed code: %+v\n%s", e, v.Prompt)
	}
}

func TestPinnedNotesSurviveEviction(t *testing.T) {
	root := gitRepo(t)
	v, _ := ProjectMemory(root, "")
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, Text: "Use tabs.", Pin: pin(true)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < notesKeep+5; i++ {
		addRepoNote(root, fmt.Sprintf("task %d", i), "ok", nil)
	}
	v, _ = ProjectMemory(root, "")
	if len(v.Entries) != notesKeep || v.Entries[0].Text != "Use tabs." || !v.Entries[0].Included {
		t.Fatalf("%d entries, first %+v", len(v.Entries), v.Entries[0])
	}
	if !strings.Contains(v.Entries[len(v.Entries)-1].Text, fmt.Sprintf("task %d", notesKeep+4)) {
		t.Fatal("newest note evicted")
	}
}

func TestChangeProjectMemoryChecks(t *testing.T) {
	root := gitRepo(t)
	writeNoteFile(t, root, "src/a.go", "package a")
	outside := filepath.Join(t.TempDir(), "x.go")
	os.WriteFile(outside, []byte("x"), 0o644)
	v, _ := ProjectMemory(root, "")
	for _, c := range []MemoryChange{
		{Revision: v.Revision, Text: "note", Refs: []string{"missing.go"}},
		{Revision: v.Revision, Text: "note", Refs: []string{"../x.go"}},
		{Revision: v.Revision, Text: "note", Refs: []string{outside}},
		{Revision: v.Revision, Text: "note", Refs: []string{"src"}},
		{Revision: v.Revision, Text: "note\n  pinned: yes"},
		{Revision: v.Revision, Text: "note\n  refs: a.go"},
		{Revision: v.Revision, Pin: pin(true)},
	} {
		if err := ChangeProjectMemory(root, c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, Text: "note\r\n", Refs: []string{filepath.Join(root, "src", "a.go"), "src/a.go"}}); err != nil {
		t.Fatal(err)
	}
	v, _ = ProjectMemory(root, "")
	if s := v.Entries[0].Sources; len(s) != 1 || s[0].Path != "src/a.go" {
		t.Fatalf("%+v", s)
	}
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, ID: v.Entries[0].ID}); err == nil {
		t.Fatal("empty change accepted")
	}
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, ID: v.Entries[0].ID, Refs: []string{}}); err != nil {
		t.Fatal(err)
	}
	if v, _ = ProjectMemory(root, ""); len(v.Entries[0].Sources) != 0 || v.Entries[0].Text != "note" {
		t.Fatalf("%+v", v.Entries[0])
	}
	for i := 0; i < notesPinned; i++ {
		v, _ = ProjectMemory(root, "")
		if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, Text: fmt.Sprintf("rule %d", i), Pin: pin(true)}); err != nil {
			t.Fatal(err)
		}
	}
	v, _ = ProjectMemory(root, "")
	if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, ID: v.Entries[0].ID, Pin: pin(true)}); err == nil {
		t.Fatal("pinned more than the prompt takes")
	}
}

func TestNotesBudget(t *testing.T) {
	root := gitRepo(t)
	big := strings.Repeat("word ", noteMax/5-1)
	for i := 0; i < 3; i++ {
		v, _ := ProjectMemory(root, "")
		if err := ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, Text: fmt.Sprintf("%d %s", i, big)}); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := ProjectMemory(root, "")
	if !v.Entries[2].Included || !v.Entries[1].Included || v.Entries[0].Included || !strings.Contains(v.Entries[0].Reason, "budget") {
		t.Fatalf("%+v", v.Entries)
	}
	if len(v.Prompt) > notesBudget+300 {
		t.Fatalf("prompt %d bytes", len(v.Prompt))
	}
}
