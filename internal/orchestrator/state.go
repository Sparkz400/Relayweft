package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/proc"
)

// Task state for resume:
//
//   - Every task writes <user config dir>/switchyard/tasks/<id>.json: the
//     task text, its directory, the (approved) plan and each finished
//     subtask's result. It is rewritten after every step, so a crash, a
//     closed window or a frozen PC loses at most the steps that were running.
//   - While a task runs, sy holds <id>.lock; a state that says "running" but
//     whose lock is free was interrupted and can be resumed.
//   - Resuming skips the planner and every subtask that already succeeded
//     (their changes are already in the working tree) and runs the rest,
//     then verify and the final review as usual.

// StepState is a finished subtask.
type StepState struct {
	OK    bool   `json:"ok"`
	Final string `json:"final,omitempty"`
	Err   string `json:"err,omitempty"`
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
	// (sy review asks the other one).
	Authors map[string]int `json:"authors,omitempty"`
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
		return filepath.Join(d, "switchyard", "tasks")
	}
	return filepath.Join(os.TempDir(), "switchyard-tasks")
}

var stateMu sync.Mutex

func statePath(id string) string { return filepath.Join(stateDir(), id+".json") }

// save writes the state atomically.
func (s *TaskState) save() {
	if s == nil || s.ID == "" {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	s.Updated = time.Now()
	os.MkdirAll(stateDir(), 0o755)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := statePath(s.ID) + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, statePath(s.ID))
	}
}

func (s *TaskState) setResult(id string, r stepResult) {
	if s == nil {
		return
	}
	stateMu.Lock()
	if s.Results == nil {
		s.Results = map[string]StepState{}
	}
	s.Results[id] = StepState{OK: r.ok, Final: clip(r.final, 4000), Err: clip(r.err, 1000)}
	stateMu.Unlock()
	s.save()
}

// lock takes the task's lock file for as long as it runs; ok is false when
// another sy holds it.
func (s *TaskState) lock() (unlock func(), ok bool) {
	os.MkdirAll(stateDir(), 0o755)
	return proc.TryLock(filepath.Join(stateDir(), s.ID+".lock"))
}

// Interrupted reports whether the task stopped without finishing and no
// sy is running it now.
func (s TaskState) Interrupted() bool {
	if s.Status != "running" {
		return false
	}
	unlock, ok := proc.TryLock(filepath.Join(stateDir(), s.ID+".lock"))
	if ok {
		unlock()
	}
	return ok
}

// LoadTask reads one task state.
func LoadTask(id string) (*TaskState, error) {
	data, err := os.ReadFile(statePath(id))
	if err != nil {
		return nil, fmt.Errorf("no task %q (sy history lists them)", id)
	}
	var s TaskState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
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
