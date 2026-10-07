package web

import (
	"errors"
	"net/http"
	"sort"

	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/report"
)

// inspectionView uses the same persisted evidence as rw report. Reading a
// result never runs checks or modifies the project's working tree.
type inspectionView struct {
	Report   *report.Data              `json:"report"`
	Recovery orchestrator.RecoveryItem `json:"recovery"`
	Here     bool                      `json:"here"`
}

func (s *Server) handleInspection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var st *orchestrator.TaskState
	var err error
	if id == "last" {
		// A resumed task may have been created before many newer tasks. The
		// result card means most recently finished, not most recently created.
		for _, task := range orchestrator.History(s.opt.Dir, 0) {
			if task.Status == "running" && !task.Interrupted() {
				continue
			}
			if st == nil || task.Updated.After(st.Updated) {
				st = &task
			}
		}
		if st == nil {
			err = errors.New("no finished task in this folder yet")
		}
	} else {
		if !validTaskID(id) {
			fail(w, http.StatusBadRequest, errors.New("invalid task id"))
			return
		}
		st, err = orchestrator.LoadTask(id)
	}
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	d := report.Build(st, report.Options{SessionDir: s.store.Get().SessionDir(), Version: s.opt.Version})
	here := sameDir(st.Dir, s.opt.Dir)
	i := orchestrator.RecoveryItem{ID: st.ID, Task: st.Task, Status: d.Status,
		Saved: st.UnfinishedSaved(), Branches: append([]string{}, st.Kept...)}
	if d.Diff != nil && d.Diff.Undone && i.Status != "running" {
		i.Status = "undone"
	}
	i.CanResume = here && (i.Status == "interrupted" || i.Status == "failed" || i.Status == "cancelled")
	i.CanUndo = here && i.Status != "running" && d.Diff != nil && !d.Diff.Undone && d.Diff.Before != ""
	seen := map[string]bool{}
	for _, b := range i.Branches {
		seen[b] = true
	}
	for _, b := range st.Branches {
		if !seen[b] {
			i.Branches = append(i.Branches, b)
			seen[b] = true
		}
	}
	sort.Strings(i.Branches)
	for id, run := range st.Running {
		if run.Resolve != nil {
			i.Conflicts = append(i.Conflicts, id+": conflict with "+run.Resolve.With)
		}
	}
	sort.Strings(i.Conflicts)
	writeJSON(w, inspectionView{Report: d, Recovery: i, Here: here})
}
