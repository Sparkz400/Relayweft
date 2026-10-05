package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/runner"
)

// Review fixes of the held-edits rescue and Codex follow-ups (PR #29).

// expire makes c's hold older than any hold.
func expire(t *testing.T, st *TaskState) StepRun {
	t.Helper()
	run := st.Running["c"]
	run.Started = time.Now().Add(-holdMaxAge - time.Hour)
	st.Running["c"] = run
	st.save()
	return run
}

// giveUp is what a scan of the pool does with a slot: lock it and let the
// hold decide.
func giveUp(t *testing.T, slot string) (held bool, note string) {
	t.Helper()
	unlock, ok := lockSlot(slot)
	if !ok {
		t.Fatal("lock")
	}
	defer unlock()
	return slotHeldNote(slot)
}

// splitArgs splits a command line at spaces, keeping "quoted parts".
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, any := false, false
	for _, r := range s {
		switch {
		case r == '"':
			quoted, any = !quoted, true
		case r == ' ' && !quoted:
			if any {
				out = append(out, cur.String())
			}
			cur.Reset()
			any = false
		default:
			cur.WriteRune(r)
			any = true
		}
	}
	if any {
		out = append(out, cur.String())
	}
	return out
}

// The hint's commands get the saved edits into the tree byte for byte:
// through a patch file (a pipe in Windows PowerShell 5.1 turns "€" into "?"
// and LF into CRLF), with binary files, and from a subfolder too.
func TestSavedHintCommandsApplyExactly(t *testing.T) {
	dir := gitRepo(t)
	text := "c1 € ä\n"
	bin := make([]byte, 256)
	for i := range bin {
		bin[i] = byte(i)
	}
	st, _ := interruptStep(t, dir, nil, longTask, "c", midstepPlan, func(s runner.Spec) {
		os.WriteFile(filepath.Join(s.Dir, "c.txt"), []byte(text), 0o644)
		os.WriteFile(filepath.Join(s.Dir, "bin.dat"), bin, 0o644)
		s.OnSession("sess-c")
	})
	run := expire(t, st)
	if held, note := giveUp(t, run.Slot); held || !strings.Contains(note, "sy-unfinished.patch") {
		t.Fatalf("held %v, note %q", held, note)
	}
	after, _ := LoadTask(st.ID)
	h := after.Saved[0].Hint()
	if strings.Contains(h, "| git apply") || !strings.Contains(h, "--binary") {
		t.Errorf("hint pipes the patch or lacks --binary: %s", h)
	}
	parts := strings.Split(h, "`")
	var cmds [][]string
	for i := 1; i < len(parts); i += 2 {
		if a := splitArgs(parts[i]); len(a) > 0 && a[0] == "git" && strings.Contains(parts[i], "sy-unfinished.patch") {
			cmds = append(cmds, a)
		}
	}
	if len(cmds) != 2 {
		t.Fatalf("want the diff and apply commands in %s", h)
	}
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0o755)
	for _, a := range cmds {
		cmd := exec.Command(a[0], a[1:]...)
		cmd.Dir = sub // the person may be anywhere in the repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", a, err, out)
		}
	}
	if got := read(t, filepath.Join(dir, "c.txt")); got != text {
		t.Errorf("c.txt = %q, want %q", got, text)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "bin.dat")); !bytes.Equal(got, bin) {
		t.Errorf("bin.dat differs (%d bytes)", len(got))
	}
}

