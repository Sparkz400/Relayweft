// Package report builds one self-contained, shareable page per task (`sy
// report`): the task, its plan and results, every routing decision with its
// rule and reason, reviewer verdicts, verify checks, the task's diff and
// its cost. Sources are the task state (orchestrator.LoadTask), the session
// log records of that task and the task's undo snapshots (before -> after).
//
// Everything shown comes from those three sources; nothing from the
// environment or the config is read, so a report carries no tokens or
// environment values (only what the task text, agent answers and the repo
// diff contain).
package report

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// Options tunes Build.
type Options struct {
	SessionDir string // where the session logs are ("" = no routing details)
	Version    string // sy version, shown in the footer
	// Diff limits (0 = defaults).
	MaxFileLines int   // lines shown per file
	MaxDiffLines int   // lines shown in total
	MaxDiffBytes int64 // bytes of `git diff` read
	Now          func() time.Time
}

const (
	defMaxFileLines = 600
	defMaxDiffLines = 5000
	defMaxDiffBytes = 4 << 20
)

// Data is everything a report shows.
type Data struct {
	ID, Task, Status, Mode, Phase, Dir, Summary string
	Created, Updated, Generated                 time.Time
	Duration                                    time.Duration
	Version                                     string

	PlanSummary string
	Steps       []Step
	Routes      []Route
	Reviews     []Review
	Checks      []Check
	Limits      []Limit
	Merges      []Merge

	Cost     event.TaskCost
	HasCost  bool
	CostLine string
	Tokens   event.TokenUsage

	Diff    *Diff
	UndoKey string
	UndoCmd string
	Notes   []string // what could not be found
}

// Step is one planned subtask and its result.
type Step struct {
	ID, Title, Kind, Role, Route string
	DependsOn, Files             []string
	Result                       string // ok, failed, not run
	Final, Err                   string
}

// Route is one routing decision and the run it led to.
type Route struct {
	Agent, Step                   string
	Attempt                       int
	Role, Provider, Model, Effort string
	Rule, Reason                  string
	Confidence                    float64
	Judged, Fallback              bool
	Ran                           bool // an agent_end record exists
	OK, LimitHit                  bool
	Error, Final                  string
	Tokens                        event.TokenUsage
	Duration                      time.Duration
	Files                         []string
}

// Label is provider:model@effort.
func (r Route) Label() string { return routeLabel(r.Provider, r.Model, r.Effort) }

func routeLabel(p, m, e string) string {
	s := p + ":" + m
	if e != "" {
		s += " @" + e
	}
	return s
}

// Review is a reviewer verdict.
type Review struct {
	Checkpoint, Provider, Model string
	Approve                     bool
	Advice                      string
}

// Check is one verify command (or hook) result.
type Check struct {
	Kind     string // verify | hook
	Command  string
	OK       bool
	Duration time.Duration
	Scope    string // verify: full | affected (only the tests the changes affect)
	Why      string // verify: which tests ran and why
}

// Tests says which tests a verify check ran and why ("affected: 3 of 31
// packages, ..."); "" for hooks and older logs.
func (c Check) Tests() string {
	switch {
	case c.Scope == "":
		return ""
	case c.Why == "":
		return c.Scope
	}
	return c.Scope + ": " + c.Why
}

// Limit is a usage-limit hit.
type Limit struct{ Agent, Provider, Model, Text string }

// Merge is one merge of an agent's changes.
type Merge struct {
	Step, Text string
	OK         bool
}

