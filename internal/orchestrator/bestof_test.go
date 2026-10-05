package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/router"
	"github.com/sparkz400/switchyard/internal/runner"
	"github.com/sparkz400/switchyard/internal/sandbox"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// bestOfTask is short: one worker step, no planner.
const bestOfTask = "fix the greeting"

// bestOfOn turns best of N on for every writing step.
func bestOfOn(c *config.Config) { c.Routing.BestOf.When = config.BestOfAlways }

// bestOfRecs returns the task's best_of records.
func bestOfRecs(t *testing.T, o *Orchestrator) []sessionlog.Record {
	t.Helper()
	recs, err := sessionlog.ReadDir(filepath.Dir(o.opts.Log.Path()))
	if err != nil {
		t.Fatal(err)
	}
	var out []sessionlog.Record
	for _, r := range recs {
		if r.Type == sessionlog.TypeBestOf {
			out = append(out, r)
		}
	}
	return out
}

// spy records what each agent was asked, by agent id.
type spy struct {
	mu    sync.Mutex
	specs map[string]runner.Spec
	picks []string // the reviewer's best-of prompts
}

func (s *spy) note(sp runner.Spec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.specs == nil {
		s.specs = map[string]runner.Spec{}
	}
	if strings.Contains(sp.Prompt, runner.MarkerBestOf) {
		s.picks = append(s.picks, sp.Prompt)
		return
	}
	s.specs[sp.AgentID] = sp
}

// With writing agents in a sandbox, each candidate's checks run in it too
// (they run the candidate's code): without docker here, no candidate's
// check passes, although on this machine claude's would.
func TestBestOfCandidateChecksInSandbox(t *testing.T) {
	old := sandbox.LookPath
	sandbox.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	defer func() { sandbox.LookPath = old }()
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte("hello from "+s.Provider+"\n"), 0o644)
		if s.Provider == event.Claude {
			os.WriteFile(filepath.Join(s.Dir, "ok.txt"), []byte("ok\n"), 0o644)
		}
		return runner.Result{Final: "done", Files: []string{"greet.txt"}}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) {
		bestOfOn(c)
		c.Verify.Commands = []string{fileCheck("ok.txt")}
		c.Sandbox = config.SandboxCfg{Mode: config.SandboxDocker}
	})
	o.Run(context.Background(), bestOfTask)
	recs := bestOfRecs(t, o)
	if len(recs) != 2 {
		t.Fatalf("best_of records = %d, want 2", len(recs))
	}
	for _, r := range recs {
		if r.Passed != nil && *r.Passed {
			t.Errorf("a candidate's check ran outside the sandbox: %+v", r)
		}
	}
}

