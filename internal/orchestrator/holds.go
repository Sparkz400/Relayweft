package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
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

// holdState reports whether slot is held, and whether its mark is stale
// (it surely no longer applies and may be removed). It changes nothing.
func holdState(slot string) (held, stale bool) {
	h, ok, err := readHold(slot)
	if !ok {
		return false, false
	}
	if err != nil {
		// Unreadable: held, unless the mark is older than any hold.
		if st, serr := os.Stat(holdPath(slot)); serr == nil && time.Since(st.ModTime()) > holdMaxAge {
			return false, true
		}
		return true, false
	}
	st, err := readTask(h.Task)
	if errors.Is(err, fs.ErrNotExist) {
		return false, true // the task's state is gone (pruned)
	}
	if err != nil {
		return true, false // cannot tell right now: keep the edits
	}
	r, running := st.Running[h.Step]
	if !running || !samePath(r.Slot, slot) || time.Since(r.Started) >= holdMaxAge {
		return false, true
	}
	return true, false
}

// slotHeld is holdState that removes a stale mark. The caller holds the
// slot lock.
func slotHeld(slot string) bool {
	held, stale := holdState(slot)
	if stale {
		os.Remove(holdPath(slot))
	}
	return held
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
