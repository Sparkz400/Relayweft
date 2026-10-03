// Package tui is the Bubble Tea interface: a live, animated agent tree with
// the reviewer, router, session log, prompt and model picker.
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/harmonica"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
)

const (
	tickEvery    = 70 * time.Millisecond
	maxLog       = 2000
	maxNodeLines = 200
	maxDecisions = 12
)

type nodeStatus int

const (
	stQueued nodeStatus = iota
	stRunning
	stOK
	stFailed
	stKilled
)

type node struct {
	id, parent, role, kind, title string
	provider, model               string
	status                        nodeStatus
	last                          string
	lines                         []logLine
	tokens                        event.TokenUsage
	started, ended                time.Time
	attempts                      int
	files                         map[string]bool
	flash                         float64
	merge                         string
	mergeOK                       bool
}

type logLine struct {
	ts    time.Time
	agent string
	prov  string
	kind  event.Kind
	text  string
}

type checkpoint struct {
	name string
	ok   bool
	text string
	at   time.Time
}

type reviewerState struct {
	status      nodeStatus
	provider    string
	model       string
	checkpoints []checkpoint
	calls       int
	tokens      event.TokenUsage
	flash       float64
	last        string
}

type decisionView struct {
	d        event.Decision
	agent    string
	bar, vel float64
	at       time.Time
}

type provView struct {
	until    time.Time
	tokens   event.TokenUsage
	calls    int
	quota    *event.QuotaInfo
	bar, vel float64
}

// pulse travels along the branch line (dispatch, outwards) or the join line
// (return, inwards) of the agent row.
type pulse struct {
	join   bool
	target string // agent id
	t      float64
	color  string
}

type focusArea int

const (
	focusPrompt focusArea = iota
	focusTree
)

// Options configure the TUI.
type Options struct {
	Orc        *orchestrator.Orchestrator
	Events     <-chan event.Event
	Dir        string
	Theme      Theme
	Demo       bool
	DemoTask   string
	SessionLog string
	Version    string
	// Approver, when set (and also passed to the orchestrator), shows plan
	// approval and change review in the TUI.
	Approver *Approver
	// AllowSleep: do not keep the machine awake while scheduled tasks
	// wait or run.
	AllowSleep bool
}

// Model is the Bubble Tea model.
type Model struct {
	opt   Options
	orc   *orchestrator.Orchestrator
	store *config.Store
	th    Theme

	width, height int
	frame         int

	nodes    map[string]*node
	order    []string // subtask agents in display order
	reviewer reviewerState
	judge    *node

	decisions []*decisionView
	provs     map[string]*provView
	pulses    []pulse
	logs      []logLine
	logScroll int
	fullLog   bool

	input    promptBox
	focus    focusArea
	selected int // index into order (tree focus)

	phase      string
	running    bool
	taskText   string
	taskStart  time.Time
	result     string
	resultOK   bool
	cost       string
	cancelTask context.CancelFunc
	cancelling bool
	taskDone   chan struct{}
	quitArmed  time.Time
	killArmed  string
	killAt     time.Time
	notice     string
	noticeAt   time.Time
	dirty      bool // config changed since last save

	picker *picker
	spring harmonica.Spring

	approvals   []*approvalReq // waiting for the person; the first is on screen
	overlay     overlay        // plan approval or change review
	overlayArm  time.Time      // the overlay takes keys from then on
	queue       []job          // tasks typed while one ran, and scheduled ones (schedule.go)
	current     job            // the job running now (when running)
	awake       func()         // releases the keep-awake while scheduled work is pending
	interrupted *orchestrator.TaskState
	complete    struct { // tab completion of @agent ids
		active       bool
		prefix, last string
	}
}

// New builds the model.
func New(o Options) *Model {
	in := newPrompt(o.Theme.ASCII)
	if o.Theme.ASCII {
		ellipsis = "..."
	}
	m := &Model{
		opt: o, orc: o.Orc, store: o.Orc.Store(), th: o.Theme,
		nodes: map[string]*node{}, input: in,
		provs:  map[string]*provView{event.Codex: {}, event.Claude: {}},
		spring: harmonica.NewSpring(harmonica.FPS(int(time.Second/tickEvery)), 6.0, 0.8),
		phase:  "idle",
	}
	m.resetTree()
	return m
}