// Checks decide: the candidate whose checks pass wins and lands; the other
// one's work is kept on a branch; both are logged with how it was decided.
func TestBestOfChecksPickTheWinner(t *testing.T) {
	dir := gitRepo(t)
	head := headOf(t, dir)
	var sp spy
	set := both(func(s runner.Spec) runner.Result {
		sp.note(s)
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte("hello from "+s.Provider+"\n"), 0o644)
		if s.Provider == event.Claude {
			os.WriteFile(filepath.Join(s.Dir, "ok.txt"), []byte("ok\n"), 0o644)
		}
		return runner.Result{Final: "done", Files: []string{"greet.txt"}}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		bestOfOn(c)
		c.Verify.Commands = []string{fileCheck("ok.txt")}
	})
	res := o.Run(context.Background(), bestOfTask)
	if !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if got := read(t, filepath.Join(dir, "greet.txt")); got != "hello from claude\n" {
		t.Errorf("greet.txt = %q, want the winner's", got)
	}
	if headOf(t, dir) != head {
		t.Error("HEAD moved")
	}
	cx, cl := sp.specs["work--codex"], sp.specs["work--claude"]
	if cx.Dir == "" || cl.Dir == "" {
		t.Fatalf("both candidates must run: %v", sp.specs)
	}
	if cx.Dir == cl.Dir || !strings.HasPrefix(cx.Dir, poolDir(dir)) || !strings.HasPrefix(cl.Dir, poolDir(dir)) {
		t.Errorf("candidates must work in separate pool worktrees: %s, %s", cx.Dir, cl.Dir)
	}
	if cx.Prompt != cl.Prompt {
		t.Error("the candidates got different prompts")
	}
	if len(sp.picks) != 0 {
		t.Error("the reviewer was asked although the checks decided")
	}
	// The loser's work is on a branch.
	branches := tgit(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/sy/")
	var kept string
	for _, b := range strings.Fields(branches) {
		if strings.HasSuffix(b, "/work--codex") {
			kept = b
		}
	}
	if kept == "" {
		t.Fatalf("the loser's work is not on a branch: %q", branches)
	}
	if strings.Contains(branches, "/work--claude") {
		t.Errorf("the winner's branch was not deleted after it landed: %q", branches)
	}
	if got := tgit(t, dir, "show", kept+":greet.txt"); strings.TrimSpace(got) != "hello from codex" {
		t.Errorf("kept branch has %q", got)
	}
	recs := bestOfRecs(t, o)
	if len(recs) != 2 {
		t.Fatalf("best_of records = %d, want 2", len(recs))
	}
	for _, r := range recs {
		won := r.OK != nil && *r.OK
		if won != (r.Provider == event.Claude) || r.Reason != sessionlog.BestOfByChecks || r.Passed == nil || *r.Passed != won {
			t.Errorf("record %+v", r)
		}
	}
	st, err := LoadTask(History(dir, 1)[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Results["work"].BestOf; !strings.Contains(b, "kept work--claude") || !strings.Contains(b, kept) {
		t.Errorf("step result best_of = %q", b)
	}
	if st.Author() != event.Claude {
		t.Errorf("author = %q: only the winner counts", st.Author())
	}
	// Both candidates are visible in the agent tree, and the step ends ok.
	seen := map[string]bool{}
	stepDone := false
	for _, e := range rec.all() {
		if e.Kind == event.AgentQueued {
			seen[e.AgentID] = true
		}
		if e.Kind == event.Done && e.AgentID == "work" && e.OK {
			stepDone = true
		}
	}
	if !seen["work--codex"] || !seen["work--claude"] || !stepDone {
		t.Errorf("agent tree: queued %v, step done %v", seen, stepDone)
	}
	// Slots are free again, and no candidate holds one for a resume.
	for _, d := range []string{cx.Dir, cl.Dir} {
		slot := slotOf(dir, d)
		unlock, ok := proc.TryLock(slot + ".lock")
		if !ok {
			t.Errorf("slot %s is still locked", slot)
			continue
		}
		unlock()
		if held, _ := holdState(slot); held {
			t.Errorf("slot %s is held", slot)
		}
	}
	// sy undo reverts the whole best-of task.
	if _, err := Undo(dir, res.UndoKey, false, false); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "greet.txt")); !os.IsNotExist(err) {
		t.Error("undo did not remove the winner's file")
	}
}

