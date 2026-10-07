package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req orchestrator.MemoryChange
		if !readJSON(w, r, &req) {
			return
		}
		if err := orchestrator.ChangeProjectMemory(s.opt.Dir, req); err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
	}
	// ?task= previews which notes a task with this text would get.
	v, err := orchestrator.ProjectMemory(s.opt.Dir, r.URL.Query().Get("task"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, v)
}

func (s *Server) handleRecovery(w http.ResponseWriter, r *http.Request) {
	v, err := orchestrator.Recovery(s.opt.Dir)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, v)
}

func (s *Server) handleRecoveryUndo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Apply bool   `json:"apply"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !validTaskID(req.ID) {
		fail(w, http.StatusBadRequest, errors.New("invalid task id"))
		return
	}
	t, err := orchestrator.LoadTask(req.ID)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if !sameDir(t.Dir, s.opt.Dir) {
		fail(w, http.StatusConflict, errors.New("open this task's project to undo it"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.orc.Running() || t.Status == "running" && !t.Interrupted() {
		fail(w, http.StatusConflict, errors.New("wait for the running task before undoing work"))
		return
	}
	if t.UndoKey == "" {
		fail(w, http.StatusConflict, errors.New("this task has no undo snapshot"))
		return
	}
	var p orchestrator.UndoPlan
	if req.Apply {
		p, err = orchestrator.Undo(s.opt.Dir, t.UndoKey, false, true)
	} else {
		p, err = orchestrator.PreviewUndo(s.opt.Dir, t.UndoKey, false)
	}
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, struct {
		Plan    orchestrator.UndoPlan `json:"plan"`
		Message string                `json:"message"`
	}{p, fmt.Sprintf("%d changed files; unreported files are kept", p.TotalChanges())})
}
