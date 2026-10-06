package web

import (
	"net/http"
	"time"

	"github.com/sparkz400/relayweft/internal/morning"
)

// handleMorning returns the summary of the unattended work since ?since=
// (12h, 2d or 18:00; default 12h), all projects; ?all=1 adds the tasks
// someone watched. The Overnight panel shows it.
func (s *Server) handleMorning(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	if since == "" {
		since = "12h"
	}
	now := time.Now()
	from, err := morning.ParseSince(since, now)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, morning.Collect(s.store.Get(), morning.Options{Since: from, Until: now, All: r.URL.Query().Get("all") == "1"}))
}
