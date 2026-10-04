package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/proc"
)

// Held pool worktrees:
//
//   - A pool worktree in which a plan step's agent works is marked as held
//     (<slot>.hold names the task, the step and the run's token) until the
//     step ends.
//   - If sy dies, or the task is cancelled, the mark keeps the step's
//     half-done edits there for sy resume: other tasks, follow-ups and
//     pruning do not take the worktree while the task's state still records
//     the step in it, for at most holdMaxAge. sy undo of the task releases
//     it; sy clean removes it.
//   - A mark that cannot be read for sure (a read racing a save on Windows,
//     a state file another sy is replacing) counts as held: wrongly freeing
//     a slot would delete the edits.
//   - A resume claims the worktree only if the mark is still its own run's
//     (same token): any other use of the worktree replaces or removes it.
//   - Before a hold that expired (or whose task's state is gone) is
//     released, and before sy clean removes a held worktree, the half-done
//     edits in it are saved on a branch (sy/<task>/<step>-unfinished, like
//     rejected work) and recorded in the task's state (Saved), so sy history
//     and sy resume can say where they are. Only a new branch is created:
//     the person's branches, index and working tree are never touched. If
//     they cannot be saved, the worktree stays held.

const holdMaxAge = 7 * 24 * time.Hour

// slotHold is a hold mark.
type slotHold struct {
	Task  string `json:"task"`
	Step  string `json:"step"`
	Token string `json:"token,omitempty"`
}

func holdPath(slot string) string { return slot + ".hold" }

// newToken returns a random token for a step run.
func newToken() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// holdSlot marks slot as holding the work of a step run (the slot lock is
// held).
func holdSlot(slot string, h slotHold) {
	if data, err := json.Marshal(h); err == nil {
		writeFileAtomic(holdPath(slot), data)
	}
}

// readHold reads a slot's mark; ok is false when there is none.
func readHold(slot string) (h slotHold, ok bool, err error) {
	data, err := readRetry(holdPath(slot))
	if errors.Is(err, fs.ErrNotExist) {
		return h, false, nil
	}
	if err != nil {
		return h, true, err
	}
	if err := json.Unmarshal(data, &h); err != nil || h.Task == "" {
		return h, true, fmt.Errorf("unreadable hold mark %s", holdPath(slot))
	}
	return h, true, nil
}

// unholdSlot removes the mark if it is task's step's.
func unholdSlot(slot, task, step string) {
	if h, ok, err := readHold(slot); ok && err == nil && h.Task == task && h.Step == step {
		os.Remove(holdPath(slot))
	}
}

// holdInfo is what a slot's mark means now.
type holdInfo struct {
	held  bool
	stale bool // it surely no longer applies and may be removed
	// save: stale, but the worktree may still hold half-done edits nobody
	// landed or threw away (the hold expired, or its task's state is
	// gone); they are saved before the mark goes (saveHeldEdits).
	save bool
	hold slotHold // the mark, when it could be read
	run  StepRun  // the step run the task's state records, if any
}

// checkHold reads slot's mark and the state of its task. It changes
// nothing.
func checkHold(slot string) holdInfo {
	h, ok, err := readHold(slot)
	if !ok {
		return holdInfo{}
	}
	if err != nil {
		// Unreadable: held, unless the mark is older than any hold.
		if st, serr := os.Stat(holdPath(slot)); serr == nil && time.Since(st.ModTime()) > holdMaxAge {
			return holdInfo{stale: true, save: true}
		}
		return holdInfo{held: true}
	}
	i := holdInfo{hold: h}
	st, err := readTask(h.Task)
	if errors.Is(err, fs.ErrNotExist) {
		i.stale, i.save = true, true // the task's state is gone (pruned)
		return i
	}
	if err != nil {
		i.held = true // cannot tell right now: keep the edits
		return i
	}
	r, running := st.Running[h.Step]
	if !running || !samePath(r.Slot, slot) {
		i.stale = true // the step ended, or works elsewhere now
		return i
	}
	i.run = r
	if time.Since(r.Started) < holdMaxAge || taskLocked(h.Task) {
		// Held; or expired, but a sy is running the task right now and
		// may still claim the worktree: that sy decides.
		i.held = true
		return i
	}
	i.stale, i.save = true, true
	return i
}

