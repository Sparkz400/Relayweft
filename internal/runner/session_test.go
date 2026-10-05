package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/canon"
	"github.com/sparkz400/switchyard/internal/event"
)

// A Codex follow-up or interrupted step resumed in a pool worktree:
// `codex exec resume` takes no -C, so the folder is the process's working
// directory, and the sandbox comes through config. Both must be the
// worktree's: workspace-write lets Codex write only below its folder.
// This pins down runner behaviour the Codex follow-up fix relies on; it
// passed before that fix too (the regression test is the orchestrator's
// TestFollowUpResumesInPoolWorktree).
func TestCodexResumeRunsInItsFolder(t *testing.T) {
	pc, det := providerCfg(t, event.Codex)
	dump := fakeExe(t, &pc, "codex_real_resume.jsonl", 0, "")
	slot := filepath.Join(t.TempDir(), "pool", "0")
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "01a1017b-c8e1-7400-a637-8305ef91e912"
	res := NewCodex(pc, det).Run(context.Background(), Spec{AgentID: "a", Dir: slot, Resume: id, Prompt: "go on"}, func(event.Event) {})
	if !res.OK() || res.SessionID != id {
		t.Fatalf("result %+v", res)
	}
	d := readDump(t, dump)
	if canon.Path(d.Cwd) != canon.Path(slot) {
		t.Errorf("codex exec resume ran in %s, want the worktree %s", d.Cwd, slot)
	}
	args := strings.Join(d.Args, " ")
	if !strings.HasPrefix(args, "exec resume ") || !strings.Contains(args, "-c sandbox_mode=workspace-write") ||
		strings.Contains(args, "-C ") || !strings.HasSuffix(args, id+" -") {
		t.Errorf("args %s", args)
	}
}

