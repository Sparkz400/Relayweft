package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// minTuneTasks is roughly how many tasks the heuristics need before their
// rates mean anything (most need >= 5 samples of one kind).
const minTuneTasks = 10

// cmdTune implements `sy tune`: routing suggestions from the session logs.
func cmdTune(args []string) error {
	fs := flag.NewFlagSet("sy tune", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	here := fs.Bool("here", false, "only sessions run in the current directory")
	since := fs.String("since", "", "only records newer than this (e.g. 24h, 7d)")
	fs.Parse(args)
	cfg, _, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	recs, err := sessionlog.ReadDir(cfg.SessionDir())
	if err != nil {
		return err
	}
	var f sessionlog.Filter
	if *here {
		f.Cwd, _ = os.Getwd()
	}
	if *since != "" {
		d, err := parseSince(*since)
		if err != nil {
			return err
		}
		f.Since = time.Now().Add(-d)
	}
	printTune(os.Stdout, tuneTasks(recs, f), sessionlog.Suggest(recs, f))
	fmt.Println("\nlogs:", cfg.SessionDir())
	return nil
}

// tuneTasks counts finished tasks that pass the filter.
func tuneTasks(recs []sessionlog.Record, f sessionlog.Filter) int {
	n := 0
	for _, m := range sessionlog.Aggregate(recs, f).Modes {
		n += m.Tasks
	}
	return n
}

// printTune writes the suggestions, or why there are none.
func printTune(w io.Writer, tasks int, sugs []sessionlog.Suggestion) {
	if tasks < minTuneTasks && len(sugs) == 0 {
		fmt.Fprintf(w, "Only %d task(s) logged. sy tune needs at least ~%d tasks before its suggestions mean anything.\n", tasks, minTuneTasks)
		return
	}
	if len(sugs) == 0 {
		fmt.Fprintf(w, "No suggestions from %d task(s): the current routing looks fine.\n", tasks)
		return
	}
	fmt.Fprintf(w, "Routing suggestions from %d task(s)\n", tasks)
	if tasks < minTuneTasks {
		fmt.Fprintf(w, "(few tasks logged - treat these as hints until ~%d tasks)\n", minTuneTasks)
	}
	marker := map[string]string{"high": "[!!]", "medium": "[! ]", "info": "[i ]"}
	for _, s := range sugs {
		fmt.Fprintf(w, "\n%s %s\n", marker[s.Severity], s.Title)
		fmt.Fprintf(w, "     %s\n", s.Detail)
		if len(s.Commands) == 0 {
			continue
		}
		fmt.Fprintln(w, "     apply in the TUI (then /save to keep):")
		for _, c := range s.Commands {
			fmt.Fprintf(w, "       %s", c)
			if flag := routeFlag(c); flag != "" {
				fmt.Fprintf(w, "    (or: %s)", flag)
			}
			fmt.Fprintln(w)
		}
	}
}

// routeFlag turns "/route role spec" into its command-line form; other
// commands have no flag equivalent.
func routeFlag(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) != 3 || f[0] != "/route" {
		return ""
	}
	return fmt.Sprintf("sy --route %s=%s", f[1], f[2])
}
