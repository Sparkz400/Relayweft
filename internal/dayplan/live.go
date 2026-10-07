package dayplan

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/schedule"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// LearnDays is how far back the planner reads the session logs.
const LearnDays = 30

// LogMaxAge is how old a Live planner's copy of the session logs may get
// before it reads them again (the TUI and rw web plan every second).
var LogMaxAge = time.Minute

// Live plans the queue of a running rw (rw run --fill, the TUI's and rw
// web's queue with fill on): from the live config, this rw's limit
// tracker and the session logs. It is safe for concurrent use.
type Live struct {
	Cfg     func() *config.Config
	Dir     string          // the project folder: its tasks make the typical task
	Tracker *limits.Tracker // nil: the logs alone
	// ReadLogs returns the records since a time (nil: the config's session
	// folder; tests and demo mode swap it).
	ReadLogs func(since time.Time) []sessionlog.Record

	mu      sync.Mutex
	until   time.Time
	freshAt time.Time
	read    time.Time
	recs    []sessionlog.Record
	hist    History
}

// SetBounds sets --until and --fresh-at (zero = none).
func (l *Live) SetBounds(until, freshAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.until, l.freshAt = until, freshAt
}

// Bounds returns --until and --fresh-at.
func (l *Live) Bounds() (until, freshAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.until, l.freshAt
}

// State is one plan with what it was made from.
type State struct {
	Plan      Plan
	Providers []Provider
	Tasks     []Task
	History   History
	Options   Options
}

// SubscriptionProviders are the enabled providers with a usage window
// (Claude and Codex CLIs), in routing order. A provider kept to the roles
// that name it (a local model) is not planned for.
func SubscriptionProviders(cfg *config.Config) []string {
	var out []string
	for _, p := range cfg.Enabled() {
		k := cfg.Kind(p)
		if (k == event.Claude || k == event.Codex) && !cfg.Providers[p].OnlyPreferred {
			out = append(out, p)
		}
	}
	return out
}

// LimitHits counts the limit hits the tracker saw on the planned
// providers: when it grows during a task, a limit stopped it.
func LimitHits(tr *limits.Tracker, cfg *config.Config) int {
	if tr == nil {
		return 0
	}
	n := 0
	for _, p := range SubscriptionProviders(cfg) {
		n += tr.Snapshot(p).LimitHits
	}
	return n
}

// logs returns the cached records and what was learned from them.
func (l *Live) logs(cfg *config.Config, now time.Time) ([]sessionlog.Record, History, time.Time, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.read.IsZero() || now.Sub(l.read) >= LogMaxAge || now.Before(l.read) {
		since := now.AddDate(0, 0, -LearnDays)
		if l.ReadLogs != nil {
			l.recs = l.ReadLogs(since)
		} else {
			l.recs, _ = sessionlog.ReadDirSince(cfg.SessionDir(), since) // a file another rw holds is skipped
		}
		l.hist = Learn(l.recs, l.Dir)
		l.read = now
	}
	return l.recs, l.hist, l.until, l.freshAt
}

// Plan plans the queue (task texts) as of now.
func (l *Live) Plan(queue []string, now time.Time) (State, error) {
	cfg := l.Cfg()
	recs, h, until, freshAt := l.logs(cfg, now)
	o := Options{Now: now, Until: until, FreshAt: freshAt, Ceiling: cfg.Routing.SwitchAtUtilization}
	o.DayTokens, o.DayUSD = float64(cfg.Budget.DayTokens), cfg.Budget.DayUSD
	dt, du := sessionlog.DayUsage(recs, now)
	o.DayTokensUsed, o.DayUSDUsed = float64(dt), du
	names := SubscriptionProviders(cfg)
	if len(names) == 0 {
		return State{}, fmt.Errorf("no Claude or Codex provider is enabled: nothing to plan for")
	}
	var provs []Provider
	for _, p := range names {
		provs = append(provs, h.Provider(p, l.Tracker, now, o.Ceiling))
	}
	tasks := make([]Task, len(queue))
	for i, q := range queue {
		tasks[i] = h.Task(q)
	}
	return State{Plan: Make(tasks, provs, o), Providers: provs, Tasks: tasks, History: h, Options: o}, nil
}

// Next is what the queue does now.
type Next struct {
	// Task is the queue index to run now, leaning on Lean (-1 = none).
	Task  int
	Lean  router.Lean
	Share float64
	// When nothing runs now: At is when the plan's first task starts (zero
	// = nothing fits), Why says what it waits for or why nothing fits.
	At  time.Time
	Why string
}