// When no branch and no ref can be created for the edits, nothing
// references them: the worktree must stay held, not be freed and reset.
// A branch named "sy" blocks every sy/... branch.
func TestSavedEditsNeedARef(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	run := expire(t, st)
	tgit(t, dir, "branch", "sy")
	head := headOf(t, dir)
	tgit(t, dir, "update-ref", "refs/switchyard/kept", head) // blocks refs/switchyard/kept/<commit>
	held, note := giveUp(t, run.Slot)
	if !held || !strings.Contains(note, "could not be saved") {
		t.Fatalf("held %v, note %q", held, note)
	}
	if _, err := os.Stat(holdPath(run.Slot)); err != nil {
		t.Error("the mark was removed although the edits are on no ref")
	}
	if after, _ := LoadTask(st.ID); len(after.Saved) != 0 || after.Running["c"].Slot == "" {
		t.Errorf("state changed: %+v", after)
	}

	// Only the "sy" branch: the edits are kept under refs/switchyard/kept,
	// and the hint names that ref, not a bare commit id.
	tgit(t, dir, "update-ref", "-d", "refs/switchyard/kept")
	held, note = giveUp(t, run.Slot)
	if held {
		t.Fatalf("still held: %s", note)
	}
	sv := checkSaved(t, dir, st, run.Base)
	if !strings.HasPrefix(sv.Branch, "refs/switchyard/kept/") {
		t.Errorf("saved on %q", sv.Branch)
	}
}

// Writing the task's state fails: the branch is dropped again and the
// worktree stays held, so a later resume does not start over without
// naming where the edits went.
func TestSavedEditsNeedTheState(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	run := expire(t, st)
	// A folder where the state's temporary file goes makes every save fail.
	if err := os.MkdirAll(statePath(st.ID)+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	held, note := giveUp(t, run.Slot)
	os.RemoveAll(statePath(st.ID) + ".tmp")
	if !held || !strings.Contains(note, "state") {
		t.Fatalf("held %v, note %q", held, note)
	}
	if refs := tgit(t, dir, "for-each-ref", "refs/heads/sy"); refs != "" {
		t.Errorf("a branch was left although nothing records it: %s", refs)
	}
	if _, err := os.Stat(holdPath(run.Slot)); err != nil {
		t.Error("the mark was removed")
	}
	if read(t, filepath.Join(run.Slot, "c.txt")) != "c1\n" {
		t.Error("the edit is gone from the worktree")
	}
}

// The agent committed its work and left nothing uncommitted, and the
// task's state is gone: the commit the slot was prepared at (in the mark)
// shows the commits are new, so they are saved, not reset away.
func TestSavedEditsKeepAgentCommits(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	run := st.Running["c"]
	tgit(t, run.Slot, "add", "-A")
	tgit(t, run.Slot, "commit", "-q", "-m", "agent's own commit")
	os.Remove(statePath(st.ID))
	held, note := giveUp(t, run.Slot)
	if held || !strings.Contains(note, "-unfinished") {
		t.Fatalf("held %v, note %q (the agent's commit was taken for nothing)", held, note)
	}
	branch := strings.Fields(note[strings.Index(note, "branch ")+len("branch "):])[0]
	if got := tgit(t, dir, "show", branch+":c.txt"); got != "c1" {
		t.Errorf("c.txt on %s = %q", branch, got)
	}
	if names := tgit(t, dir, "diff", "--name-only", run.Base, branch); names != "c.txt" {
		t.Errorf("diff from the prepared commit names %q", names)
	}
}

// What a branch cannot hold (a nested repository the agent created) is
// named, so nobody thinks it is saved.
func TestSavedEditsNameWhatIsLeft(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	run := expire(t, st)
	nested := filepath.Join(run.Slot, "nested")
	os.MkdirAll(nested, 0o755)
	tgit(t, nested, "init", "-q")
	os.WriteFile(filepath.Join(nested, "x.txt"), []byte("x\n"), 0o644)
	if held, note := giveUp(t, run.Slot); held || !strings.Contains(note, "Not on the branch") || !strings.Contains(note, "nested") {
		t.Fatalf("held %v, note %q", held, note)
	}
}

// sy resume waits a moment for the task's lock: another sy may hold it
// briefly to look at a hold or record saved edits.
func TestResumeWaitsForABriefTaskLock(t *testing.T) {
	dir := gitRepo(t)
	st := interruptC(t, dir)
	unlock, ok := proc.TryLock(filepath.Join(stateDir(), st.ID+".lock"))
	if !ok {
		t.Fatal("lock")
	}
	go func() { time.Sleep(300 * time.Millisecond); unlock() }()
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if strings.Contains(s.Prompt, runner.MarkerFinalReview) {
			return approve()
		}
		finishC(s.Dir)
		return runner.Result{Final: "c"}
	})
	o, _ := newOrc(t, dir, set, nil)
	if res := o.RunWith(context.Background(), "", TaskOptions{Resume: st}); !res.OK {
		t.Fatalf("resume refused while another sy held the lock for a moment: %+v", res)
	}
}

