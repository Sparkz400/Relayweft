// Package web is `rw web` and `rw app`: a local browser UI on the same
// engine as the TUI. One process serves one orchestrator on 127.0.0.1; the
// page is embedded (no build step, no CDN) and talks to a small JSON API,
// with live events over server-sent events.
package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/dayplan"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/notify"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
)

// Options configure a server.
type Options struct {
	Orc *orchestrator.Orchestrator
	// Events is the orchestrator's event channel; the server drains it.
	Events <-chan event.Event
	// Approver must be the one passed to the orchestrator.
	Approver *Approver
	Dir      string
	Demo     bool
	// DemoTask prefills the prompt in demo mode.
	DemoTask   string
	Version    string
	SessionLog string
	// Warn prints a security warning on rw's terminal (a link used twice).
	Warn func(string)
	// AllowSleep: do not keep the machine awake while scheduled tasks
	// wait or run.
	AllowSleep bool
}

// Server is the web UI backend.
type Server struct {
	opt   Options
	orc   *orchestrator.Orchestrator
	store *config.Store
	ap    *Approver
	auth  *auth
	hub   *hub

	addr    string // host:port the server listens on
	httpSrv *http.Server

	mu          sync.Mutex
	running     bool
	cancelling  bool
	cancel      context.CancelFunc
	taskDone    chan struct{}
	current     *job
	taskStart   time.Time
	queue       []*job
	jobSeq      int
	phase       string
	mainProv    string
	dirty       bool
	interrupted *orchestrator.TaskState
	last        *resultView
	awake       func()               // releases the keep-awake while scheduled work is pending
	limitUntil  map[string]time.Time // per provider: the limit last posted to webhooks
	fill        fillState            // the day planner (fill.go)
	dash        dashCache

	stateKick chan struct{}
	stop      chan struct{}
	stopOnce  sync.Once
	pumpDone  chan struct{}
}