// When the checks do not decide, the reviewer picks from both diffs, which
// it sees as untrusted data under neutral names; change review then sees
// only the winner.
func TestBestOfReviewerPicksAndReviewSeesWinner(t *testing.T) {
	dir := gitRepo(t)
	var sp spy
	words := map[string]string{event.Codex: "alpha", event.Claude: "beta"}
	set := both(func(s runner.Spec) runner.Result {
		sp.note(s)
		if strings.Contains(s.Prompt, runner.MarkerBestOf) {
			// Pick the candidate whose diff says beta.
			i := strings.Index(s.Prompt, "+beta")
			a := strings.LastIndex(s.Prompt[:i], "### Candidate ")
			return runner.Result{Final: "```json\n{\"pick\": \"" + s.Prompt[a+14:a+15] + "\", \"why\": \"cleaner\"}\n```"}
		}
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte(words[s.Provider]+"\n"), 0o644)
		return runner.Result{Final: "changed the greeting"}
	})
	o, rec := newOrc(t, dir, set, bestOfOn)
	ap := &fakeApprover{}
	withApprover(o, ap)
	o.opts.Store.Update(func(c *config.Config) error {
		c.Orchestrator.ReviewChanges = true
		c.Orchestrator.ApprovePlan = false
		return nil
	})
	res := o.Run(context.Background(), bestOfTask)
	if !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if got := read(t, filepath.Join(dir, "greet.txt")); got != "beta\n" {
		t.Errorf("greet.txt = %q, want the reviewer's pick", got)
	}
	if len(sp.picks) != 1 {
		t.Fatalf("reviewer pick prompts = %d", len(sp.picks))
	}
	p := sp.picks[0]
	for _, want := range []string{"UNTRUSTED", "+alpha", "+beta", "### Candidate A", "### Candidate B"} {
		if !strings.Contains(p, want) {
			t.Errorf("pick prompt lacks %q", want)
		}
	}
	for _, bad := range []string{"work--", "codex:", "claude:"} {
		if strings.Contains(p, bad) {
			t.Errorf("pick prompt names the candidates' routes (%q)", bad)
		}
	}
	ap.mu.Lock()
	seen := ap.seen
	ap.mu.Unlock()
	if len(seen) != 1 || seen[0].StepID != "work" || len(seen[0].Files) != 1 {
		t.Fatalf("change review saw %+v, want the winner only", seen)
	}
	recs := bestOfRecs(t, o)
	if len(recs) != 2 || recs[0].Reason != sessionlog.BestOfByReviewer {
		t.Errorf("records %+v", recs)
	}
	checkpoint := false
	for _, e := range rec.all() {
		if e.Kind == event.Checkpoint && strings.HasPrefix(e.Text, "best-of work: picked work--claude") {
			checkpoint = true
		}
	}
	if !checkpoint {
		t.Error("the reviewer's pick is not shown")
	}
}

// An unusable reviewer answer falls back to the fixed order: here the
// smaller diff.
func TestBestOfFixedOrderWithoutReviewer(t *testing.T) {
	dir := gitRepo(t)
	set := both(func(s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerBestOf) {
			return runner.Result{Final: "I like both."}
		}
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		body := "short\n"
		if s.Provider == event.Codex {
			body = "long\nlong\nlong\n"
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte(body), 0o644)
		return runner.Result{Final: "done"}
	})
	o, _ := newOrc(t, dir, set, bestOfOn)
	if res := o.Run(context.Background(), bestOfTask); !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if got := read(t, filepath.Join(dir, "greet.txt")); got != "short\n" {
		t.Errorf("greet.txt = %q, want the smaller change", got)
	}
	if recs := bestOfRecs(t, o); len(recs) != 2 || recs[0].Reason != sessionlog.BestOfByOrder {
		t.Errorf("records %+v", recs)
	}
}

// A provider at its limit is no candidate: with one route left the step
// runs once, as usual, and says why.
func TestBestOfSkipsLimitedProvider(t *testing.T) {
	dir := gitRepo(t)
	var sp spy
	set := both(func(s runner.Spec) runner.Result {
		sp.note(s)
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte("hi\n"), 0o644)
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, dir, set, bestOfOn)
	o.Tracker().MarkLimited(event.Codex, time.Now().Add(time.Hour))
	if res := o.Run(context.Background(), bestOfTask); !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if _, ok := sp.specs["work"]; !ok {
		t.Errorf("want one agent with the step's id, got %v", sp.specs)
	}
	for id := range sp.specs {
		if strings.Contains(id, "--") {
			t.Errorf("candidate %s ran", id)
		}
	}
	var logged bool
	for _, e := range rec.all() {
		if e.Kind == event.Log && strings.Contains(e.Text, "best of N leaves out codex") {
			logged = true
		}
	}
	if !logged {
		t.Error("no log line says why best of N did not run")
	}
}

