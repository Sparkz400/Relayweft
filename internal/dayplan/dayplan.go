// Package dayplan is the quota-aware day planner. It lays a queue of tasks
// over the subscription providers' usage windows (5 hours from the first
// use), so an unattended night spends every window of both subscriptions
// instead of failing the rest of the queue at the first limit.
//
// The plan is a forecast from what is known: each provider's last window
// reading, its window capacity learned from the session logs (learn.go),
// the typical task, the day budget. `rw dayplan` prints it; `rw run
// --fill` plans again before every task with the live readings and follows
// the first slot: run now on a provider, or wait for a reset.
//
// The rule: spend the quota that expires first. A task goes to the
// provider whose running window resets soonest and still has room; a new
// window opens only when the running ones are full (its 5 hours start with
// that task). With FreshAt, no window opens that would still run then, so
// both subscriptions start the working day with a full window.
package dayplan

import (
	"fmt"
	"time"
)

// WindowLen is how long a subscription usage window lasts once it starts.
const WindowLen = 5 * time.Hour

// DefaultCeiling is the share of a window above which a provider takes no
// new task (routing.switch_at_utilization when that is set).
const DefaultCeiling = 0.9

// DefaultShare is a task's share of a window when the capacity of the
// provider's window is unknown: about ten tasks per window.
const DefaultShare = 0.1

// Provider is one provider's window as the plan starts.
type Provider struct {
	Name string
	// Used is the share of the running window in use (0..1); 0 when no
	// window runs.
	Used float64
	// ResetsAt is when the running window ends; zero means none runs (the
	// next task on this provider starts one).
	ResetsAt time.Time
	// LimitedUntil: the provider is at its limit (or its weekly window is
	// full) until then. Zero = not limited.
	LimitedUntil time.Time
	// Note says where the reading came from ("" = no reading).
	Note string
	// Capacity is how many fresh tokens a full window holds (0 = unknown),
	// learned from CapacitySamples readings.
	Capacity        float64
	CapacitySamples int
}

// Task is one queued task with its estimate.
type Task struct {
	Text     string
	Tokens   float64 // estimated fresh tokens
	USD      float64 // estimated API-equivalent $ (0 = unknown)
	Duration time.Duration
}

// Options shape a plan.
type Options struct {
	Now time.Time
	// Until: no task starts at or after it (zero = no deadline).
	Until time.Time
	// FreshAt: no window opens that would still run at this time, so every
	// provider has a full window then (zero = no such rule).
	FreshAt time.Time
	// Ceiling is the share of a window above which a provider takes no new
	// task (0 = DefaultCeiling).
	Ceiling float64
	// Window is the window length (0 = WindowLen).
	Window time.Duration
	// The day budget (budget.day_tokens, budget.day_usd; 0 = no limit) and
	// what today's finished tasks used. It starts over at local midnight.
	DayTokens, DayTokensUsed float64
	DayUSD, DayUSDUsed       float64
}

// Slot is one task's place in the plan.
type Slot struct {
	Task     int // index into the queue
	Provider string
	Start    time.Time
	// Share is the estimated share of Provider's window the task uses;
	// Guess says the capacity was unknown (Share is DefaultShare).
	Share float64
	Guess bool
	// Opens: the task starts a new window on Provider, which then resets
	// at Start + the window length.
	Opens bool
	// Wait says what the slot waits for ("" = it starts when the task
	// before it ends).
	Wait string
	// Strict: no other provider may take work while it runs (each of them
	// is full, limited or held back for FreshAt), so the task should not
	// send even its review there.
	Strict bool
}

// Skip is a task the plan does not reach, and why.
type Skip struct {
	Task int
	Why  string
}

// Reset is a window that resets inside the plan.
type Reset struct {
	Provider string
	At       time.Time
}

// Plan is the outcome: the slots in order, the tasks left over, the
// resets passed on the way and when the last slot should end.
type Plan struct {
	Slots   []Slot
	Skipped []Skip
	Resets  []Reset
	End     time.Time
}

