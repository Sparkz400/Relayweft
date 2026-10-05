package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/runner"
)

// conflictTest drives a task whose two writers x and y change the same
// file in parallel pool worktrees: whichever lands second conflicts, and a
// resolve agent (<step>--resolve) may run.
type conflictTest struct {
	t   *testing.T
	dir string
	// write is what writer x or y does in its folder.
	write func(s runner.Spec)
	// resolve is the resolve agent (nil: it fails the test).
	resolve func(ctx context.Context, s runner.Spec) runner.Result
	edit    func(*config.Config)

	mu       sync.Mutex
	resolves []runner.Spec
	writers  []string
	arrived  int
	both     chan struct{}
}

func newConflictTest(t *testing.T, dir string) *conflictTest {
	return &conflictTest{t: t, dir: dir, both: make(chan struct{}), write: func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("changed by "+s.StepID+"\n"), 0o644)
	}}
}

// arrive holds a writer until both writers started: both work from the same
// commit, so the second to land always conflicts.
func (c *conflictTest) arrive() {
	c.mu.Lock()
	c.arrived++
	if c.arrived == 2 {
		close(c.both)
	}
	c.mu.Unlock()
	select {
	case <-c.both:
	case <-time.After(30 * time.Second):
	}
}

func (c *conflictTest) runners() runner.Set {
	return bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		switch {
		case strings.Contains(s.Prompt, runner.MarkerResolve):
			c.mu.Lock()
			c.resolves = append(c.resolves, s)
			c.mu.Unlock()
			if c.resolve == nil {
				c.t.Errorf("unexpected resolve agent %s", s.AgentID)
				return runner.Result{Err: errors.New("unexpected")}
			}
			return c.resolve(ctx, s)
		case strings.Contains(s.Prompt, runner.MarkerPlan):
			return runner.Result{Final: planJSON(
				map[string]any{"id": "x", "title": "say x", "kind": "edit", "prompt": "make shared.txt say x", "files": []string{"shared.txt"}},
				map[string]any{"id": "y", "title": "say y", "kind": "edit", "prompt": "make shared.txt say y", "files": []string{"shared.txt"}},
			)}
		case strings.Contains(s.Prompt, runner.MarkerPlanReview), strings.Contains(s.Prompt, runner.MarkerFinalReview):
			return approve()
		}
		c.mu.Lock()
		c.writers = append(c.writers, s.StepID)
		c.mu.Unlock()
		c.arrive()
		c.write(s)
		return runner.Result{Final: "made shared.txt say " + s.StepID, SessionID: "sess-" + s.StepID}
	})
}

func (c *conflictTest) run(ctx context.Context) (TaskResult, *recorder) {
	o, rec := newOrc(c.t, c.dir, c.runners(), c.edit)
	return o.RunWith(ctx, longTask, TaskOptions{}), rec
}

// second is the writer that landed second (the one that conflicted).
func (c *conflictTest) second() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.resolves) > 0 {
		return strings.TrimSuffix(c.resolves[0].StepID, "--resolve")
	}
	return ""
}

