package web

import (
	"errors"
	"net/http"

	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/report"
)

// explainView is report.Explanation with the wording rw explain prints, so
// the browser shows the same lines.
type explainView struct {
	*report.Explanation
	Headline   string   `json:"headline"`
	Agents     string   `json:"agents"`
	TokenDelta string   `json:"token_delta"`
	USDDelta   string   `json:"usd_delta"`
	RunLabels  []string `json:"run_labels"`  // provider:model@effort per run
	RunDeltas  []string `json:"run_deltas"`  // actual against estimate per run
	Where      []string `json:"escalate_at"` // step per escalation ("" for a fix round)
}

// handleExplain is rw explain for the browser: why a task ran as one agent
// or several, each run's rule and reason, estimate against use, and what
// escalated. "last" is the newest finished task in this folder.
func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var st *orchestrator.TaskState
	switch {
	case id == "last":
		var err error
		if st, err = report.LastEnded(s.opt.Dir); err != nil {
			fail(w, http.StatusNotFound, err)
			return
		}
	case !validTaskID(id):
		fail(w, http.StatusBadRequest, errors.New("invalid task id"))
		return
	default:
		var err error
		if st, err = orchestrator.LoadTask(id); err != nil {
			fail(w, http.StatusNotFound, err)
			return
		}
	}
	e := report.Explain(st, report.Options{SessionDir: s.store.Get().SessionDir()})
	v := explainView{Explanation: e, Headline: e.Headline(), Agents: e.Agents(), TokenDelta: e.TokenDelta(), USDDelta: e.USDDelta(),
		RunLabels: []string{}, RunDeltas: []string{}, Where: []string{}}
	for i := range e.Runs {
		e.Runs[i].Final = "" // the answers are in the report, not needed here
		v.RunLabels = append(v.RunLabels, e.Runs[i].Label())
		v.RunDeltas = append(v.RunDeltas, e.Runs[i].EstDelta())
	}
	for _, x := range e.Escalations {
		v.Where = append(v.Where, x.Where())
	}
	writeJSON(w, v)
}