type pstate struct {
	Provider
	share func(t Task) (float64, bool)
}

// Make plans the queue. provs are the subscription providers in routing
// order (ties go to the earlier one).
func Make(queue []Task, provs []Provider, o Options) Plan {
	if o.Ceiling <= 0 || o.Ceiling > 1 {
		o.Ceiling = DefaultCeiling
	}
	if o.Window <= 0 {
		o.Window = WindowLen
	}
	if !o.FreshAt.IsZero() && (o.Until.IsZero() || o.FreshAt.Before(o.Until)) {
		o.Until = o.FreshAt // work past it would use the windows it keeps full
	}
	var plan Plan
	st := make([]*pstate, len(provs))
	for i, p := range provs {
		s := &pstate{Provider: p}
		if full(s.Used, o.Ceiling) && s.ResetsAt.IsZero() && s.LimitedUntil.IsZero() {
			// Full with no known reset: assume the latest one possible.
			s.ResetsAt = o.Now.Add(o.Window)
		}
		capacity := p.Capacity
		s.share = func(t Task) (float64, bool) {
			if capacity <= 0 {
				return DefaultShare, true
			}
			return t.Tokens / capacity, false
		}
		st[i] = s
	}
	t := o.Now
	day := dayKey(t)
	dayTok, dayUSD := o.DayTokensUsed, o.DayUSDUsed
	// advance moves the clock to `to`, resetting windows on the way.
	advance := func(to time.Time, log bool) {
		for _, s := range st {
			if !s.LimitedUntil.IsZero() && !s.LimitedUntil.After(to) {
				if log {
					plan.Resets = append(plan.Resets, Reset{s.Name, s.LimitedUntil})
				}
				s.LimitedUntil, s.Used, s.ResetsAt = time.Time{}, 0, time.Time{}
			}
			if !s.ResetsAt.IsZero() && !s.ResetsAt.After(to) {
				if log && s.Used > 0 {
					plan.Resets = append(plan.Resets, Reset{s.Name, s.ResetsAt})
				}
				s.Used, s.ResetsAt = 0, time.Time{}
			}
		}
		if k := dayKey(to); k != day {
			day, dayTok, dayUSD = k, 0, 0
		}
		t = to
	}
	advance(t, false) // readings whose window has passed already
	// blocked says why s takes no task at t ("" = it can).
	blocked := func(s *pstate) string {
		switch {
		case s.LimitedUntil.After(t):
			return fmt.Sprintf("%s is at its limit until %s", s.Name, clock(s.LimitedUntil, o.Now))
		case full(s.Used, o.Ceiling):
			return fmt.Sprintf("%s's window is %.0f%% used", s.Name, s.Used*100)
		case s.ResetsAt.IsZero() && !o.FreshAt.IsZero() && t.Add(o.Window).After(o.FreshAt):
			return fmt.Sprintf("a new %s window would still run at %s", s.Name, clock(o.FreshAt, o.Now))
		}
		return ""
	}
	budgetWhy := func(task Task) (why string, never bool) {
		switch {
		case o.DayTokens > 0 && task.Tokens > o.DayTokens:
			return fmt.Sprintf("its estimate (%s tokens) is over budget.day_tokens", human(task.Tokens)), true
		case o.DayUSD > 0 && task.USD > o.DayUSD:
			return fmt.Sprintf("its estimate ($%.2f) is over budget.day_usd", task.USD), true
		case o.DayTokens > 0 && dayTok+task.Tokens > o.DayTokens:
			return "today's token budget is spent", false
		case o.DayUSD > 0 && dayUSD+task.USD > o.DayUSD:
			return "today's $ budget is spent", false
		}
		return "", false
	}
	skipRest := func(from int, why string) {
		for j := from; j < len(queue); j++ {
			plan.Skipped = append(plan.Skipped, Skip{j, why})
		}
	}
	for i, task := range queue {
		wait := ""
		for {
			if !o.Until.IsZero() && !t.Before(o.Until) {
				skipRest(i, "it would start after "+clock(o.Until, o.Now))
				plan.End = t
				return plan
			}
			bwhy, never := budgetWhy(task)
			if never {
				plan.Skipped = append(plan.Skipped, Skip{i, bwhy})
				break
			}
			var best *pstate
			var whys []string
			free := 0
			for _, s := range st {
				if why := blocked(s); why != "" {
					whys = append(whys, why)
					continue
				}
				free++
				if best == nil || expiresBefore(s, best, t, o.Window) {
					best = s
				}
			}
			if best != nil && bwhy == "" {
				share, guess := best.share(task)
				sl := Slot{Task: i, Provider: best.Name, Start: t, Share: share, Guess: guess, Wait: wait, Strict: free == 1 && len(st) > 1}
				if best.ResetsAt.IsZero() {
					sl.Opens = true
					best.ResetsAt = t.Add(o.Window)
				}
				best.Used += share
				if full(best.Used, 1) {
					best.LimitedUntil = best.ResetsAt
				}
				dayTok += task.Tokens
				dayUSD += task.USD
				plan.Slots = append(plan.Slots, sl)
				advance(t.Add(task.Duration), true) // windows reset while it runs too
				break
			}
			// Nothing can start now: wait for the next reset (or midnight).
			next, what := time.Time{}, ""
			consider := func(at time.Time, w string) {
				if at.After(t) && (next.IsZero() || at.Before(next)) {
					next, what = at, w
				}
			}
			if best == nil {
				for _, s := range st {
					switch {
					case s.LimitedUntil.After(t):
						consider(s.LimitedUntil, s.Name+"'s limit resets")
					case full(s.Used, o.Ceiling):
						consider(s.ResetsAt, s.Name+"'s window resets")
					}
				}
			} else {
				whys = []string{bwhy}
				next, what = nextMidnight(t), "the day budget starts over at midnight"
			}
			if next.IsZero() {
				skipRest(i, joinWhy(whys)+", and no reset is known before then")
				plan.End = t
				return plan
			}
			if !o.Until.IsZero() && !next.Before(o.Until) {
				skipRest(i, joinWhy(whys)+fmt.Sprintf(" (next: %s at %s, after %s)", what, clock(next, o.Now), clock(o.Until, o.Now)))
				plan.End = t
				return plan
			}
			wait = fmt.Sprintf("waits for %s at %s", what, clock(next, o.Now))
			advance(next, true)
		}
	}
	plan.End = t
	return plan
}

