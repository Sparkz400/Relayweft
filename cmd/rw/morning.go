package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/morning"
)

// cmdMorning prints (or posts) the summary of the unattended work since a
// time: the night's queued, scheduled and task-file runs.
func cmdMorning(args []string) error {
	fs := flag.NewFlagSet("rw morning", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	since := fs.String("since", "12h", "summarize from this long ago (12h, 2d) or this time of day (18:00, the last one)")
	here := fs.Bool("here", false, "only tasks run in the current directory")
	all := fs.Bool("all", false, "also tasks you started and watched, not only unattended ones")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	send := fs.Bool("send", false, "post it to notify.webhooks (event summary) and as a desktop notification instead of printing it; nothing is sent when nothing ran")
	sched := fs.Bool("schedule", false, "print a Task Scheduler / cron command that runs `rw morning --send` every day (installs nothing)")
	at := fs.String("at", "07:30", "with --schedule: the time of day")
	goos := fs.String("os", runtime.GOOS, "with --schedule: print for this OS: windows, darwin or linux")
	parseFlags(fs, args)
	if *sched {
		clock, err := time.Parse("15:04", *at)
		if err != nil {
			return fmt.Errorf("--at %q: want HH:MM", *at)
		}
		exe, err := os.Executable()
		if err != nil {
			exe = "rw"
		}
		printMorningSchedule(os.Stdout, *goos, exe, clock)
		return nil
	}
	cfg, _, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	now := time.Now()
	from, err := morning.ParseSince(*since, now)
	if err != nil {
		return err
	}
	o := morning.Options{Since: from, Until: now, All: *all}
	if *here {
		if o.Dir, err = absDir(""); err != nil {
			return err
		}
	}
	s := morning.Collect(cfg, o)
	switch {
	case *send:
		if s.Empty() {
			fmt.Println("rw morning:", s.Title(), "- nothing sent")
			return nil
		}
		host, _ := os.Hostname()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := morning.Deliver(ctx, cfg, s, host); err != nil {
			return fmt.Errorf("sending the summary: %w", err)
		}
		fmt.Println("sent:", s.Title())
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	default:
		for _, l := range s.Lines() {
			fmt.Println(l)
		}
		if s.Empty() && !*all {
			fmt.Println("(rw morning --all also lists the tasks you started yourself)")
		}
	}
	return nil
}

// printMorningSchedule prints how to have the OS run `rw morning --send`
// every day: a Task Scheduler or cron line to copy.
func printMorningSchedule(w io.Writer, goos, exe string, clock time.Time) {
	hhmm := clock.Format("15:04")
	fmt.Fprintf(w, "Post the overnight summary every day at %s with `rw morning --send`. Nothing is installed: copy the command.\n", hhmm)
	fmt.Fprintln(w, "Or set notify.morning: \""+hhmm+"\" in your config: an rw web, TUI or long rw run that is running then posts it.")
	fmt.Fprintln(w)
	switch goos {
	case "windows":
		if strings.Contains(exe, "%") {
			fmt.Fprintln(w, "Windows Task Scheduler: rw's path contains %, which cmd.exe would expand, so there is no command to paste.")
			fmt.Fprintf(w, "  Create a daily task at %s in the Task Scheduler app: program %s, arguments: morning --send\n", hhmm, exe)
			return
		}
		tr := fmt.Sprintf(`"%s" morning --send`, exe)
		fmt.Fprintln(w, "Windows Task Scheduler (run in a terminal; the task runs while you are logged in):")
		fmt.Fprintf(w, "  schtasks /create /tn \"Relayweft morning\" /sc daily /st %s /tr %s\n\n", hhmm, cmdCaret(argvQuote(tr)))
		fmt.Fprintln(w, "  remove it: schtasks /delete /tn \"Relayweft morning\" /f")
	default:
		fmt.Fprintln(w, "cron (crontab -e, then add this line):")
		fmt.Fprintf(w, "  %d %d * * * %s morning --send\n", clock.Minute(), clock.Hour(), shQuote(exe))
		if goos == "darwin" {
			fmt.Fprintln(w, "  macOS skips cron while asleep: `pmset repeat wake MTWRFSU <time>` can wake it a minute before.")
		}
	}
}

// startMorning posts the morning summary at notify.morning while ctx
// lives (rw web, the TUI and unattended rw runs).
func startMorning(ctx context.Context, cfg func() *config.Config) {
	host, _ := os.Hostname()
	go morning.Daily(ctx, cfg, host, func(err error) { fmt.Fprintln(os.Stderr, "morning summary:", err) })
}
