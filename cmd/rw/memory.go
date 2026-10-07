package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func cmdMemory(args []string) error {
	fs := flag.NewFlagSet("rw memory", flag.ExitOnError)
	dir := fs.String("dir", "", "project directory")
	asJSON := fs.Bool("json", false, "print notes, sources, inclusion reasons and revision as JSON")
	task := fs.String("task", "", "show which notes a task with this text would get")
	revision := fs.String("revision", "", "revision from rw memory; required for edits")
	id := fs.String("id", "", "note id to edit or remove; omit to add")
	file := fs.String("file", "", "read the note's text from this UTF-8 file")
	remove := fs.Bool("delete", false, "remove the note named by --id")
	pin := fs.Bool("pin", false, "pin the note: every task gets it as a project convention")
	unpin := fs.Bool("unpin", false, "unpin the note")
	var refs multiFlag
	fs.Var(&refs, "ref", "source file the note describes (repeatable, or comma-separated); the note retires when they all change")
	noRefs := fs.Bool("no-refs", false, "remove the note's source files")
	refresh := fs.Bool("refresh", false, "confirm the note still holds: record its source files as they are now")
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: rw memory [--dir path] [--json] [--task text] [--revision revision [--id id] (--file note.txt | --delete | --pin | --unpin | --ref path | --no-refs | --refresh)]")
	}
	d, err := absDir(*dir)
	if err != nil {
		return err
	}
	switch {
	case *file != "" && *remove:
		return fmt.Errorf("give --file or --delete, not both")
	case *pin && *unpin:
		return fmt.Errorf("give --pin or --unpin, not both")
	case len(refs) > 0 && *noRefs:
		return fmt.Errorf("give --ref or --no-refs, not both")
	}
	c := orchestrator.MemoryChange{Revision: *revision, ID: *id, Delete: *remove, Refresh: *refresh}
	if *pin || *unpin {
		c.Pin = pin
	}
	for _, r := range refs {
		c.Refs = append(c.Refs, strings.Split(r, ",")...)
	}
	if *noRefs {
		c.Refs = []string{}
	}
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		c.Text = string(data)
	}
	if *file != "" || *remove || c.Pin != nil || c.Refs != nil || *refresh {
		if err = orchestrator.ChangeProjectMemory(d, c); err != nil {
			return err
		}
	}
	v, err := orchestrator.ProjectMemory(d, *task)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(v)
	}
	fmt.Printf("Project memory: %s\nRevision: %s\n", v.Path, v.Revision)
	if v.Task != "" {
		fmt.Printf("Selection for task: %s\n", v.Task)
	}
	for _, e := range v.Entries {
		mark := map[bool]string{true: "in", false: "out"}[e.Included]
		if e.Pinned {
			mark += ", pinned"
		}
		fmt.Printf("\n%s [%s] %s\n%s\n", e.ID, mark, e.Reason, e.Text)
		if len(e.Sources) > 0 || e.More > 0 {
			var src []string
			for _, s := range e.Sources {
				src = append(src, s.Path+" ("+s.State+")")
			}
			if e.More > 0 {
				src = append(src, fmt.Sprintf("+%d more", e.More))
			}
			fmt.Printf("  sources: %s\n", strings.Join(src, ", "))
		}
	}
	if len(v.Entries) == 0 {
		fmt.Println("No stored project notes.")
	}
	return nil
}
