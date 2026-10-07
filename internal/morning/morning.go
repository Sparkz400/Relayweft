// Package morning summarizes the unattended work since a time: the queued,
// scheduled and task-file runs of a night, what they used, which limits
// they hit and what needs you now. `rw morning` prints it, rw web shows it
// (the Overnight panel) and notify.morning posts it once a day.
package morning

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// Task is one task in the summary.
type Task struct {
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	Project    string    `json:"project"` // the folder's name
	Dir        string    `json:"dir"`
	Status     string    `json:"status"` // done, failed, cancelled, interrupted, running
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended,omitzero"` // zero while it runs
	Summary    string    `json:"summary,omitempty"`
	Cost       string    `json:"cost,omitempty"`
	UndoKey    string    `json:"undo_key,omitempty"`
	Unattended bool      `json:"unattended"`
	Kept       []string  `json:"kept,omitempty"`  // branches a merge conflict left
	Saved      []string  `json:"saved,omitempty"` // branches with half-done edits
}

// Limit is a usage limit a provider hit.
type Limit struct {
	Provider string    `json:"provider"`
	At       time.Time `json:"at"`
	Until    time.Time `json:"until,omitzero"`
}

// Action is something that needs you, with the command that does it.
type Action struct {
	Task    string `json:"task"` // the task's id
	What    string `json:"what"`
	Command string `json:"command,omitempty"`
}

// Summary is the unattended work between Since and Until.
type Summary struct {
	Since       time.Time        `json:"since"`
	Until       time.Time        `json:"until"`
	Tasks       []Task           `json:"tasks"` // oldest first
	Done        int              `json:"done"`
	Failed      int              `json:"failed"`
	Cancelled   int              `json:"cancelled"`
	Interrupted int              `json:"interrupted"`
	Running     int              `json:"running"`
	Tokens      map[string]int64 `json:"tokens"` // fresh tokens per provider
	USD         float64          `json:"usd"`    // API-equivalent
	Limits      []Limit          `json:"limits"`
	Actions     []Action         `json:"actions"`
	All         bool             `json:"all"` // also tasks someone watched
}

// Options select the tasks.
type Options struct {
	Since, Until time.Time
	Dir          string // only this project ("" = all)
	All          bool   // also tasks someone watched, not only unattended ones
	// Interrupted reports whether a task marked running stopped without
	// finishing (default: TaskState.Interrupted, which checks its lock).
	Interrupted func(orchestrator.TaskState) bool
}

// Build summarizes the task states and session log records (any order).
func Build(states []orchestrator.TaskState, recs []sessionlog.Record, o Options) Summary {
	if o.Until.IsZero() {
		o.Until = time.Now()
	}
	if o.Interrupted == nil {
		o.Interrupted = orchestrator.TaskState.Interrupted
	}
	s := Summary{Since: o.Since, Until: o.Until, All: o.All, Tokens: map[string]int64{}, Tasks: []Task{}, Limits: []Limit{}, Actions: []Action{}}
	ids := map[string]bool{}
	for _, st := range states {
		if !o.All && !st.Unattended {
			continue
		}
		if o.Dir != "" && !samePath(st.Dir, o.Dir) {
			continue
		}
		last := st.Updated
		if last.IsZero() {
			last = st.Created
		}
		// It ran in the window: started before its end, and was still going
		// (or ended) after its start.
		if !st.Created.Before(o.Until) || last.Before(o.Since) {
			continue
		}
		t := Task{ID: st.ID, Text: firstLine(st.Task), Dir: st.Dir, Project: filepath.Base(st.Dir), Status: st.Status, Started: st.Created,
			Summary: st.Summary, Cost: st.CostLine, UndoKey: st.UndoKey, Unattended: st.Unattended, Kept: st.Kept}
		if st.Status != "running" {
			t.Ended = last
		} else if o.Interrupted(st) {
			t.Status = "interrupted"
		}
		for _, sv := range st.UnfinishedSaved() {
			t.Saved = append(t.Saved, sv.Branch)
		}
		s.Tasks = append(s.Tasks, t)
		ids[st.ID] = true
	}
	sort.SliceStable(s.Tasks, func(i, j int) bool { return s.Tasks[i].Started.Before(s.Tasks[j].Started) })
	for _, t := range s.Tasks {
		switch t.Status {
		case "done":
			s.Done++
		case "failed":
			s.Failed++
			s.Actions = append(s.Actions, Action{Task: t.ID, What: "failed: " + clip(t.Text, 80), Command: "rw report " + t.ID})
		case "cancelled":
			s.Cancelled++
		case "interrupted":
			s.Interrupted++
			s.Actions = append(s.Actions, Action{Task: t.ID, What: "interrupted: " + clip(t.Text, 80), Command: "rw resume " + t.ID})
		case "running":
			s.Running++
		}
		for _, b := range t.Kept {
			s.Actions = append(s.Actions, Action{Task: t.ID, What: "a merge conflict was kept on branch " + b + " (" + clip(t.Text, 60) + ")", Command: "git diff HEAD..." + b})
		}
		if len(t.Saved) > 0 && t.Status != "interrupted" {
			s.Actions = append(s.Actions, Action{Task: t.ID, What: fmt.Sprintf("%d half-done edit(s) saved on a branch (%s)", len(t.Saved), clip(t.Text, 60)), Command: "rw resume --force " + t.ID})
		}
	}
	for _, r := range recs {
		switch {
		case r.Type == sessionlog.TypeTaskEnd && ids[r.TaskID] && r.Cost != nil:
			for p, u := range r.Cost.PerProvider {
				s.Tokens[p] += u.Total()
			}
			s.USD += r.Cost.CostUSD
		case r.Type == sessionlog.TypeLimit && !sessionlog.IsUnavailable(r) && !r.TS.Before(o.Since) && r.TS.Before(o.Until):
			l := Limit{Provider: r.Provider, At: r.TS}
			if r.Until != nil {
				l.Until = *r.Until
			}
			s.Limits = append(s.Limits, l)
		}
	}
	sort.Slice(s.Limits, func(i, j int) bool { return s.Limits[i].At.Before(s.Limits[j].At) })
	s.Limits = firstPerReset(s.Limits)
	return s
}