// taskLocked reports whether a sy is running the task now (it holds the
// task's lock).
func taskLocked(id string) bool {
	unlock, ok := proc.TryLock(filepath.Join(stateDir(), id+".lock"))
	if ok {
		unlock()
	}
	return !ok
}

// holdState reports whether slot is held, and whether its mark is stale
// (it surely no longer applies and may be removed). It changes nothing.
func holdState(slot string) (held, stale bool) {
	i := checkHold(slot)
	return i.held, i.stale
}

// slotHeld is holdState that removes a stale mark, after saving what an
// expired hold kept (saveHeldEdits). The caller holds the slot lock.
func slotHeld(slot string) bool {
	held, _ := slotHeldNote(slot)
	return held
}

// unsavedSaid are the worktrees whose edits could not be saved that were
// reported already.
var unsavedSaid sync.Map

// errTaskRunning: a sy runs the task now; its state is that sy's.
var errTaskRunning = errors.New("its task is running in a sy")

// slotHeldNote is slotHeld that also says what was saved where, or why
// the slot stays held.
func slotHeldNote(slot string) (held bool, note string) {
	i := checkHold(slot)
	if !i.stale {
		return i.held, ""
	}
	if i.save {
		saved, err := saveHeldEdits(slot, i, "its worktree was freed after 7 days")
		if errors.Is(err, errTaskRunning) {
			return true, "" // its sy decides
		}
		if err != nil {
			// Not saved: keep the worktree held rather than lose them.
			// Said once per worktree and sy: every scan of the pool
			// comes here again.
			if _, said := unsavedSaid.LoadOrStore(canonPath(slot), true); said {
				return true, ""
			}
			msg := fmt.Sprintf("%s holds the half-done edits of an interrupted task that could not be saved on a branch (%v), so it is kept: copy what you need from it, then delete the folder", slot, err)
			diag.Logf("pool: %s", msg)
			diag.Health("leftover", "what", "unsaved-edits", "path", slot)
			return true, msg
		}
		if saved != nil {
			note = saved.Hint()
		}
	}
	os.Remove(holdPath(slot))
	return false, note
}

// SavedEdits are the half-done edits of an interrupted step, saved on a
// branch when its pool worktree was given up.
type SavedEdits struct {
	Step   string `json:"step,omitempty"`
	Task   string `json:"task,omitempty"`
	Branch string `json:"branch"`
	// Base is the commit the step started from: the edits are the
	// difference between Base and Branch.
	Base string    `json:"base"`
	Repo string    `json:"repo,omitempty"` // the repository the branch is in
	Why  string    `json:"why"`
	When time.Time `json:"when"`
}

// Hint says where the edits are and how to get them.
func (s SavedEdits) Hint() string {
	what := "half-done edits"
	if s.Step != "" {
		what = s.Step + "'s half-done edits"
	}
	if s.Task != "" {
		what += " (task " + s.Task + ")"
	}
	where := ""
	if s.Repo != "" {
		where = " in " + s.Repo
	}
	base := s.Base[:min(12, len(s.Base))]
	return fmt.Sprintf("%s were saved on branch %s%s when %s: `git diff %s %s` shows them, `git diff %s %s | git apply` puts them in your tree",
		what, s.Branch, where, s.Why, base, s.Branch, base, s.Branch)
}