// Turned on for one step in plan approval (the config is off), only that
// step runs as best of N; a planner cannot turn it on itself.
func TestBestOfPlanToggle(t *testing.T) {
	dir := gitRepo(t)
	var sp spy
	set := both(func(s runner.Spec) runner.Result {
		sp.note(s)
		if strings.Contains(s.Prompt, runner.MarkerPlan) && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return runner.Result{Final: planJSON(
				map[string]any{"id": "a", "title": "write a", "kind": "edit", "prompt": "write a", "files": []string{"a.txt"}, "best_of": "on"},
				map[string]any{"id": "b", "title": "write b", "kind": "edit", "prompt": "write b", "files": []string{"b.txt"}, "best_of": "on"},
			)}
		}
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, s.StepID+".txt"), []byte(s.StepID+"\n"), 0o644)
		return runner.Result{Final: "done"}
	})
	o, _ := newOrc(t, dir, set, func(c *config.Config) { c.Orchestrator.Parallel = false })
	var planned Plan
	withApprover(o, &fakeApprover{plan: func(p Plan) (Plan, bool) {
		planned = p
		p.Subtasks[1].BestOf = BestOfOn
		return p, true
	}})
	o.opts.Store.Update(func(c *config.Config) error { c.Orchestrator.ApprovePlan = true; return nil })
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if planned.Subtasks[0].BestOf != "" {
		t.Error("the planner's best_of was kept")
	}
	if _, ok := sp.specs["a"]; !ok {
		t.Errorf("a should run once: %v", sp.specs)
	}
	if _, ok := sp.specs["b--codex"]; !ok {
		t.Errorf("b should run as best of N: %v", sp.specs)
	}
	if read(t, filepath.Join(dir, "a.txt")) != "a\n" || read(t, filepath.Join(dir, "b.txt")) != "b\n" {
		t.Error("the steps' work did not land")
	}
}

// A cancel while the candidates work changes nothing in the tree, records
// no running agent for them (the step runs again as a whole on resume) and
// leaves no slot locked or held.
func TestBestOfCancel(t *testing.T) {
	dir := gitRepo(t)
	started := make(chan string, 2)
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte("half\n"), 0o644)
		started <- s.Dir
		<-ctx.Done()
		return runner.Result{Err: errString("killed"), Killed: true, SessionID: "s-" + s.Provider}
	})
	o, _ := newOrc(t, dir, set, bestOfOn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() { done <- o.Run(ctx, bestOfTask) }()
	var dirs []string
	for len(dirs) < 2 {
		select {
		case d := <-started:
			dirs = append(dirs, d)
		case <-time.After(60 * time.Second):
			t.Fatal("the candidates did not both start at once")
		}
	}
	cancel()
	res := <-done
	if res.OK {
		t.Fatalf("a cancelled task is ok: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "greet.txt")); !os.IsNotExist(err) {
		t.Error("a cancelled best-of step changed the tree")
	}
	// A follow-up must not resume a candidate whose work is not in the tree.
	for _, id := range []string{"work--codex", "work--claude"} {
		if _, ok := o.Session(id); ok {
			t.Errorf("%s can still take a follow-up", id)
		}
	}
	st := History(dir, 1)[0]
	if st.Status != "cancelled" || len(st.Running) != 0 {
		t.Errorf("state: status %s, running %+v", st.Status, st.Running)
	}
	for _, d := range dirs {
		slot := slotOf(dir, d)
		unlock, ok := proc.TryLock(slot + ".lock")
		if !ok {
			t.Errorf("slot %s is still locked", slot)
			continue
		}
		unlock()
		if held, _ := holdState(slot); held {
			t.Errorf("slot %s is held", slot)
		}
	}
}

// A cancel after one candidate finished keeps its paid-for work on a
// branch (the step starts over on resume, and its slot is reset).
func TestBestOfCancelKeepsFinishedWork(t *testing.T) {
	dir := gitRepo(t)
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
			return r
		}
		os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte(s.Provider+"\n"), 0o644)
		if s.Provider == event.Claude {
			return runner.Result{Final: "done"}
		}
		<-ctx.Done()
		return runner.Result{Err: errString("killed"), Killed: true}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) {
		bestOfOn(c)
		c.Verify.Commands = []string{fileCheck("greet.txt")}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() { done <- o.Run(ctx, bestOfTask) }()
	deadline := time.Now().Add(60 * time.Second)
	for checked := false; !checked; {
		if time.Now().After(deadline) {
			t.Fatal("the finished candidate's checks never ran")
		}
		time.Sleep(10 * time.Millisecond)
		rec.mu.Lock()
		for _, e := range rec.evs {
			checked = checked || (e.Kind == event.Log && strings.Contains(e.Text, "verify ✓ work--claude: "))
		}
		rec.mu.Unlock()
	}
	cancel()
	if res := <-done; res.OK {
		t.Fatalf("a cancelled task is ok: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "greet.txt")); !os.IsNotExist(err) {
		t.Error("a cancelled best-of step changed the tree")
	}
	branches := tgit(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/sy/")
	if !strings.Contains(branches, "/work--claude") || strings.Contains(branches, "/work--codex") {
		t.Errorf("branches = %q, want the finished candidate's only", branches)
	}
}

// Killing one candidate leaves the other; killing the step kills both.
func TestBestOfKill(t *testing.T) {
	for _, target := range []string{"work--codex", "work"} {
		t.Run(target, func(t *testing.T) {
			dir := gitRepo(t)
			started := make(chan string, 2)
			set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
				if r, ok := twoEdits(s); ok && !strings.Contains(s.Prompt, runner.MarkerStep) {
					return r
				}
				os.WriteFile(filepath.Join(s.Dir, "greet.txt"), []byte(s.Provider+"\n"), 0o644)
				started <- s.AgentID
				if s.Provider == event.Claude && target != "work" {
					return runner.Result{Final: "done"}
				}
				<-ctx.Done()
				return runner.Result{Err: errString("killed"), Killed: true}
			})
			o, _ := newOrc(t, dir, set, bestOfOn)
			done := make(chan TaskResult, 1)
			go func() { done <- o.Run(context.Background(), bestOfTask) }()
			for i := 0; i < 2; i++ {
				select {
				case <-started:
				case <-time.After(60 * time.Second):
					t.Fatal("the candidates did not start")
				}
			}
			if !o.Kill(target) {
				t.Fatalf("Kill(%s) found nothing to stop", target)
			}
			res := <-done
			_, err := os.Stat(filepath.Join(dir, "greet.txt"))
			if target == "work" {
				if res.OK || !strings.Contains(res.Summary, "0/1 subtasks ok") || !os.IsNotExist(err) {
					t.Errorf("killed step: %+v, greet.txt: %v", res, err)
				}
				return
			}
			if !res.OK || read(t, filepath.Join(dir, "greet.txt")) != "claude\n" {
				t.Errorf("the remaining candidate should win: %+v", res)
			}
			if recs := bestOfRecs(t, o); len(recs) != 2 || recs[0].Reason != sessionlog.BestOfOnly {
				t.Errorf("records %+v", recs)
			}
		})
	}
}

