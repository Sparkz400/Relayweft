package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

//go:embed static
var staticFS embed.FS

// Handler returns the HTTP handler (every route is guarded).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, _ := fs.ReadFile(static, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(static))))

	mux.HandleFunc("POST /api/session", s.handleSession)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.snapshot()) })
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/bye", func(w http.ResponseWriter, r *http.Request) {
		s.hub.bye()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/task", s.handleTask)
	mux.HandleFunc("POST /api/cancel", func(w http.ResponseWriter, r *http.Request) {
		msg, err := s.cancelTask()
		if err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
		s.notice("warn", msg)
		writeJSON(w, map[string]string{"message": msg})
	})
	mux.HandleFunc("POST /api/pause", s.handlePause)
	mux.HandleFunc("POST /api/kill", s.handleKill)
	mux.HandleFunc("POST /api/approvals/{id}/plan", s.handlePlan)
	mux.HandleFunc("POST /api/approvals/{id}/estimate", s.handleEstimate)
	mux.HandleFunc("POST /api/approvals/{id}/changes", s.handleChanges)
	mux.HandleFunc("POST /api/approvals/{id}/budget", s.handleBudget)
	mux.HandleFunc("POST /api/schedule", s.handleSchedule)
	mux.HandleFunc("GET /api/routes", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.routes()) })
	mux.HandleFunc("POST /api/routes", s.handleSetRoute)
	mux.HandleFunc("POST /api/config/save", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.Save(); err != nil {
			fail(w, http.StatusInternalServerError, fmt.Errorf("save failed: %w", err))
			return
		}
		s.mu.Lock()
		s.dirty = false
		s.mu.Unlock()
		s.kick()
		writeJSON(w, map[string]string{"message": "saved to " + s.store.Path()})
	})
	mux.HandleFunc("POST /api/settings", s.handleSettings)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/resume", s.handleResume)
	mux.HandleFunc("GET /api/queue", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.snapshot().Queue) })
	mux.HandleFunc("POST /api/queue/remove", s.handleQueueRemove)
	mux.HandleFunc("POST /api/queue/clear", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.queue)
		s.queue = nil
		s.mu.Unlock()
		s.kick()
		writeJSON(w, map[string]string{"message": fmt.Sprintf("queue cleared (%d dropped)", n)})
	})
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("POST /api/limit", s.handleLimit)
	if s.opt.Demo {
		mux.HandleFunc("POST /api/demo/review", s.handleDemoReview)
	}
	return s.guard(mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("bad request: %w", err))
		return false
	}
	return true
}