func branchList(t *testing.T, dir string) string {
	return tgit(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/rw/")
}

func hasBranchSuffix(list, suffix string) bool {
	for _, b := range strings.Fields(list) {
		if strings.HasSuffix(b, suffix) {
			return true
		}
	}
	return false
}

// Two writers rewrite the same line in parallel. The second to land
// conflicts; its resolve agent gets both intents and the hunks, resolves
// it in its own worktree, and the resolution lands. The task succeeds,
// the step's own version is kept on a branch, and your index is untouched.
func TestResolveTwoWritersSameLines(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		got := read(t, filepath.Join(s.Dir, "shared.txt"))
		if !strings.Contains(got, "<<<<<<<") || !strings.Contains(got, "changed by x") || !strings.Contains(got, "changed by y") {
			t.Errorf("the resolve worktree has no conflict in shared.txt:\n%s", got)
		}
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("x and y\n"), 0o644)
		return runner.Result{Final: "kept both"}
	}
	res, rec := c.run(context.Background())
	if !res.OK || len(res.Kept) != 0 {
		t.Fatalf("the resolved conflict did not finish the task: %+v", res)
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "x and y\n" {
		t.Errorf("shared.txt = %q, want the resolution", got)
	}
	if len(c.resolves) != 1 {
		t.Fatalf("%d resolve agents ran, want 1", len(c.resolves))
	}
	second := c.second()
	first := map[string]string{"x": "y", "y": "x"}[second]
	p := c.resolves[0].Prompt
	for _, want := range []string{
		`THE INCOMING CHANGE: step "` + second + `"`, "make shared.txt say " + second, "made shared.txt say " + second,
		`step ` + first + ` (say ` + first + `)`, "make shared.txt say " + first, "made shared.txt say " + first,
		"<<<<<<<", "changed by x", "changed by y", "- shared.txt: both changed it",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the resolve prompt lacks %q:\n%s", want, p)
		}
	}
	if c.resolves[0].ReadOnly || !within(c.resolves[0].Dir, poolDir(dir)) {
		t.Errorf("the resolve agent did not write in a pool worktree: %s", c.resolves[0].Dir)
	}
	var route *event.Decision
	resolvedMerge := false
	for _, e := range rec.all() {
		if e.Kind == event.Route && e.AgentID == second+"--resolve" {
			route = e.Decision
		}
		if e.Kind == event.Merge && e.AgentID == second && e.OK && strings.Contains(e.Text, "resolved the conflict with step "+first) {
			resolvedMerge = true
		}
	}
	if route == nil || route.Rule != router.RuleResolve || !strings.Contains(route.Reason, "conflict with step "+first) || !strings.Contains(route.Reason, "the route that wrote") {
		t.Errorf("the resolve agent's route does not say why: %+v", route)
	}
	if !resolvedMerge {
		t.Error("no merge event says the conflict was resolved")
	}
	if !hasBranchSuffix(branchList(t, dir), "/"+second) {
		t.Errorf("the step's own version is not kept: %s", branchList(t, dir))
	}
	if staged := tgit(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("your index was changed: %s", staged)
	}
	st := History(dir, 1)[0]
	if r := st.Results[second]; !r.OK || !strings.Contains(r.Resolved, "resolved the conflict") {
		t.Errorf("the task state does not record the resolution: %+v", r)
	}
	if len(st.Running) != 0 {
		t.Errorf("steps still recorded as running: %+v", st.Running)
	}

	// rw undo puts the tree back as it was before the task, redo puts the
	// resolution back.
	if _, err := Undo(dir, "", false, false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "base\n" {
		t.Errorf("after undo shared.txt = %q", got)
	}
	if _, err := Undo(dir, "", true, false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "x and y\n" {
		t.Errorf("after redo shared.txt = %q", got)
	}
}

// A resolve agent that leaves conflict markers gets another attempt that
// says so; after max_resolve_rounds the step fails as before resolve
// steps: the tree keeps the first change, both versions and the last
// attempt are on branches, and the message says how to apply it by hand.
func TestResolveMarkersLeftFallsBack(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		return runner.Result{Final: "looked at it"} // leaves the markers
	}
	res, rec := c.run(context.Background())
	if res.OK || len(res.Kept) != 1 {
		t.Fatalf("a conflict with markers left must fail the step and keep its work: %+v", res)
	}
	if len(c.resolves) != 2 {
		t.Fatalf("%d resolve attempts, want 2 (max_resolve_rounds default)", len(c.resolves))
	}
	if !strings.Contains(c.resolves[1].Prompt, "conflict markers are left in shared.txt") {
		t.Errorf("the second attempt is not told what was wrong:\n%s", c.resolves[1].Prompt)
	}
	second := c.second()
	first := map[string]string{"x": "y", "y": "x"}[second]
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "changed by "+first+"\n" {
		t.Errorf("shared.txt = %q, want the first change only", got)
	}
	branches := branchList(t, dir)
	for _, suffix := range []string{"/" + second, "/" + second + "-other-side", "/" + second + "-resolve-attempt"} {
		if !hasBranchSuffix(branches, suffix) {
			t.Errorf("no branch ending in %s: %s", suffix, branches)
		}
	}
	msg := ""
	for _, e := range rec.all() {
		if e.Kind == event.Merge && e.AgentID == second && !e.OK {
			msg = e.Text
		}
	}
	for _, want := range []string{"NOT applied", "conflict markers are left", "git diff --binary --no-ext-diff --no-color", "--output=rw-" + second + ".patch", "apply --reject"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the conflict message lacks %q: %s", want, msg)
		}
	}
}

