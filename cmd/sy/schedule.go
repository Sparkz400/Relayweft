package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/limits"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/schedule"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// scheduleFlags are `sy run`'s start-time flags. Any task loop can use
// them: register them, then call waitUntilDue before the loop.
type scheduleFlags struct {
	at        string
	in        string
	whenReset string
}

func (s *scheduleFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&s.at, "at", "", `start at this local time: 02:30 (today, or tomorrow if past), "2026-10-04 02:30" or RFC3339`)
	fs.StringVar(&s.in, "in", "", "start after this delay: 3h, 90m, 2d")
	fs.StringVar(&s.whenReset, "when-reset", "", "start when this provider's usage limit resets: claude, codex or any")
}

func (s *scheduleFlags) set() bool { return s.at != "" || s.in != "" || s.whenReset != "" }

// target works out the start time. A zero time means "now"; note says why
// (a reset that is unknown or already past).
func (s *scheduleFlags) target(cfg *config.Config, tr *limits.Tracker, now time.Time) (time.Time, string, error) {
	n := 0
	for _, v := range []string{s.at, s.in, s.whenReset} {
		if v != "" {
			n++
		}
	}
	if n > 1 {
		return time.Time{}, "", errors.New("give only one of --at, --in and --when-reset")
	}
	switch {
	case s.at != "":
		t, err := schedule.ParseAt(s.at, now)
		return t, "", err
	case s.in != "":
		d, err := schedule.ParseIn(s.in)
		return now.Add(d), "", err
	case s.whenReset != "":
		p := strings.ToLower(s.whenReset)
		if !schedule.ValidReset(p) {
			return time.Time{}, "", fmt.Errorf("--when-reset %q: want claude, codex or any", s.whenReset)
		}
		recs, _ := sessionlog.ReadDir(cfg.SessionDir())
		t, note := schedule.ResetTime(p, tr, recs, now)
		return t, note, nil
	}
	return time.Time{}, "", nil
}

// waitEvery is how often the countdown line is printed.
var waitEvery = time.Minute

// waitUntilDue waits for the scheduled start (printing a countdown every
// minute) and keeps the machine awake from now until release is called,
// unless allowSleep. Ctrl+C (ctx) cancels the wait with an error.
func waitUntilDue(ctx context.Context, out io.Writer, sf *scheduleFlags, cfg *config.Config, tr *limits.Tracker, allowSleep bool, what string) (release func(), err error) {
	release = func() {}
	if !sf.set() {
		return release, nil
	}
	now := time.Now()
	at, note, err := sf.target(cfg, tr, now)
	if err != nil {
		return release, err
	}
	if note != "" {
		fmt.Fprintln(out, "schedule:", note)
	}
	if !allowSleep {
		release = proc.KeepAwake()
	}
	if at.IsZero() || !at.After(now) {
		return release, nil
	}
	awake := " (keeping the PC awake; --allow-sleep to let it sleep)"
	if allowSleep {
		awake = " (the PC may sleep: --allow-sleep)"
	}
	fmt.Fprintf(out, "scheduled: %s starts at %s%s - Ctrl+C cancels\n", what, at.Format("2006-01-02 15:04"), awake)
	err = schedule.Wait(ctx, at, waitEvery, func(left time.Duration) {
		fmt.Fprintf(out, "%s waiting: starts in %s (at %s)\n", time.Now().Format("15:04"), schedule.Left(left), schedule.Clock(at, time.Now()))
	})
	if err != nil {
		release()
		return func() {}, errors.New("scheduled run cancelled before it started")
	}
	fmt.Fprintf(out, "%s starting the scheduled run\n", time.Now().Format("15:04"))
	return release, nil
}

// cmdSchedule prints how to have the OS run `sy run --file` at a time:
// a Task Scheduler or cron line to copy. It installs nothing.
func cmdSchedule(args []string) error {
	fs := flag.NewFlagSet("sy schedule", flag.ExitOnError)
	file := fs.String("file", "tasks.txt", "task file for sy run --file")
	at := fs.String("at", "02:30", "time of day (HH:MM)")
	daily := fs.Bool("daily", false, "every day instead of once")
	dirFlag := fs.String("dir", "", "project directory (default current directory)")
	goos := fs.String("os", runtime.GOOS, "print for this OS: windows, darwin or linux")
	fs.Parse(args)
	dir, err := absDir(*dirFlag)
	if err != nil {
		return err
	}
	clock, err := time.Parse("15:04", *at)
	if err != nil {
		return fmt.Errorf("--at %q: want HH:MM", *at)
	}
	tf := *file
	if !filepath.IsAbs(tf) {
		tf = filepath.Join(dir, tf)
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "sy"
	}
	printSchedule(os.Stdout, *goos, exe, dir, tf, clock, *daily, time.Now())
	return nil
}

