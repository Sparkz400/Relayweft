package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/diag"
	"github.com/sparkz400/relayweft/internal/proc"
)

// Task state for resume:
//
//   - Every task writes <user config dir>/relayweft/tasks/<id>.json: the
//     task text, its directory, the (approved) plan and each finished
//     subtask's result. It is rewritten after every step, so a crash, a
//     closed window or a frozen PC loses at most the steps that were running.
//   - While a task runs, rw holds <id>.lock; a state that says "running" but
//     whose lock is free was interrupted and can be resumed.
//   - Resuming skips the planner and every subtask that already succeeded
//     (their changes are already in the working tree) and runs the rest,
//     then verify and the final review as usual.
//   - A subtask's agent is recorded in Running as it starts, with its CLI
//     session id as soon as the CLI reports it (not only at the end): a
//     resume continues that session in the same folder (the main tree, or
//     the pool worktree that still holds its half-done edits), so the agent
//     finishes its step instead of starting over (resumeStep).

// StepState is a finished subtask.
type StepState struct {
	OK    bool   `json:"ok"`
	Final string `json:"final,omitempty"`
	Err   string `json:"err,omitempty"`
	// BestOf says how a best-of step's winner was picked (bestof.go).
	BestOf string `json:"best_of,omitempty"`
	// Resolved says how a merge conflict of the step was resolved
	// (resolve.go).
	Resolved string `json:"resolved,omitempty"`
}

