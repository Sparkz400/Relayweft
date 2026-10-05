package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/health"
	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// cmdHealth prints the reliability report: crashes, hangs, unclean exits,
// load peaks and leftovers from the logs, and whether the Phase 1 exit
// criterion (2 weeks of daily use without a crash or hang) is met.
func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("rw health", flag.ExitOnError)
	days := fs.Int("days", 14, "window in days, and the clean streak the criterion needs")
	minUse := fs.Int("min-use", 0, "days with use the criterion needs in the window (default 10, at most --days)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	check := fs.Bool("check", false, "exit with status 1 when the criterion is not met")
	verbose := fs.Bool("v", false, "list every incident, not only the last 10")
	logs := fs.String("logs", "", "log directory to read (default this machine's; e.g. the logs folder of an unzipped rw bugreport)")
	fs.Parse(args)
	r, err := health.Build(health.Options{Dir: *logs, Days: *days, MinUseDays: *minUse, NoLeftovers: *logs != ""})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			return err
		}
	} else {
		printHealth(os.Stdout, r, *verbose)
	}
	if *check && !r.Criterion.Met {
		return errHealthNotMet
	}
	return nil
}

var errHealthNotMet = errors.New("the reliability criterion is not met yet")

func printHealth(w io.Writer, r *health.Report, verbose bool) {
	okMark, failMark, warnMark, info := stOK.Render("ok  "), stErr.Render("FAIL"), stRev.Render("warn"), stMuted.Render("info")
	when := func(t time.Time) string { return t.Format("2006-01-02 15:04") }
	count := func(n int, bad string) string {
		if n == 0 {
			return okMark
		}
		return bad
	}

	fmt.Fprintf(w, "Reliability, last %d days (%s to %s)\n\n", r.Days, r.From.Format("2006-01-02"), r.Now.Format("2006-01-02"))
	c := r.Criterion
	mark := stErr.Render("NOT YET")
	if c.Met {
		mark = stOK.Render("MET")
	} else if c.CleanSince.IsZero() {
		mark = stMuted.Render("NO DATA")
	}
	fmt.Fprintf(w, "  %d-day criterion  %s  %s\n", c.NeedDays, mark, strings.TrimPrefix(strings.TrimPrefix(c.Summary, "met: "), "not yet: "))
	if !c.CleanSince.IsZero() {
		fmt.Fprintf(w, "                    clean since %s\n", when(c.CleanSince))
	}
	fmt.Fprintf(w, "  days             %s  %s\n", dayStrip(r), stMuted.Render("# used  . not used  X crash or hang"))
	fmt.Fprintln(w)

	fmt.Fprintf(w, "%s crashes        %d", count(len(r.Crashes), failMark), len(r.Crashes))
	if n := len(r.Crashes); n > 0 {
		fmt.Fprintf(w, ", last %s: %s", when(r.Crashes[n-1].Time), r.Crashes[n-1].Detail)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s hangs          %d", count(len(r.Hangs), failMark), len(r.Hangs))
	if n := len(r.Hangs); n > 0 {
		fmt.Fprintf(w, ", last %s: %s", when(r.Hangs[n-1].Time), r.Hangs[n-1].Detail)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s unclean exits  %d", count(len(r.Unclean), warnMark), len(r.Unclean))
	if len(r.Unclean) > 0 {
		fmt.Fprint(w, stMuted.Render("  ended without a trace: killed, window closed or power lost (not counted as a crash)"))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s agent timeouts %d", count(len(r.Timeouts), info), len(r.Timeouts))
	if len(r.Timeouts) > 0 {
		fmt.Fprint(w, stMuted.Render("  agents stopped at agent_timeout; rw carried on"))
	}
	fmt.Fprintln(w)
	if len(r.Pauses) > 0 {
		var longest time.Duration
		for _, p := range r.Pauses {
			if d, err := time.ParseDuration(strings.TrimPrefix(p.Detail, "machine slept or froze for ")); err == nil && d > longest {
				longest = d
			}
		}
		fmt.Fprintf(w, "%s pauses         %d while rw ran (sleep or a frozen machine), longest %s\n", info, len(r.Pauses), longest)
	}

	l := r.Load
	if l.CPU >= 0 {
		mark := okMark
		if l.HotSamples > 0 {
			mark = info
		}
		fmt.Fprintf(w, "%s CPU peak       %d%% at %s", mark, l.CPU, when(l.CPUAt))
		if l.Samples > 0 {
			fmt.Fprintf(w, "  (%d of %d five-minute readings at 95%% or more)", l.HotSamples, l.Samples)
		}
		fmt.Fprintln(w)
	}
	if l.MemFreeMB >= 0 {
		mark := okMark
		if l.LowMemFrees > 0 {
			mark = warnMark
		}
		fmt.Fprintf(w, "%s RAM low point  %s free", mark, orchestrator.HumanBytes(uint64(l.MemFreeMB)<<20))
		if l.MemTotalMB > 0 {
			fmt.Fprintf(w, " of %s", orchestrator.HumanBytes(uint64(l.MemTotalMB)<<20))
		}
		fmt.Fprintf(w, " at %s", when(l.MemAt))
		if l.LowMemFrees > 0 {
			fmt.Fprintf(w, "  (%s under 10%% free)", plural(l.LowMemFrees, "reading", "readings"))
		}
		fmt.Fprintln(w)
	}
	if l.RwMemMB > 0 {
		fmt.Fprintf(w, "%s rw memory      %s peak in one process, %d goroutines at most\n", okMark, orchestrator.HumanBytes(uint64(l.RwMemMB)<<20), l.Goroutines)
	}
	if l.CPU < 0 && l.MemFreeMB < 0 {
		fmt.Fprintf(w, "%s load           no readings yet (they start with this version of rw)\n", info)
	}

	switch {
	case !r.LeftoversChecked:
		fmt.Fprintf(w, "%s leftovers      not checked (logs of another folder)\n", info)
	case len(r.Leftovers) == 0:
		fmt.Fprintf(w, "%s leftovers      none\n", okMark)
	}
	for _, lo := range r.Leftovers {
		size := ""
		if lo.Bytes > 0 {
			size = ", " + orchestrator.HumanBytes(lo.Bytes)
		}
		fmt.Fprintf(w, "%s leftover       %s%s: %s\n", warnMark, lo.Detail, size, lo.Path)
	}
	for _, s := range r.Running {
		fmt.Fprintf(w, "%s running        pid %d: rw %s since %s\n", info, s.PID, cmdName(s.Cmd), when(s.Start))
	}
	fmt.Fprintf(w, "\n%s on %s in the window.", plural(r.Sessions, "rw process", "rw processes"), plural(len(r.UseDays), "day", "days"))
	switch {
	case !r.DebugSince.IsZero() && !r.HealthSince.IsZero():
		fmt.Fprintf(w, " Before %s only the debug log was there: no hang, exit or load data.", when(r.HealthSince))
	case r.HealthSince.IsZero() && !r.DebugSince.IsZero():
		fmt.Fprint(w, " Only the debug log so far: hang, exit and load data start with this version.")
	}
	fmt.Fprintln(w)

	var all []health.Incident
	for _, set := range [][]health.Incident{r.Crashes, r.Hangs, r.Unclean, r.Timeouts, r.LeftoverLogs} {
		all = append(all, set...)
	}
	if len(all) == 0 {
		return
	}
	sortIncidentsDesc(all)
	shown := all
	if !verbose && len(shown) > 10 {
		shown = shown[:10]
	}
	fmt.Fprintf(w, "\nIncidents, newest first:\n")
	for _, in := range shown {
		who := ""
		if in.PID > 0 {
			who = fmt.Sprintf("rw %s (pid %d): ", cmdName(in.Cmd), in.PID)
		}
		fmt.Fprintf(w, "  %s  %-13s %s%s\n", when(in.Time), in.Kind, who, in.Detail)
		if in.Log != "" {
			fmt.Fprintf(w, "  %s\n", stMuted.Render("                  "+in.Log))
		}
	}
	if len(shown) < len(all) {
		fmt.Fprintf(w, "  %s\n", stMuted.Render(fmt.Sprintf("... %d more (rw health -v)", len(all)-len(shown))))
	}
	fmt.Fprintf(w, "\n%s\n", stMuted.Render("Send `rw bugreport` with any crash or hang: it includes these logs."))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func cmdName(c string) string {
	if c == "" {
		return "(TUI)"
	}
	return c
}

// dayStrip draws one character per day of the window, oldest first.
func dayStrip(r *health.Report) string {
	used := map[string]bool{}
	for _, d := range r.UseDays {
		used[d] = true
	}
	bad := map[string]bool{}
	for _, set := range [][]health.Incident{r.Crashes, r.Hangs} {
		for _, in := range set {
			bad[in.Time.Format("2006-01-02")] = true
		}
	}
	var b strings.Builder
	for i := r.Days - 1; i >= 0; i-- {
		d := r.Now.AddDate(0, 0, -i).Format("2006-01-02")
		switch {
		case bad[d]:
			b.WriteString(stErr.Render("X"))
		case used[d]:
			b.WriteString(stOK.Render("#"))
		default:
			b.WriteString(stMuted.Render("."))
		}
	}
	return b.String()
}

func sortIncidentsDesc(in []health.Incident) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Time.After(in[j].Time) })
}
