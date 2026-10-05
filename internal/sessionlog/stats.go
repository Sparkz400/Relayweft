package sessionlog

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
)

// RouteStats aggregates agent runs per provider:model.
type RouteStats struct {
	Key       string
	Provider  string
	Model     string
	Calls     int
	OK        int
	Failed    int
	LimitHits int
	Tokens    event.TokenUsage
	Duration  time.Duration
	Roles     map[string]int
}

// ModeStats aggregates whole tasks per mode (routed vs single baseline).
type ModeStats struct {
	Mode     string
	Tasks    int
	OK       int
	Duration time.Duration
	Tokens   event.TokenUsage
	PerProv  map[string]int64 // tokens per provider
}

// Stats is the result of Aggregate.
type Stats struct {
	Sessions  int
	Since     time.Time
	Routes    []*RouteStats
	Rules     map[string]int
	Fallbacks int
	Modes     []*ModeStats
	Reviews   int
	Approved  int
	Merges    int
	MergeFail int
	Recent    []Record    // newest task_end records (with cost), newest first
	Days      []*DayStats // last 7 local calendar days that had tasks, newest first
	// DayLimitUSD and DayLimitTokens are the daily budget (config budget.day_*;
	// 0 = none), set by the caller to show it in the per-day table.
	DayLimitUSD    float64 `json:"day_limit_usd,omitempty"`
	DayLimitTokens int64   `json:"day_limit_tokens,omitempty"`
	// daysTitle replaces the per-day table's heading (merged exports).
	daysTitle string
}

// DayStats totals one calendar day of tasks, so a day of heavy use (and what
// it would have cost on the API) stands out without reading every task.
type DayStats struct {
	Date   string // YYYY-MM-DD, local time
	Tasks  int
	OK     int
	Codex  int64   // fresh tokens
	Claude int64   // fresh tokens
	USD    float64 // Claude API-equivalent price
	// Providers are the fresh tokens of every provider, codex and claude
	// included.
	Providers map[string]int64
}

// add counts fresh tokens on a provider.
func (d *DayStats) add(p string, n int64) {
	if n == 0 {
		return
	}
	switch p {
	case event.Codex:
		d.Codex += n
	case event.Claude:
		d.Claude += n
	}
	if d.Providers == nil {
		d.Providers = map[string]int64{}
	}
	d.Providers[p] += n
}

// Total is the day's fresh tokens on every provider.
func (d *DayStats) Total() int64 {
	var t int64
	for _, n := range d.Providers {
		t += n
	}
	return t
}

// provColumns are the provider columns of the tables: codex and claude
// always, then every other provider that used tokens.
func (s Stats) provColumns() []string {
	seen := map[string]int64{event.Codex: 0, event.Claude: 0}
	for _, d := range s.Days {
		for p := range d.Providers {
			seen[p] = 0
		}
	}
	for _, r := range s.Recent {
		if r.Cost != nil {
			for p := range r.Cost.PerProvider {
				seen[p] = 0
			}
		}
	}
	for _, m := range s.Modes {
		for p := range m.PerProv {
			seen[p] = 0
		}
	}
	return event.ProvidersOf(seen)
}

// provHead is the column headings for providers, e.g. "CODEX\tCLAUDE".
func provHead(cols []string, suffix string) string {
	out := make([]string, len(cols))
	for i, p := range cols {
		out[i] = strings.ToUpper(p) + suffix
	}
	return strings.Join(out, "\t")
}

// statsDays is how many calendar days the per-day table covers.
const statsDays = 7

// Filter selects records.
type Filter struct {
	Since time.Time
	Cwd   string // only records from this project directory
}

// keep reports whether a record passes the filter. Records without a cwd
// (very old logs) are kept by --here rather than silently dropped.
func (f Filter) keep(r Record) bool {
	if !f.Since.IsZero() && r.TS.Before(f.Since) {
		return false
	}
	if f.Cwd != "" && r.Cwd != "" && !samePath(r.Cwd, f.Cwd) {
		return false
	}
	return true
}