// StepRun is a subtask's agent that was started and has not finished: if
// rw stops, a resume continues it (resumeStep).
type StepRun struct {
	Provider string `json:"provider"`
	// Kind is the provider's CLI protocol then; a resume needs the same.
	Kind    string `json:"kind,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Role    string `json:"role,omitempty"`
	Session string `json:"session,omitempty"` // the CLI's session id ("" until reported)
	Dir     string `json:"dir"`               // the agent's working directory
	// Slot is the pool worktree Dir is in ("" = the repo's own tree), and
	// Base the commit it was prepared at: the slot's half-done edits are
	// changes against Base.
	Slot    string    `json:"slot,omitempty"`
	Base    string    `json:"base,omitempty"`
	Attempt int       `json:"attempt"`
	Started time.Time `json:"started"`
	// Token identifies this run in its slot's hold mark (holds.go).
	Token string `json:"token,omitempty"`
	// A best-of winner (bestof.go): Kept is its committed work (against
	// Base, also on a branch), landed instead if its worktree is gone;
	// BestOf how it was picked, for the step's result.
	Kept   string `json:"kept,omitempty"`
	BestOf string `json:"best_of,omitempty"`
	// Resolve is set while an agent resolves the step's merge conflict
	// (resolve.go): Kept is then the step's work against Base, and a
	// resume lands it again, resolving the conflict anew.
	Resolve *ResolveRun `json:"resolve,omitempty"`
}

// ResolveRun is a resolve step in progress.
type ResolveRun struct {
	Agent string    `json:"agent"`           // the resolve agent's id
	With  string    `json:"with"`            // what the step's change conflicts with
	Paths []string  `json:"paths,omitempty"` // the conflicted files
	Yours bool      `json:"yours,omitempty"` // with your own uncommitted edits
	Since time.Time `json:"since"`
	// Summary is what the step's agent reported about its change.
	Summary string `json:"summary,omitempty"`
}

// TaskState is the persisted progress of one task.
type TaskState struct {
	ID       string               `json:"id"`
	Task     string               `json:"task"`
	Dir      string               `json:"dir"`
	Mode     string               `json:"mode"`
	Status   string               `json:"status"` // running, done, failed, cancelled
	Phase    string               `json:"phase,omitempty"`
	Created  time.Time            `json:"created"`
	Updated  time.Time            `json:"updated"`
	Summary  string               `json:"summary,omitempty"`
	Plan     *Plan                `json:"plan,omitempty"`
	Results  map[string]StepState `json:"results,omitempty"`
	UndoKey  string               `json:"undo_key,omitempty"`
	CostLine string               `json:"cost,omitempty"`
	// Repos are the extra repos of a multi-repo task (workspace.go); the
	// plan's subtasks name them. A resume works in the same repos.
	Repos []Repo `json:"repos,omitempty"`
	// Authors counts the writing agents that finished ok, per provider
	// (rw review asks the other one).
	Authors map[string]int `json:"authors,omitempty"`
	// Running are the subtasks whose agent started and did not finish, by
	// subtask id.
	Running map[string]StepRun `json:"running,omitempty"`
	// Saved are half-done edits of interrupted steps, saved on a branch
	// when the pool worktree that held them was given up (holds.go).
	Saved []SavedEdits `json:"saved,omitempty"`
}

// setRunning records that a subtask's agent starts.
func (s *TaskState) setRunning(id string, r StepRun) {
	if s == nil {
		return
	}
	if r.Slot != "" && r.Token == "" {
		r.Token = newToken()
	}
	stateMu.Lock()
	if s.Running == nil {
		s.Running = map[string]StepRun{}
	}
	s.Running[id] = r
	stateMu.Unlock()
	s.save()
	if r.Slot != "" {
		// Keep its half-done edits there if rw dies (holds.go).
		holdSlot(r.Slot, slotHold{Task: s.ID, Step: id, Token: r.Token, Base: r.Base})
	}
}

// noteSession records the session id a running subtask's CLI reported.
func (s *TaskState) noteSession(id, session string) {
	if s == nil || session == "" {
		return
	}
	stateMu.Lock()
	r, ok := s.Running[id]
	if ok && r.Session != session {
		r.Session = session
		s.Running[id] = r
	}
	stateMu.Unlock()
	if ok {
		s.save()
	}
}

// runningStep returns the recorded run of a subtask.
func (s *TaskState) runningStep(id string) (StepRun, bool) {
	if s == nil {
		return StepRun{}, false
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	r, ok := s.Running[id]
	return r, ok
}

// noteAuthor counts a writing agent of provider that finished ok.
func (s *TaskState) noteAuthor(provider string) {
	if s == nil || provider == "" {
		return
	}
	stateMu.Lock()
	if s.Authors == nil {
		s.Authors = map[string]int{}
	}
	s.Authors[provider]++
	stateMu.Unlock()
	s.save()
}

// Author is the provider that wrote most of the task's changes; "" when
// none was recorded or both wrote as much.
func (s TaskState) Author() string {
	best, n, tie := "", 0, false
	for p, c := range s.Authors {
		switch {
		case c > n:
			best, n, tie = p, c, false
		case c == n:
			tie = true
		}
	}
	if tie {
		return ""
	}
	return best
}

// stateDir is where task states live; tests point it elsewhere.
var stateDir = func() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "relayweft", "tasks")
	}
	return filepath.Join(os.TempDir(), "relayweft-tasks")
}

var stateMu sync.Mutex

func statePath(id string) string { return filepath.Join(stateDir(), id+".json") }

// save writes the state atomically.
func (s *TaskState) save() {
	if err := s.saveErr(); err != nil {
		diag.Logf("task state %s not saved: %v", s.ID, err)
	}
}

// saveErr is save that returns the error.
func (s *TaskState) saveErr() error {
	if s == nil || s.ID == "" {
		return nil
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	s.Updated = time.Now()
	_ = os.MkdirAll(stateDir(), 0o755) // writeFileAtomic reports it
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath(s.ID), data)
}

// setResult records a finished subtask. interrupted keeps its running
// agent on record: the step was cancelled while its agent worked (the task
// was cancelled), and a later rw resume --force can continue it.
func (s *TaskState) setResult(id string, r stepResult, interrupted bool) {
	if s == nil {
		return
	}
	stateMu.Lock()
	if s.Results == nil {
		s.Results = map[string]StepState{}
	}
	run, had := s.Running[id]
	bestOf := r.bestOf
	if bestOf == "" {
		bestOf = run.BestOf // a best-of winner that finished through a resume
	}
	s.Results[id] = StepState{OK: r.ok, Final: clip(r.final, 4000), Err: clip(r.err, 1000), BestOf: clip(bestOf, 600), Resolved: clip(r.resolved, 600)}
	if !interrupted {
		delete(s.Running, id)
	}
	stateMu.Unlock()
	if had && run.Slot != "" && !interrupted {
		unholdSlot(run.Slot, s.ID, id)
	}
	s.save()
}

// dropRunning forgets a subtask's running agent in slot and frees the slot
// (dropCleanHold).
func (s *TaskState) dropRunning(id, slot string) {
	if s == nil {
		return
	}
	stateMu.Lock()
	run, had := s.Running[id]
	if had && samePath(run.Slot, slot) {
		delete(s.Running, id)
	}
	stateMu.Unlock()
	if had && samePath(run.Slot, slot) {
		s.save()
		unholdSlot(slot, s.ID, id)
	}
}

// lock takes the task's lock file for as long as it runs; ok is false when
// another rw holds it. Another rw may hold it for a moment without running
// the task (checking a hold, recording saved edits), so it is retried
// briefly.
func (s *TaskState) lock() (unlock func(), ok bool) {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		// The lock below fails then and reads as "another rw has it".
		diag.Logf("task state: %v", err)
	}
	return lockRetry(filepath.Join(stateDir(), s.ID+".lock"))
}

// Interrupted reports whether the task stopped without finishing and no
// rw is running it now.
func (s TaskState) Interrupted() bool {
	if s.Status != "running" {
		return false
	}
	return !proc.Locked(filepath.Join(stateDir(), s.ID+".lock"))
}

// LoadTask reads one task state.
func LoadTask(id string) (*TaskState, error) {
	s, err := readTask(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no task %q (rw history lists them)", id)
	}
	return s, err
}

// readTask reads one task state; the error wraps fs.ErrNotExist when there
// is none. A read that fails while a save replaces the file is retried.
func readTask(id string) (*TaskState, error) {
	var err error
	for i := 0; i < 4; i++ {
		var data []byte
		stateMu.Lock() // not while this process replaces it
		data, err = readRetry(statePath(id))
		stateMu.Unlock()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			var s TaskState
			if err = json.Unmarshal(data, &s); err == nil {
				return &s, nil
			}
		}
		time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
	}
	return nil, err
}

// History lists task states, newest first; dir filters by project ("" = all).
func History(dir string, limit int) []TaskState {
	files, _ := filepath.Glob(filepath.Join(stateDir(), "*.json"))
	var out []TaskState
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s TaskState
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		if dir != "" && !samePath(s.Dir, dir) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// recentReads caps the state files Recent reads.
const recentReads = 40

// Recent is History for shell completion, which runs on every Tab: it
// reads only the recentReads newest state files (by modification time),
// so it stays quick however many there are. dir filters by project
// ("" = all); at most limit states, newest first.
func Recent(dir string, limit int) []TaskState {
	files, _ := filepath.Glob(filepath.Join(stateDir(), "*.json"))
	type file struct {
		path string
		mod  time.Time
	}
	var fs []file
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			fs = append(fs, file{f, st.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.After(fs[j].mod) })
	var out []TaskState
	for _, f := range fs[:min(len(fs), recentReads)] {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		var s TaskState
		if json.Unmarshal(data, &s) != nil || s.ID == "" {
			continue
		}
		if dir != "" && !samePath(s.Dir, dir) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// LastInterrupted returns the newest interrupted task in dir, if any.
func LastInterrupted(dir string) *TaskState {
	for _, s := range History(dir, 20) {
		if s.Interrupted() {
			s := s
			return &s
		}
	}
	return nil
}

// pruneStates keeps the newest 200 task states.
func pruneStates() {
	all := History("", 0)
	for _, s := range all[min(len(all), 200):] {
		os.Remove(statePath(s.ID))
		os.Remove(filepath.Join(stateDir(), s.ID+".lock"))
	}
}

// UnfinishedSaved are the saved half-done edits of steps that have not
// succeeded since (a resume runs them again).
func (s TaskState) UnfinishedSaved() []SavedEdits {
	var out []SavedEdits
	for _, sv := range s.Saved {
		if r, ok := s.Results[sv.Step]; !ok || !r.OK {
			out = append(out, sv)
		}
	}
	return out
}

// resumeSummary describes what a resume will skip.
func (s TaskState) resumeSummary() string {
	if s.Plan == nil {
		return "no plan was saved: starting over"
	}
	var done []string
	for _, st := range s.Plan.Subtasks {
		if r, ok := s.Results[st.ID]; ok && r.OK {
			done = append(done, st.ID)
		}
	}
	if len(done) == 0 {
		return fmt.Sprintf("resuming the saved plan (%d subtasks)", len(s.Plan.Subtasks))
	}
	return fmt.Sprintf("resuming: %s already done, %d of %d left", strings.Join(done, ", "), len(s.Plan.Subtasks)-len(done), len(s.Plan.Subtasks))
}
