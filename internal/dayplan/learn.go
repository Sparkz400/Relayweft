package dayplan

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Defaults for a typical task when the logs have none.
const (
	DefaultTaskTokens   = 150_000
	DefaultTaskDuration = 8 * time.Minute
)

// minRepoTasks is how many finished tasks in a repo make its own typical
// task; with fewer, every repo's tasks count.
const minRepoTasks = 3

// History is what the session logs say: each provider's last window
// reading and window capacity, and the typical task.
type History struct {
	quota map[string]reading // newest quota record per provider
	limit map[string]reading // newest limit record per provider (usage limits only)
	// capacity samples per provider: fresh tokens per full window.
	capacity map[string][]float64

	// The typical finished task (medians) and how many tasks it is from;
	// Repo says whether they are this repo's own.
	TaskTokens   float64
	TaskUSD      float64
	TaskDuration time.Duration
	Tasks        int
	Repo         bool
}

type reading struct {
	q     event.QuotaInfo
	until time.Time
	seen  time.Time
}

// Learn reads the records (any order) for the plan. dir is the project
// folder: its own tasks make the typical task when there are enough.
func Learn(recs []sessionlog.Record, dir string) History {
	recs = slices.Clone(recs)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].TS.Before(recs[j].TS) })
	h := History{quota: map[string]reading{}, limit: map[string]reading{}, capacity: map[string][]float64{}}
	// Capacity: the fresh tokens agents spent on a provider between two
	// readings of the same short window, over how far its use moved.
	type acc struct {
		used   float64
		seen   time.Time
		tokens float64
		ok     bool
	}
	accs := map[string]*acc{}
	var all, own []sessionlog.Record
	for _, r := range recs {
		switch r.Type {
		case sessionlog.TypeQuota:
			if r.Quota == nil || r.Provider == "" {
				continue
			}
			h.quota[r.Provider] = reading{q: *r.Quota, seen: r.TS}
			u, ok := shortUse(*r.Quota)
			a := accs[r.Provider]
			if a == nil {
				a = &acc{}
				accs[r.Provider] = a
			}
			if ok && a.ok && u > a.used+0.02 && r.TS.Sub(a.seen) < WindowLen && a.tokens > 0 {
				h.capacity[r.Provider] = append(h.capacity[r.Provider], a.tokens/(u-a.used))
			}
			*a = acc{used: u, seen: r.TS, ok: ok}
		case sessionlog.TypeAgentEnd:
			if a := accs[r.Provider]; a != nil && r.Tokens != nil {
				a.tokens += float64(r.Tokens.Total())
			}
		case sessionlog.TypeLimit:
			if r.Until != nil && !sessionlog.IsUnavailable(r) {
				h.limit[r.Provider] = reading{until: *r.Until, seen: r.TS}
			}
		case sessionlog.TypeTaskEnd:
			if r.Mode == "demo" || (r.Cost == nil && r.Tokens == nil) {
				continue
			}
			all = append(all, r)
			if dir != "" && sameDir(r.Cwd, dir) {
				own = append(own, r)
			}
		}
	}
	tasks := all
	if len(own) >= minRepoTasks {
		tasks, h.Repo = own, true
	}
	h.Tasks = len(tasks)
	var tok, usd, secs []float64
	for _, r := range tasks {
		var t, u float64
		if r.Cost != nil {
			for _, x := range r.Cost.PerProvider {
				t += float64(x.Total())
			}
			u = r.Cost.CostUSD
		} else {
			t, u = float64(r.Tokens.Total()), r.Tokens.CostUSD
		}
		tok = append(tok, t)
		usd = append(usd, u)
		if r.DurationMS > 0 {
			secs = append(secs, float64(r.DurationMS)/1000)
		}
	}
	h.TaskTokens, h.TaskUSD, h.TaskDuration = DefaultTaskTokens, 0, DefaultTaskDuration
	if len(tok) > 0 {
		h.TaskTokens, h.TaskUSD = median(tok), median(usd)
	}
	if len(secs) > 0 {
		h.TaskDuration = time.Duration(median(secs) * float64(time.Second)).Round(time.Second)
	}
	return h
}