func (m *Model) resetTree() {
	m.nodes = map[string]*node{orchestrator.AgentMain: {id: orchestrator.AgentMain, role: event.RolePlanner, title: "main agent", status: stQueued}}
	m.order = nil
	m.reviewer = reviewerState{status: stQueued}
	m.judge = nil
	m.decisions = nil
	m.pulses = nil
	m.selected = -1
}

type tickMsg time.Time
type eventsMsg []event.Event

func tick() tea.Cmd { return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) }) }

// waitEvents blocks for one event, then drains whatever else is queued so a
// burst of events costs one render.
func waitEvents(ch <-chan event.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return nil
		}
		batch := []event.Event{e}
		for len(batch) < 256 {
			select {
			case e, ok := <-ch:
				if !ok {
					return eventsMsg(batch)
				}
				batch = append(batch, e)
			default:
				return eventsMsg(batch)
			}
		}
		return eventsMsg(batch)
	}
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tick(), waitEvents(m.opt.Events), textarea.Blink}
	if m.opt.Approver != nil {
		cmds = append(cmds, waitApproval(m.opt.Approver))
	}
	if m.opt.Demo && m.opt.DemoTask != "" {
		task := m.opt.DemoTask
		cmds = append(cmds, func() tea.Msg { return submitMsg(task) })
	}
	m.addLog(logLine{kind: event.Log, text: fmt.Sprintf("Switchyard %s · %s · config %s", m.opt.Version, m.opt.Dir, m.store.Path())})
	if m.opt.SessionLog != "" {
		m.addLog(logLine{kind: event.Log, text: "session log: " + m.opt.SessionLog})
	}
	if m.opt.Demo {
		m.addLog(logLine{kind: event.Log, text: "demo mode: fake agents, no files are touched, nothing is logged"})
	}
	m.checkInterrupted()
	return tea.Batch(cmds...)
}

type submitMsg string

// Shutdown cancels a running task and waits (bounded) for agents to die, so
// no CLI keeps running after sy exits.
func (m *Model) Shutdown() {
	m.queue = nil
	if m.awake != nil {
		m.awake()
		m.awake = nil
	}
	if m.cancelTask != nil {
		m.cancelTask()
	}
	if m.opt.Approver != nil {
		m.opt.Approver.Close()
	}
	if m.taskDone != nil {
		select {
		case <-m.taskDone:
		case <-time.After(8 * time.Second):
		}
	}
}

// cancelRunning cancels the whole task: every running agent's process tree
// is killed and nothing new starts. The task ends with a TaskDone event.
func (m *Model) cancelRunning() {
	switch {
	case !m.running || m.cancelTask == nil:
		m.flashNotice("no task is running")
	case m.cancelling:
		m.flashNotice("already cancelling - waiting for agents to stop...")
	default:
		m.cancelling = true
		m.phase = "cancelling"
		m.cancelTask()
		if n := len(m.queue); n > 0 {
			m.flashNotice(fmt.Sprintf("cancelling: stopping all agents... (%d queued task(s) still run next; /queue clear drops them)", n))
		} else {
			m.flashNotice("cancelling: stopping all agents...")
		}
	}
}

func (m *Model) startSingle(text, provider string, r config.Route) {
	if m.running {
		m.flashNotice("a task is already running")
		return
	}
	m.resetTree()
	m.running = true
	m.taskText = text
	m.taskStart = time.Now()
	m.nodes[orchestrator.AgentMain].title = text
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTask = cancel
	done := make(chan struct{})
	m.taskDone = done
	go func() {
		defer close(done)
		m.orc.RunSingle(ctx, text, provider, r)
	}()
}

func (m *Model) flashNotice(s string) {
	m.notice = s
	m.noticeAt = time.Now()
	m.addLog(logLine{kind: event.Log, text: s})
}

func (m *Model) addLog(l logLine) {
	if l.ts.IsZero() {
		l.ts = time.Now()
	}
	m.logs = append(m.logs, l)
	if len(m.logs) > maxLog {
		m.logs = m.logs[len(m.logs)-maxLog:]
	}
}

