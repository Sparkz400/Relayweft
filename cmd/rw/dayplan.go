package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/dayplan"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/notify"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/router"
	"github.com/sparkz400/relayweft/internal/schedule"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// dayFlags are the day planner's flags, shared by rw dayplan and rw run
// --fill.
type dayFlags struct {
	until   string
	freshAt string
}

func (d *dayFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&d.until, "until", "", "day plan: start no task at or after this local time (07:00, \"2026-10-07 07:00\")")
	fs.StringVar(&d.freshAt, "fresh-at", "", "day plan: open no usage window that would still run at this time, so every subscription has a full one then (implies --until)")
}

func (d *dayFlags) set() bool { return d.until != "" || d.freshAt != "" }

// options reads the times (a clock time is the next one after now).
func (d *dayFlags) options(now time.Time) (dayplan.Options, error) {
	o := dayplan.Options{Now: now}
	var err error
	if d.until != "" {
		if o.Until, err = schedule.ParseAt(d.until, now); err != nil {
			return o, fmt.Errorf("--until: %w", err)
		}
	}
	if d.freshAt != "" {
		if o.FreshAt, err = schedule.ParseAt(d.freshAt, now); err != nil {
			return o, fmt.Errorf("--fresh-at: %w", err)
		}
	}
	return o, nil
}

// dayPlanner plans the queue from the config, the session logs and (in a
// running rw) the live limit tracker.
type dayPlanner struct {
	cfg   *config.Config
	dir   string
	tr    *limits.Tracker // nil: the logs alone
	flags dayFlags
	// readLogs returns the records the plan learns from (tests swap it).
	readLogs func(since time.Time) []sessionlog.Record
}

func newDayPlanner(cfg *config.Config, dir string, tr *limits.Tracker, flags dayFlags) *dayPlanner {
	return &dayPlanner{cfg: cfg, dir: dir, tr: tr, flags: flags, readLogs: func(since time.Time) []sessionlog.Record {
		recs, _ := sessionlog.ReadDirSince(cfg.SessionDir(), since) // a file another rw holds is skipped
		return recs
	}}
}

// dayPlanLearnDays is how far back the planner reads the logs.
const dayPlanLearnDays = 30

// subscriptionProviders are the enabled providers with a usage window
// (Claude and Codex CLIs), in routing order. A provider kept to the roles
// that name it (a local model) is not planned for.
func subscriptionProviders(cfg *config.Config) []string {
	var out []string
	for _, p := range cfg.Enabled() {
		k := cfg.Kind(p)
		if (k == event.Claude || k == event.Codex) && !cfg.Providers[p].OnlyPreferred {
			out = append(out, p)
		}
	}
	return out
}

// dayState is one plan with what it was made from.
type dayState struct {
	plan  dayplan.Plan
	provs []dayplan.Provider
	tasks []dayplan.Task
	hist  dayplan.History
	opts  dayplan.Options
}

func (d *dayPlanner) plan(queue []string, now time.Time) (dayState, error) {
	o, err := d.flags.options(now)
	if err != nil {
		return dayState{}, err
	}
	o.Ceiling = d.cfg.Routing.SwitchAtUtilization
	b := d.cfg.Budget
	o.DayTokens, o.DayUSD = float64(b.DayTokens), b.DayUSD
	recs := d.readLogs(now.AddDate(0, 0, -dayPlanLearnDays))
	dt, du := sessionlog.DayUsage(recs, now)
	o.DayTokensUsed, o.DayUSDUsed = float64(dt), du
	h := dayplan.Learn(recs, d.dir)
	names := subscriptionProviders(d.cfg)
	if len(names) == 0 {
		return dayState{}, errors.New("no Claude or Codex provider is enabled: nothing to plan for")
	}
	var provs []dayplan.Provider
	for _, p := range names {
		provs = append(provs, h.Provider(p, d.tr, now, o.Ceiling))
	}
	tasks := make([]dayplan.Task, len(queue))
	for i, q := range queue {
		tasks[i] = h.Task(q)
	}
	return dayState{plan: dayplan.Make(tasks, provs, o), provs: provs, tasks: tasks, hist: h, opts: o}, nil
}