// conflicts: fail keeps the old behaviour: no agent, the change on a branch.
func TestResolveOffKeepsBranch(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	c.edit = func(cfg *config.Config) { cfg.Orchestrator.Conflicts = config.ConflictsFail }
	res, rec := c.run(context.Background())
	if res.OK || len(res.Kept) != 1 {
		t.Fatalf("%+v", res)
	}
	found := false
	for _, e := range rec.all() {
		if e.Kind == event.Merge && !e.OK && strings.Contains(e.Text, "orchestrator.conflicts is fail") && strings.Contains(e.Text, "apply --reject") {
			found = true
		}
	}
	if !found {
		t.Error("the conflict message does not say why no agent ran and how to resolve it")
	}
}

// A resolution whose checks fail is another attempt with the check
// output; a resolution that keeps failing checks that passed before the
// merge never lands.
func TestResolveChecksFail(t *testing.T) {
	for _, fixes := range []bool{true, false} {
		name := map[bool]string{true: "second attempt passes", false: "always fails"}[fixes]
		t.Run(name, func(t *testing.T) {
			dir := gitRepo(t)
			os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("ok\n"), 0o644)
			tgit(t, dir, "add", "ok.txt")
			tgit(t, dir, "commit", "-q", "-m", "checks pass")
			c := newConflictTest(t, dir)
			c.edit = func(cfg *config.Config) { cfg.Verify.Commands = []string{fileCheck("ok.txt")} }
			c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
				os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("x and y\n"), 0o644)
				if fixes && strings.Contains(s.Prompt, "the repo's checks fail") && strings.Contains(s.Prompt, "missing ok.txt") {
					os.WriteFile(filepath.Join(s.Dir, "ok.txt"), []byte("ok\n"), 0o644)
				} else {
					os.Remove(filepath.Join(s.Dir, "ok.txt")) // breaks the checks
				}
				return runner.Result{Final: "resolved"}
			}
			res, _ := c.run(context.Background())
			if len(c.resolves) != 2 {
				t.Fatalf("%d resolve attempts, want 2", len(c.resolves))
			}
			if fixes {
				if !res.OK || read(t, filepath.Join(dir, "shared.txt")) != "x and y\n" || read(t, filepath.Join(dir, "ok.txt")) != "ok\n" {
					t.Fatalf("the fixed resolution did not land: %+v", res)
				}
				return
			}
			if res.OK || len(res.Kept) != 1 {
				t.Fatalf("a resolution failing the checks landed: %+v", res)
			}
			if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err != nil {
				t.Error("the failed resolution's deletion reached the tree")
			}
		})
	}
}

// Cancelled while the resolve agent works: the task state keeps the
// step's work and the resolve, and rw resume --force lands it with a new
// resolve agent; no writer runs again.
func TestResolveCancelThenResume(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	started := make(chan struct{}, 1)
	c.resolve = func(ctx context.Context, s runner.Spec) runner.Result {
		started <- struct{}{}
		<-ctx.Done()
		return runner.Result{Err: errors.New("killed"), Killed: true}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan TaskResult, 1)
	go func() {
		r, _ := c.run(ctx)
		done <- r
	}()
	select {
	case <-started:
	case r := <-done:
		t.Fatalf("the task ended before a resolve agent started: %+v", r)
	case <-time.After(60 * time.Second):
		t.Fatal("no resolve agent started")
	}
	cancel()
	if r := <-done; r.OK {
		t.Fatalf("a cancelled task reported success: %+v", r)
	}
	second := c.second()
	st := History(dir, 1)[0]
	run, ok := st.Running[second]
	if st.Status != "cancelled" || !ok || run.Resolve == nil || run.Kept == "" || run.Base == "" {
		t.Fatalf("the cancelled resolve is not recorded: status %s, %+v", st.Status, st.Running)
	}

	c2 := newConflictTest(t, dir)
	c2.arrived = 2 // no writer may wait for another
	close(c2.both)
	c2.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		if !strings.Contains(read(t, filepath.Join(s.Dir, "shared.txt")), "<<<<<<<") {
			t.Error("the resumed resolve has no conflict to resolve")
		}
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("x and y\n"), 0o644)
		return runner.Result{Final: "kept both"}
	}
	o, _ := newOrc(t, dir, c2.runners(), nil)
	res := o.RunWith(context.Background(), "", TaskOptions{Resume: &st, Force: true})
	if !res.OK {
		t.Fatalf("resume failed: %+v", res)
	}
	if len(c2.writers) != 0 || len(c2.resolves) != 1 {
		t.Fatalf("resume ran writers %v and %d resolve agents; want only one resolve agent", c2.writers, len(c2.resolves))
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "x and y\n" {
		t.Errorf("shared.txt = %q", got)
	}
}