func (m *Model) node(id string) *node {
	n, ok := m.nodes[id]
	if !ok {
		n = &node{id: id, status: stQueued, parent: orchestrator.AgentMain}
		m.nodes[id] = n
		if id != orchestrator.AgentMain && id != orchestrator.AgentReviewer && id != orchestrator.AgentJudge {
			m.order = append(m.order, id)
		}
	}
	return n
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if n > 0 && len(r) > n {
		return string(r[:max(0, n-1)]) + "…"
	}
	return s
}

// handleEvent folds one event into the view state.
func (m *Model) handleEvent(e event.Event) {
	switch e.Kind {
	case event.TaskStart:
		m.addLog(logLine{ts: e.Timestamp, kind: e.Kind, text: "task: " + e.Text})
		return
	case event.TaskDone:
		m.running = false
		m.dropApprovals()
		// Agents that never finished (queued behind a cancel, or killed
		// mid-run without a Done) must not keep spinning.
		for _, n := range m.nodes {
			if n.status == stQueued || n.status == stRunning {
				if n.id == orchestrator.AgentMain && e.OK {
					continue
				}
				n.status = stKilled
				if n.ended.IsZero() {
					n.ended = time.Now()
				}
			}
		}
		if m.reviewer.status == stRunning {
			m.reviewer.status = stKilled
		}
		if m.cancelling {
			m.cancelling = false
			m.flashNotice("task cancelled; all agents stopped")
		}
		m.result = e.Text
		m.resultOK = e.OK
		mark := m.th.G.OK
		if !e.OK {
			mark = m.th.G.Fail
		}
		main := m.nodes[orchestrator.AgentMain]
		if e.OK {
			main.status = stOK
		} else if main.status != stKilled {
			main.status = stFailed
		}
		main.ended = time.Now()
		m.addLog(logLine{ts: e.Timestamp, kind: e.Kind, text: fmt.Sprintf("%s task finished in %s: %s", mark, time.Since(m.taskStart).Round(time.Second), e.Text)})
		m.cost = ""
		if e.Cost != nil {
			m.cost = e.Cost.Summary()
			m.addLog(logLine{ts: e.Timestamp, kind: event.Log, text: "cost: " + m.cost})
		}
		if !m.opt.Demo {
			m.addLog(logLine{ts: e.Timestamp, kind: event.Log, text: "not happy with the result? /undo shows what undoing this task would change"})
		}
		m.notifyDone(e.OK, e.Text, time.Since(m.taskStart))
		m.focus = focusPrompt
		m.input.Focus()
		m.startNext()
		return
	case event.Phase:
		m.phase = e.Text
		if e.Text == "review" || e.Text == "fix" {
			for _, id := range m.order {
				m.pulses = append(m.pulses, pulse{join: true, target: id})
			}
		}
		m.addLog(logLine{ts: e.Timestamp, kind: e.Kind, text: "phase " + e.Text})
		return
	case event.Log:
		m.addLog(logLine{ts: e.Timestamp, kind: e.Kind, text: e.Text})
		return
	case event.ProviderState:
		pv := m.provs[e.Provider]
		if pv != nil {
			if e.Until.After(time.Now()) && !e.Until.Equal(pv.until) {
				m.alert("Switchyard: "+e.Provider+" hit its limit", e.Text)
			}
			pv.until = e.Until
		}
		m.addLog(logLine{ts: e.Timestamp, kind: event.LimitHit, prov: e.Provider, text: e.Text})
		return
	case event.Route:
		if e.Decision != nil {
			m.decisions = append(m.decisions, &decisionView{d: *e.Decision, agent: e.AgentID, at: e.Timestamp})
			if len(m.decisions) > maxDecisions {
				m.decisions = m.decisions[len(m.decisions)-maxDecisions:]
			}
			d := e.Decision
			txt := fmt.Sprintf("route %s %s %s [%s %.2f] %s", e.AgentID, m.th.G.Arrow, d.Label(), d.Rule, d.Confidence, d.Reason)
			m.addLog(logLine{ts: e.Timestamp, agent: e.AgentID, prov: d.Provider, kind: e.Kind, text: txt})
		}
		return
	case event.Checkpoint:
		r := &m.reviewer
		r.checkpoints = append(r.checkpoints, checkpoint{name: strings.SplitN(e.Text, ":", 2)[0], ok: e.OK, text: e.Text, at: e.Timestamp})
		r.flash = 1
		verdict := "approved"
		if !e.OK {
			verdict = "changes requested"
		}
		m.addLog(logLine{ts: e.Timestamp, agent: orchestrator.AgentReviewer, prov: e.Provider, kind: e.Kind, text: verdict + " · " + oneLine(e.Text, 0)})
		return
	case event.Merge:
		n := m.node(e.AgentID)
		n.merge, n.mergeOK = e.Text, e.OK
		m.pulses = append(m.pulses, pulse{join: true, target: e.AgentID})
		mark := m.th.G.OK
		if !e.OK {
			mark = m.th.G.Fail
		}
		m.addLog(logLine{ts: e.Timestamp, agent: e.AgentID, kind: e.Kind, text: "merge " + mark + " " + e.Text})
		return
	case event.Quota:
		if pv := m.provs[e.Provider]; pv != nil && e.Quota != nil {
			q := *e.Quota
			pv.quota = &q
		}
		return
	}

	// Agent-level events.
	switch e.AgentID {
	case orchestrator.AgentReviewer:
		m.reviewerEvent(e)
		return
	case orchestrator.AgentJudge:
		if m.judge == nil {
			m.judge = &node{id: orchestrator.AgentJudge, role: event.RoleJudge}
		}
		m.nodeEvent(m.judge, e)
		return
	}
	if e.AgentID == "" {
		return
	}
	n := m.node(e.AgentID)
	if e.Kind == event.AgentQueued && e.Role != "" && n.kind == "" && e.Provider == "" {
		n.kind = e.Role // plan kind (explore/research/edit) before routing
	}
	m.nodeEvent(n, e)
}