// Build assembles the report data for a task.
func Build(st *orchestrator.TaskState, o Options) *Data {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	d := &Data{
		ID: st.ID, Task: st.Task, Status: st.Status, Mode: st.Mode, Phase: st.Phase, Dir: st.Dir,
		Summary: st.Summary, Created: st.Created, Updated: st.Updated, Generated: now(),
		Version: o.Version, CostLine: st.CostLine, UndoKey: st.UndoKey,
	}
	if st.Status == "running" && st.Interrupted() {
		d.Status = "interrupted"
	}
	recs, parts := taskRecords(o.SessionDir, st)
	if o.SessionDir != "" && parts == 0 {
		d.Notes = append(d.Notes, "no session log found for this task: routing, reviews and checks are missing")
	}
	d.fromRecords(recs)
	if d.Duration == 0 && !st.Updated.IsZero() && st.Updated.After(st.Created) {
		d.Duration = st.Updated.Sub(st.Created)
	}
	d.fromPlan(st)
	if st.UndoKey != "" && st.Dir != "" {
		diff, err := loadDiff(st.Dir, st.UndoKey, o)
		switch {
		case err != nil:
			d.Notes = append(d.Notes, "diff unavailable: "+err.Error())
		default:
			d.Diff = diff
			if diff.Undone {
				d.UndoCmd = "sy undo --redo --dir " + shellQuote(st.Dir) + " " + st.UndoKey
			} else {
				d.UndoCmd = "sy undo --dir " + shellQuote(st.Dir) + " " + st.UndoKey
			}
		}
		// A multi-repo task recorded each extra repo under the same key:
		// its files are listed too, labelled with the repo's name.
		for _, r := range st.Repos {
			rd, err := loadDiff(r.Dir, st.UndoKey, o)
			if err != nil {
				d.Notes = append(d.Notes, "diff of repo "+r.Name+" unavailable: "+err.Error())
				continue
			}
			if d.Diff == nil {
				d.Diff = &Diff{}
			}
			for _, f := range rd.Files {
				f.Path = "[" + r.Name + "] " + f.Path
				d.Diff.Files = append(d.Diff.Files, f)
			}
			d.Diff.Add += rd.Add
			d.Diff.Del += rd.Del
			d.Diff.Hidden += rd.Hidden
			d.Diff.Truncated = d.Diff.Truncated || rd.Truncated
		}
	}
	return d
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'&|<>^()%!;$`") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// partKey identifies one run of the task in one sy session (a resumed task
// has several).
type partKey struct{ session, task string }

// taskRecords returns the log records of the task: its own session's
// records with its task id, plus those of later sessions that resumed it.
func taskRecords(dir string, st *orchestrator.TaskState) ([]sessionlog.Record, int) {
	if dir == "" {
		return nil, 0
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	sort.Strings(files)
	parts := map[partKey]bool{}
	var out []sessionlog.Record
	for _, f := range files {
		session := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		own := strings.HasPrefix(st.ID, session+"-")
		if !own && !mayResume(session, st) {
			continue
		}
		recs := readLog(f)
		if own {
			parts[partKey{session, st.ID[len(session)+1:]}] = true
		} else {
			// A resume logs a new task_start with the same text and dir.
			for _, r := range recs {
				if r.Type == sessionlog.TypeTask && r.Task == st.Task && r.Mode != "followup" && sameDir(r.Cwd, st.Dir) {
					parts[partKey{r.Session, r.TaskID}] = true
				}
			}
		}
		for _, r := range recs {
			if parts[partKey{r.Session, r.TaskID}] {
				out = append(out, r)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, len(parts)
}

// mayResume reports whether a session started while the task was still
// unfinished (session ids start with their start time).
func mayResume(session string, st *orchestrator.TaskState) bool {
	if len(session) < 15 {
		return false
	}
	t, err := time.ParseInLocation("20060102-150405", session[:15], time.Local)
	if err != nil {
		return false
	}
	return t.After(st.Created) && !t.After(st.Updated.Add(time.Minute))
}

func sameDir(a, b string) bool {
	return a != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func readLog(path string) []sessionlog.Record {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []sessionlog.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var r sessionlog.Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (d *Data) fromRecords(recs []sessionlog.Record) {
	type runKey struct {
		session, agent, step string
		attempt              int
	}
	open := map[runKey]int{} // decision without agent_end yet -> index in Routes
	ends := 0
	for _, r := range recs {
		switch r.Type {
		case sessionlog.TypeTask:
			if d.Mode == "" {
				d.Mode = r.Mode
			}
		case sessionlog.TypeDecision:
			d.Routes = append(d.Routes, Route{Agent: r.Agent, Step: r.Step, Attempt: r.Attempt, Role: r.Role,
				Provider: r.Provider, Model: r.Model, Effort: r.Effort, Rule: r.Rule, Reason: r.Reason,
				Confidence: r.Confidence, Judged: r.Judged, Fallback: r.Fallback})
			open[runKey{r.Session, r.Agent, r.Step, r.Attempt}] = len(d.Routes) - 1
		case sessionlog.TypeAgentEnd:
			k := runKey{r.Session, r.Agent, r.Step, r.Attempt}
			i, ok := open[k]
			if !ok {
				d.Routes = append(d.Routes, Route{Agent: r.Agent, Step: r.Step, Attempt: r.Attempt, Role: r.Role,
					Provider: r.Provider, Model: r.Model, Effort: r.Effort})
				i = len(d.Routes) - 1
			}
			delete(open, k)
			rt := &d.Routes[i]
			rt.Ran, rt.OK, rt.LimitHit, rt.Error, rt.Final = true, r.OK != nil && *r.OK, r.LimitHit, r.Error, r.Text
			rt.Duration = time.Duration(r.DurationMS) * time.Millisecond
			rt.Files = r.Files
			if r.Tokens != nil {
				rt.Tokens = *r.Tokens
			}
		case sessionlog.TypeReview:
			d.Reviews = append(d.Reviews, Review{Checkpoint: r.Step, Provider: r.Provider, Model: r.Model,
				Approve: r.OK != nil && *r.OK, Advice: r.Text})
		case "verify", "hook":
			d.Checks = append(d.Checks, Check{Kind: r.Type, Command: r.Text, OK: r.OK != nil && *r.OK,
				Duration: time.Duration(r.DurationMS) * time.Millisecond, Scope: r.Kind, Why: r.Reason})
		case sessionlog.TypeLimit:
			d.Limits = append(d.Limits, Limit{Agent: r.Agent, Provider: r.Provider, Model: r.Model, Text: r.Text})
		case sessionlog.TypeMerge:
			d.Merges = append(d.Merges, Merge{Step: r.Step, Text: r.Text, OK: r.OK == nil || *r.OK})
		case sessionlog.TypeTaskEnd:
			ends++
			d.Duration += time.Duration(r.DurationMS) * time.Millisecond
			if r.Tokens != nil {
				d.Tokens = d.Tokens.Add(*r.Tokens)
			}
			if r.Text != "" {
				d.Summary = r.Text
			}
			if c := r.Cost; c != nil {
				d.HasCost = true
				d.Cost.CostUSD += c.CostUSD
				for p, u := range c.PerProvider {
					if d.Cost.PerProvider == nil {
						d.Cost.PerProvider = map[string]event.TokenUsage{}
					}
					d.Cost.PerProvider[p] = d.Cost.PerProvider[p].Add(u)
				}
				for p, v := range c.QuotaBefore {
					if d.Cost.QuotaBefore == nil {
						d.Cost.QuotaBefore = map[string]float64{}
					}
					if _, seen := d.Cost.QuotaBefore[p]; !seen {
						d.Cost.QuotaBefore[p] = v
					}
				}
				for p, v := range c.QuotaAfter {
					if d.Cost.QuotaAfter == nil {
						d.Cost.QuotaAfter = map[string]float64{}
					}
					d.Cost.QuotaAfter[p] = v
				}
			}
		}
	}
	if d.HasCost {
		d.CostLine = d.Cost.Summary()
	}
}

func (d *Data) fromPlan(st *orchestrator.TaskState) {
	if st.Plan == nil {
		return
	}
	d.PlanSummary = st.Plan.Summary
	for _, s := range st.Plan.Subtasks {
		step := Step{ID: s.ID, Title: s.Title, Kind: string(s.Kind), Role: s.Role, DependsOn: s.DependsOn, Files: s.Files, Result: "not run"}
		// The last routing decision for the step is the one that counted.
		for _, r := range d.Routes {
			if r.Step == s.ID {
				step.Route = r.Label()
				if s.Role == "" {
					step.Role = r.Role
				}
			}
		}
		if res, ok := st.Results[s.ID]; ok {
			step.Result = map[bool]string{true: "ok", false: "failed"}[res.OK]
			step.Final, step.Err = res.Final, res.Err
		}
		d.Steps = append(d.Steps, step)
	}
}

// Providers lists the providers with cost in display order.
func (d *Data) Providers() []string {
	return event.ProvidersOf(d.Cost.PerProvider)
}