// With core.autocrlf the files in worktrees and in your tree have CRLF:
// marker lines ending in \r still count as left, the prompt shows the
// hunks without \r, and the resolution lands with your line endings.
func TestResolveCRLF(t *testing.T) {
	dir := gitRepo(t)
	tgit(t, dir, "config", "core.autocrlf", "true")
	c := newConflictTest(t, dir)
	c.write = func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("changed by "+s.StepID+"\r\n"), 0o644)
	}
	c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		path := filepath.Join(s.Dir, "shared.txt")
		got := read(t, path)
		if strings.Contains(s.Prompt, "\r") {
			t.Error("the prompt has \\r in it")
		}
		if !strings.Contains(s.Prompt, "conflict markers are left") {
			if !strings.Contains(got, "<<<<<<< ") || !strings.Contains(got, "\r\n=======\r\n") {
				t.Errorf("no CRLF conflict in the worktree: %q", got)
			}
			// First attempt: drop only the outer markers.
			var keep []string
			for _, l := range strings.SplitAfter(got, "\n") {
				if !strings.HasPrefix(l, "<<<<<<<") && !strings.HasPrefix(l, ">>>>>>>") {
					keep = append(keep, l)
				}
			}
			os.WriteFile(path, []byte(strings.Join(keep, "")), 0o644)
			return runner.Result{Final: "half done"}
		}
		os.WriteFile(path, []byte("x and y\r\n"), 0o644)
		return runner.Result{Final: "resolved"}
	}
	res, _ := c.run(context.Background())
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if len(c.resolves) != 2 {
		t.Fatalf("%d resolve attempts; the \\r marker lines were not seen as left", len(c.resolves))
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "x and y\r\n" {
		t.Errorf("shared.txt = %q", got)
	}
}

// Binary files, Git LFS files and files git merges as binary never go to
// an agent: the message says which and why, and the change is kept.
func TestResolveBinaryOrLFSNoAgent(t *testing.T) {
	cases := []struct {
		name, file, attrs, why string
		content                func(step string) string
	}{
		{"binary", "pic.bin", "", "a binary file", func(step string) string { return "\x00\x01png " + step }},
		{"lfs", "model.dat", "", "a Git LFS file", func(step string) string {
			return "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat(step, 64) + "\nsize 12\n"
		}},
		{"merge attribute", "deps.lock", "*.lock merge=binary\n", "a binary file (its merge attribute)", func(step string) string { return "lock " + step + "\n" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitRepo(t)
			if tc.attrs != "" {
				os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(tc.attrs), 0o644)
			}
			os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.content("o")), 0o644)
			tgit(t, dir, "add", "-A")
			tgit(t, dir, "commit", "-q", "-m", "add "+tc.file)
			c := newConflictTest(t, dir)
			c.write = func(s runner.Spec) {
				os.WriteFile(filepath.Join(s.Dir, tc.file), []byte(tc.content(s.StepID)), 0o644)
			}
			res, rec := c.run(context.Background())
			if res.OK || len(res.Kept) != 1 || len(c.resolves) != 0 {
				t.Fatalf("result %+v, %d resolve agents", res, len(c.resolves))
			}
			msg := ""
			for _, e := range rec.all() {
				if e.Kind == event.Merge && !e.OK {
					msg = e.Text
				}
			}
			if !strings.Contains(msg, "does not give "+tc.file+" ("+tc.why+") to an agent") || !strings.Contains(msg, "apply --reject") {
				t.Errorf("the message does not say why: %s", msg)
			}
		})
	}
}

// A file one writer deletes and the other changes is a conflict the
// resolve agent is told about as such.
func TestResolveDeleteModify(t *testing.T) {
	dir := gitRepo(t)
	c := newConflictTest(t, dir)
	c.write = func(s runner.Spec) {
		if s.StepID == "x" {
			os.Remove(filepath.Join(s.Dir, "shared.txt"))
			return
		}
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("changed by y\n"), 0o644)
	}
	c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		if !strings.Contains(s.Prompt, "deleted (or renamed) it") {
			t.Errorf("the prompt does not say a side deleted the file:\n%s", s.Prompt)
		}
		os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("changed by y; x wanted it gone\n"), 0o644)
		return runner.Result{Final: "kept it with y's change"}
	}
	res, _ := c.run(context.Background())
	if !res.OK || len(c.resolves) != 1 {
		t.Fatalf("%+v (%d resolve agents)", res, len(c.resolves))
	}
	if got := read(t, filepath.Join(dir, "shared.txt")); got != "changed by y; x wanted it gone\n" {
		t.Errorf("shared.txt = %q", got)
	}
}