func (m *Model) nodeEvent(n *node, e event.Event) {
	switch e.Kind {
	case event.AgentQueued:
		if e.Text != "" && n.id != orchestrator.AgentMain {
			n.title = e.Text
		}
		if e.Provider != "" {
			n.provider, n.model, n.role = e.Provider, e.Model, e.Role
			n.status = stQueued
			n.attempts++
			n.flash = 1
			if n.id != orchestrator.AgentMain {
				m.pulses = append(m.pulses, pulse{target: n.id})
			}
		}
		return
	case event.Started:
		n.status = stRunning
		n.started = e.Timestamp
		n.ended = time.Time{}
		n.provider, n.model = e.Provider, e.Model
		if e.Role != "" {
			n.role = e.Role
		}
	case event.Thinking:
		n.last = e.Text
	case event.ToolCall:
		n.last = e.Text
	case event.FileEdit:
		if n.files == nil {
			n.files = map[string]bool{}
		}
		n.files[e.Text] = true
		n.last = "edit " + e.Text
	case event.Message:
		n.last = e.Text
	case event.Usage:
		n.tokens = n.tokens.Add(e.Tokens)
		if pv := m.provs[e.Provider]; pv != nil {
			pv.tokens = pv.tokens.Add(e.Tokens)
			pv.calls++
		}
	case event.Error:
		n.last = e.Text
	case event.LimitHit:
		n.last = "usage limit: " + e.Text
	case event.Done:
		n.ended = e.Timestamp
		switch {
		case e.OK:
			n.status = stOK
		case strings.Contains(e.Text, "killed"):
			n.status = stKilled
		default:
			n.status = stFailed
		}
		if e.Text != "" {
			n.last = e.Text
		}
		n.flash = 1
	}
	if e.Kind == event.Usage {
		return
	}
	text := e.Text
	if e.Kind == event.Done {
		if strings.TrimSpace(text) == "" {
			text = statusWord(n.status)
		}
		if e.OK {
			text = m.th.G.OK + " " + text
		} else {
			text = m.th.G.Fail + " " + text
		}
	}
	l := logLine{ts: e.Timestamp, agent: n.id, prov: e.Provider, kind: e.Kind, text: text}
	n.lines = append(n.lines, l)
	if len(n.lines) > maxNodeLines {
		n.lines = n.lines[len(n.lines)-maxNodeLines:]
	}
	if e.Kind != event.Thinking || len(strings.TrimSpace(e.Text)) > 0 {
		m.addLog(l)
	}
}

