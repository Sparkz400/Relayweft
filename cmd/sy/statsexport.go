package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// sy stats --json and --merge: usage exports for a team.
//
//   - --json writes this machine's per-day usage (the format is
//     documented in internal/sessionlog/export.go): a random machine id,
//     tokens per provider and model, $ and limit hits. Task texts and
//     folders are left out unless --with-tasks; a label only with --name.
//   - --merge reads exports (files, or every *.json in a folder such as
//     the team budget's) and prints the combined tables plus one row per
//     machine. Each machine's day is counted once, however often it is
//     merged.

// statsOut is where the stats commands print (tests capture it).
var statsOut io.Writer = os.Stdout

// statsExportFlags are sy stats' export and merge flags.
type statsExportFlags struct {
	json      bool
	out       string
	name      string
	withTasks bool
	merge     bool
}

func (x *statsExportFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&x.json, "json", false, "write this machine's usage as a JSON export (no task texts or paths)")
	fs.StringVar(&x.out, "out", "", "with --json: write the export to this file instead of stdout")
	fs.StringVar(&x.name, "name", "", "with --json: a label for this machine in the export (default: none, only a random id)")
	fs.BoolVar(&x.withTasks, "with-tasks", false, "with --json: include each task's text and folder")
	fs.BoolVar(&x.merge, "merge", false, "merge exports (files or folders of *.json) and print the combined tables")
}

// check rejects flag combinations that do not fit.
func (x *statsExportFlags) check(args []string) error {
	switch {
	case x.json && x.merge:
		return errors.New("--json and --merge do not go together")
	case x.merge && len(args) == 0:
		return errors.New("--merge needs export files or folders: sy stats --merge a.json b.json | <folder>")
	case !x.merge && len(args) > 0:
		return fmt.Errorf("unexpected argument %q", args[0])
	case !x.json && (x.out != "" || x.name != "" || x.withTasks):
		return errors.New("--out, --name and --with-tasks go with --json")
	}
	return nil
}

// export writes this machine's usage.
func (x *statsExportFlags) export(recs []sessionlog.Record, f sessionlog.Filter) error {
	id, err := sessionlog.MachineID()
	if err != nil {
		return fmt.Errorf("machine id: %w", err)
	}
	e := sessionlog.BuildExport(recs, sessionlog.ExportOptions{Machine: id, Name: strings.TrimSpace(x.name), Filter: f, WithTasks: x.withTasks})
	data, err := e.Marshal()
	if err != nil {
		return err
	}
	if x.out == "" {
		_, err = statsOut.Write(data)
		return err
	}
	tmp := x.out + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, x.out); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d days, machine %s)\n", x.out, len(e.Days), id)
	return nil
}

// mergeExports reads the exports named in args and prints the combined
// tables. A file given by name must be a valid export; files found in a
// folder are skipped with a warning when they are not.
func mergeExports(cfg *config.Config, args []string, since time.Time) error {
	var exps []sessionlog.Export
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			e, err := sessionlog.ReadExport(a)
			if err != nil {
				return err
			}
			exps = append(exps, e)
			continue
		}
		files, _ := filepath.Glob(filepath.Join(a, "*.json"))
		sort.Strings(files)
		n := 0
		for _, f := range files {
			if strings.HasPrefix(filepath.Base(f), ".") {
				continue
			}
			e, err := sessionlog.ReadExport(f)
			if err != nil {
				fmt.Fprintln(os.Stderr, "warning: skipped", err)
				continue
			}
			exps = append(exps, e)
			n++
		}
		if n == 0 {
			fmt.Fprintf(os.Stderr, "warning: no stats exports in %s\n", a)
		}
	}
	merged := sessionlog.MergeExports(exps)
	if !since.IsZero() {
		first := since.Local().Format("2006-01-02")
		for i := range merged {
			var keep []sessionlog.ExportDay
			for _, d := range merged[i].Days {
				if d.Date >= first {
					keep = append(keep, d)
				}
			}
			merged[i].Days = keep
		}
	}
	sessionlog.PrintMerged(statsOut, merged, cfg.Budget.Team.DayUSD, cfg.Budget.Team.DayTokens)
	return nil
}