// Next reads the plan: run its first task now, or wait.
func (st State) Next(now time.Time) Next {
	if s, ok := st.Plan.First(now); ok {
		return Next{Task: s.Task, Lean: router.Lean{Provider: s.Provider, Strict: s.Strict}, Share: s.Share}
	}
	if len(st.Plan.Slots) > 0 {
		s := st.Plan.Slots[0]
		return Next{Task: -1, At: s.Start, Why: s.Wait}
	}
	why := "nothing is queued"
	if len(st.Plan.Skipped) > 0 {
		why = st.Plan.Skipped[0].Why
	}
	return Next{Task: -1, Why: why}
}

// Lines writes the plan as text lines: the windows as they are, the
// typical task, the slots and resets in time order, and the tasks left
// over. queue are the task texts the plan was made from.
func (st State) Lines(queue []string) []string {
	now := st.Options.Now
	head := fmt.Sprintf("Day plan · %d queued task(s) · now %s", len(queue), schedule.Clock(now, now))
	if !st.Options.Until.IsZero() {
		head += " · until " + schedule.Clock(st.Options.Until, now)
	}
	if !st.Options.FreshAt.IsZero() {
		head += " · full windows at " + schedule.Clock(st.Options.FreshAt, now)
	}
	out := []string{head}
	width := 0
	for _, p := range st.Providers {
		width = max(width, len(p.Name))
	}
	for _, p := range st.Providers {
		state := p.Note
		if state == "" {
			state = "no window reading yet: assumed unused"
		}
		capa := "window size unknown: a task counts as " + pct(DefaultShare)
		if p.Capacity > 0 {
			capa = fmt.Sprintf("a window holds ~%s tokens (%d reading(s))", event.HumanTokens(int64(p.Capacity)), p.CapacitySamples)
		}
		out = append(out, fmt.Sprintf("  %-*s  %s · %s", width, p.Name, state, capa))
	}
	from := "no finished tasks in the logs: a default"
	if h := st.History; h.Tasks > 0 {
		from = fmt.Sprintf("the median of %d task(s)", h.Tasks)
		if h.Repo {
			from += " in this repo"
		}
	}
	out = append(out, fmt.Sprintf("  a task: ~%s tokens, %s (%s)", event.HumanTokens(int64(st.History.TaskTokens)), st.History.TaskDuration, from), "")

	type line struct {
		at   time.Time
		text string
	}
	var lines []line
	for _, r := range st.Plan.Resets {
		lines = append(lines, line{r.At, fmt.Sprintf("  %s  -- %s window resets", schedule.Clock(r.At, now), r.Provider)})
	}
	for _, s := range st.Plan.Slots {
		lines = append(lines, line{s.Start, fmt.Sprintf("  %s  %-*s  %d. %s  · %s", schedule.Clock(s.Start, now), width, s.Provider,
			s.Task+1, oneLine(queue[s.Task], 60), strings.Join(s.Notes(), " · "))})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })
	for _, l := range lines {
		out = append(out, l.text)
	}
	if len(st.Plan.Slots) > 0 {
		out = append(out, fmt.Sprintf("  %s  done (expected) · %d of %d task(s)", schedule.Clock(st.Plan.End, now), len(st.Plan.Slots), len(queue)))
	}
	if len(st.Plan.Skipped) > 0 {
		if len(lines) > 0 {
			out = append(out, "")
		}
		out = append(out, "not in this plan:")
		for _, s := range st.Plan.Skipped {
			out = append(out, fmt.Sprintf("  %d. %s - %s", s.Task+1, oneLine(queue[s.Task], 60), s.Why))
		}
	}
	return out
}

// Write writes Lines to w.
func (st State) Write(w io.Writer, queue []string) {
	for _, l := range st.Lines(queue) {
		fmt.Fprintln(w, l)
	}
}

// Notes describe a slot: its share of the window, whether it opens one,
// whether only its provider has room and what it waits for.
func (s Slot) Notes() []string {
	notes := []string{fmt.Sprintf("~%s of the window", pct(s.Share))}
	if s.Guess {
		notes[0] += " (guess)"
	}
	if s.Opens {
		notes = append(notes, "opens a window")
	}
	if s.Strict {
		notes = append(notes, "only "+s.Provider+" has room")
	}
	if s.Wait != "" {
		notes = append(notes, s.Wait)
	}
	return notes
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(sessionlog.StripControl(s)), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