func (m *Model) reviewerEvent(e event.Event) {
	r := &m.reviewer
	switch e.Kind {
	case event.AgentQueued:
		if e.Provider != "" {
			r.provider, r.model = e.Provider, e.Model
			r.status = stQueued
			m.pulses = append(m.pulses, pulse{target: orchestrator.AgentReviewer})
		}
		return
	case event.Started:
		r.status = stRunning
		r.calls++
		r.provider, r.model = e.Provider, e.Model
	case event.Usage:
		r.tokens = r.tokens.Add(e.Tokens)
		if pv := m.provs[e.Provider]; pv != nil {
			pv.tokens = pv.tokens.Add(e.Tokens)
			pv.calls++
		}
		return
	case event.Done:
		if e.OK {
			r.status = stOK
		} else {
			r.status = stFailed
		}
	case event.ToolCall, event.Thinking, event.Message, event.Error, event.LimitHit:
		r.last = e.Text
	}
	if e.Kind == event.Message || e.Kind == event.Thinking {
		return // the checkpoint event carries the verdict
	}
	m.addLog(logLine{ts: e.Timestamp, agent: orchestrator.AgentReviewer, prov: e.Provider, kind: e.Kind, text: e.Text})
}

// animate advances springs, pulses and flashes by one frame.
func (m *Model) animate() {
	m.frame++
	for _, d := range m.decisions {
		d.bar, d.vel = m.spring.Update(d.bar, d.vel, d.d.Confidence)
	}
	now := time.Now()
	for _, p := range event.Providers {
		pv := m.provs[p]
		target := 0.0
		if pv.quota != nil {
			target = pv.quota.Utilization
		} else {
			var total int64
			for _, q := range m.provs {
				total += q.tokens.Total()
			}
			if total > 0 {
				target = float64(pv.tokens.Total()) / float64(total)
			}
		}
		if now.Before(pv.until) {
			target = 1
		}
		pv.bar, pv.vel = m.spring.Update(pv.bar, pv.vel, target)
	}
	alive := m.pulses[:0]
	for _, p := range m.pulses {
		p.t += 0.09
		if p.t < 1 {
			alive = append(alive, p)
		}
	}
	m.pulses = alive
	for _, n := range m.nodes {
		if n.flash > 0 {
			n.flash -= 0.06
		}
	}
	if m.reviewer.flash > 0 {
		m.reviewer.flash -= 0.025
	}
	if m.notice != "" && time.Since(m.noticeAt) > 6*time.Second {
		m.notice = ""
	}
}

func (m *Model) selectedAgent() string {
	if m.focus != focusTree || m.selected < 0 {
		return ""
	}
	ids := m.focusable()
	if m.selected >= len(ids) {
		m.selected = len(ids) - 1
	}
	return ids[m.selected]
}

// focusable lists agent ids in tab order: main, subtasks, reviewer.
func (m *Model) focusable() []string {
	ids := []string{orchestrator.AgentMain}
	ids = append(ids, m.order...)
	return append(ids, orchestrator.AgentReviewer)
}

// Update implements tea.Model. A panic is written to a crash log, then
// re-raised so Bubble Tea restores the terminal before sy exits.
func (m *Model) Update(msg tea.Msg) (_ tea.Model, cmd tea.Cmd) {
	defer func() {
		if r := recover(); r != nil {
			diag.Crash("tui update", r, debug.Stack())
			panic(r)
		}
	}()
	return m.update(msg)
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(max(10, msg.Width-4))
		return m, nil
	case tickMsg:
		m.animate()
		m.pruneApprovals()
		m.tickSchedule()
		return m, tick()
	case approvalMsg:
		if msg.req.ctx.Err() == nil {
			m.onApproval(msg.req)
		}
		return m, waitApproval(m.opt.Approver)
	case eventsMsg:
		for _, e := range msg {
			m.handleEvent(e)
		}
		return m, waitEvents(m.opt.Events)
	case submitMsg:
		m.startTask(string(msg))
		return m, nil
	case undoMsg:
		for _, l := range msg {
			m.addLog(logLine{kind: event.Log, text: l})
		}
		return m, nil
	case submitCheckMsg:
		text, ok := m.input.confirm(msg)
		if !ok {
			return m, nil
		}
		if strings.HasPrefix(text, "/") && !strings.Contains(text, "\n") {
			return m, m.command(text)
		}
		m.submit(text)
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress {
			rv, inReview := m.overlay.(*reviewOverlay)
			switch {
			case inReview && msg.Button == tea.MouseButtonWheelUp:
				rv.wheel(-3)
			case inReview && msg.Button == tea.MouseButtonWheelDown:
				rv.wheel(3)
			case msg.Button == tea.MouseButtonWheelUp:
				m.logScroll += 3
			case msg.Button == tea.MouseButtonWheelDown:
				m.logScroll = max(0, m.logScroll-3)
			}
		}
		return m, nil
	}
	if m.focus == focusPrompt && m.picker == nil && m.overlay == nil {
		return m, m.input.update(msg)
	}
	return m, nil
}

