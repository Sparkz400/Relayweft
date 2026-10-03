package orchestrator

import (
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
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
)

// fakeApprover answers with functions.
type fakeApprover struct {
	plan   func(Plan) (Plan, bool)
	review func(ChangeSet) ChangeDecision
	mu     sync.Mutex
	seen   []ChangeSet
}

func (f *fakeApprover) ApprovePlan(ctx context.Context, task string, p Plan) (Plan, bool) {
	if f.plan == nil {
		return p, true
	}
	return f.plan(p)
}

func (f *fakeApprover) ReviewChanges(ctx context.Context, cs ChangeSet) ChangeDecision {
	f.mu.Lock()
	f.seen = append(f.seen, cs)
	f.mu.Unlock()
	if f.review == nil {
		return ChangeDecision{Apply: cs.AllPaths()}
	}
	return f.review(cs)
}

func withApprover(o *Orchestrator, a Approver) *Orchestrator {
	o.opts.Approver = a
	return o
}

// twoEdits plans two independent edit steps a and b.
func twoEdits(s runner.Spec) (runner.Result, bool) {
	switch {
	case strings.Contains(s.Prompt, runner.MarkerPlan):
		return runner.Result{Final: planJSON(
			map[string]any{"id": "a", "title": "write a", "kind": "edit", "prompt": "write a", "files": []string{"a.txt"}},
			map[string]any{"id": "b", "title": "write b", "kind": "edit", "prompt": "write b", "files": []string{"b.txt"}},
		)}, true
	case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
		return approve(), true
	}
	return runner.Result{}, false
}

func TestPlanApprovalEditsPlan(t *testing.T) {
	var mu sync.Mutex
	ran := map[string]string{}
	set := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		mu.Lock()
		ran[s.StepID] = s.Role
		mu.Unlock()
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, "", set, nil)
	withApprover(o, &fakeApprover{plan: func(p Plan) (Plan, bool) {
		p.Subtasks = p.Subtasks[1:] // drop a
		p.Subtasks[0].Role = event.RoleWorkerHigh
		return p, true
	}})
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if _, ok := ran["a"]; ok || ran["b"] != event.RoleWorkerHigh {
		t.Fatalf("ran %v, want only b as worker_high", ran)
	}
	forced := false
	for _, e := range rec.all() {
		if e.Kind == event.Route && e.Decision.StepID == "b" && e.Decision.Rule == router.RuleForced {
			forced = true
		}
	}
	if !forced {
		t.Error("b was not routed by the role chosen in approval")
	}

	// Declining runs nothing.
	ran = map[string]string{}
	o2, _ := newOrc(t, "", set, nil)
	withApprover(o2, &fakeApprover{plan: func(p Plan) (Plan, bool) { return p, false }})
	if res := o2.Run(context.Background(), longTask); res.OK || len(ran) != 0 || !strings.Contains(res.Summary, "not approved") {
		t.Fatalf("declined plan: %+v ran %v", res, ran)
	}

	// Unattended tasks never ask.
	asked := false
	o3, _ := newOrc(t, "", set, nil)
	withApprover(o3, &fakeApprover{plan: func(p Plan) (Plan, bool) { asked = true; return p, false }})
	if res := o3.RunWith(context.Background(), longTask, TaskOptions{Unattended: true}); !res.OK || asked {
		t.Fatalf("unattended: %+v asked=%v", res, asked)
	}
}

func TestChangeReviewFeedbackAndPartialApply(t *testing.T) {
	dir := gitRepo(t)
	var mu sync.Mutex
	prompts := map[string][]string{}
	set := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		mu.Lock()
		prompts[s.StepID] = append(prompts[s.StepID], s.Prompt)
		mu.Unlock()
		switch s.StepID {
		case "a":
			os.WriteFile(filepath.Join(s.Dir, "a.txt"), []byte("a\n"), 0o644)
			os.WriteFile(filepath.Join(s.Dir, "junk.txt"), []byte("junk\n"), 0o644)
			if strings.Contains(s.Prompt, "also write a2") {
				os.WriteFile(filepath.Join(s.Dir, "a2.txt"), []byte("a2\n"), 0o644)
			}
		case "b":
			os.WriteFile(filepath.Join(s.Dir, "b.txt"), []byte("b\n"), 0o644)
		}
		return runner.Result{Final: "wrote " + s.StepID}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.ReviewChanges = true })
	ap := &fakeApprover{review: func(cs ChangeSet) ChangeDecision {
		switch {
		case cs.StepID == "a" && cs.Round == 1:
			return ChangeDecision{Feedback: "also write a2"}
		case cs.StepID == "a":
			return ChangeDecision{Apply: []string{"a.txt", "a2.txt"}} // not junk.txt
		}
		return ChangeDecision{} // reject b
	}}
	withApprover(o, ap)
	res := o.Run(context.Background(), longTask)
	if res.OK {
		t.Fatalf("task with a rejected step reported OK: %+v", res)
	}
	if read(t, filepath.Join(dir, "a.txt")) != "a\n" || read(t, filepath.Join(dir, "a2.txt")) != "a2\n" {
		t.Error("accepted files were not applied")
	}
	for _, f := range []string{"junk.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was applied although it was not accepted", f)
		}
	}
	if len(prompts["a"]) != 2 || !strings.Contains(prompts["a"][1], "also write a2") {
		t.Errorf("feedback did not reach the agent: %d prompts", len(prompts["a"]))
	}
	var a1 *ChangeSet
	for i, cs := range ap.seen {
		if cs.StepID == "a" && cs.Round == 1 {
			a1 = &ap.seen[i]
		}
	}
	if a1 == nil || len(a1.Files) != 2 || !strings.Contains(a1.Files[0].Patch+a1.Files[1].Patch, "+a") {
		t.Fatalf("first change set of a: %+v", a1)
	}
	branches, _ := git{dir}.out("branch", "--list", "sy/*")
	if !strings.HasSuffix(strings.TrimSpace(branches), "/b") || !strings.Contains(branches, "a-full") {
		t.Errorf("rejected / full changes not kept on branches:\n%s", branches)
	}
}

