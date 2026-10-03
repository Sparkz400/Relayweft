package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// minTuneTasks is roughly how many tasks the heuristics need before their
// rates mean anything (most need >= 5 samples of one kind).
const minTuneTasks = 10

// cmdTune implements `sy tune`: routing suggestions from the session logs,
// and this repo's learned routes (--apply, --learned, --reset).
func cmdTune(args []string) error {
	fs := flag.NewFlagSet("sy tune", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	here := fs.Bool("here", false, "only sessions run in the current directory (suggestions; learned routes always use this repo's runs)")
	since := fs.String("since", "", "only records newer than this (e.g. 24h, 7d; suggestions only: learned routes weigh old runs less instead)")
	apply := fs.Bool("apply", false, "update this repo's learned routes from the logs and print what changed and why")
	learned := fs.Bool("learned", false, "show this repo's learned routes and their evidence")
	reset := fs.Bool("reset", false, "forget this repo's learned routes")
	dirFlag := fs.String("dir", "", "project directory (default current directory)")
	fs.Parse(args)
	if n := countTrue(*apply, *learned, *reset); n > 1 {
		return errors.New("give one of --apply, --learned or --reset")
	}
	dir, err := absDir(*dirFlag)
	if err != nil {
		return err
	}
	cfg, _, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	switch {
	case *reset:
		return tuneReset(os.Stdout, dir)
	case *learned:
		return tuneLearned(os.Stdout, cfg, dir)
	}
	// Learned routes are compared with your config plus the repo file
	// (never with themselves).
	if _, err := config.ApplyRepo(cfg, dir); err != nil {
		return err
	}
	recs, err := sessionlog.ReadDir(cfg.SessionDir())
	if err != nil {
		return err
	}
	if *apply {
		rep, err := orchestrator.UpdateLearned(dir, cfg, recs, time.Now(), false)
		if err != nil {
			return err
		}
		printLearnDiff(os.Stdout, rep, cfg.LearnMode(), true)
		return nil
	}
	var f sessionlog.Filter
	if *here {
		f.Cwd = dir
	}
	if *since != "" {
		d, err := parseSince(*since)
		if err != nil {
			return err
		}
		f.Since = time.Now().Add(-d)
	}
	printTune(os.Stdout, tuneTasks(recs, f), sessionlog.SuggestFor(recs, f, sessionlog.CatalogFrom(cfg)))
	// What --apply would change, so the suggestion and the learned layer
	// can be compared (a dry run: nothing is saved).
	if rep, err := orchestrator.UpdateLearned(dir, cfg, recs, time.Now(), true); err == nil && len(rep.Result.Changes) > 0 {
		fmt.Println()
		printLearnDiff(os.Stdout, rep, cfg.LearnMode(), false)
	}
	fmt.Println("\nlogs:", cfg.SessionDir())
	return nil
}

func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
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

// printLearnDiff writes what an update changed (saved) or would change
// (dry run), with the evidence for each change.
func printLearnDiff(w io.Writer, rep orchestrator.LearnReport, mode string, saved bool) {
	res := rep.Result
	head := "Learned routes for " + rep.Root
	if !saved {
		head += ": `sy tune --apply` would change"
	}
	fmt.Fprintln(w, head)
	if len(res.Changes) == 0 {
		fmt.Fprintln(w, "  no change: no route has enough recent runs here and clearly beats the current one")
	}
	changed := map[string]bool{}
	for _, c := range res.Changes {
		changed[c.Role] = true
		to := c.To.String()
		if c.Remove {
			to += " (back to your configured route)"
		}
		fmt.Fprintf(w, "  %s: %s -> %s\n", c.Role, c.From, to)
		fmt.Fprintf(w, "      %s\n", c.Why)
		printEvidence(w, "      ", c.Evidence)
	}
	var kept []string
	for _, role := range sessionlog.LearnRoles {
		if !changed[role] {
			if lr, ok := rep.Learned.Routes[role]; ok {
				kept = append(kept, role+" (keeps "+lr.Spec()+")")
			}
		}
	}
	if len(kept) > 0 {
		fmt.Fprintf(w, "  unchanged learned routes: %s\n", strings.Join(kept, ", "))
	}
	if !saved {
		return
	}
	fmt.Fprintf(w, "saved: %s\n", config.LearnedPath(rep.Root))
	switch mode {
	case config.LearnOff:
		fmt.Fprintln(w, "note: routing.learn is off, so these routes are not used (set it to suggest or auto)")
	case config.LearnSuggest:
		fmt.Fprintln(w, "they apply from the next task on; routing.learn: auto refreshes them once a day by itself")
	}
}

// printEvidence writes a table of routes with their runs.
func printEvidence(w io.Writer, indent string, ev []config.RouteEvidence) {
	if len(ev) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "%sROUTE\tRUNS\tRECENT\tOK\tTOKENS/RUN\tWALL/RUN\n", indent)
	for _, e := range ev {
		fmt.Fprintf(tw, "%s%s\t%d\t%.1f\t%.0f%%\t%s\t%s\n", indent, e.Route, e.Samples, e.Weight, e.Success*100,
			sessionlog.Human(e.Tokens), (time.Duration(e.WallMS) * time.Millisecond).Round(time.Second))
	}
	tw.Flush()
}

