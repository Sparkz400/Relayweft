package sessionlog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
)

// The dashboard (`rw web`) is one pass over the session records of a range
// of local calendar days: tasks and cost per day (as `rw stats` counts
// them), results per route and role with what `rw tune` flags, limit hits
// and switches, and the learned routes decisions used. Everything in it is
// counts and the records' own words: the page escapes all text.

// Dashboard is the result of BuildDashboard. New fields only add, so
// clients written for an older rw keep working.
type Dashboard struct {
	From      string      `json:"from"` // first day of the range, YYYY-MM-DD local
	To        string      `json:"to"`   // last day (today)
	Days      []DashDay   `json:"days"`
	Providers []string    `json:"providers"` // codex, claude, then every other provider that used tokens
	Totals    DashTotals  `json:"totals"`
	Routes    []*RouteRow `json:"routes"`
	// Flags names the decision flags of RouteRow.Decisions, in display
	// order (DecisionFlags).
	Flags       []string       `json:"decision_flags"`
	Suggestions []Suggestion   `json:"suggestions"`
	Limits      DashLimits     `json:"limits"`
	Learned     []LearnedEvent `json:"learned_events"` // newest first
}

// DashDay is one local calendar day. Every day of the range is listed,
// oldest first, also those without tasks.
type DashDay struct {
	Date      string           `json:"date"`
	Tasks     int              `json:"tasks"`
	OK        int              `json:"ok"`
	Failed    int              `json:"failed"` // not ok and not cancelled
	Cancelled int              `json:"cancelled"`
	Providers map[string]int64 `json:"providers"` // fresh tokens
	Tokens    int64            `json:"fresh_tokens"`
	USD       float64          `json:"usd"` // Claude's API-equivalent price
	WallMS    int64            `json:"wall_ms"`
}

// DashTotals adds up the range.
type DashTotals struct {
	Tasks     int              `json:"tasks"`
	OK        int              `json:"ok"`
	Failed    int              `json:"failed"`
	Cancelled int              `json:"cancelled"`
	Providers map[string]int64 `json:"providers"`
	Tokens    int64            `json:"fresh_tokens"`
	USD       float64          `json:"usd"`
	WallMS    int64            `json:"wall_ms"`
}

// RouteRow is one role on one provider:model[:effort].
type RouteRow struct {
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	Route    string `json:"route"` // provider:model[:effort]
	// Runs are the agent runs; limit hits are counted apart, as `rw tune`
	// does: they say nothing about the route.
	Runs      int     `json:"runs"`
	OK        int     `json:"ok"`
	Failed    int     `json:"failed"`
	LimitHits int     `json:"limit_hits"`
	Success   float64 `json:"success"` // OK / Runs, 0..1
	Tokens    int64   `json:"fresh_tokens"`
	USD       float64 `json:"usd"`
	WallMS    int64   `json:"wall_ms"`
	AvgTokens int64   `json:"avg_tokens"`
	AvgUSD    float64 `json:"avg_usd"`
	AvgMS     int64   `json:"avg_ms"`
	// Escalated counts steps that hit the same error twice on this route and
	// were moved up (the "error-repeats" rule). A step counts once, on the
	// route of its first escalation, as `rw tune` counts it.
	Escalated int `json:"escalated"`
	// Reviews and Rejected count the final reviews of tasks this route
	// wrote in (worker roles), and how many asked for changes.
	Reviews  int `json:"reviews"`
	Rejected int `json:"rejected"`
	// Decisions counts runs whose routing decision had a flag (judged,
	// fallback, learned, ...), and how many of them failed.
	Decisions map[string]FlagCount `json:"decisions,omitempty"`
	// Suggested are the indexes of the suggestions about this route.
	Suggested []int `json:"suggested,omitempty"`
	// Unavailable counts runs whose CLI was logged out or missing (rw
	// routed around it as at a limit); they are not in LimitHits.
	Unavailable int `json:"unavailable"`
}

// FlagCount is runs with a decision flag and how many failed.
type FlagCount struct {
	Runs   int `json:"runs"`
	Failed int `json:"failed"`
}