// Capacity is a provider's learned window capacity in fresh tokens (the
// median of its samples) and how many samples it is from; 0 = unknown.
func (h History) Capacity(provider string) (float64, int) {
	s := h.capacity[provider]
	if len(s) == 0 {
		return 0, 0
	}
	return median(s), len(s)
}

// Task is a queued task with the typical task's estimate.
func (h History) Task(text string) Task {
	return Task{Text: text, Tokens: h.TaskTokens, USD: h.TaskUSD, Duration: h.TaskDuration}
}

// Provider is a provider's window as the plan starts: the live tracker of
// this rw first (it has the newest reading once a task ran), then the
// logs. ceiling is the plan's (a weekly window above it counts as a limit).
func (h History) Provider(name string, tr *limits.Tracker, now time.Time, ceiling float64) Provider {
	if ceiling <= 0 || ceiling > 1 {
		ceiling = DefaultCeiling
	}
	p := Provider{Name: name}
	p.Capacity, p.CapacitySamples = h.Capacity(name)
	var snap limits.ProviderState
	if tr != nil {
		snap = tr.Snapshot(name)
	}
	q, seen, live := snap.Quota, now, snap.Quota != nil
	if q == nil {
		if r, ok := h.quota[name]; ok {
			q, seen = &r.q, r.seen
		}
	}
	if q != nil {
		applyQuota(&p, *q, seen, now, ceiling)
		if p.Note != "" && !live {
			p.Note += " (read " + clock(seen, now) + ")"
		}
	}
	until := snap.LimitedUntil
	if until.IsZero() {
		until = h.limit[name].until
	}
	if until.After(now) && until.After(p.LimitedUntil) {
		p.LimitedUntil = until
		p.Note = "at its limit until " + clock(until, now)
	}
	return p
}

// applyQuota turns a reading into the window state at now.
func applyQuota(p *Provider, q event.QuotaInfo, seen, now time.Time, ceiling float64) {
	if q.ResetsAt.IsZero() || !q.ResetsAt.After(now) {
		if !q.ResetsAt.IsZero() || now.Sub(seen) >= WindowLen {
			p.Note = "its last window has reset"
			return // that window is over
		}
	}
	if !isShort(q.Window) {
		// The weekly window is the fullest one.
		if q.Utilization >= ceiling {
			p.LimitedUntil = q.ResetsAt
			p.Note = fmt.Sprintf("weekly window %.0f%% used until %s", q.Utilization*100, clock(q.ResetsAt, now))
			return
		}
		u, ok := shortUse(q)
		if !ok || now.Sub(seen) >= WindowLen {
			p.Note = fmt.Sprintf("weekly window %.0f%% used", q.Utilization*100)
			return
		}
		// The 5-hour window's reset is not reported here: it is at the
		// latest one window after the reading.
		p.Used, p.ResetsAt = u, seen.Add(WindowLen)
		p.Note = fmt.Sprintf("5h window %.0f%% used, resets by %s", u*100, clock(p.ResetsAt, now))
		return
	}
	p.Used, p.ResetsAt = q.Utilization, q.ResetsAt
	if p.ResetsAt.IsZero() {
		p.ResetsAt = seen.Add(WindowLen)
	}
	p.Note = fmt.Sprintf("5h window %.0f%% used, resets %s", p.Used*100, clock(p.ResetsAt, now))
}

// isShort reports whether a window name is the 5-hour one: Claude's
// five_hour, Codex's 5h (or primary when it gives no length).
func isShort(w string) bool {
	switch w {
	case "", "five_hour", "primary":
		return true
	}
	return strings.HasSuffix(w, "h")
}

// shortUse is the 5-hour window's use in a reading.
func shortUse(q event.QuotaInfo) (float64, bool) {
	for name, u := range q.Windows {
		if isShort(name) {
			return u, true
		}
	}
	if isShort(q.Window) {
		return q.Utilization, true
	}
	return 0, false
}

func sameDir(a, b string) bool {
	clean := func(s string) string { return strings.TrimRight(strings.ToLower(strings.ReplaceAll(s, `\`, "/")), "/") }
	return a != "" && clean(a) == clean(b)
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