// firstPerReset keeps one limit per provider and reset time (every agent
// that ran into the same limit logs it).
func firstPerReset(ls []Limit) []Limit {
	seen := map[string]bool{}
	out := []Limit{}
	for _, l := range ls {
		k := l.Provider + "|" + l.Until.Truncate(time.Minute).String()
		if !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	return out
}

// Empty reports whether no task ran in the window.
func (s Summary) Empty() bool { return len(s.Tasks) == 0 }

// Title is a one-line headline, e.g.
// "overnight: 4 of 6 tasks done, 1 failed - 2 things need you".
func (s Summary) Title() string {
	if s.Empty() {
		if s.All {
			return "no task ran since " + Clock(s.Since, s.Until)
		}
		return "nothing ran unattended since " + Clock(s.Since, s.Until)
	}
	parts := []string{fmt.Sprintf("%d of %d task(s) done", s.Done, len(s.Tasks))}
	for _, c := range []struct {
		n    int
		what string
	}{{s.Failed, "failed"}, {s.Interrupted, "interrupted"}, {s.Cancelled, "cancelled"}, {s.Running, "still running"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	t := strings.Join(parts, ", ")
	if n := len(s.Actions); n > 0 {
		t += fmt.Sprintf(" - %d thing(s) need you", n)
	}
	return t
}

// Lines is the summary as text: the headline, what needs you, each task,
// the usage and the limits hit.
func (s Summary) Lines() []string {
	out := []string{fmt.Sprintf("Since %s: %s", Clock(s.Since, s.Until), s.Title())}
	if s.Empty() {
		return out
	}
	if len(s.Actions) > 0 {
		out = append(out, "", "Needs you:")
		for _, a := range s.Actions {
			l := "  - " + a.What
			if a.Command != "" {
				l += "  →  " + a.Command
			}
			out = append(out, l)
		}
	}
	out = append(out, "", "Tasks:")
	projects := map[string]bool{}
	for _, t := range s.Tasks {
		projects[t.Project] = true
	}
	for _, t := range s.Tasks {
		l := fmt.Sprintf("  %-9s  %-11s ", Clock(t.Started, s.Until), t.Status)
		if len(projects) > 1 {
			l += t.Project + ": "
		}
		l += clip(t.Text, 70)
		var meta []string
		if !t.Ended.IsZero() {
			meta = append(meta, t.Ended.Sub(t.Started).Round(time.Minute).String())
		}
		if t.Status == "done" && t.UndoKey != "" {
			meta = append(meta, "undo: rw undo "+t.UndoKey)
		}
		if len(meta) > 0 {
			l += " · " + strings.Join(meta, " · ")
		}
		out = append(out, l)
		if t.Summary != "" && t.Status != "done" {
			out = append(out, "      "+clip(t.Summary, 140))
		}
	}
	if u := s.Usage(); u != "" {
		out = append(out, "", "Used: "+u)
	}
	if len(s.Limits) > 0 {
		var ls []string
		for _, l := range s.Limits {
			x := l.Provider + " at " + Clock(l.At, s.Until)
			if !l.Until.IsZero() {
				x += " (until " + Clock(l.Until, s.Until) + ")"
			}
			ls = append(ls, x)
		}
		out = append(out, "Limits hit: "+strings.Join(ls, ", "))
	}
	return out
}

// Text is Lines joined.
func (s Summary) Text() string { return strings.Join(s.Lines(), "\n") }

// Usage is e.g. "claude 1.2M · codex 400k fresh tokens · ≈$3.10 API-equivalent".
func (s Summary) Usage() string {
	var ps []string
	for _, p := range event.ProvidersOf(s.Tokens) {
		if n := s.Tokens[p]; n > 0 {
			ps = append(ps, p+" "+event.HumanTokens(n))
		}
	}
	if len(ps) == 0 {
		return ""
	}
	u := strings.Join(ps, " · ") + " fresh tokens"
	if s.USD > 0 {
		u += fmt.Sprintf(" · ≈$%.2f API-equivalent", s.USD)
	}
	return u
}

// ParseSince reads --since: a duration ("12h", "2d") back from now, or a
// clock time ("18:00"), the last one before now.
func ParseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if n, ok := strings.CutSuffix(v, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d > 0 && d <= 90 {
			return now.AddDate(0, 0, -d), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	for _, layout := range []string{"15:04", "3pm", "3:04pm"} {
		if c, err := time.Parse(layout, strings.ToLower(v)); err == nil {
			t := time.Date(now.Year(), now.Month(), now.Day(), c.Hour(), c.Minute(), 0, 0, now.Location())
			if t.After(now) {
				t = t.AddDate(0, 0, -1)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: want a duration (12h, 2d) or a time of day (18:00)", v)
}

// Clock formats t as 15:04, with the weekday when it is not on now's day.
func Clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

func firstLine(task string) string {
	if i := strings.Index(task, "\n\nFollow-up: "); i >= 0 {
		task = task[:i]
	}
	return task
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(sessionlog.StripControl(s)), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func samePath(a, b string) bool {
	return orchestrator.SamePath(a, b)
}