// cmdDayplan prints the day plan for a task file: which task runs when, on
// which subscription, and which windows reset on the way. Nothing runs.
func cmdDayplan(args []string) error {
	fs := flag.NewFlagSet("rw dayplan", flag.ExitOnError)
	var c common
	c.register(fs)
	file := fs.String("file", "tasks.txt", "the task file to plan (as for rw run --file)")
	var df dayFlags
	df.register(fs)
	parseFlags(fs, args)
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	queue := parseTaskFile(string(data))
	if len(queue) == 0 {
		return fmt.Errorf("%s has no tasks", *file)
	}
	store, dir, err := c.setup()
	if err != nil {
		return err
	}
	st, err := newDayPlanner(store.Get(), dir, nil, df).plan(queue, time.Now())
	if err != nil {
		return err
	}
	printDayPlan(os.Stdout, st, queue)
	fmt.Printf("\nrun it: rw run --file %s --fill%s\n", *file, dayFlagsLine(df))
	return nil
}

func dayFlagsLine(df dayFlags) string {
	s := ""
	if df.until != "" {
		s += " --until " + df.until
	}
	if df.freshAt != "" {
		s += " --fresh-at " + df.freshAt
	}
	return s
}

// printDayPlan writes the plan: the windows as they are, the typical task,
// then the slots and resets in time order and the tasks left over.
func printDayPlan(w io.Writer, st dayState, queue []string) {
	now := st.opts.Now
	head := fmt.Sprintf("Day plan · %d queued task(s) · now %s", len(queue), schedule.Clock(now, now))
	if !st.opts.Until.IsZero() {
		head += " · until " + schedule.Clock(st.opts.Until, now)
	}
	if !st.opts.FreshAt.IsZero() {
		head += " · full windows at " + schedule.Clock(st.opts.FreshAt, now)
	}
	fmt.Fprintln(w, head)
	width := 0
	for _, p := range st.provs {
		width = max(width, len(p.Name))
	}
	for _, p := range st.provs {
		state := p.Note
		if state == "" {
			state = "no window reading yet: assumed unused"
		}
		capa := "window size unknown: a task counts as " + pct(dayplan.DefaultShare)
		if p.Capacity > 0 {
			capa = fmt.Sprintf("a window holds ~%s tokens (%d reading(s))", event.HumanTokens(int64(p.Capacity)), p.CapacitySamples)
		}
		fmt.Fprintf(w, "  %-*s  %s · %s\n", width, p.Name, state, capa)
	}
	from := "no finished tasks in the logs: a default"
	if h := st.hist; h.Tasks > 0 {
		from = fmt.Sprintf("the median of %d task(s)", h.Tasks)
		if h.Repo {
			from += " in this repo"
		}
	}
	fmt.Fprintf(w, "  a task: ~%s tokens, %s (%s)\n\n", event.HumanTokens(int64(st.hist.TaskTokens)), st.hist.TaskDuration, from)

	type line struct {
		at   time.Time
		text string
	}
	var lines []line
	for _, r := range st.plan.Resets {
		lines = append(lines, line{r.At, fmt.Sprintf("  %s  -- %s window resets", schedule.Clock(r.At, now), r.Provider)})
	}
	for _, s := range st.plan.Slots {
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
		lines = append(lines, line{s.Start, fmt.Sprintf("  %s  %-*s  %d. %s  · %s", schedule.Clock(s.Start, now), width, s.Provider,
			s.Task+1, oneLine(queue[s.Task], 60), strings.Join(notes, " · "))})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })
	for _, l := range lines {
		fmt.Fprintln(w, l.text)
	}
	if len(st.plan.Slots) > 0 {
		fmt.Fprintf(w, "  %s  done (expected) · %d of %d task(s)\n", schedule.Clock(st.plan.End, now), len(st.plan.Slots), len(queue))
	}
	if len(st.plan.Skipped) > 0 {
		fmt.Fprintln(w, "\nnot in this plan:")
		for _, s := range st.plan.Skipped {
			fmt.Fprintf(w, "  %d. %s - %s\n", s.Task+1, oneLine(queue[s.Task], 60), s.Why)
		}
	}
}

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// loadTask reads a task's saved state (tests swap it).
var loadTask = orchestrator.LoadTask

// fillRetries is how often a task that a usage limit stopped is resumed
// after a reset.
const fillRetries = 1