// tuneLearned shows the repo's learned routes and whether each is used.
// cfg is your config without the repo file.
func tuneLearned(w io.Writer, cfg *config.Config, dir string) error {
	root, err := orchestrator.LearnedRoot(dir)
	if err != nil {
		return err
	}
	l, err := config.LoadLearned(root)
	if err != nil {
		return err
	}
	// Which ones apply: the same layering as a task (repo file roles win).
	store := config.NewStore(cfg.Clone(), "")
	if _, err := store.ApplyRepo(dir); err != nil {
		return err
	}
	mode := store.Get().LearnMode()
	fmt.Fprintf(w, "Learned routes for %s (routing.learn: %s)\n", root, mode)
	if len(l.Routes) == 0 {
		fmt.Fprintln(w, "  none yet: `sy tune --apply` learns them from this repo's logs")
		return nil
	}
	fmt.Fprintf(w, "  last update %s\n", l.Updated.Local().Format("2006-01-02 15:04"))
	inUse := map[string]bool{}
	if mode != config.LearnOff {
		for _, r := range store.ApplyLearned(l) {
			inUse[r] = true
		}
	}
	roles := make([]string, 0, len(l.Routes))
	for r := range l.Routes {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, role := range roles {
		lr := l.Routes[role]
		state := ""
		switch {
		case mode == config.LearnOff:
			state = " (not used: routing.learn is off)"
		case store.Pinned(role):
			state = " (not used: " + config.RepoFileName + " sets this role)"
		case !inUse[role]:
			state = " (not used: the route is no longer configured)"
		}
		fmt.Fprintf(w, "\n  %s: %s%s\n", role, lr.Spec(), state)
		fmt.Fprintf(w, "      since %s: %s\n", lr.Since.Local().Format("2006-01-02"), lr.Why)
		printEvidence(w, "      ", lr.Evidence)
	}
	fmt.Fprintf(w, "\nfile: %s   (sy tune --reset forgets them)\n", config.LearnedPath(root))
	return nil
}

// tuneReset forgets the repo's learned routes.
func tuneReset(w io.Writer, dir string) error {
	root, err := orchestrator.LearnedRoot(dir)
	if err != nil {
		return err
	}
	l, _ := config.LoadLearned(root) // a broken file is removed too
	if err := config.ResetLearned(root); err != nil {
		return err
	}
	fmt.Fprintf(w, "forgot %d learned route(s) for %s\n", len(l.Routes), root)
	return nil
}