// handleEvents streams server-sent events: the state, a reset, the replay
// buffer, "synced", then live events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c, replay, err := s.hub.subscribe()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	defer s.hub.unsubscribe(c)
	// The stream is long-lived: no read deadline for it.
	http.NewResponseController(w).SetReadDeadline(time.Time{})
	var buf bytes.Buffer
	buf.WriteString("retry: 1500\n\n")
	buf.Write(frame("state", s.snapshot()))
	buf.Write(frame("reset", map[string]any{}))
	for _, f := range replay {
		buf.Write(f)
		if buf.Len() > 256<<10 {
			if _, err := w.Write(buf.Bytes()); err != nil {
				return
			}
			buf.Reset()
		}
	}
	buf.Write(frame("synced", map[string]any{}))
	if _, err := w.Write(buf.Bytes()); err != nil {
		return
	}
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		case f, ok := <-c.ch:
			if !ok {
				return
			}
			if _, err := w.Write(f); err != nil {
				return
			}
			// Write whatever else is queued before flushing once.
		drain:
			for i := 0; i < 256; i++ {
				select {
				case f, ok := <-c.ch:
					if !ok {
						fl.Flush()
						return
					}
					if _, err := w.Write(f); err != nil {
						return
					}
				default:
					break drain
				}
			}
			fl.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string `json:"text"`
		Single string `json:"single"` // provider:model[:effort]: one agent only
	}
	if !readJSON(w, r, &req) {
		return
	}
	var (
		res submitResult
		err error
	)
	if req.Single != "" {
		prov, route, perr := config.ParseRouteSpec(req.Single)
		if perr != nil {
			fail(w, http.StatusBadRequest, perr)
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			fail(w, http.StatusBadRequest, errors.New("type a task first"))
			return
		}
		res, err = s.startJob(&job{text: strings.TrimSpace(req.Text), single: &singleRoute{provider: prov, route: route}})
	} else {
		res, err = s.submit(req.Text)
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errBusy) {
			code = http.StatusConflict
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paused bool `json:"paused"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.orc.SetPaused(req.Paused)
	msg := "resumed"
	if req.Paused {
		msg = "paused: running agents finish their current run, nothing new starts"
	}
	s.notice("info", msg)
	s.kick()
	writeJSON(w, map[string]any{"paused": req.Paused, "message": msg})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent string `json:"agent"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.orc.Kill(req.Agent) {
		fail(w, http.StatusNotFound, fmt.Errorf("%s is not running (running: %s)", req.Agent, strings.Join(s.orc.RunningAgents(), ", ")))
		return
	}
	s.notice("warn", "killed "+req.Agent)
	writeJSON(w, map[string]string{"message": "killed " + req.Agent})
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OK   bool              `json:"ok"`
		Plan orchestrator.Plan `json:"plan"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	p, err := s.ap.AnswerPlan(r.PathValue("id"), req.Plan, req.OK)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNoRequest) {
			code = http.StatusGone
		}
		fail(w, code, err)
		return
	}
	msg := "plan not approved: cancelling the task"
	if req.OK {
		msg = fmt.Sprintf("plan approved: %d subtasks", len(p.Subtasks))
	}
	writeJSON(w, map[string]any{"message": msg, "plan": p})
}

// handleEstimate re-estimates a waiting plan as edited on the page.
func (s *Server) handleEstimate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Plan orchestrator.Plan `json:"plan"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	e, err := s.ap.EstimatePlan(r.PathValue("id"), req.Plan)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNoRequest) {
			code = http.StatusGone
		}
		fail(w, code, err)
		return
	}
	writeJSON(w, e)
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Apply    []string         `json:"apply"`
		Hunks    map[string][]int `json:"hunks"`
		Feedback string           `json:"feedback"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	d, err := s.ap.AnswerChanges(r.PathValue("id"), orchestrator.ChangeDecision{Apply: req.Apply, Hunks: req.Hunks, Feedback: strings.TrimSpace(req.Feedback)})
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNoRequest) {
			code = http.StatusGone
		}
		fail(w, code, err)
		return
	}
	var msg string
	switch {
	case d.Feedback != "":
		msg = "sent back to the agent with your feedback"
	case len(d.Apply) == 0:
		msg = "all changes rejected (kept on a branch)"
	default:
		msg = fmt.Sprintf("applying %d file(s)", len(d.Apply))
		if n := len(d.Hunks); n > 0 {
			msg += fmt.Sprintf(", %d of them partly (selected hunks)", n)
		}
	}
	writeJSON(w, map[string]any{"message": msg, "apply": d.Apply, "hunks": d.Hunks})
}

// --- routes ------------------------------------------------------------------

type routeRow struct {
	Role   string `json:"role"`
	Prefer string `json:"prefer"`
	// Codex and Claude repeat Routes for clients written before other
	// providers existed (the VS Code extension).
	Codex  routeJSON            `json:"codex"`
	Claude routeJSON            `json:"claude"`
	Routes map[string]routeJSON `json:"routes"`
	Now    event.Decision       `json:"now"`
}

// routeJSON and modelJSON give the config types (YAML-tagged) JSON names.
type routeJSON struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type modelJSON struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Tier  string `json:"tier,omitempty"`
}

type catalog struct {
	Models   []modelJSON `json:"models"`
	Efforts  []string    `json:"efforts"`
	Disabled bool        `json:"disabled,omitempty"`
	Label    string      `json:"label"`
	Kind     string      `json:"kind"`
}

type routesView struct {
	Roles     []routeRow         `json:"roles"`
	Providers map[string]catalog `json:"providers"`
	Order     []string           `json:"provider_order"` // every provider, in routing order
	Prefer    []string           `json:"prefer_options"`
	Dirty     bool               `json:"dirty"`
	Path      string             `json:"path"`
}

func (s *Server) routes() routesView {
	cfg := s.store.Get()
	s.mu.Lock()
	mainProv, dirty := s.mainProv, s.dirty
	s.mu.Unlock()
	v := routesView{Providers: map[string]catalog{}, Order: cfg.ProviderNames(), Dirty: dirty, Path: s.store.Path()}
	v.Prefer = append(append(v.Prefer, v.Order...), config.PreferOptions...)
	for _, role := range event.Roles {
		rc := cfg.Roles[role]
		row := routeRow{Role: role, Prefer: rc.Prefer, Codex: routeJSON(rc.Codex), Claude: routeJSON(rc.Claude),
			Routes: map[string]routeJSON{}, Now: s.orc.Router().Preview(role, mainProv)}
		for _, p := range v.Order {
			row.Routes[p] = routeJSON(rc.For(p))
		}
		v.Roles = append(v.Roles, row)
	}
	for _, p := range v.Order {
		pc := cfg.Providers[p]
		c := catalog{Models: []modelJSON{}, Efforts: pc.Efforts, Disabled: pc.Disabled, Label: cfg.ProviderLabel(p), Kind: cfg.Kind(p)}
		for _, m := range pc.Models {
			c.Models = append(c.Models, modelJSON(m))
		}
		v.Providers[p] = c
	}
	return v
}

func (s *Server) markDirty() {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
	s.kick()
}

func (s *Server) handleSetRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role     string  `json:"role"`
		Provider string  `json:"provider"`
		Model    *string `json:"model"`
		Effort   *string `json:"effort"`
		Prefer   string  `json:"prefer"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var err error
	var what string
	switch {
	case req.Prefer != "":
		roles := []string{req.Role}
		if req.Role == "all" {
			roles = event.Roles
		}
		for _, role := range roles {
			if req.Role == "all" && role == event.RoleReviewer && s.store.Get().IsProvider(req.Prefer) {
				continue // keep the reviewer on the other provider unless asked by name
			}
			if err = s.store.SetPrefer(role, req.Prefer); err != nil {
				break
			}
		}
		what = fmt.Sprintf("%s: prefer %s", req.Role, req.Prefer)
	case s.store.Get().IsProvider(req.Provider):
		rt := s.store.Get().Roles[req.Role].For(req.Provider)
		if req.Model != nil {
			rt.Model = strings.TrimSpace(*req.Model)
		}
		if req.Effort != nil {
			rt.Effort = strings.TrimSpace(*req.Effort)
		}
		err = s.store.SetRoute(req.Role, req.Provider, rt)
		what = fmt.Sprintf("%s on %s: %s", req.Role, req.Provider, routeLabel(rt))
	default:
		err = fmt.Errorf("want prefer, or a provider (%s) with model and/or effort", strings.Join(s.store.Get().ProviderNames(), ", "))
	}
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("not changed: %w", err))
		return
	}
	s.markDirty()
	s.notice("info", what+" (applies to the next agent; save to keep)")
	writeJSON(w, s.routes())
}