// Recorded from a real Claude Code 2.1.288 on Windows (4 Oct 2026), haiku
// at low effort, in a git worktree: the agent was asked to write a.txt,
// b.txt and c.txt one at a time and was killed (taskkill /F /T) right after
// it wrote a.txt, as when sy dies mid-step. claude_real_midstep_resume is
// `claude -p ... --resume <that session>` started in the same worktree
// with sy's interrupted-step prompt: it read a.txt instead of writing it
// again, then wrote b.txt and c.txt. (A resume started from the main
// worktree of the same repo also found the session in this version, but
// with the main tree as its folder while its history names the worktree's
// absolute paths: sy resumes in the folder the agent ran in.) The init
// lines are trimmed of the recording machine's tools and paths.
func TestClaudeRealKilledAndResumed(t *testing.T) {
	const session = "9c70a986-7bea-482d-8acd-5e50dff8f86b"
	p := &claudeParser{}
	feed(t, p, "claude_real_killed.jsonl")
	if p.sessionID() != session {
		t.Errorf("killed run: session %q", p.sessionID())
	}
	var r Result
	p.Finish(&r)
	if r.Final != "" || len(r.Files) != 1 || !strings.HasSuffix(strings.ReplaceAll(r.Files[0], `\`, "/"), "/slot/a.txt") {
		t.Errorf("killed run: final %q files %q", r.Final, r.Files)
	}

	p = &claudeParser{}
	evs := feed(t, p, "claude_real_midstep_resume.jsonl")
	r = Result{}
	p.Finish(&r)
	if r.Err != nil || r.Final != "done" || r.SessionID != session {
		t.Errorf("resumed run: %+v", r)
	}
	var files []string
	for _, f := range r.Files {
		files = append(files, f[strings.LastIndexAny(f, `\/`)+1:])
	}
	if strings.Join(files, ",") != "b.txt,c.txt" {
		t.Errorf("resumed run wrote %q, want b.txt and c.txt (a.txt was already there)", r.Files)
	}
	if k := kinds(evs); k[event.ToolCall] != 1 || k[event.FileEdit] != 2 {
		t.Errorf("resumed run: event kinds %v (want the Read of a.txt and two writes)", k)
	}
}

// Exec reports the session id through Spec.OnSession as soon as the CLI
// prints it, while the agent still works: sy saves it in the task state,
// so a resume after sy died mid-step can continue the session.
func TestExecReportsSessionEarly(t *testing.T) {
	pc, det := providerCfg(t, event.Claude)
	fakeExe(t, &pc, "claude_real_killed.jsonl", 0, "")
	pc.Env["SY_FAKE_SLEEP"] = "60000" // still working after its first lines
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []string
	reported := make(chan struct{})
	spec := Spec{AgentID: "s", Dir: t.TempDir(), OnSession: func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
		if len(got) == 1 {
			close(reported)
		}
	}}
	done := make(chan Result, 1)
	go func() { done <- NewClaude(pc, det).Run(ctx, spec, func(event.Event) {}) }()
	select {
	case <-reported:
	case r := <-done:
		t.Fatalf("the run ended before the session was reported: %+v", r)
	case <-time.After(30 * time.Second):
		t.Fatal("no session reported while the agent works")
	}
	select {
	case r := <-done:
		t.Fatalf("the agent should still be running: %+v", r)
	default:
	}
	cancel()
	r := <-done
	mu.Lock()
	defer mu.Unlock()
	if !r.Killed || r.SessionID != "9c70a986-7bea-482d-8acd-5e50dff8f86b" {
		t.Errorf("result %+v", r)
	}
	// Every line carries the id; it is reported once.
	if len(got) != 1 || got[0] != r.SessionID {
		t.Errorf("OnSession calls %q", got)
	}
}

// The other parsers report their session before the end too: Codex's
// thread.started, Gemini's init, and a generic CLI's session rule (only
// when it can be resumed).
func TestParsersReportSessionEarly(t *testing.T) {
	cp := &codexParser{}
	cp.Line([]byte(`{"type":"thread.started","thread_id":"T-1"}`))
	if cp.sessionID() != "T-1" {
		t.Errorf("codex: %q", cp.sessionID())
	}
	gp := &geminiParser{}
	gp.Line([]byte(`{"type":"init","session_id":"G-1","model":"gemini-x"}`))
	if gp.sessionID() != "G-1" {
		t.Errorf("gemini: %q", gp.sessionID())
	}
	q := genericCfg(t, "generic_qwen.yaml").Generic
	jp := newGenericParser(q).(*jsonRuleParser)
	jp.Line([]byte(`{"type":"system","subtype":"init","session_id":"Q-1","model":"qwen"}`))
	if jp.sessionID() != "Q-1" {
		t.Errorf("generic: %q", jp.sessionID())
	}
	q.ResumeArgs = nil
	if jp.sessionID() != "" {
		t.Error("a generic CLI without resume_args reported a session")
	}
}

// A session id that is not plain (it came from a saved file) never reaches
// the CLI's command line, whatever the CLI.
func TestExecRefusesUnsafeSessionID(t *testing.T) {
	for _, kind := range []string{event.Claude, event.Codex, event.Gemini, event.Qwen} {
		pc, det := providerCfg(t, kind)
		dump := fakeExe(t, &pc, "", 0, "")
		x := map[string]func() *Exec{
			event.Claude: func() *Exec { return NewClaude(pc, det) },
			event.Codex:  func() *Exec { return NewCodex(pc, det) },
			event.Gemini: func() *Exec { return NewGemini(pc, det) },
			event.Qwen:   func() *Exec { return NewQwen(pc, det) },
		}[kind]()
		for _, id := range []string{"-x", "--yolo", "a b", "a;b"} {
			res := x.Run(context.Background(), Spec{AgentID: "a", Dir: t.TempDir(), Resume: id}, func(event.Event) {})
			if res.Err == nil || !strings.Contains(res.Err.Error(), "not safe") {
				t.Errorf("%s: session %q: %v", kind, id, res.Err)
			}
		}
		if _, err := os.Stat(dump); err == nil {
			t.Errorf("%s: the CLI was started", kind)
		}
	}
	if !ValidSessionID("9c70a986-7bea-482d-8acd-5e50dff8f86b") || ValidSessionID("-9c70") {
		t.Error("ValidSessionID")
	}
}