type resultView struct {
	OK      bool    `json:"ok"`
	Text    string  `json:"text"`
	Cost    string  `json:"cost,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	Took    string  `json:"took,omitempty"`
}

// New creates a server. It starts draining Events right away.
func New(o Options) (*Server, error) {
	if o.Orc == nil || o.Approver == nil {
		return nil, errors.New("web: Orc and Approver are required")
	}
	s := &Server{
		opt: o, orc: o.Orc, store: o.Orc.Store(), ap: o.Approver, auth: newAuth(), hub: newHub(),
		phase: "idle", stateKick: make(chan struct{}, 1), stop: make(chan struct{}), pumpDone: make(chan struct{}),
	}
	if !o.Demo {
		s.interrupted = orchestrator.LastInterrupted(o.Dir)
	}
	s.ap.setNotify(s.kick, func(r *Request) {
		switch r.Type {
		case "plan":
			s.alert(notify.EventWaiting, "Relayweft needs you", "approve the plan: "+oneLine(r.Task, 120))
		case "budget":
			s.alert(notify.EventWaiting, "Relayweft needs you", "budget reached: "+r.Budget.Text)
		case "conflict":
			s.alert(notify.EventWaiting, "Relayweft needs you", "merge conflict: "+r.Conflict.Text)
		default:
			s.alert(notify.EventWaiting, "Relayweft needs you", "review the changes of "+r.Changes.StepID)
		}
	})
	go s.pump()
	go s.stateLoop()
	go s.scheduleLoop()
	return s, nil
}

// Listen binds 127.0.0.1:port (0 = a free port). The server never listens
// on other interfaces.
func (s *Server) Listen(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	s.Bind(ln)
	return ln, nil
}

// Bind records the address of a listener the caller opened (tests).
func (s *Server) Bind(ln net.Listener) { s.addr = ln.Addr().String() }

// Addr is host:port.
func (s *Server) Addr() string { return s.addr }

// NewLink returns a fresh link to open: a single-use bootstrap in the URL
// fragment (never sent to the server), valid for bootstrapTTL.
func (s *Server) NewLink() string { return "http://" + s.addr + "/#b=" + s.auth.newBootstrap() }

// Serve serves HTTP on ln until Shutdown.
func (s *Server) Serve(ln net.Listener) error {
	if s.addr == "" {
		s.Bind(ln)
	}
	// httpSrv is shared with Shutdown, which may run at once (an editor
	// client that closes its pipe right away).
	s.mu.Lock()
	select {
	case <-s.stop:
		s.mu.Unlock()
		ln.Close()
		return nil
	default:
	}
	s.httpSrv = s.newHTTPServer()
	hs := s.httpSrv
	s.mu.Unlock()
	err := hs.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// newHTTPServer configures timeouts: requests must arrive quickly and idle
// keep-alive connections are closed. The event stream clears its own read
// deadline, and there is no write timeout (the stream is long-lived).
func (s *Server) newHTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

// Shutdown cancels the running task (the queue is dropped), unblocks every
// approval and waits (bounded) for agents to stop, then closes the HTTP
// server.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.queue = nil
	if s.cancel != nil {
		s.cancel()
	}
	done := s.taskDone
	s.mu.Unlock()
	s.ap.Close()
	if done != nil {
		select {
		case <-done:
		case <-time.After(8 * time.Second):
		}
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.hub.closeAll()
	s.mu.Lock()
	hs := s.httpSrv
	s.mu.Unlock()
	if hs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = hs.Shutdown(ctx)
		cancel()
	}
}

// byeGrace is how long `rw app` waits after its page said goodbye: a
// reload reconnects within it, a closed window does not.
const byeGrace = 5 * time.Second

// WaitIdle returns when a page has been connected once and then no page
// has been connected for byeGrace after it said goodbye, or for idle
// without one (rw app: the window was closed), or when ctx ends.
func (s *Server) WaitIdle(ctx context.Context, idle time.Duration) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.hub.gone(time.Now(), idle, byeGrace) {
				return
			}
		}
	}
}

// kick schedules a state broadcast (coalesced).
func (s *Server) kick() {
	select {
	case s.stateKick <- struct{}{}:
	default:
	}
}

// stateLoop broadcasts the state at most every 100ms.
func (s *Server) stateLoop() {
	for {
		select {
		case <-s.stop:
			return
		case <-s.stateKick:
		}
		s.hub.publish(frame("state", s.snapshot()), false, false)
		select {
		case <-s.stop:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// pump drains the orchestrator's events: it folds them into the server's
// state and forwards them to the pages. It never blocks on a page.
func (s *Server) pump() {
	defer close(s.pumpDone)
	for e := range s.opt.Events {
		s.observe(e)
		s.hub.publish(frame("ev", e), true, e.Kind == event.TaskStart)
		switch e.Kind {
		case event.TaskStart, event.TaskDone, event.Phase, event.Usage, event.Quota, event.ProviderState, event.Started, event.Done, event.AgentQueued:
			s.kick()
		}
	}
}

func (s *Server) observe(e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Kind {
	case event.Phase:
		s.phase = e.Text
	case event.TaskStart:
		s.phase = "starting"
		s.mainProv = ""
	case event.TaskDone:
		s.phase = "done"
		s.dash.clear()
		rv := &resultView{OK: e.OK, Text: e.Text}
		if e.Cost != nil {
			rv.Cost, rv.CostUSD = e.Cost.Summary(), e.Cost.CostUSD
		}
		if !s.taskStart.IsZero() {
			took := time.Since(s.taskStart)
			rv.Took = took.Round(time.Second).String()
			if took >= s.store.Get().Notify.MinTask.D() {
				title, ev := "Relayweft: done", notify.EventDone
				if !e.OK {
					title, ev = "Relayweft: failed", notify.EventFailed
				}
				s.alert(ev, title, oneLine(e.Text, 600)+"\n"+rv.Took)
			}
		}
		s.last = rv
	case event.Started:
		if e.AgentID == orchestrator.AgentMain {
			s.mainProv = e.Provider
		}
	case event.ProviderState:
		// An open page shows the limit; the webhook is for when you are away.
		if e.Until.After(time.Now()) && !e.Until.Equal(s.limitUntil[e.Provider]) {
			if s.limitUntil == nil {
				s.limitUntil = map[string]time.Time{}
			}
			s.limitUntil[e.Provider] = e.Until
			s.webhook(notify.EventLimit, "Relayweft: "+e.Provider+" hit its limit", e.Text)
		}
	}
}

// sendNotify is notify.Send; tests replace it.
var sendNotify = notify.Send

// sendWebhooks is notify.Broadcast; tests replace it.
var sendWebhooks = notify.Broadcast

// alert posts to the webhooks and shows a desktop notification.
func (s *Server) alert(ev, title, body string) {
	s.webhook(ev, title, body)
	s.desktopAlert(title, oneLine(body, 200))
}

// webhook posts to the configured webhooks in the background, whether a
// page is open or not: they are for when you are away from the PC. A
// failure shows on the open pages.
func (s *Server) webhook(ev, title, body string) {
	if s.opt.Demo {
		return
	}
	hooks := s.store.Get().Notify.Webhooks
	if !notify.Wanted(hooks, ev) {
		return
	}
	msg := notify.Message{Event: ev, Title: title, Body: body, Source: filepath.Base(s.opt.Dir)}
	post := sendWebhooks
	go func() {
		if err := post(context.Background(), hooks, msg); err != nil {
			s.notice("warn", "notification not sent: "+err.Error())
		}
	}()
}

// desktopAlert shows a desktop notification from the rw process, only
// when no page is open (an open page notifies through the browser).
func (s *Server) desktopAlert(title, body string) {
	if s.opt.Demo || !s.store.Get().Notify.Enabled {
		return
	}
	if n, _, _ := s.hub.connections(); n > 0 {
		return
	}
	send := sendNotify
	go send(title, body) //nolint:errcheck // a desktop notification is best effort
}

// notice shows a message on every page and records it in the log.
func (s *Server) notice(level, text string) {
	s.hub.publish(frame("notice", map[string]string{"level": level, "text": text}), false, false)
	s.hub.publish(frame("ev", event.Event{Kind: event.Log, Text: text}.Stamp()), true, false)
}

// --- state snapshot --------------------------------------------------------

type providerView struct {
	Tokens       event.TokenUsage `json:"tokens"`
	Fresh        int64            `json:"fresh"`
	Calls        int              `json:"calls"`
	LimitHits    int              `json:"limit_hits"`
	LimitedUntil *time.Time       `json:"limited_until,omitempty"`
	Quota        *event.QuotaInfo `json:"quota,omitempty"`
	Share        float64          `json:"share"`
	Disabled     bool             `json:"disabled,omitempty"`
}

type settingsView struct {
	ApprovePlan   bool     `json:"approve_plan"`
	ReviewChanges bool     `json:"review_changes"`
	Verify        []string `json:"verify"`
	Parallel      bool     `json:"parallel"`
	MaxThreads    int      `json:"max_threads"`
	Review        bool     `json:"review"`
	Judge         bool     `json:"judge"`
	Tiers         bool     `json:"tiers"`
	Notify        bool     `json:"notify"`
}

type jobView struct {
	ID    int        `json:"id"`
	Label string     `json:"label"`
	Kind  string     `json:"kind"`
	At    *time.Time `json:"at,omitempty"`   // scheduled start
	Plan  *slotView  `json:"plan,omitempty"` // its place in the day plan (fill on)
}

type stateView struct {
	Version     string                    `json:"version"`
	Dir         string                    `json:"dir"`
	Project     string                    `json:"project"`
	Demo        bool                      `json:"demo"`
	DemoTask    string                    `json:"demo_task,omitempty"`
	ConfigPath  string                    `json:"config_path"`
	SessionLog  string                    `json:"session_log,omitempty"`
	Running     bool                      `json:"running"`
	Cancelling  bool                      `json:"cancelling"`
	Paused      bool                      `json:"paused"`
	Phase       string                    `json:"phase"`
	Task        string                    `json:"task,omitempty"`
	TaskStart   *time.Time                `json:"task_start,omitempty"`
	Queue       []jobView                 `json:"queue"`
	Approvals   []*Request                `json:"approvals"`
	Providers   map[string]providerView   `json:"providers"`
	Settings    settingsView              `json:"settings"`
	Dirty       bool                      `json:"dirty"`
	Agents      []string                  `json:"running_agents"`
	Interrupted *orchestrator.TaskState   `json:"interrupted,omitempty"`
	Last        *resultView               `json:"last,omitempty"`
	Now         time.Time                 `json:"now"`
	Budget      orchestrator.BudgetStatus `json:"budget"`
	Fill        fillView                  `json:"fill"`
}

func (s *Server) snapshot() stateView {
	cfg := s.store.Get()
	tr := s.orc.Tracker()
	provs := map[string]providerView{}
	for _, p := range cfg.ProviderNames() {
		st := tr.Snapshot(p)
		pv := providerView{Tokens: st.Tokens, Fresh: st.Tokens.Total(), Calls: st.Calls, LimitHits: st.LimitHits, Share: tr.Share(p),
			Disabled: cfg.Providers[p].Disabled}
		if st.Limited(time.Now()) {
			u := st.LimitedUntil
			pv.LimitedUntil = &u
		}
		if _, ok := tr.Utilization(p); ok {
			pv.Quota = st.Quota
		}
		provs[p] = pv
	}
	oc := cfg.Orchestrator
	v := stateView{
		Version: s.opt.Version, Dir: s.opt.Dir, Project: orchestrator.WorkspaceLabel(s.opt.Dir, s.orc.Repos()), Demo: s.opt.Demo, DemoTask: s.opt.DemoTask,
		ConfigPath: s.store.Path(), SessionLog: s.opt.SessionLog, Paused: s.orc.Paused(),
		Approvals: s.ap.Pending(), Providers: provs, Agents: s.orc.RunningAgents(), Now: time.Now(),
		Settings: settingsView{
			ApprovePlan: oc.ApprovePlan, ReviewChanges: oc.ReviewChanges, Verify: append([]string{}, cfg.Verify.Commands...),
			Parallel: oc.Parallel, MaxThreads: oc.MaxThreads, Review: oc.ReviewBeforePlan || oc.ReviewBeforeDone || oc.ReviewOnRepeatError,
			Judge: cfg.Routing.Judge, Tiers: cfg.Routing.Tiers == config.TiersAuto, Notify: cfg.Notify.Enabled,
		},
	}
	s.mu.Lock()
	v.Running, v.Cancelling, v.Phase, v.Dirty, v.Interrupted, v.Last = s.running, s.cancelling, s.phase, s.dirty, s.interrupted, s.last
	if s.current != nil {
		v.Task = s.current.label()
		t := s.taskStart
		v.TaskStart = &t
	}
	v.Queue = []jobView{}
	for _, j := range s.queue {
		jv := jobView{ID: j.ID, Label: j.label(), Kind: j.kind()}
		if !j.at.IsZero() {
			at := j.at
			jv.At = &at
		}
		if sl, ok := s.fill.slots[j.ID]; ok && s.fill.on && j.plannable() {
			jv.Plan = &sl
		}
		v.Queue = append(v.Queue, jv)
	}
	v.Fill = s.fillViewLocked()
	s.mu.Unlock()
	v.Budget = s.orc.BudgetStatus()
	return v
}

// --- jobs --------------------------------------------------------------------

// job is something the server runs as a task: a new task, a resumed one, a
// single-agent run or a follow-up to a finished agent (like the TUI).
type job struct {
	ID         int
	text       string
	followUp   bool
	agent      string
	session    orchestrator.AgentSession // resolved when submitted
	resume     *orchestrator.TaskState
	force      bool
	single     *singleRoute
	unattended bool
	at         time.Time // scheduled start (zero = as soon as possible)
	// The day plan (fill.go): the provider it leans on, whether the plan
	// started it, and how often a limit-stopped task was resumed.
	lean    router.Lean
	planned bool
	retries int
}

type singleRoute struct {
	provider string
	route    config.Route
}

func (j *job) label() string {
	switch {
	case j.followUp && j.agent == "":
		return "@ " + j.text
	case j.followUp:
		return "@" + j.agent + " " + j.text
	case j.resume != nil:
		return "resume: " + j.resume.Task
	case j.single != nil:
		return "single " + j.single.provider + ":" + j.single.route.Model + ": " + j.text
	}
	return j.text
}

func (j *job) kind() string {
	switch {
	case j.followUp:
		return "followup"
	case j.resume != nil:
		return "resume"
	case j.single != nil:
		return "single"
	}
	return "task"
}

// parseFollowUp reads "@<agent> message" or "@ message" (the newest
// agent). ok is false when text is not a follow-up at all.
func parseFollowUp(text string) (agent, msg string, ok bool) {
	if !strings.HasPrefix(text, "@") {
		return "", "", false
	}
	rest := text[1:]
	if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' {
		return "", strings.TrimSpace(rest), true
	}
	i := strings.IndexAny(rest, " \t\n")
	if i < 0 {
		if !isAgentID(rest) {
			return "", "", false
		}
		return rest, "", true
	}
	agent = rest[:i]
	if !isAgentID(agent) {
		return "", "", false // "@types/node ..." is a task, not a follow-up
	}
	if agent == "last" {
		agent = ""
	}
	return agent, strings.TrimSpace(rest[i:]), true
}

func isAgentID(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return s != ""
}

// submitResult tells the page what happened to a submission.
type submitResult struct {
	Status  string `json:"status"` // started, queued, told
	Message string `json:"message"`
	JobID   int    `json:"job_id,omitempty"`
}

var errBusy = errors.New("the orchestrator is still busy - try again in a moment")

// submit starts typed text: a message to a running agent, a follow-up to a
// finished one, or a task.
func (s *Server) submit(text string) (submitResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return submitResult{}, errors.New("type a task first")
	}
	if agent, msg, ok := parseFollowUp(text); ok {
		if msg == "" {
			return submitResult{}, errors.New("usage: @<agent> <message>, or @ <message> for the newest agent")
		}
		if agent != "" && contains(s.orc.RunningAgents(), agent) {
			if err := s.orc.Tell(agent, msg); err == nil {
				m := fmt.Sprintf("message for %s queued: delivered when its current turn ends", agent)
				s.notice("info", m)
				return submitResult{Status: "told", Message: m}, nil
			}
		}
		sess, have := s.orc.Session(agent)
		if !have {
			name := agent
			if name == "" {
				name = "last"
			}
			return submitResult{}, fmt.Errorf("no finished agent %q to follow up", name)
		}
		return s.startJob(&job{text: msg, followUp: true, agent: agent, session: sess})
	}
	return s.startJob(&job{text: text})
}

// startJob runs j now, or queues it (unattended) while a task runs.
func (s *Server) startJob(j *job) (submitResult, error) {
	s.mu.Lock()
	s.jobSeq++
	j.ID = s.jobSeq
	if s.fill.on && j.plannable() {
		j.unattended = true
		s.queue = append(s.queue, j)
		s.fill.check = time.Time{}
		n := len(s.queue)
		s.mu.Unlock()
		go s.planQueue(time.Now())
		return submitResult{Status: "queued", JobID: j.ID,
			Message: fmt.Sprintf("queued (%d) for the day plan: runs unattended when a window has room", n)}, nil
	}
	if s.running {
		if j.resume != nil {
			s.mu.Unlock()
			return submitResult{}, errors.New("a task is running - resume once it has finished")
		}
		j.unattended = true
		s.queue = append(s.queue, j)
		n := len(s.queue)
		s.mu.Unlock()
		s.kick()
		return submitResult{Status: "queued", JobID: j.ID,
			Message: fmt.Sprintf("queued (%d): runs unattended (no approvals) after the current task", n)}, nil
	}
	if s.orc.Running() {
		s.mu.Unlock()
		return submitResult{}, errBusy
	}
	s.launchLocked(j)
	s.mu.Unlock()
	s.kick()
	return submitResult{Status: "started", JobID: j.ID, Message: "started: " + oneLine(j.label(), 80)}, nil
}

// launchLocked starts a job (s.mu held).
func (s *Server) launchLocked(j *job) {
	s.running, s.cancelling = true, false
	s.current = j
	s.taskStart = time.Now()
	s.phase = "starting"
	s.last = nil
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	done := make(chan struct{})
	s.taskDone = done
	if j.resume != nil {
		s.interrupted = nil
		s.orc.SetPaused(false) // a resumed task must not start paused
	}
	orc := s.orc
	hits := 0
	if j.planned {
		hits = dayplan.LimitHits(orc.Tracker(), s.store.Get())
	}
	go func() {
		var res orchestrator.TaskResult
		var id string
		defer func() {
			cancelled := ctx.Err() != nil
			cancel()
			close(done)
			s.jobFinished(j, res, id, cancelled, hits)
		}()
		switch {
		case j.followUp:
			orc.FollowUpSession(ctx, j.session, j.text)
		case j.single != nil:
			orc.RunSingle(ctx, j.text, j.single.provider, j.single.route)
		default:
			res = orc.RunWith(ctx, j.text, orchestrator.TaskOptions{Unattended: j.unattended, Resume: j.resume, Force: j.force,
				Lean: j.lean, Started: func(s string) { id = s }})
		}
	}()
}

// jobFinished runs the next queued job, if any (with fill on, the day
// plan picks the next task).
func (s *Server) jobFinished(j *job, res orchestrator.TaskResult, id string, cancelled bool, hits int) {
	s.mu.Lock()
	s.running, s.cancelling = false, false
	s.cancel = nil
	s.current = nil
	note := ""
	if j.planned {
		note = s.fillAfterLocked(j, res, id, cancelled, hits)
	}
	next := s.popDueLocked(time.Now()) // scheduled jobs wait for their time
	left := len(s.queue)
	if next != nil {
		select {
		case <-s.stop:
			next = nil
		default:
			s.launchLocked(next)
		}
	}
	fill := s.fill.on
	s.mu.Unlock()
	if note != "" {
		s.notice("warn", note)
	}
	if next != nil {
		s.notice("info", fmt.Sprintf("starting queued task (%d left): %s", left, oneLine(next.label(), 80)))
	} else if fill {
		s.planQueue(time.Now())
	}
	s.kick()
}

// cancelTask cancels the running task (the queue stays).
func (s *Server) cancelTask() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.running || s.cancel == nil:
		return "", errors.New("no task is running")
	case s.cancelling:
		return "already cancelling - waiting for agents to stop", nil
	}
	s.cancelling = true
	s.phase = "cancelling"
	s.cancel()
	s.kick()
	if n := len(s.queue); n > 0 {
		return fmt.Sprintf("cancelling: stopping all agents (%d queued task(s) still run next)", n), nil
	}
	return "cancelling: stopping all agents", nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if n > 0 && len(r) > n {
		return string(r[:max(0, n-1)]) + "…"
	}
	return s
}