// saveHeldEdits saves what the step run that holds slot left there on a
// new branch and records it in the task's state, which then no longer
// counts the step as running there. The caller holds the slot lock. A
// slot that is no longer a worktree, or holds no edits, gives nil. It
// fails when the task is running in a sy right now: that sy owns its
// state and may still claim the worktree.
func saveHeldEdits(slot string, i holdInfo, why string) (*SavedEdits, error) {
	if _, err := os.Lstat(filepath.Join(slot, ".git")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil // gone, or never prepared: nothing to save
	}
	task, step := i.hold.Task, i.hold.Step
	var st *TaskState
	if task != "" {
		s, err := readTask(task)
		switch {
		case err == nil:
			unlock, ok := proc.TryLock(filepath.Join(stateDir(), task+".lock"))
			if !ok {
				return nil, fmt.Errorf("task %s: %w", task, errTaskRunning)
			}
			defer unlock()
			if s, err = readTask(task); err != nil { // as of now, under its lock
				return nil, err
			}
			st = s
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	wg := git{slot}
	head, err := wg.out("rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("%s has no HEAD: %v", slot, err)
	}
	base := head
	if b := i.run.Base; b != "" && (b == head || wg.isAncestor(b, head)) {
		base = b // the agent may have committed: that is part of its work
	}
	label := "unfinished work"
	if step != "" {
		label = "unfinished " + step + " of " + task
	}
	sc, err := wg.commitWork(head, "switchyard: "+label+" (saved from "+slot+" when "+why+")")
	if err != nil {
		return nil, err
	}
	if sc.Commit == base {
		return nil, nil // the agent had not changed anything yet
	}
	name := "sy/unfinished-" + time.Now().Format("20060102") + "-" + refPart(filepath.Base(slot))
	if task != "" {
		name = "sy/" + refPart(task) + "/" + refPart(step) + "-unfinished"
	}
	saved := &SavedEdits{Step: step, Task: task, Branch: newBranch(wg, name, sc.Commit), Base: base, Why: why, When: time.Now()}
	if repo := slotRepo(slot); st == nil || !samePath(repo, st.Dir) {
		saved.Repo = repo // another repo of a multi-repo task, or no state says
	}
	diag.Logf("pool: %s", saved.Hint())
	if st != nil {
		stateMu.Lock()
		st.Saved = append(st.Saved, *saved)
		if r, ok := st.Running[step]; ok && samePath(r.Slot, slot) {
			delete(st.Running, step) // its edits are on the branch now
		}
		stateMu.Unlock()
		st.save()
	}
	return saved, nil
}

// ownHold reports (as an error) whether the slot's mark is still the one
// of the step run h: then nothing used the worktree since.
func ownHold(slot string, h slotHold) error {
	got, ok, err := readHold(slot)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("%s is no longer held for this task (sy clean, or another task used it)", slot)
	case got.Task != h.Task || got.Step != h.Step || (h.Token != "" && got.Token != h.Token):
		return fmt.Errorf("%s was used by another run since", slot)
	}
	return nil
}

// releaseHolds forgets the running steps of the tasks recorded under undo
// key (sy undo reverted them) and removes their hold marks, so their pool
// worktrees are free again. A task that a sy is running now is left alone.
func releaseHolds(key string) {
	if key == "" {
		return
	}
	for _, s := range History("", 0) {
		if s.UndoKey != key && s.ID != key || len(s.Running) == 0 {
			continue
		}
		if s.Status == "running" && !s.Interrupted() {
			continue
		}
		s := s
		stateMu.Lock()
		runs := s.Running
		s.Running = nil
		stateMu.Unlock()
		s.save()
		for step, r := range runs {
			if r.Slot != "" {
				unholdSlot(r.Slot, s.ID, step)
			}
		}
	}
}

// readRetry reads a file, retrying a few times on errors other than "does
// not exist": on Windows a read can fail while another process replaces
// the file.
func readRetry(path string) ([]byte, error) {
	var err error
	for i := 0; i < 6; i++ {
		var data []byte
		if data, err = os.ReadFile(path); err == nil || errors.Is(err, fs.ErrNotExist) {
			return data, err
		}
		time.Sleep(time.Duration(i+1) * 10 * time.Millisecond)
	}
	return nil, err
}

// writeFileAtomic writes data to path through a temporary file and a
// rename, retrying the rename while a reader has the file open (Windows
// refuses to replace it then).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.Remove(tmp)
	return err
}