func (m *Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Type == tea.KeyCtrlC {
		return m.tryQuit()
	}
	if m.overlay != nil {
		if k.Type == tea.KeyCtrlX {
			m.cancelRunning()
			return m, nil
		}
		if now := time.Now(); now.Before(m.overlayArm) {
			m.overlayArm = now.Add(overlayGrace)
			m.flashNotice("an approval just opened - keys are ignored until you pause typing")
			return m, nil
		}
		return m, m.overlay.update(m, k)
	}
	if m.picker != nil {
		return m, m.picker.update(m, k)
	}
	switch k.String() {
	case "pgup":
		m.logScroll += 10
		return m, nil
	case "pgdown":
		m.logScroll = max(0, m.logScroll-10)
		return m, nil
	case "ctrl+o":
		m.openPicker()
		return m, nil
	case "ctrl+x": // cancel from anywhere, also while typing
		m.cancelRunning()
		return m, nil
	}
	if m.focus == focusPrompt {
		if k.Type == tea.KeyTab && !m.input.pending && !m.input.inBurst() && m.completeAgent() {
			return m, nil
		}
		if cmd, handled := m.input.key(k); handled {
			return m, cmd
		}
		// tab / esc: move focus to the agents
		m.focus = focusTree
		m.input.Blur()
		return m, nil
	}
	// Tree focus: single-key commands.
	ids := m.focusable()
	switch k.String() {
	case "tab", "right", "down":
		// -1 means "no agent selected": the log shows everything.
		m.selected++
		if m.selected >= len(ids) {
			m.selected = -1
		}
	case "shift+tab", "left", "up":
		m.selected--
		if m.selected < -1 {
			m.selected = len(ids) - 1
		}
	case "enter", "i", "/", "esc":
		m.focus = focusPrompt
		m.input.Focus()
		if k.String() == "/" {
			m.input.SetValue("/")
			m.input.CursorEnd()
		}
		return m, textarea.Blink
	case "l":
		m.fullLog = !m.fullLog
		m.logScroll = 0
	case "p":
		p := !m.orc.Paused()
		m.orc.SetPaused(p)
		if p {
			m.flashNotice("paused: running agents finish, no new agent starts (p to resume)")
		} else {
			m.flashNotice("resumed")
		}
	case "k":
		id := m.selectedAgent()
		switch {
		case id == "":
			m.flashNotice("select an agent with tab first, then press k")
		case m.killArmed != id || time.Since(m.killAt) > 3*time.Second:
			m.killArmed, m.killAt = id, time.Now()
			m.flashNotice("press k again to kill " + id)
		case !m.orc.Kill(id):
			m.killArmed = ""
			m.flashNotice("nothing running on " + id + " to kill")
		default:
			m.killArmed = ""
			m.flashNotice("killed " + id)
		}
	case "x":
		m.cancelRunning()
	case "m":
		m.openPicker()
	case "c":
		if !m.running {
			m.resetTree()
			m.logs = nil
		}
	case "q":
		return m.tryQuit()
	}
	return m, nil
}

func (m *Model) tryQuit() (tea.Model, tea.Cmd) {
	if m.running && time.Since(m.quitArmed) > 3*time.Second {
		m.quitArmed = time.Now()
		m.flashNotice("agents are running - press again to quit (they will be stopped)")
		return m, nil
	}
	if m.dirty {
		m.addLog(logLine{kind: event.Log, text: "note: model changes were not saved (/save)"})
	}
	return m, tea.Quit
}

func shortPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}