// Both writers rename the same file to different names: git leaves both
// names and the old one unmerged, and the prompt says what each side did.
func TestResolveRenameConflict(t *testing.T) {
	dir := gitRepo(t)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte(strings.Repeat("a line long enough for rename detection\n", 5)), 0o644)
	tgit(t, dir, "commit", "-q", "-am", "longer")
	c := newConflictTest(t, dir)
	c.write = func(s runner.Spec) {
		os.Rename(filepath.Join(s.Dir, "shared.txt"), filepath.Join(s.Dir, s.StepID+".txt"))
	}
	c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
		for _, want := range []string{"- shared.txt: deleted on both sides (renamed differently)", "- x.txt: only in", "- y.txt: only in"} {
			if !strings.Contains(s.Prompt, want) {
				t.Errorf("the prompt lacks %q:\n%s", want, s.Prompt)
			}
		}
		// Keep one file under a name both can live with.
		data, _ := os.ReadFile(filepath.Join(s.Dir, "x.txt"))
		os.Remove(filepath.Join(s.Dir, "x.txt"))
		os.Remove(filepath.Join(s.Dir, "y.txt"))
		os.WriteFile(filepath.Join(s.Dir, "renamed.txt"), data, 0o644)
		return runner.Result{Final: "one name"}
	}
	res, _ := c.run(context.Background())
	if !res.OK || len(c.resolves) != 1 {
		t.Fatalf("%+v (%d resolve agents)", res, len(c.resolves))
	}
	for f, want := range map[string]bool{"renamed.txt": true, "x.txt": false, "y.txt": false, "shared.txt": false} {
		if _, err := os.Stat(filepath.Join(dir, f)); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", f, err == nil, want)
		}
	}
}

// conflictAsker is a fakeApprover that also answers ApproveResolve.
type conflictAsker struct {
	fakeApprover
	answer bool
	asked  []ConflictQuestion
}

func (a *conflictAsker) ApproveResolve(ctx context.Context, q ConflictQuestion) bool {
	a.mu.Lock()
	a.asked = append(a.asked, q)
	a.mu.Unlock()
	return a.answer
}

