package orchestrator

import (
	"slices"
	"sort"
)

type RecoveryItem struct {
	ID        string       `json:"id"`
	Task      string       `json:"task"`
	Status    string       `json:"status"`
	Summary   string       `json:"summary"`
	Done      int          `json:"done"`
	Steps     int          `json:"steps"`
	CanResume bool         `json:"can_resume"`
	CanUndo   bool         `json:"can_undo"`
	UndoKey   string       `json:"undo_key"`
	Saved     []SavedEdits `json:"saved"`
	Branches  []string     `json:"branches"`
	Conflicts []string     `json:"conflicts"`
}

// Recovery lists this project's recent work, including completed tasks that can
// still be undone. Running tasks never expose mutating recovery actions.
func Recovery(dir string) ([]RecoveryItem, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, err
	}
	undos, err := UndoList(root)
	if err != nil {
		return nil, err
	}
	canUndo := map[string]bool{}
	undone := map[string]bool{}
	for _, u := range undos {
		canUndo[u.Key] = !u.Undone
		undone[u.Key] = u.Undone
	}
	items := []RecoveryItem{}
	for _, t := range History(root, 100) {
		i := RecoveryItem{ID: t.ID, Task: t.Task, Status: t.Status, Summary: t.Summary, UndoKey: t.UndoKey, Saved: t.UnfinishedSaved(), Branches: t.Kept, Conflicts: []string{}}
		for _, b := range t.Branches {
			if !slices.Contains(i.Branches, b) {
				i.Branches = append(i.Branches, b)
			}
		}
		sort.Strings(i.Branches)
		if t.Interrupted() {
			i.Status = "interrupted"
		}
		if i.Status != "running" && undone[t.UndoKey] {
			i.Status = "undone"
		}
		i.CanResume = i.Status == "interrupted" || i.Status == "failed" || i.Status == "cancelled"
		i.CanUndo = i.Status != "running" && canUndo[t.UndoKey]
		if t.Plan != nil {
			i.Steps = len(t.Plan.Subtasks)
		}
		for id, r := range t.Results {
			if r.OK {
				i.Done++
			} else if r.Err != "" {
				i.Conflicts = append(i.Conflicts, id+": "+r.Err)
			}
		}
		for id, r := range t.Running {
			if r.Resolve != nil {
				i.Conflicts = append(i.Conflicts, id+": conflict with "+r.Resolve.With)
			}
		}
		sort.Strings(i.Conflicts)
		items = append(items, i)
	}
	return items, nil
}
