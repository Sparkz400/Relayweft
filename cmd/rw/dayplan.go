package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/dayplan"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/notify"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/schedule"
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

// bounds reads the times (a clock time is the next one after now).
func (d *dayFlags) bounds(now time.Time) (until, freshAt time.Time, err error) {
	if d.until != "" {
		if until, err = schedule.ParseAt(d.until, now); err != nil {
			return until, freshAt, fmt.Errorf("--until: %w", err)
		}
	}
	if d.freshAt != "" {
		if freshAt, err = schedule.ParseAt(d.freshAt, now); err != nil {
			return until, freshAt, fmt.Errorf("--fresh-at: %w", err)
		}
	}
	return until, freshAt, nil
}

// newDayPlanner is the planner of rw dayplan and rw run --fill.
func newDayPlanner(cfg *config.Config, dir string, tr *limits.Tracker, flags dayFlags) (*dayplan.Live, error) {
	until, freshAt, err := flags.bounds(time.Now())
	if err != nil {
		return nil, err
	}
	l := &dayplan.Live{Cfg: func() *config.Config { return cfg }, Dir: dir, Tracker: tr}
	l.SetBounds(until, freshAt)
	return l, nil
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
	d, err := newDayPlanner(store.Get(), dir, nil, df)
	if err != nil {
		return err
	}
	st, err := d.Plan(queue, time.Now())
	if err != nil {
		return err
	}
	st.Write(os.Stdout, queue)
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
func runFill(ctx context.Context, out io.Writer, d *dayplan.Live, queue []string, run func(i int, opts orchestrator.TaskOptions) orchestrator.TaskResult) (ran, failed int, left []dayplan.Skip) {
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
		st, err := d.Plan(texts, now)
		if err != nil {
			fmt.Fprintln(out, "fill:", err)
			break
		}
		next := st.Next(now)
		if next.Task < 0 {
			if next.At.IsZero() {
				for _, s := range st.Plan.Skipped {
					left = append(left, dayplan.Skip{Task: pending[s.Task], Why: s.Why})
				}
				break
			}
			first := st.Plan.Slots[0]
			fmt.Fprintf(out, "\nfill: nothing has room now; task %d %s, on %s\n", pending[first.Task]+1, next.Why, first.Provider)
			err := schedule.Wait(ctx, next.At, waitEvery, func(left time.Duration) {
				fmt.Fprintf(out, "%s waiting: next task in %s (at %s)\n", time.Now().Format("15:04"), schedule.Left(left), schedule.Clock(next.At, time.Now()))
			})
			if err != nil {
				break
			}
			continue
		}
		qi := pending[next.Task]
		where := "on " + next.Lean.Provider
		if next.Lean.Strict {
			where = "only on " + next.Lean.Provider
		}
		fmt.Fprintf(out, "\n=== task %d/%d %s (~%.0f%% of its window): %s\n", qi+1, len(queue), where, next.Share*100, oneLine(queue[qi], 100))
		cfg := d.Cfg()
		hitsBefore := dayplan.LimitHits(d.Tracker, cfg)
		opts := orchestrator.TaskOptions{Unattended: true, Lean: next.Lean}
		var id string
		opts.Started = func(s string) { id = s }
		if st := resume[qi]; st != nil {
			opts.Resume, opts.Force = st, true
		}
		res := run(qi, opts)
		if !res.OK && ctx.Err() == nil && retries[qi] < fillRetries && dayplan.LimitHits(d.Tracker, cfg) > hitsBefore {
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
		pending = append(pending[:next.Task:next.Task], pending[next.Task+1:]...)
	}
	if ctx.Err() != nil {
		for _, q := range pending {
			left = append(left, dayplan.Skip{Task: q, Why: "cancelled"})
		}
	}
	return ran, failed, left
}

// runFillCmd is rw run --file ... --fill on a started headless rw.
func runFillCmd(h *headless, tasks []string, df dayFlags) error {
	d, err := newDayPlanner(h.cfg, h.dir, h.orc.Tracker(), df)
	if err != nil {
		return err
	}
	st, err := d.Plan(tasks, time.Now())
	if err != nil {
		return err
	}
	st.Write(os.Stdout, tasks)
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