// Aggregate builds statistics from records.
func Aggregate(recs []Record, f Filter) Stats {
	s := Stats{Rules: map[string]int{}}
	routes := map[string]*RouteStats{}
	modes := map[string]*ModeStats{}
	sessions := map[string]bool{}
	taskMode := map[string]string{}
	taskTokens := map[string]map[string]int64{}
	days := map[string]*DayStats{}
	for _, r := range recs {
		if !f.keep(r) {
			continue
		}
		sessions[r.Session] = true
		if s.Since.IsZero() || r.TS.Before(s.Since) {
			s.Since = r.TS
		}
		switch r.Type {
		case TypeDecision:
			s.Rules[r.Rule]++
			if r.Fallback {
				s.Fallbacks++
			}
		case TypeAgentEnd:
			key := r.Provider + ":" + r.Model
			rs := routes[key]
			if rs == nil {
				rs = &RouteStats{Key: key, Provider: r.Provider, Model: r.Model, Roles: map[string]int{}}
				routes[key] = rs
			}
			rs.Calls++
			rs.Roles[r.Role]++
			if r.OK != nil && *r.OK {
				rs.OK++
			} else {
				rs.Failed++
			}
			if r.LimitHit {
				rs.LimitHits++
			}
			if r.Tokens != nil {
				rs.Tokens = rs.Tokens.Add(*r.Tokens)
				tk := r.Session + "/" + r.TaskID
				if taskTokens[tk] == nil {
					taskTokens[tk] = map[string]int64{}
				}
				taskTokens[tk][r.Provider] += r.Tokens.Total()
			}
			rs.Duration += time.Duration(r.DurationMS) * time.Millisecond
		case TypeTask:
			taskMode[r.Session+"/"+r.TaskID] = r.Mode
		case TypeTaskEnd:
			s.Recent = append(s.Recent, r)
			addDay(days, r)
			mode := r.Mode
			if mode == "" {
				mode = taskMode[r.Session+"/"+r.TaskID]
			}
			ms := modes[mode]
			if ms == nil {
				ms = &ModeStats{Mode: mode, PerProv: map[string]int64{}}
				modes[mode] = ms
			}
			ms.Tasks++
			if r.OK != nil && *r.OK {
				ms.OK++
			}
			ms.Duration += time.Duration(r.DurationMS) * time.Millisecond
			if r.Tokens != nil {
				ms.Tokens = ms.Tokens.Add(*r.Tokens)
			}
			for p, n := range taskTokens[r.Session+"/"+r.TaskID] {
				ms.PerProv[p] += n
			}
		case TypeReview:
			s.Reviews++
			if r.OK != nil && *r.OK {
				s.Approved++
			}
		case TypeMerge:
			s.Merges++
			if r.OK == nil || !*r.OK {
				s.MergeFail++
			}
		}
	}
	s.Sessions = len(sessions)
	for _, rs := range routes {
		s.Routes = append(s.Routes, rs)
	}
	sort.Slice(s.Routes, func(i, j int) bool { return s.Routes[i].Tokens.Total() > s.Routes[j].Tokens.Total() })
	for _, m := range modes {
		s.Modes = append(s.Modes, m)
	}
	sort.Slice(s.Modes, func(i, j int) bool { return s.Modes[i].Mode < s.Modes[j].Mode })
	sort.Slice(s.Recent, func(i, j int) bool { return s.Recent[i].TS.After(s.Recent[j].TS) })
	if len(s.Recent) > 10 {
		s.Recent = s.Recent[:10]
	}
	s.Days = lastDays(days, statsDays)
	return s
}

// addDay adds one task_end record to its local calendar day.
func addDay(days map[string]*DayStats, r Record) {
	date := r.TS.Local().Format("2006-01-02")
	d := days[date]
	if d == nil {
		d = &DayStats{Date: date}
		days[date] = d
	}
	d.Tasks++
	if r.OK != nil && *r.OK {
		d.OK++
	}
	if r.Cost != nil {
		for p, u := range r.Cost.PerProvider {
			d.add(p, u.Total())
		}
		d.USD += r.Cost.CostUSD
	}
}

// now is the clock for the per-day window (replaced in tests).
var now = time.Now

// lastDays returns the days with tasks among the last n calendar days
// (today included), newest first. Dates compare as strings because the
// YYYY-MM-DD layout sorts chronologically.
func lastDays(days map[string]*DayStats, n int) []*DayStats {
	first := now().Local().AddDate(0, 0, -(n - 1)).Format("2006-01-02")
	var out []*DayStats
	for _, d := range days {
		if d.Date >= first {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return strings.EqualFold(a, b)
}

// Print writes a human-readable report.
func (s Stats) Print(w io.Writer) {
	if s.Sessions == 0 {
		fmt.Fprintln(w, "No sessions logged yet. Run a task with `rw` or `rw run \"...\"` first.")
		return
	}
	fmt.Fprintf(w, "Relayweft stats - %d session(s) since %s\n\n", s.Sessions, s.Since.Format("2006-01-02 15:04"))

	fmt.Fprintln(w, "Usage per model")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  ROUTE\tCALLS\tOK\tFAIL\tLIMIT\tIN (FRESH)\tCACHED\tOUT\tAVG TIME\tROLES")
	for _, r := range s.Routes {
		avg := time.Duration(0)
		if r.Calls > 0 {
			avg = r.Duration / time.Duration(r.Calls)
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", r.Key, r.Calls, r.OK, r.Failed, r.LimitHits,
			human(r.Tokens.Input-r.Tokens.Cached), human(r.Tokens.Cached), human(r.Tokens.Output), avg.Round(time.Second), roles(r.Roles))
	}
	tw.Flush()

	fmt.Fprintln(w, "\nRouting rules fired")
	keys := make([]string, 0, len(s.Rules))
	for k := range s.Rules {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return s.Rules[keys[i]] > s.Rules[keys[j]] })
	tw = tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s\t%d\n", k, s.Rules[k])
	}
	tw.Flush()
	fmt.Fprintf(w, "  limit fallbacks: %d   reviews: %d (%d approved)   merges: %d (%d failed)\n",
		s.Fallbacks, s.Reviews, s.Approved, s.Merges, s.MergeFail)

	if len(s.Modes) > 0 {
		fmt.Fprintln(w, "\nTasks: Relayweft (routed) vs single-agent baseline (`rw run --single ...`)")
		tw = tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		cols := s.provColumns()
		fmt.Fprintln(tw, "  MODE\tTASKS\tOK\tSUCCESS\tAVG WALL\t"+provHead(cols, " FRESH TOK/TASK"))
		for _, m := range s.Modes {
			rate, avg := 0.0, time.Duration(0)
			per := make([]string, len(cols))
			for i, p := range cols {
				per[i] = human(0)
				if m.Tasks > 0 {
					per[i] = human(m.PerProv[p] / int64(m.Tasks))
				}
			}
			if m.Tasks > 0 {
				rate = float64(m.OK) / float64(m.Tasks) * 100
				avg = m.Duration / time.Duration(m.Tasks)
			}
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%.0f%%\t%s\t%s\n", m.Mode, m.Tasks, m.OK, rate, avg.Round(time.Second), strings.Join(per, "\t"))
		}
		tw.Flush()
	}
	s.printDays(w)
	s.printRecent(w)
}