func printSchedule(w io.Writer, goos, exe, dir, file string, clock time.Time, daily bool, now time.Time) {
	hhmm := clock.Format("15:04")
	fmt.Fprintf(w, "Run `sy run --file %s` in %s at %s%s. Nothing is installed: copy the command.\n\n", file, dir, hhmm, map[bool]string{true: " every day", false: ""}[daily])
	switch goos {
	case "windows":
		if strings.Contains(dir+exe+file, "%") {
			// cmd.exe expands %NAME% inside quotes too, when the line is
			// pasted and again when the task runs; there is no escape.
			fmt.Fprintln(w, "Windows Task Scheduler: a path contains %, which cmd.exe would expand, so there is no command to paste.")
			fmt.Fprintf(w, "  Create the task in the Task Scheduler app: program %s, arguments: run --file \"%s\", start in: %s\n", exe, file, dir)
			break
		}
		// schtasks /tr takes one command line; cmd /c cds into the project.
		// Inside it, the paths are quoted for cmd; the task file also for
		// sy's own argv parser (a trailing backslash would escape the quote).
		tr := fmt.Sprintf(`cmd /c cd /d "%s" && "%s" run --file "%s" > "%s" 2>&1`, dir, exe, argvTrail(file), filepath.Join(dir, "sy-scheduled.log"))
		sched := "/sc once"
		if daily {
			sched = "/sc daily"
		} else {
			day := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
			if !day.After(now) {
				day = day.AddDate(0, 0, 1)
			}
			// /sd takes the system's short date format; this is the US form.
			sched += " /sd " + day.Format("01/02/2006")
		}
		fmt.Fprintln(w, "Windows Task Scheduler (run in a terminal; the task runs while you are logged in):")
		fmt.Fprintf(w, "  schtasks /create /tn \"Switchyard %s\" %s /st %s /tr %s\n\n", hhmm, sched, hhmm, cmdCaret(argvQuote(tr)))
		fmt.Fprintln(w, "  (if /sd is rejected, use your system's date format; check with: schtasks /query /tn \"Switchyard "+hhmm+"\")")
		fmt.Fprintln(w, "  remove it: schtasks /delete /tn \"Switchyard "+hhmm+"\" /f")
		fmt.Fprintln(w, "  wake the PC for it: Task Scheduler > the task > Conditions > \"Wake the computer to run this task\"")
	default:
		days := "*"
		dom, mon := "*", "*"
		if !daily {
			day := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
			if !day.After(now) {
				day = day.AddDate(0, 0, 1)
			}
			dom, mon = fmt.Sprint(day.Day()), fmt.Sprint(int(day.Month()))
		}
		fmt.Fprintln(w, "cron (crontab -e, then add this line):")
		fmt.Fprintf(w, "  %d %d %s %s %s cd %s && %s run --file %s >> %s 2>&1\n", clock.Minute(), clock.Hour(), dom, mon, days,
			shQuote(dir), shQuote(exe), shQuote(file), shQuote(filepath.Join(dir, "sy-scheduled.log")))
		if !daily {
			fmt.Fprintln(w, "  (a one-off: remove the line afterwards, or it runs again next year)")
		}
		if goos == "darwin" {
			fmt.Fprintln(w, "  macOS sleeps through cron: `pmset schedule wake` can wake it a minute before.")
		}
	}
	fmt.Fprintln(w, "\nOr keep a terminal open instead: sy run --file", file, "--at", hhmm)
}

// argvTrail doubles the trailing backslashes of s, which goes between
// double quotes on a command line read by the MSVCRT rules: "C:\" would
// read as C:" plus the rest of the line.
func argvTrail(s string) string {
	t := strings.TrimRight(s, `\`)
	return s + s[len(t):]
}

// argvQuote quotes s as one argument by the MSVCRT rules (schtasks reads
// its argv that way): " becomes \", and the backslashes before a " or the
// closing quote are doubled.
func argvQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		b.WriteRune(r)
		slashes = 0
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// cmdCaret escapes s for an interactive cmd.exe prompt. cmd.exe toggles
// quoting at every " (it does not know \" escapes), so the parts of an
// argvQuote'd string between \" pairs are outside quotes for it: there,
// & | < > ( ) ^ get a ^ so cmd passes them on instead of acting on them.
func cmdCaret(s string) string {
	var b strings.Builder
	quoted := false
	for _, r := range s {
		if r == '"' {
			quoted = !quoted
		} else if !quoted && strings.ContainsRune("&|<>()^", r) {
			b.WriteByte('^')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func shQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"$`\\!*?&;|<>()[]{}#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