func routeLabel(r config.Route) string {
	if r.Model == "" {
		return "(none)"
	}
	if r.Effort == "" {
		return r.Model
	}
	return r.Model + " @" + r.Effort
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApprovePlan   *bool     `json:"approve_plan"`
		ReviewChanges *bool     `json:"review_changes"`
		Verify        *[]string `json:"verify"`
		Parallel      *bool     `json:"parallel"`
		MaxThreads    *int      `json:"max_threads"`
		Review        *bool     `json:"review"`
		Judge         *bool     `json:"judge"`
		Tiers         *bool     `json:"tiers"`
		Notify        *bool     `json:"notify"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var changed []string
	err := s.store.Update(func(c *config.Config) error {
		oc := &c.Orchestrator
		if v := req.ApprovePlan; v != nil {
			oc.ApprovePlan = *v
			changed = append(changed, fmt.Sprintf("approve plan %s", onOff(*v)))
		}
		if v := req.ReviewChanges; v != nil {
			oc.ReviewChanges = *v
			changed = append(changed, fmt.Sprintf("review changes %s", onOff(*v)))
		}
		if v := req.Verify; v != nil {
			var cmds []string
			for _, c := range *v {
				if c = strings.TrimSpace(c); c != "" {
					cmds = append(cmds, c)
				}
			}
			c.Verify.Commands = cmds
			changed = append(changed, fmt.Sprintf("%d verify command(s)", len(cmds)))
		}
		if v := req.Parallel; v != nil {
			oc.Parallel = *v
			changed = append(changed, fmt.Sprintf("parallel %s", onOff(*v)))
		}
		if v := req.MaxThreads; v != nil {
			if *v < 1 || *v > 64 {
				return errors.New("threads must be 1-64")
			}
			oc.MaxThreads = *v
			changed = append(changed, fmt.Sprintf("max threads %d", *v))
		}
		if v := req.Review; v != nil {
			oc.ReviewBeforePlan, oc.ReviewOnRepeatError, oc.ReviewBeforeDone = *v, *v, *v
			changed = append(changed, fmt.Sprintf("reviewer checkpoints %s", onOff(*v)))
		}
		if v := req.Judge; v != nil {
			c.Routing.Judge = *v
			changed = append(changed, fmt.Sprintf("judge %s", onOff(*v)))
		}
		if v := req.Tiers; v != nil {
			c.Routing.Tiers = config.TiersOff
			if *v {
				c.Routing.Tiers = config.TiersAuto
			}
			changed = append(changed, fmt.Sprintf("model tiers %s", onOff(*v)))
		}
		if v := req.Notify; v != nil {
			c.Notify.Enabled = *v
			changed = append(changed, fmt.Sprintf("desktop notifications %s", onOff(*v)))
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("not changed: %w", err))
		return
	}
	if len(changed) > 0 {
		s.markDirty()
		s.notice("info", strings.Join(changed, " · ")+" (next task; save to keep)")
	}
	writeJSON(w, s.snapshot().Settings)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// --- history, resume, queue --------------------------------------------------

type historyRow struct {
	ID          string    `json:"id"`
	Task        string    `json:"task"`
	Status      string    `json:"status"`
	Interrupted bool      `json:"interrupted"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	Summary     string    `json:"summary,omitempty"`
	Cost        string    `json:"cost,omitempty"`
	Steps       int       `json:"steps"`
	Done        int       `json:"done"`
	Dir         string    `json:"dir"`
	Here        bool      `json:"here"`
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	dir := s.opt.Dir
	if r.URL.Query().Get("all") == "1" {
		dir = ""
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 200 {
		n = 30
	}
	rows := []historyRow{}
	for _, t := range orchestrator.History(dir, n) {
		h := historyRow{ID: t.ID, Task: t.Task, Status: t.Status, Interrupted: t.Interrupted(), Created: t.Created, Updated: t.Updated,
			Summary: t.Summary, Cost: t.CostLine, Dir: t.Dir, Here: sameDir(t.Dir, s.opt.Dir)}
		if t.Plan != nil {
			h.Steps = len(t.Plan.Subtasks)
			for _, st := range t.Plan.Subtasks {
				if res, ok := t.Results[st.ID]; ok && res.OK {
					h.Done++
				}
			}
		}
		if h.Interrupted {
			h.Status = "interrupted"
		}
		rows = append(rows, h)
	}
	writeJSON(w, rows)
}

func sameDir(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, `/\`), strings.TrimRight(b, `/\`))
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var t *orchestrator.TaskState
	force := false
	if req.ID == "" {
		s.mu.Lock()
		t = s.interrupted
		s.mu.Unlock()
		if t == nil && !s.opt.Demo {
			t = orchestrator.LastInterrupted(s.opt.Dir)
		}
		if t == nil {
			fail(w, http.StatusNotFound, errors.New("nothing to resume here - the history lists past tasks"))
			return
		}
	} else {
		if !validTaskID(req.ID) {
			fail(w, http.StatusBadRequest, fmt.Errorf("invalid task id %q", req.ID))
			return
		}
		var err error
		if t, err = orchestrator.LoadTask(req.ID); err != nil {
			fail(w, http.StatusNotFound, err)
			return
		}
		switch {
		case t.Status == "running" && !t.Interrupted():
			fail(w, http.StatusConflict, fmt.Errorf("task %s is still running in another sy", t.ID))
			return
		case t.Status != "running":
			force = true // failed, cancelled or done: run the steps that did not succeed
		}
	}
	res, err := s.startJob(&job{text: t.Task, resume: t, force: force})
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	if t.Dir != "" && !sameDir(t.Dir, s.opt.Dir) {
		s.notice("warn", fmt.Sprintf("note: task %s ran in %s, this sy works in %s", t.ID, t.Dir, s.opt.Dir))
	}
	writeJSON(w, res)
}

func (s *Server) handleQueueRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	var gone *job
	for i, j := range s.queue {
		if j.ID == req.ID {
			gone = j
			s.queue = append(s.queue[:i:i], s.queue[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if gone == nil {
		fail(w, http.StatusNotFound, errors.New("not in the queue any more"))
		return
	}
	s.kick()
	writeJSON(w, map[string]string{"message": "removed from the queue: " + oneLine(gone.label(), 80)})
}

// --- stats, sessions, limits -----------------------------------------------

type statsView struct {
	Stats       sessionlog.Stats        `json:"stats"`
	Text        string                  `json:"text"`
	Tasks       int                     `json:"tasks"`
	MinTasks    int                     `json:"min_tasks"`
	Suggestions []sessionlog.Suggestion `json:"suggestions"`
	LogDir      string                  `json:"log_dir"`
	Totals      map[string]int64        `json:"totals"`
	USD         float64                 `json:"usd"`
}

// minTuneTasks matches `sy tune`: below it suggestions are hints.
const minTuneTasks = 10

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	dir := cfg.SessionDir()
	recs, err := sessionlog.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	var f sessionlog.Filter
	q := r.URL.Query()
	if q.Get("here") == "1" {
		f.Cwd = s.opt.Dir
	}
	if since := q.Get("since"); since != "" && since != "all" {
		d, err := parseSince(since)
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		f.Since = time.Now().Add(-d)
	}
	st := sessionlog.Aggregate(recs, f)
	st.DayLimitUSD, st.DayLimitTokens = cfg.Budget.DayUSD, cfg.Budget.DayTokens
	var b strings.Builder
	st.Print(&b)
	v := statsView{Stats: st, Text: b.String(), MinTasks: minTuneTasks, LogDir: dir, Totals: map[string]int64{},
		Suggestions: sessionlog.Suggest(recs, f)}
	if v.Suggestions == nil {
		v.Suggestions = []sessionlog.Suggestion{}
	}
	for _, m := range st.Modes {
		v.Tasks += m.Tasks
	}
	for _, d := range st.Days {
		for p, n := range d.Providers {
			v.Totals[p] += n
		}
		v.USD += d.USD
	}
	writeJSON(w, v)
}

func parseSince(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("since %q: want e.g. 7d or 24h", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("since %q: want e.g. 7d or 24h", s)
	}
	return d, nil
}

type sessionRow struct {
	Agent    string    `json:"agent"`
	Role     string    `json:"role"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Title    string    `json:"title"`
	Ended    time.Time `json:"ended"`
	Running  bool      `json:"running"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	rows := []sessionRow{}
	running := s.orc.RunningAgents()
	seen := map[string]bool{}
	for _, id := range running {
		if id == orchestrator.AgentMain || id == orchestrator.AgentJudge {
			continue
		}
		seen[id] = true
		rows = append(rows, sessionRow{Agent: id, Running: true})
	}
	for _, ss := range s.orc.Sessions() {
		if seen[ss.AgentID] {
			continue
		}
		rows = append(rows, sessionRow{Agent: ss.AgentID, Role: ss.Role, Provider: ss.Provider, Model: ss.Model, Title: ss.Title, Ended: ss.Ended})
	}
	writeJSON(w, rows)
}

func (s *Server) handleLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Action   string `json:"action"` // reset or set
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.store.Get().IsProvider(req.Provider) {
		fail(w, http.StatusBadRequest, fmt.Errorf("provider must be one of %s", strings.Join(s.store.Get().ProviderNames(), ", ")))
		return
	}
	tr := s.orc.Tracker()
	var msg string
	if req.Action == "set" {
		until := time.Now().Add(s.store.Get().Providers[req.Provider].LimitCooldown.D())
		tr.MarkLimited(req.Provider, until)
		msg = fmt.Sprintf("%s marked at its limit until %s", req.Provider, until.Format("15:04"))
	} else {
		tr.Clear(req.Provider)
		msg = req.Provider + " marked available"
	}
	s.notice("info", msg)
	s.kick()
	writeJSON(w, map[string]string{"message": msg})
}