// full reports whether a window's use reached a share (summed shares
// land a hair below it).
func full(used, share float64) bool { return used >= share-1e-9 }

// expiresBefore reports whether a's quota expires before b's: a running
// window resets at its reset time, a new one would run a full window from
// t. Ties go to the less used one, then the earlier provider.
func expiresBefore(a, b *pstate, t time.Time, window time.Duration) bool {
	ea, eb := a.ResetsAt, b.ResetsAt
	if ea.IsZero() {
		ea = t.Add(window)
	}
	if eb.IsZero() {
		eb = t.Add(window)
	}
	if !ea.Equal(eb) {
		return ea.Before(eb)
	}
	return a.Used < b.Used
}

// First is the plan's first slot when it starts now: the task to run next
// and its provider. ok is false when the plan starts with a wait or has no
// slot.
func (p Plan) First(now time.Time) (Slot, bool) {
	if len(p.Slots) == 0 || p.Slots[0].Start.After(now) {
		return Slot{}, false
	}
	return p.Slots[0], true
}

func dayKey(t time.Time) string { return t.Local().Format("2006-01-02") }

func nextMidnight(t time.Time) time.Time {
	l := t.Local()
	return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, l.Location())
}

func joinWhy(whys []string) string {
	switch len(whys) {
	case 0:
		return "no provider can take it"
	case 1:
		return whys[0]
	}
	s := whys[0]
	for _, w := range whys[1 : len(whys)-1] {
		s += ", " + w
	}
	return s + " and " + whys[len(whys)-1]
}

// clock formats t as 15:04, with the weekday when it is not on now's day.
func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if dayKey(t) == dayKey(now) {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

func human(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}