// fileCheck is a verify command that passes when name exists.
func fileCheck(name string) string {
	if runtime.GOOS == "windows" {
		return "if exist " + name + " (exit 0) else (echo missing " + name + " & exit 1)"
	}
	return "test -f " + name + " || { echo missing " + name + "; exit 1; }"
}

func TestVerifyFailureGoesToFixRound(t *testing.T) {
	dir := gitRepo(t)
	var fixPrompt string
	set := both(func(s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(map[string]any{"id": "w", "title": "work", "kind": "edit", "prompt": "work"})}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			if strings.Contains(s.Prompt, runner.MarkerFinalReview) && !strings.Contains(s.Prompt, "THE REPO'S CHECKS") {
				return runner.Result{Final: `{"approve": false, "advice": "no check report"}`}
			}
			return approve()
		case strings.Contains(s.Prompt, runner.MarkerFix):
			fixPrompt = s.Prompt
			os.WriteFile(filepath.Join(s.Dir, "fixed.txt"), []byte("ok\n"), 0o644)
		}
		return runner.Result{Final: "done"}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Verify.Commands = []string{fileCheck("fixed.txt")} })
	res := o.Run(context.Background(), longTask)
	if !res.OK || !strings.Contains(res.Summary, "checks pass") {
		t.Fatalf("task: %+v", res)
	}
	if !strings.Contains(fixPrompt, "missing fixed.txt") {
		t.Fatalf("fix round did not get the failing output:\n%s", fixPrompt)
	}
}

func TestResumeSkipsFinishedSteps(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	planned := false
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerPlan) {
			planned = true
		}
		if r, ok := twoEdits(s); ok {
			return r
		}
		mu.Lock()
		ran = append(ran, s.StepID)
		mu.Unlock()
		return runner.Result{Final: "done " + s.StepID}
	})
	o, _ := newOrc(t, "", set, nil)
	st := &TaskState{ID: "resume-test", Task: longTask, Status: "running", Created: time.Now(),
		Plan: &Plan{Summary: "p", Subtasks: []Subtask{
			{ID: "a", Title: "a", Kind: router.KindEdit, Prompt: "a"},
			{ID: "b", Title: "b", Kind: router.KindEdit, Prompt: "b", DependsOn: []string{"a"}},
		}},
		Results: map[string]StepState{"a": {OK: true, Final: "a was done"}}}
	st.save()
	if !st.Interrupted() {
		t.Fatal("a running state without its lock is not interrupted")
	}
	if got := LastInterrupted(""); got == nil || got.ID != "resume-test" {
		t.Fatalf("LastInterrupted = %+v", got)
	}
	res := o.RunWith(context.Background(), "", TaskOptions{Resume: st})
	if !res.OK || planned || len(ran) != 1 || ran[0] != "b" {
		t.Fatalf("resume: %+v planned=%v ran=%v", res, planned, ran)
	}
	saved, err := LoadTask("resume-test")
	if err != nil || saved.Status != "done" || !saved.Results["b"].OK {
		t.Fatalf("saved state %+v %v", saved, err)
	}
}

