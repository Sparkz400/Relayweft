package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
)

// fixRun scripts a one-step task whose first final review asks for changes.
type fixRun struct {
	mu       sync.Mutex
	reviews  int
	fixes    []runner.Spec
	resumeOK bool // a resumed fix agent succeeds
}

func (f *fixRun) set() runner.Set {
	return both(func(s runner.Spec) runner.Result {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(s.Prompt, runner.MarkerFinalReview):
			f.reviews++
			if f.reviews == 1 {
				return runner.Result{Final: `{"approve": false, "advice": "add the fallback", "issues": ["missing: fallback"]}`}
			}
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFix):
			f.fixes = append(f.fixes, s)
			if s.Resume != "" && !f.resumeOK {
				return runner.Result{Err: errSessionGone}
			}
			os.WriteFile(filepath.Join(s.Dir, "fix.txt"), []byte("fixed\n"), 0o644)
			return runner.Result{Final: "fixed", SessionID: "sess-fix"}
		}
		os.WriteFile(filepath.Join(s.Dir, "out.txt"), []byte("work\n"), 0o644)
		return runner.Result{Final: "done", SessionID: "sess-worker"}
	})
}

var errSessionGone = &sessionErr{}

type sessionErr struct{}

func (*sessionErr) Error() string { return "no conversation found with session id" }

// A fix round continues the worker's session, on its route, instead of a
// fresh agent that reads the repository again.
func TestFixRoundContinuesWriterSession(t *testing.T) {
	dir := gitRepo(t)
	f := &fixRun{resumeOK: true}
	o, rec := newOrc(t, dir, f.set(), shortcuts)
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK || len(f.fixes) != 1 {
		t.Fatalf("fixes=%d: %+v", len(f.fixes), res)
	}
	if got := f.fixes[0].Resume; got != "sess-worker" {
		t.Errorf("fix agent resumed %q, want the worker's session", got)
	}
	found := false
	for _, e := range rec.all() {
		if e.Kind == event.Route && e.Decision != nil && strings.HasPrefix(e.Decision.StepID, "fix-") {
			found = strings.Contains(e.Decision.Reason, "continues the writer's session")
		}
	}
	if !found {
		t.Error("the fix decision does not say it continues the writer's session")
	}
}

// When the session cannot be continued, a fresh fix agent does the fix,
// and the failed continuation does not count as a failed attempt.
func TestFixRoundFallsBackToFreshAgent(t *testing.T) {
	dir := gitRepo(t)
	f := &fixRun{resumeOK: false}
	o, rec := newOrc(t, dir, f.set(), func(c *config.Config) {
		shortcuts(c)
		c.Orchestrator.MaxAttempts = 1
	})
	res := o.Run(context.Background(), oneStepTask)
	if !res.OK || len(f.fixes) != 2 || f.fixes[0].Resume == "" || f.fixes[1].Resume != "" {
		t.Fatalf("fixes=%d (resume %q then %q): %+v", len(f.fixes), resumeOf(f.fixes, 0), resumeOf(f.fixes, 1), res)
	}
	if !logged(rec, "could not continue the") {
		t.Errorf("log:\n%s", strings.Join(logLines(rec), "\n"))
	}
}

func resumeOf(specs []runner.Spec, i int) string {
	if i < len(specs) {
		return specs[i].Resume
	}
	return "-"
}

// With change review on, the fix runs in a pool worktree, where the
// worker's main-tree session does not belong: it starts fresh.
func TestFixSessionOnlyInTheWritersTree(t *testing.T) {
	o, _ := newOrc(t, "", runner.NewFakeSet(0), nil)
	tk := &task{cfg: o.opts.Store.Get(), runners: runner.NewFakeSet(0), dir: "/repo"}
	if o.fixSession(tk, tk) != nil {
		t.Error("a task without a writer continued a session")
	}
	tk.noteWriter(StepRun{Provider: event.Codex, Kind: tk.cfg.Kind(event.Codex), Session: "s", Dir: "/elsewhere"})
	if o.fixSession(tk, tk) != nil {
		t.Error("continued a session from another folder")
	}
	tk.noteWriter(StepRun{Provider: event.Codex, Kind: tk.cfg.Kind(event.Codex), Session: "s", Dir: "/repo"})
	if w := o.fixSession(tk, tk); w == nil || w.Session != "s" || w.why == "" {
		t.Errorf("fix session = %+v", w)
	}
	tk.repos = []*task{{}}
	if o.fixSession(tk, tk) != nil {
		t.Error("a multi-repo task continued a session")
	}
}

func TestRequirementPrompts(t *testing.T) {
	edit := stepPrompt("task", Subtask{ID: "w", Title: "work", Kind: router.KindEdit, Prompt: "do it"}, nil, "", "", false)
	if !strings.Contains(edit, "list every requirement your subtask states") || !strings.Contains(edit, "REQUIREMENTS list") {
		t.Errorf("edit step prompt lacks the requirements check:\n%s", edit)
	}
	read := stepPrompt("task", Subtask{ID: "e", Title: "look", Kind: router.KindExplore, Prompt: "find it"}, nil, "", "", true)
	if strings.Contains(read, "REQUIREMENTS") {
		t.Errorf("read-only prompt asks for requirements:\n%s", read)
	}
	rev := finalReviewPrompt("task", Plan{Summary: "s"}, nil, "stat", "diff", nil, "", "", nil)
	for _, want := range []string{"List every requirement the TASK states", `"missing: <requirement>"`, "Open other files only to settle a specific doubt"} {
		if !strings.Contains(rev, want) {
			t.Errorf("final review prompt lacks %q", want)
		}
	}
	if fix := fixPrompt("task", Verdict{Advice: "a"}, true); !strings.Contains(fix, "every requirement of the TASK") {
		t.Errorf("fix prompt:\n%s", fix)
	}
}

// The final review gets the conventions shortened before they are fenced,
// so the untrusted block always closes.
func TestReviewDocsClippedInsideFence(t *testing.T) {
	tk := &task{repoDocs: strings.Repeat("x", 5000)}
	got := tk.docsContextMax(reviewDocsMax)
	if !strings.Contains(got, "(conventions shortened)") || !strings.HasSuffix(strings.TrimSpace(got), ">>>") {
		t.Errorf("clipped docs:\n%s", got[len(got)-200:])
	}
	if strings.Count(got, "x") > reviewDocsMax+10 {
		t.Errorf("docs not clipped: %d bytes", len(got))
	}
	if full := tk.docsContext(); strings.Contains(full, "shortened") {
		t.Error("the planner's docs were clipped")
	}
}