// printDays lists per-day totals, newest first.
func (s Stats) printDays(w io.Writer) {
	if len(s.Days) == 0 {
		return
	}
	if s.daysTitle != "" {
		fmt.Fprintln(w, "\n"+s.daysTitle)
	} else {
		fmt.Fprintf(w, "\nPer day (last %d days; $ is Claude's API-equivalent price)\n", statsDays)
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	limited := s.DayLimitUSD > 0 || s.DayLimitTokens > 0
	cols := s.provColumns()
	head := "  DATE\tTASKS\tOK\t" + provHead(cols, "") + "\t$"
	if limited {
		head += "\tDAILY BUDGET"
	}
	fmt.Fprintln(tw, head)
	for _, d := range s.Days {
		per := make([]string, len(cols))
		for i, p := range cols {
			per[i] = human(d.Providers[p])
		}
		line := fmt.Sprintf("  %s\t%d\t%d\t%s\t%.2f", d.Date, d.Tasks, d.OK, strings.Join(per, "\t"), d.USD)
		if limited {
			line += "\t" + s.dayBudget(d)
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
}

// dayBudget is a day's use of the daily budget, e.g. "42% of $5.00".
func (s Stats) dayBudget(d *DayStats) string {
	var parts []string
	mark := func(p float64) string {
		if p >= 100 {
			return fmt.Sprintf("%.0f%%!", p)
		}
		return fmt.Sprintf("%.0f%%", p)
	}
	if s.DayLimitUSD > 0 {
		parts = append(parts, fmt.Sprintf("%s of $%.2f", mark(d.USD/s.DayLimitUSD*100), s.DayLimitUSD))
	}
	if s.DayLimitTokens > 0 {
		parts = append(parts, fmt.Sprintf("%s of %s tok", mark(float64(d.Total())/float64(s.DayLimitTokens)*100), human(s.DayLimitTokens)))
	}
	return strings.Join(parts, ", ")
}

// printRecent lists the newest tasks with what each one cost.
func (s Stats) printRecent(w io.Writer) {
	if len(s.Recent) == 0 {
		return
	}
	fmt.Fprintln(w, "\nRecent tasks (fresh tokens; $ is Claude's API-equivalent price, not billed on a subscription)")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	cols := s.provColumns()
	fmt.Fprintln(tw, "  WHEN\tMODE\tOK\tWALL\t"+provHead(cols, "")+"\t$\tTASK")
	for _, r := range s.Recent {
		ok := "yes"
		if r.OK == nil || !*r.OK {
			ok = "no"
		}
		per := make([]string, len(cols))
		var usd float64
		for i, p := range cols {
			var n int64
			if r.Cost != nil {
				n = r.Cost.PerProvider[p].Total()
			}
			per[i] = human(n)
		}
		if r.Cost != nil {
			usd = r.Cost.CostUSD
		}
		task := strings.Join(strings.Fields(r.Task), " ")
		if len(task) > 50 {
			task = task[:50] + "..."
		}
		mode := r.Mode
		if r.Bench != "" {
			mode += " (bench)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%.2f\t%s\n", r.TS.Format("Jan 2 15:04"), mode, ok,
			(time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second), strings.Join(per, "\t"), usd, task)
	}
	tw.Flush()
}

func roles(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s:%d", k, m[k])
	}
	return strings.Join(parts, " ")
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// Human formats a token count (exported for the TUI).
func Human(n int64) string { return human(n) }