// DecisionFlags are what a routing decision can say about a step, counted
// per route on the dashboard. A new decision field is one more entry here
// (the page shows every flag it gets, in this order).
var DecisionFlags = []struct {
	Name string
	Has  func(Record) bool
}{
	{"judged", func(r Record) bool { return r.Judged || r.Rule == "judge" }},
	{"learned", func(r Record) bool { return learnedRe.MatchString(r.Reason) }},
	{"tier", func(r Record) bool { return r.Tier != "" }},
	{"repeat_error", func(r Record) bool { return escalateRe.MatchString(r.Reason) }},
	{"fallback", func(r Record) bool { return r.Fallback && r.Rule != "quota-preempt" }},
	{"preempt", func(r Record) bool { return r.Rule == "quota-preempt" }},
}

// DashLimits is the limit side of the range. Limits belong to the account,
// so this part ignores the repo filter.
type DashLimits struct {
	PerProvider map[string]*LimitCount `json:"per_provider"`
	Hits        []LimitEvent           `json:"hits"`     // newest first
	Switches    []LimitEvent           `json:"switches"` // pre-emptive switches and fallbacks, newest first
	// Unavailable are the times a provider's CLI was logged out or missing,
	// newest first: rw routed around it as at a limit, but it was none.
	Unavailable []LimitEvent `json:"unavailable"`
	// Quota is the newest provider-reported reading per provider.
	Quota map[string]QuotaReading `json:"quota"`
}

// LimitCount totals one provider's limit events.
type LimitCount struct {
	Hits      int `json:"hits"`
	Preempts  int `json:"preempts"`  // steps moved away before the limit
	Fallbacks int `json:"fallbacks"` // steps moved away after a limit hit (or an unavailable CLI)
	// Unavailable counts the times its CLI was logged out or missing.
	Unavailable int `json:"unavailable"`
}

// LimitEvent is a limit hit (Provider hit it) or a switch (work moved from
// Provider to To).
type LimitEvent struct {
	TS       time.Time  `json:"ts"`
	Provider string     `json:"provider"`
	To       string     `json:"to,omitempty"`
	Model    string     `json:"model,omitempty"`
	Role     string     `json:"role,omitempty"`
	Rule     string     `json:"rule,omitempty"`
	Until    *time.Time `json:"until,omitempty"`
	Text     string     `json:"text,omitempty"` // the CLI's message, shortened
	// Unavailable marks a fallback away from a CLI that was logged out or
	// missing (the last limit record of that provider in that session).
	Unavailable bool `json:"unavailable,omitempty"`
}

// QuotaReading is a logged quota record.
type QuotaReading struct {
	TS    time.Time       `json:"ts"`
	Quota event.QuotaInfo `json:"quota"`
}

// LearnedEvent is a learned route taking over a role, as decisions show it
// ("learned route <spec>: <why>"): when it was first used, until when, and
// the evidence it was learned on.
type LearnedEvent struct {
	TS   time.Time `json:"ts"`
	Last time.Time `json:"last"`
	Role string    `json:"role"`
	From string    `json:"from,omitempty"` // the learned route used before ("" = the configured one)
	To   string    `json:"to"`
	Why  string    `json:"why"`
	Runs int       `json:"runs"` // decisions that used it
}

// learnedRe finds the learned route in a decision's reason (router.learned).
var learnedRe = regexp.MustCompile(`; learned route (\S+): ([^;]*)`)

// Dashboard list sizes.
const (
	dashEvents  = 20
	dashLearned = 30
	dashText    = 200
)

// DashboardOptions select the range.
type DashboardOptions struct {
	Days    int    // calendar days, today included (default 7)
	Cwd     string // only tasks and runs of this project folder ("" = all)
	Now     time.Time
	Catalog Catalog // for the suggestions (zero: DefaultCatalog)
}

// DashboardSince is where a range of days starts: local midnight days-1
// days before now.
func DashboardSince(now time.Time, days int) time.Time {
	if days <= 0 {
		days = 7
	}
	return DayStart(now).AddDate(0, 0, -(days - 1))
}