// Without worktrees (not a git repo) best of N falls back to one agent,
// with a log line.
func TestBestOfNeedsWorktrees(t *testing.T) {
	dir := t.TempDir()
	var sp spy
	set := both(func(s runner.Spec) runner.Result {
		sp.note(s)
		return runner.Result{Final: "done"}
	})
	o, rec := newOrc(t, dir, set, func(c *config.Config) { bestOfOn(c); c.Orchestrator.ReviewBeforeDone = false })
	if res := o.Run(context.Background(), bestOfTask); !res.OK {
		t.Fatalf("task failed: %+v", res)
	}
	if _, ok := sp.specs["work"]; !ok || len(sp.specs) != 1 {
		t.Errorf("want one agent: %v", sp.specs)
	}
	logged := false
	for _, e := range rec.all() {
		logged = logged || (e.Kind == event.Log && strings.Contains(e.Text, "best of N needs pool worktrees (not a git repo)"))
	}
	if !logged {
		t.Error("no log line says why best of N did not run")
	}
}

// The estimate before approval counts every candidate.
func TestBestOfEstimate(t *testing.T) {
	o, _ := newOrc(t, "", runner.Set{event.Codex: scripted{}, event.Claude: scripted{}}, nil)
	cfg := o.opts.Store.Get()
	tk := &task{cfg: cfg, runners: o.opts.Runners(cfg)}
	plan := Plan{Subtasks: []Subtask{{ID: "w", Title: "work", Kind: router.KindEdit, Prompt: "change it"}}}
	one := o.estimatePlan(tk, nil, plan)
	plan.Subtasks[0].BestOf = BestOfOn
	two := o.estimatePlan(tk, nil, plan)
	se, _ := two.Step("w")
	if se.BestOf != 2 || !strings.Contains(se.Route, " + ") {
		t.Fatalf("best-of step estimate %+v", se)
	}
	s1, _ := one.Step("w")
	if se.Tokens.Mid <= s1.Tokens.Mid || two.Tokens.Mid <= one.Tokens.Mid {
		t.Errorf("best of 2 should cost more: %v vs %v", se.Tokens, s1.Tokens)
	}
}

