package sessionlog

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
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
}

// Filter selects records.
type Filter struct {
	Since time.Time
	Cwd   string // only records from this project directory
}

// Aggregate builds statistics from records.
func Aggregate(recs []Record, f Filter) Stats {
	s := Stats{Rules: map[string]int{}}
	routes := map[string]*RouteStats{}
	modes := map[string]*ModeStats{}
	sessions := map[string]bool{}
	taskMode := map[string]string{}
	taskTokens := map[string]map[string]int64{}
	for _, r := range recs {
		if !f.Since.IsZero() && r.TS.Before(f.Since) {
			continue
		}
		if f.Cwd != "" && r.Cwd != "" && !samePath(r.Cwd, f.Cwd) {
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
	return s
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return strings.EqualFold(a, b)
}

// Print writes a human-readable report.
func (s Stats) Print(w io.Writer) {
	if s.Sessions == 0 {
		fmt.Fprintln(w, "No sessions logged yet. Run a task with `sy` or `sy run \"...\"` first.")
		return
	}
	fmt.Fprintf(w, "Switchyard stats - %d session(s) since %s\n\n", s.Sessions, s.Since.Format("2006-01-02 15:04"))

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
		fmt.Fprintln(w, "\nTasks: Switchyard (routed) vs single-agent baseline (`sy run --single ...`)")
		tw = tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  MODE\tTASKS\tOK\tSUCCESS\tAVG WALL\tCODEX FRESH TOK/TASK\tCLAUDE FRESH TOK/TASK")
		for _, m := range s.Modes {
			rate, avg := 0.0, time.Duration(0)
			cx, cl := int64(0), int64(0)
			if m.Tasks > 0 {
				rate = float64(m.OK) / float64(m.Tasks) * 100
				avg = m.Duration / time.Duration(m.Tasks)
				cx = m.PerProv[event.Codex] / int64(m.Tasks)
				cl = m.PerProv[event.Claude] / int64(m.Tasks)
			}
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%.0f%%\t%s\t%s\t%s\n", m.Mode, m.Tasks, m.OK, rate, avg.Round(time.Second), human(cx), human(cl))
		}
		tw.Flush()
	}
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
