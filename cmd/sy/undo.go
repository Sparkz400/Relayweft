package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/sparkz400/switchyard/internal/orchestrator"
)

// cmdUndo reverts (or re-applies) one task's changes in the working tree.
func cmdUndo(args []string) error {
	fs := flag.NewFlagSet("sy undo", flag.ExitOnError)
	dir := fs.String("dir", ".", "project directory")
	list := fs.Bool("list", false, "list the recorded tasks")
	redo := fs.Bool("redo", false, "put an undone task's changes back")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	agentOnly := fs.Bool("agent-files-only", false, "only touch files an agent reported changing (keeps your own edits made while the task ran)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: sy undo [--list] [--redo] [--yes] [--agent-files-only] [--dir <path>] [task]

Reverts the changes a task made to your files: files it changed are
restored, files it created are removed. Files you edited after the task
keep your edits (3-way merge); if that conflicts, nothing is changed.
Files that changed while the task ran but that no agent reported are
listed separately (they may be your own edits); --agent-files-only skips them.
Without a task, the newest task that is not undone yet is used.
--redo puts an undone task's changes back.
`)
	}
	// Accept flags after the task key too (sy undo <key> --yes).
	var key string
	rest := args
	for len(rest) > 0 {
		fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		if key == "" {
			key = fs.Arg(0)
		}
		rest = fs.Args()[1:]
	}

	if *list {
		tasks, err := orchestrator.UndoList(*dir)
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			fmt.Println("No recorded tasks in this repo yet.")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "TASK\tWHEN\tSTATE\tDESCRIPTION")
		for _, t := range tasks {
			state := "applied"
			if t.Undone {
				state = "undone"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Key, t.When.Format("Jan 2 15:04"), state, oneLine(t.Task, 70))
		}
		tw.Flush()
		return nil
	}

	verb := "Undo"
	if *redo {
		verb = "Redo"
	}
	plan, err := orchestrator.PreviewUndo(*dir, key, *redo)
	if err != nil {
		return err
	}
	fmt.Printf("%s task %s from %s:\n  %s\n\n", verb, plan.Task.Key, plan.Task.When.Format("Mon Jan 2 15:04"), oneLine(plan.Task.Task, 200))
	if len(plan.Changes) == 0 {
		fmt.Println("The task did not change any files; nothing to do.")
		return nil
	}
	fmt.Printf("This changes %d file(s)  (M restore, A recreate, D delete):\n", len(plan.Changes))
	for i, c := range plan.Changes {
		if i == 40 {
			fmt.Printf("  ... and %d more\n", len(plan.Changes)-40)
			break
		}
		fmt.Println("  " + c)
	}
	if len(plan.Skipped) > 0 {
		fmt.Printf("\nSubmodule changes are left as they are (update them with git submodule): %s\n", strings.Join(plan.Skipped, ", "))
	}
	if len(plan.Edited) > 0 {
		fmt.Printf("\nYou edited %d of these files after the task; your edits are kept (3-way merge).\nIf an edit overlaps, nothing at all is changed and you are told which file.\n", len(plan.Edited))
	}
	if len(plan.Unreported) > 0 {
		fmt.Printf("\nNo agent reported changing these, so they may be your own edits made while the task ran:\n  %s\n", strings.Join(plan.Unreported, "\n  "))
		if *agentOnly {
			fmt.Println("--agent-files-only: they are left as they are.")
		} else {
			fmt.Println("They are reverted too; add --agent-files-only to leave them alone.")
		}
	}
	if !*yes {
		fmt.Printf("\n%s these changes? [y/N] ", verb)
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Println("Nothing changed.")
			return nil
		}
	}
	if _, err := orchestrator.Undo(*dir, plan.Task.Key, *redo, *agentOnly); err != nil {
		return fmt.Errorf("%s failed, nothing was changed: %w", strings.ToLower(verb), err)
	}
	fmt.Printf("%s done.\n", verb)
	if !*redo {
		fmt.Printf("Changed your mind? sy undo --redo %s\n", plan.Task.Key)
	}
	return nil
}