func TestBestOfRoutes(t *testing.T) {
	o, _ := newOrc(t, "", runner.Set{event.Codex: scripted{}, event.Claude: scripted{}}, nil)
	cfg := o.opts.Store.Get()
	tk := &task{cfg: cfg, runners: o.opts.Runners(cfg)}
	step := router.Step{ID: "w", Title: "work", Kind: router.KindEdit, Prompt: "change it"}
	routes, _ := o.bestOfRoutes(tk, step)
	if len(routes) != 2 || routes[0].Provider != event.Codex || routes[1].Provider != event.Claude || routes[1].Rule != router.RuleBestOf {
		t.Fatalf("default routes %+v", routes)
	}
	if routes[1].Model != cfg.Roles[event.RoleWorker].For(event.Claude).Model {
		t.Errorf("the other candidate should use the worker route on claude: %+v", routes[1])
	}
	tk.cfg.Routing.BestOf.Routes = []string{"claude", "claude:opus:high", "nope:x"}
	routes, skipped := o.bestOfRoutes(tk, step)
	if len(routes) != 2 || routes[0].Model != "sonnet" || routes[1].Model != "opus" || len(skipped) != 1 {
		t.Errorf("configured routes %+v, skipped %v", routes, skipped)
	}
	// Near its limit: left out.
	tk.cfg.Routing.BestOf.Routes = nil
	o.Tracker().SetQuota(event.Claude, event.QuotaInfo{Utilization: 0.95})
	if routes, skipped := o.bestOfRoutes(tk, step); len(routes) != 1 || len(skipped) != 1 || !strings.Contains(skipped[0], "near its usage limit") {
		t.Errorf("near the limit: %+v, %v", routes, skipped)
	}
}

func TestBestOfID(t *testing.T) {
	used := map[string]bool{}
	plan := map[string]bool{"w--codex": true}
	if id := bestOfID("w", "codex", used, plan); id != "w--codex-2" {
		t.Errorf("id = %s", id)
	}
	if id := bestOfID("w", "Claude Code", used, plan); id != "w--claude-code" {
		t.Errorf("id = %s", id)
	}
	if id := bestOfID("w", "codex", used, plan); id != "w--codex-3" {
		t.Errorf("id = %s", id)
	}
}

func TestParseBestOfPick(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n{\"pick\": \"B\", \"why\": \"x\"}\n```": "B",
		`{"pick": "candidate a"}`:                         "A",
		"A":                                               "A",
		"both are fine":                                   "",
		`{"pick": "AB"}`:                                  "",
	} {
		if got, _ := parseBestOfPick(in); got != want {
			t.Errorf("parseBestOfPick(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBestOfOrder(t *testing.T) {
	a := &bestOfCand{id: "a", failing: 1, changed: true, lines: 1}
	b := &bestOfCand{id: "b", changed: false}
	c := &bestOfCand{id: "c", changed: true, lines: 9}
	d := &bestOfCand{id: "d", changed: true, lines: 2, res: stepResult{tokens: event.TokenUsage{CostUSD: 2}}}
	e := &bestOfCand{id: "e", changed: true, lines: 2, res: stepResult{tokens: event.TokenUsage{CostUSD: 1}}}
	var got []string
	for _, x := range bestOfOrder([]*bestOfCand{a, b, c, d, e}) {
		got = append(got, x.id)
	}
	if strings.Join(got, "") != "edcba" {
		t.Errorf("order = %v", got)
	}
}