// A follow-up in a pool worktree that is stopped before its work lands
// (here: a usage limit) keeps what it changed on a branch and says so; the
// next follow-up tells the resumed agent its changes are not in its folder.
func TestStoppedFollowUpKeepsItsWork(t *testing.T) {
	dir := gitRepo(t)
	var mu sync.Mutex
	var specs []runner.Spec
	limit := true
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		if r, ok := midstepPlan(s); ok {
			if s.StepID == "a" {
				r.SessionID = "sess-a"
			}
			return r
		}
		if s.StepID == "c" {
			finishC(s.Dir)
			return runner.Result{Final: "c", SessionID: "sess-c"}
		}
		mu.Lock()
		specs = append(specs, s)
		stop := limit
		mu.Unlock()
		os.WriteFile(filepath.Join(s.Dir, "half.txt"), []byte("half\n"), 0o644)
		if stop {
			return runner.Result{Err: errors.New("usage limit reached"), LimitHit: true, SessionID: s.Resume}
		}
		return runner.Result{Final: "done", SessionID: s.Resume}
	})
	o, rec := newOrc(t, dir, set, nil)
	if res := o.Run(context.Background(), longTask); !res.OK {
		t.Fatalf("task: %+v", res)
	}
	sess, _ := o.Session("a")
	sess.Provider = event.Codex
	if res := o.FollowUpSession(context.Background(), sess, "write half.txt"); res.OK {
		t.Fatalf("the follow-up should have hit the limit: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "half.txt")); err == nil {
		t.Fatal("a stopped follow-up's work landed")
	}
	again, _ := o.Session("a")
	if again.Unlanded == "" {
		t.Fatalf("the session does not say its last changes did not land: %+v", again)
	}
	if got := tgit(t, dir, "show", again.Unlanded+":half.txt"); got != "half" {
		t.Errorf("half.txt on %s = %q", again.Unlanded, got)
	}
	if !strings.Contains(logText(rec), again.Unlanded) {
		t.Errorf("the log does not say where the work went:\n%s", logText(rec))
	}
	mu.Lock()
	limit = false
	mu.Unlock()
	if res := o.FollowUpSession(context.Background(), again, "go on"); !res.OK {
		t.Fatalf("next follow-up: %+v", res)
	}
	mu.Lock()
	last := specs[len(specs)-1]
	mu.Unlock()
	if last.Resume == "" || !strings.Contains(last.Prompt, again.Unlanded) || !strings.Contains(last.Prompt, "NOT in this folder") {
		t.Errorf("the resumed agent was not told its changes are elsewhere: resume %q prompt %q", last.Resume, last.Prompt)
	}
	if s3, _ := o.Session("a"); s3.Unlanded != "" {
		t.Errorf("the note outlived a follow-up that landed: %+v", s3)
	}
}

// Multi-repo: an agent that ran in another repo's pool worktree is not
// this repo's to land; Codex resumes it by id as before instead of a fresh
// agent.
func TestCodexFollowUpFromAnotherRepoPool(t *testing.T) {
	dir := gitRepo(t)
	other := gitRepo(t)
	var got runner.Spec
	set := bothCtx(func(ctx context.Context, s runner.Spec) runner.Result {
		got = s
		return runner.Result{Final: "ok", SessionID: s.Resume}
	})
	o, _ := newOrc(t, dir, set, nil)
	slot := filepath.Join(poolDir(other), "0")
	s := AgentSession{AgentID: "w", Provider: event.Codex, Role: event.RoleWorker, SessionID: "sess-w", Dir: slot, Slot: slot, Task: "t"}
	if res := o.FollowUpSession(context.Background(), s, "more"); !res.OK {
		t.Fatalf("follow-up: %+v", res)
	}
	if got.Resume != "sess-w" || !samePath(got.Dir, dir) {
		t.Errorf("resume %q in %s, want sess-w resumed by id from %s", got.Resume, got.Dir, dir)
	}
}
