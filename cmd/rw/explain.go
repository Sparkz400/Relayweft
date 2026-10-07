package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/report"
)

// cmdExplain says why a task was routed the way it was.
func cmdExplain(args []string) error {
	fs := flag.NewFlagSet("rw explain", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	dirFlag := fs.String("dir", "", "project directory for the default task (default current directory)")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: rw explain [task-id] [--json]

Explains a task's routing: why it ran as one agent or several, why each
agent got its provider and model (the rule and its reason), what each run
was estimated to use against what it used, and what escalated the work (a
repeating error, a usage limit, a risky change, a hard step, a fix round).
Default: the last task in this directory (rw history lists them).

Estimates are recomputed from the runs logged before the task, the way plan
approval and budget reservations compute them.
`)
	}
	// Accept flags after the task id too (rw explain <id> --json).
	var id string
	rest := args
	for { // parse at least once, also without arguments
		parseFlags(fs, rest)
		if fs.NArg() == 0 {
			break
		}
		if id != "" {
			return fmt.Errorf("one task id at a time (got %q and %q)", id, fs.Arg(0))
		}
		id, rest = fs.Arg(0), fs.Args()[1:]
	}
	cfg, _, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var st *orchestrator.TaskState
	if id != "" {
		if st, err = orchestrator.LoadTask(id); err != nil {
			return err
		}
	} else {
		dir, err := absDir(*dirFlag)
		if err != nil {
			return err
		}
		h := orchestrator.History(dir, 1)
		if len(h) == 0 {
			return errors.New("no task in this directory yet (rw history --all lists every task)")
		}
		st = &h[0]
	}
	e := report.Explain(st, report.Options{SessionDir: cfg.SessionDir()})
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	}
	return e.Text(os.Stdout)
}