// runFill runs the queue the day planner's way (rw run --fill): before
// every task it plans again with the live readings, then runs the first
// slot's task leaning on its provider, or waits for the reset the plan
// waits for. A task stopped by a usage limit is resumed after the reset.
// It returns how many tasks ran, failed and were left over.
func runFill(ctx context.Context, out io.Writer, d *dayPlanner, queue []string, run func(i int, opts orchestrator.TaskOptions) orchestrator.TaskResult) (ran, failed int, left []dayplan.Skip) {
	pending := make([]int, len(queue)) // queue indexes still to run
	for i := range pending {
		pending[i] = i
	}
	resume := map[int]*orchestrator.TaskState{}
	retries := map[int]int{}
	for len(pending) > 0 && ctx.Err() == nil {
		texts := make([]string, len(pending))
		for i, q := range pending {
			texts[i] = queue[q]
		}
		now := time.Now()
		st, err := d.plan(texts, now)
		if err != nil {
			fmt.Fprintln(out, "fill:", err)
			break
		}
		slot, ok := st.plan.First(now)
		if !ok {
			if len(st.plan.Slots) == 0 {
				for _, s := range st.plan.Skipped {
					left = append(left, dayplan.Skip{Task: pending[s.Task], Why: s.Why})
				}
				break
			}
			next := st.plan.Slots[0]
			fmt.Fprintf(out, "\nfill: nothing has room now; task %d %s, on %s\n", pending[next.Task]+1, next.Wait, next.Provider)
			err := schedule.Wait(ctx, next.Start, waitEvery, func(left time.Duration) {
				fmt.Fprintf(out, "%s waiting: next task in %s (at %s)\n", time.Now().Format("15:04"), schedule.Left(left), schedule.Clock(next.Start, time.Now()))
			})
			if err != nil {
				break
			}
			continue
		}
		qi := pending[slot.Task]
		where := "on " + slot.Provider
		if slot.Strict {
			where = "only on " + slot.Provider
		}
		fmt.Fprintf(out, "\n=== task %d/%d %s (~%s of its window): %s\n", qi+1, len(queue), where, pct(slot.Share), oneLine(queue[qi], 100))
		var hitsBefore int
		if d.tr != nil {
			hitsBefore = limitHits(d.tr, subscriptionProviders(d.cfg))
		}
		opts := orchestrator.TaskOptions{Unattended: true, Lean: router.Lean{Provider: slot.Provider, Strict: slot.Strict}}
		var id string
		opts.Started = func(s string) { id = s }
		if st := resume[qi]; st != nil {
			opts.Resume, opts.Force = st, true
		}
		res := run(qi, opts)
		if !res.OK && ctx.Err() == nil && d.tr != nil && retries[qi] < fillRetries &&
			limitHits(d.tr, subscriptionProviders(d.cfg)) > hitsBefore {
			// A usage limit stopped it: resume it once a window has room.
			if s, err := loadTask(id); err == nil && id != "" {
				retries[qi]++
				resume[qi] = s
				fmt.Fprintf(out, "fill: task %d stopped at a usage limit; it resumes when a window has room\n", qi+1)
				continue
			}
		}
		ran++
		if !res.OK {
			failed++
		}
		pending = slicesDelete(pending, slot.Task)
	}
	if ctx.Err() != nil {
		for _, q := range pending {
			left = append(left, dayplan.Skip{Task: q, Why: "cancelled"})
		}
	}
	return ran, failed, left
}

func limitHits(tr *limits.Tracker, provs []string) int {
	n := 0
	for _, p := range provs {
		n += tr.Snapshot(p).LimitHits
	}
	return n
}

func slicesDelete(s []int, i int) []int { return append(s[:i:i], s[i+1:]...) }

// runFillCmd is rw run --file ... --fill on a started headless rw.
func runFillCmd(h *headless, tasks []string, df dayFlags) error {
	d := newDayPlanner(h.cfg, h.dir, h.orc.Tracker(), df)
	st, err := d.plan(tasks, time.Now())
	if err != nil {
		return err
	}
	printDayPlan(os.Stdout, st, tasks)
	ran, failed, left := runFill(h.ctx, os.Stdout, d, tasks, func(i int, opts orchestrator.TaskOptions) orchestrator.TaskResult {
		res := h.orc.RunWith(h.ctx, tasks[i], opts)
		h.report(res)
		return res
	})
	if h.log != nil {
		fmt.Println("session log:", h.log.Path())
	}
	fmt.Printf("\n%d of %d task(s) ran, %d failed\n", ran, len(tasks), failed)
	if len(left) > 0 {
		fmt.Println("not run:")
		for _, s := range left {
			fmt.Printf("  %d. %s - %s\n", s.Task+1, oneLine(tasks[s.Task], 80), s.Why)
		}
	}
	if h.ctx.Err() == nil {
		title, ev := "Relayweft: day plan done", notify.EventDone
		if failed > 0 {
			title, ev = "Relayweft: tasks failed", notify.EventFailed
		}
		body := fmt.Sprintf("%d of %d task(s) succeeded", ran-failed, len(tasks))
		if len(left) > 0 {
			body += fmt.Sprintf(", %d not run (%s)", len(left), left[0].Why)
		}
		h.webhook(ev, title, body)
	}
	if failed > 0 {
		return errTaskFailed
	}
	return nil
}