// SuggestSince is where the suggestions of a range of days start: days
// times 24 hours before now, like `rw tune --since <days>d`. It is up to a
// day before DashboardSince, so read from here.
func SuggestSince(now time.Time, days int) time.Time {
	if days <= 0 {
		days = 7
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

// BuildDashboard aggregates the records of the range.
func BuildDashboard(recs []Record, o DashboardOptions) Dashboard {
	if o.Days <= 0 {
		o.Days = 7
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Catalog.Cheap == nil {
		o.Catalog = DefaultCatalog
	}
	since := DashboardSince(o.Now, o.Days)
	f := Filter{Since: since, Cwd: o.Cwd}
	account := Filter{Since: since} // limits are per account, not per repo
	// The suggestions look back N times 24 hours, as `rw tune --since Nd`
	// does; the per-day charts and rows use whole calendar days.
	tune := Filter{Since: SuggestSince(o.Now, o.Days), Cwd: o.Cwd}
	d := Dashboard{From: since.Format("2006-01-02"), To: DayStart(o.Now).Format("2006-01-02"),
		Totals: DashTotals{Providers: map[string]int64{}}, Routes: []*RouteRow{}, Suggestions: []Suggestion{}, Learned: []LearnedEvent{},
		Limits: DashLimits{PerProvider: map[string]*LimitCount{}, Hits: []LimitEvent{}, Switches: []LimitEvent{}, Unavailable: []LimitEvent{}, Quota: map[string]QuotaReading{}}}
	for _, fl := range DecisionFlags {
		d.Flags = append(d.Flags, fl.Name)
	}

	days := map[string]*DayStats{} // what `rw stats` counts per day
	extra := map[string]*DashDay{} // failed, cancelled and wall time
	routes := map[string]*RouteRow{}
	decisions := map[string]Record{}         // session|task|step|attempt -> decision
	escalated := map[string]bool{}           // session|task|step counted already
	wrote := map[string]map[*RouteRow]bool{} // session/task -> worker routes
	type reviews struct{ n, rejected int }
	finals := map[string]*reviews{}
	type use struct {
		ts            time.Time
		role, to, why string
	}
	var learned []use
	limit := func(p string) *LimitCount {
		c := d.Limits.PerProvider[p]
		if c == nil {
			c = &LimitCount{}
			d.Limits.PerProvider[p] = c
		}
		return c
	}
	// Whether each provider's last limit record in a session was an
	// unavailable CLI: the fallbacks after it moved away from that.
	unavailableNow := map[string]bool{}
	key := func(r Record, attempt int) string {
		return fmt.Sprintf("%s|%s|%s|%d", r.Session, r.TaskID, r.Step, attempt)
	}

	var kept []Record // the records `rw tune --since Nd` would read
	for _, r := range recs {
		if tune.keep(r) {
			kept = append(kept, r)
		}
		if account.keep(r) {
			switch {
			case r.Type == TypeLimit && IsUnavailable(r):
				unavailableNow[r.Session+"|"+r.Provider] = true
				limit(r.Provider).Unavailable++
				d.Limits.Unavailable = append(d.Limits.Unavailable, LimitEvent{TS: r.TS, Provider: r.Provider, Model: r.Model, Until: r.Until, Text: clipText(r.Text)})
			case r.Type == TypeLimit:
				unavailableNow[r.Session+"|"+r.Provider] = false
				limit(r.Provider).Hits++
				d.Limits.Hits = append(d.Limits.Hits, LimitEvent{TS: r.TS, Provider: r.Provider, Model: r.Model, Until: r.Until, Text: clipText(r.Text)})
			case r.Type == TypeQuota && r.Quota != nil:
				if q, ok := d.Limits.Quota[r.Provider]; !ok || !r.TS.Before(q.TS) {
					d.Limits.Quota[r.Provider] = QuotaReading{TS: r.TS, Quota: *r.Quota}
				}
			case r.Type == TypeDecision && (r.Rule == "quota-preempt" || r.Rule == "limit-fallback"):
				from := o.Catalog.from(r)
				if r.Rule == "quota-preempt" {
					limit(from).Preempts++
				} else {
					limit(from).Fallbacks++
				}
				d.Limits.Switches = append(d.Limits.Switches, LimitEvent{TS: r.TS, Provider: from, To: r.Provider, Model: r.Model, Role: r.Role, Rule: r.Rule,
					Unavailable: r.Rule == "limit-fallback" && unavailableNow[r.Session+"|"+from]})
			}
		}
		if !f.keep(r) {
			continue
		}
		switch r.Type {
		case TypeTaskEnd:
			addDay(days, r)
			date := r.TS.Local().Format("2006-01-02")
			x := extra[date]
			if x == nil {
				x = &DashDay{}
				extra[date] = x
			}
			switch {
			case r.OK != nil && *r.OK:
			case Cancelled(r):
				x.Cancelled++
			default:
				x.Failed++
			}
			x.WallMS += r.DurationMS
		case TypeDecision:
			decisions[key(r, r.Attempt)] = r
			if step := r.Session + "|" + r.TaskID + "|" + r.Step; escalateRe.MatchString(r.Reason) && r.Attempt > 1 && !escalated[step] {
				if prev, ok := decisions[key(r, r.Attempt-1)]; ok {
					escalated[step] = true
					routeRow(routes, prev.Role, prev.Provider, prev.Model, prev.Effort).Escalated++
				}
			}
			if m := learnedRe.FindStringSubmatch(r.Reason); m != nil {
				learned = append(learned, use{r.TS, r.Role, m[1], strings.TrimSpace(m[2])})
			}
		case TypeAgentEnd:
			if r.Role == "" {
				continue
			}
			row := routeRow(routes, r.Role, r.Provider, r.Model, r.Effort)
			if r.LimitHit {
				if IsUnavailable(r) {
					row.Unavailable++
				} else {
					row.LimitHits++
				}
				continue
			}
			bad := failed(r)
			row.Runs++
			if bad {
				row.Failed++
			} else {
				row.OK++
			}
			if r.Tokens != nil {
				row.Tokens += r.Tokens.Total()
				row.USD += r.Tokens.CostUSD
			}
			row.WallMS += r.DurationMS
			if dec, ok := decisions[key(r, r.Attempt)]; ok {
				for _, fl := range DecisionFlags {
					if fl.Has(dec) {
						if row.Decisions == nil {
							row.Decisions = map[string]FlagCount{}
						}
						c := row.Decisions[fl.Name]
						c.Runs++
						if bad {
							c.Failed++
						}
						row.Decisions[fl.Name] = c
					}
				}
			}
			if r.Role == event.RoleWorker || r.Role == event.RoleWorkerHigh {
				tk := r.Session + "/" + r.TaskID
				if wrote[tk] == nil {
					wrote[tk] = map[*RouteRow]bool{}
				}
				wrote[tk][row] = true
			}
		case TypeReview:
			if r.Step != "final" {
				continue
			}
			tk := r.Session + "/" + r.TaskID
			if finals[tk] == nil {
				finals[tk] = &reviews{}
			}
			finals[tk].n++
			if failed(r) {
				finals[tk].rejected++
			}
		}
	}

	// Every day of the range, oldest first.
	seen := map[string]int64{event.Codex: 0, event.Claude: 0}
	for day := since; !day.After(o.Now); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		dd := DashDay{Date: date, Providers: map[string]int64{}}
		if s := days[date]; s != nil {
			dd.Tasks, dd.OK, dd.USD = s.Tasks, s.OK, s.USD
			for p, n := range s.Providers {
				dd.Providers[p] = n
				dd.Tokens += n
				seen[p] = 0
			}
		}
		if x := extra[date]; x != nil {
			dd.Failed, dd.Cancelled, dd.WallMS = x.Failed, x.Cancelled, x.WallMS
		}
		t := &d.Totals
		t.Tasks += dd.Tasks
		t.OK += dd.OK
		t.Failed += dd.Failed
		t.Cancelled += dd.Cancelled
		t.Tokens += dd.Tokens
		t.USD += dd.USD
		t.WallMS += dd.WallMS
		for p, n := range dd.Providers {
			t.Providers[p] += n
		}
		d.Days = append(d.Days, dd)
	}
	d.Providers = event.ProvidersOf(seen)

	for tk, rows := range wrote {
		if fr := finals[tk]; fr != nil {
			for row := range rows {
				row.Reviews += fr.n
				row.Rejected += fr.rejected
			}
		}
	}
	d.Suggestions = SuggestFor(kept, Filter{}, o.Catalog)
	if d.Suggestions == nil {
		d.Suggestions = []Suggestion{}
	}
	rank := map[string]int{}
	for i, role := range event.Roles {
		rank[role] = i
	}
	for _, row := range routes {
		if row.Runs > 0 {
			row.Success = float64(row.OK) / float64(row.Runs)
			row.AvgTokens = row.Tokens / int64(row.Runs)
			row.AvgUSD = row.USD / float64(row.Runs)
			row.AvgMS = row.WallMS / int64(row.Runs)
		}
		for i, s := range d.Suggestions {
			if s.Role != "" && s.Role == row.Role && (s.Provider == "" || s.Provider == row.Provider) && (s.Model == "" || s.Model == row.Model) {
				row.Suggested = append(row.Suggested, i)
			}
		}
		d.Routes = append(d.Routes, row)
	}
	sort.Slice(d.Routes, func(i, j int) bool {
		a, b := d.Routes[i], d.Routes[j]
		ra, okA := rank[a.Role]
		rb, okB := rank[b.Role]
		switch {
		case okA != okB:
			return okA
		case ra != rb:
			return ra < rb
		case a.Role != b.Role:
			return a.Role < b.Role
		case a.Runs+a.LimitHits+a.Unavailable != b.Runs+b.LimitHits+b.Unavailable:
			return a.Runs+a.LimitHits+a.Unavailable > b.Runs+b.LimitHits+b.Unavailable
		}
		return a.Route < b.Route
	})

	d.Limits.Hits = newest(d.Limits.Hits, dashEvents)
	d.Limits.Switches = newest(d.Limits.Switches, dashEvents)
	d.Limits.Unavailable = newest(d.Limits.Unavailable, dashEvents)

	// Learned routes: a new event when a role's learned route changes.
	sort.SliceStable(learned, func(i, j int) bool { return learned[i].ts.Before(learned[j].ts) })
	cur := map[string]int{} // role -> index of its current event
	for _, u := range learned {
		if i, ok := cur[u.role]; ok && d.Learned[i].To == u.to {
			d.Learned[i].Last = u.ts
			d.Learned[i].Runs++
			d.Learned[i].Why = u.why
			continue
		}
		ev := LearnedEvent{TS: u.ts, Last: u.ts, Role: u.role, To: u.to, Why: u.why, Runs: 1}
		if i, ok := cur[u.role]; ok {
			ev.From = d.Learned[i].To
		}
		cur[u.role] = len(d.Learned)
		d.Learned = append(d.Learned, ev)
	}
	for i, j := 0, len(d.Learned)-1; i < j; i, j = i+1, j-1 {
		d.Learned[i], d.Learned[j] = d.Learned[j], d.Learned[i]
	}
	if len(d.Learned) > dashLearned {
		d.Learned = d.Learned[:dashLearned]
	}
	return d
}

// routeRow returns (creating) the row of a role on a route.
func routeRow(rows map[string]*RouteRow, role, provider, model, effort string) *RouteRow {
	k := role + "|" + provider + "|" + model + "|" + effort
	row := rows[k]
	if row == nil {
		row = &RouteRow{Role: role, Provider: provider, Model: model, Effort: effort, Route: routeSpec(provider, model, effort)}
		rows[k] = row
	}
	return row
}

// Cancelled reports whether a task_end record is of a cancelled task (the
// orchestrator starts its summary with "cancelled:").
func Cancelled(r Record) bool {
	return r.Type == TypeTaskEnd && (r.OK == nil || !*r.OK) && strings.HasPrefix(r.Text, "cancelled:")
}

// newest sorts events newest first and keeps n.
func newest(evs []LimitEvent, n int) []LimitEvent {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS.After(evs[j].TS) })
	if len(evs) > n {
		evs = evs[:n]
	}
	return evs
}

// clipText keeps a CLI message short, on one line and without control
// characters.
func clipText(s string) string {
	s = printable(s)
	if r := []rune(s); len(r) > dashText {
		s = string(r[:dashText]) + "..."
	}
	return s
}
