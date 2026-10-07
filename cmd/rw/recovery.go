package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func cmdRecovery(args []string) error {
	fs := flag.NewFlagSet("rw recovery", flag.ExitOnError)
	dir := fs.String("dir", "", "project directory")
	asJSON := fs.Bool("json", false, "print recovery actions as JSON")
	resume := fs.String("resume", "", "continue an interrupted task")
	retry := fs.String("retry", "", "retry the unfinished steps of a failed or cancelled task")
	undo := fs.String("undo", "", "preview and undo a task's agent-reported changes")
	parseFlags(fs, args)
	n := 0
	for _, v := range []string{*resume, *retry, *undo} {
		if v != "" {
			n++
		}
	}
	if n > 1 || fs.NArg() != 0 {
		return fmt.Errorf("choose one of --resume, --retry or --undo")
	}
	d, err := absDir(*dir)
	if err != nil {
		return err
	}
	items, err := orchestrator.Recovery(d)
	if err != nil {
		return err
	}
	if n != 0 {
		for _, i := range items {
			if i.ID != *resume && i.ID != *retry && i.ID != *undo {
				continue
			}
			if *undo != "" {
				if !i.CanUndo {
					return fmt.Errorf("task has no available undo")
				}
				return cmdUndo([]string{"--dir", d, "--agent-files-only", i.UndoKey})
			}
			if !i.CanResume {
				return fmt.Errorf("task is %s, with no unfinished recovery action", i.Status)
			}
			a := []string{"--dir", d, "--approve"}
			if *retry != "" {
				a = append(a, "--force")
			}
			return cmdResume(append(a, i.ID))
		}
		return fmt.Errorf("task does not belong to this project's recent history")
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(items)
	}
	for _, i := range items {
		fmt.Printf("\n%s  %s  %d/%d steps\n%s\n", i.ID, i.Status, i.Done, i.Steps, i.Task)
		for _, e := range i.Conflicts {
			fmt.Println("  " + e)
		}
		for _, s := range i.Saved {
			fmt.Println("  " + s.Hint())
		}
		for _, b := range i.Branches {
			fmt.Println("  preserved branch: " + b)
		}
		if i.CanResume {
			action := "retry"
			if i.Status == "interrupted" {
				action = "resume"
			}
			fmt.Printf("  rw recovery --%s %s\n", action, i.ID)
		}
		if i.CanUndo {
			fmt.Printf("  rw recovery --undo %s\n", i.ID)
		}
	}
	if len(items) == 0 {
		fmt.Println("No recorded tasks in this project.")
	}
	return nil
}