func TestFollowUpResumesSession(t *testing.T) {
	var mu sync.Mutex
	var specs []runner.Spec
	failResume := false
	set := both(func(s runner.Spec) runner.Result {
		mu.Lock()
		specs = append(specs, s)
		mu.Unlock()
		if s.Resume != "" && failResume {
			return runner.Result{Err: errString("no conversation found")}
		}
		return runner.Result{Final: "answer to " + s.StepID, SessionID: "sess-" + s.StepID}
	})
	o, rec := newOrc(t, "", set, nil)
	if res := o.Run(context.Background(), "add a test"); !res.OK {
		t.Fatalf("task: %+v", res)
	}
	sess, ok := o.Session("last")
	if !ok || sess.SessionID != "sess-work" {
		t.Fatalf("session not remembered: %+v", o.Sessions())
	}
	res := o.FollowUp(context.Background(), sess.AgentID, "also cover the error path")
	last := specs[len(specs)-1]
	if !res.OK || last.Resume != "sess-work" || last.Prompt != "also cover the error path" {
		t.Fatalf("follow-up: %+v spec %+v", res, last)
	}
	// A session the CLI cannot resume falls back to a fresh agent with context.
	failResume = true
	n := len(specs)
	res = o.FollowUp(context.Background(), "", "one more thing")
	if !res.OK || len(specs) != n+2 {
		t.Fatalf("fallback: %+v, %d runs", res, len(specs)-n)
	}
	fresh := specs[len(specs)-1]
	if fresh.Resume != "" || !strings.Contains(fresh.Prompt, "THE USER'S FOLLOW-UP") || !strings.Contains(fresh.Prompt, "add a test") {
		t.Fatalf("fresh prompt:\n%s", fresh.Prompt)
	}
	if res := o.FollowUp(context.Background(), "nobody", "x"); res.OK || !strings.Contains(res.Summary, "no finished agent") {
		t.Fatalf("unknown agent: %+v", res)
	}
	// A refused follow-up still ends with TaskDone, or a UI would wait forever.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := 0
		for _, e := range rec.all() {
			if e.Kind == event.TaskDone && strings.Contains(e.Text, "no finished agent") {
				n++
			}
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refused follow-up emitted %d TaskDone", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Feedback after the last feedback round rejects instead of applying.
func TestChangeReviewFeedbackNeverApplies(t *testing.T) {
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte("x\n"), 0o644)
		return runner.Result{Final: "wrote"}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.ReviewChanges = true })
	withApprover(o, &fakeApprover{review: func(cs ChangeSet) ChangeDecision {
		return ChangeDecision{Apply: cs.AllPaths(), Feedback: "still wrong"}
	}})
	o.Run(context.Background(), longTask)
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s applied although every review asked for changes", f)
		}
	}
}

// A resume works from the state on disk: a task another sy finished since
// is not run again.
func TestResumeRefusesFinishedTask(t *testing.T) {
	ran := false
	set := both(func(s runner.Spec) runner.Result { ran = true; return runner.Result{Final: "x"} })
	o, _ := newOrc(t, "", set, nil)
	st := &TaskState{ID: "stale-test", Task: longTask, Status: "running", Created: time.Now(),
		Plan: &Plan{Summary: "p", Subtasks: []Subtask{{ID: "a", Title: "a", Kind: router.KindEdit, Prompt: "a"}}}}
	st.save()
	stale := *st
	st.Status = "done" // another sy finished it
	st.save()
	res := o.RunWith(context.Background(), "", TaskOptions{Resume: &stale})
	if res.OK || ran || !strings.Contains(res.Summary, "is done now") {
		t.Fatalf("stale resume ran: %+v ran=%v", res, ran)
	}
	// While another sy holds the lock, it is refused too.
	st.Status = "running"
	st.save()
	unlock, ok := st.lock()
	if !ok {
		t.Fatal("lock")
	}
	res = o.RunWith(context.Background(), "", TaskOptions{Resume: st})
	unlock()
	if res.OK || ran || !strings.Contains(res.Summary, "another sy") {
		t.Fatalf("locked resume ran: %+v", res)
	}
}

func TestChangeSetLiteralPaths(t *testing.T) {
	dir := gitRepo(t)
	g := git{dir}
	base := headOf(t, dir)
	os.WriteFile(filepath.Join(dir, "[id].tsx"), []byte("page\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "i.tsx"), []byte("other\n"), 0o644)
	g.out("add", "-A")
	g.out("commit", "-qm", "x")
	files, err := g.changeSet(base, headOf(t, dir))
	if err != nil || len(files) != 2 {
		t.Fatalf("files %+v %v", files, err)
	}
	for _, f := range files {
		if strings.Count(f.Patch, "diff --git") != 1 {
			t.Errorf("%s patch has %d files:\n%s", f.Path, strings.Count(f.Patch, "diff --git"), f.Patch)
		}
	}
	pc, err := g.partialCommit(base, headOf(t, dir), []string{"[id].tsx"}, "only one")
	if err != nil {
		t.Fatal(err)
	}
	if names, _ := g.out("diff", "--name-only", base, pc); names != "[id].tsx" {
		t.Fatalf("partial commit has %q", names)
	}
}