// A step's change that overlaps with edits you made meanwhile: in auto, rw
// asks first; the agent resolves against a snapshot of your tree in a
// pool worktree, the result lands through the 3-way path, and your
// version is kept on a branch. Nobody to ask, or a no: your file stays as
// you left it and the change is kept.
func TestResolveYourEdits(t *testing.T) {
	for _, mode := range []string{"asked yes", "asked no", "nobody to ask", "unattended resolve"} {
		t.Run(mode, func(t *testing.T) {
			dir := gitRepo(t)
			var mu sync.Mutex
			var resolves []runner.Spec
			set := both(func(s runner.Spec) runner.Result {
				switch {
				case strings.Contains(s.Prompt, runner.MarkerResolve):
					mu.Lock()
					resolves = append(resolves, s)
					mu.Unlock()
					got := read(t, filepath.Join(s.Dir, "shared.txt"))
					if !strings.Contains(got, "your edit") || !strings.Contains(got, "changed by x") {
						t.Errorf("the conflict with your edit is not in the worktree: %q", got)
					}
					if !strings.Contains(s.Prompt, "the person's own uncommitted edits") {
						t.Errorf("the prompt does not say the other side is yours:\n%s", s.Prompt)
					}
					os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("your edit\nchanged by x\n"), 0o644)
					return runner.Result{Final: "kept both"}
				case strings.Contains(s.Prompt, runner.MarkerPlan):
					return runner.Result{Final: planJSON(
						map[string]any{"id": "x", "title": "say x", "kind": "edit", "prompt": "make shared.txt say x"},
						map[string]any{"id": "y", "title": "write y", "kind": "edit", "prompt": "write y.txt"},
					)}
				case strings.Contains(s.Prompt, "[RW:"+"PLAN-REVIEW]"), strings.Contains(s.Prompt, runner.MarkerFinalReview):
					return approve()
				}
				if s.StepID == "x" {
					os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("changed by x\n"), 0o644)
					// Meanwhile you edit the same line in your tree.
					os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("your edit\n"), 0o644)
				} else {
					os.WriteFile(filepath.Join(s.Dir, "y.txt"), []byte("y\n"), 0o644)
				}
				return runner.Result{Final: "done " + s.StepID}
			})
			ask := &conflictAsker{answer: mode == "asked yes"}
			o, rec := newOrc(t, dir, set, func(c *config.Config) {
				c.Orchestrator.ApprovePlan = false
				if mode == "unattended resolve" {
					c.Orchestrator.Conflicts = config.ConflictsResolve
				}
			})
			switch mode {
			case "asked yes", "asked no":
				withApprover(o, ask)
			case "nobody to ask":
				withApprover(o, &fakeApprover{})
			}
			res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: mode == "unattended resolve"})
			resolved := mode == "asked yes" || mode == "unattended resolve"
			if (mode == "asked yes" || mode == "asked no") && (len(ask.asked) != 1 || !ask.asked[0].Yours || ask.asked[0].StepID != "x") {
				t.Errorf("not asked about the conflict with your edits: %+v", ask.asked)
			}
			if !resolved {
				if res.OK || len(res.Kept) != 1 || len(resolves) != 0 {
					t.Fatalf("%+v (%d resolve agents)", res, len(resolves))
				}
				if got := read(t, filepath.Join(dir, "shared.txt")); got != "your edit\n" {
					t.Errorf("your file was changed: %q", got)
				}
				why := map[string]string{"asked no": "you chose to keep it on a branch", "nobody to ask": "nobody can be asked"}[mode]
				found := false
				for _, e := range rec.all() {
					found = found || (e.Kind == event.Merge && !e.OK && strings.Contains(e.Text, why))
				}
				if !found {
					t.Errorf("no merge event says %q", why)
				}
				return
			}
			if !res.OK || len(resolves) != 1 {
				t.Fatalf("%+v (%d resolve agents)", res, len(resolves))
			}
			if within(resolves[0].Dir, dir) {
				t.Errorf("the resolve agent worked in your tree: %s", resolves[0].Dir)
			}
			if got := read(t, filepath.Join(dir, "shared.txt")); got != "your edit\nchanged by x\n" {
				t.Errorf("shared.txt = %q", got)
			}
			if read(t, filepath.Join(dir, "y.txt")) != "y\n" {
				t.Error("y did not land")
			}
			if !hasBranchSuffix(branchList(t, dir), "/x") {
				t.Errorf("x's own version is not kept: %s", branchList(t, dir))
			}
			if staged := tgit(t, dir, "diff", "--cached", "--name-only"); staged != "" {
				t.Errorf("your index was changed: %s", staged)
			}
		})
	}
}

// With review_changes on, you see the resolution as a change set marked as
// a conflict resolution; rejecting it keeps the step's change on a branch.
func TestResolveReviewedAsConflict(t *testing.T) {
	for _, accept := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted", false: "rejected"}[accept], func(t *testing.T) {
			dir := gitRepo(t)
			c := newConflictTest(t, dir)
			c.edit = func(cfg *config.Config) {
				cfg.Orchestrator.ReviewChanges = true
				cfg.Orchestrator.ApprovePlan = false
			}
			c.resolve = func(_ context.Context, s runner.Spec) runner.Result {
				os.WriteFile(filepath.Join(s.Dir, "shared.txt"), []byte("x and y\n"), 0o644)
				return runner.Result{Final: "kept both"}
			}
			ap := &fakeApprover{review: func(cs ChangeSet) ChangeDecision {
				if cs.Conflict != "" && !accept {
					return ChangeDecision{}
				}
				return ChangeDecision{Apply: cs.AllPaths()}
			}}
			o, _ := newOrc(t, dir, c.runners(), c.edit)
			withApprover(o, ap)
			res := o.RunWith(context.Background(), longTask, TaskOptions{})
			var marked []ChangeSet
			for _, cs := range ap.seen {
				if cs.Conflict != "" {
					marked = append(marked, cs)
				}
			}
			if len(marked) != 1 || !strings.HasPrefix(marked[0].Title, "Conflict resolution: ") || !strings.Contains(marked[0].Conflict, "conflicts with step") ||
				len(marked[0].Files) != 1 || !strings.Contains(marked[0].Files[0].Patch, "+x and y") {
				t.Fatalf("the resolution was not reviewed as one: %+v", marked)
			}
			if accept != res.OK {
				t.Fatalf("accept %v, result %+v", accept, res)
			}
			want := "x and y\n"
			if !accept {
				want = "changed by " + map[string]string{"x": "y", "y": "x"}[c.second()] + "\n"
			}
			if got := read(t, filepath.Join(dir, "shared.txt")); got != want {
				t.Errorf("shared.txt = %q, want %q", got, want)
			}
		})
	}
}
