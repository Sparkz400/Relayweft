package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/sparkz400/relayweft/internal/notify"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/workflow"
	"gopkg.in/yaml.v3"
)

func cmdWorkflow(args []string) error {
	fs := flag.NewFlagSet("rw workflow", flag.ExitOnError)
	init := fs.Bool("init", false, "install four starter workflows without replacing saved workflows")
	show := fs.String("show", "", "print a saved workflow's YAML")
	save := fs.String("save", "", "install a reviewed YAML workflow from this file")
	replace := fs.Bool("replace", false, "replace an existing workflow with --save")
	parseFlags(fs, args)
	n := 0
	if *init {
		n++
	}
	if *show != "" {
		n++
	}
	if *save != "" {
		n++
	}
	if n > 1 || fs.NArg() != 0 || *replace && *save == "" {
		return fmt.Errorf("usage: rw workflow [--init | --show name | --save file [--replace]]")
	}
	if *init {
		for _, d := range workflow.Builtins() {
			data, err := yaml.Marshal(d)
			if err != nil {
				return err
			}
			if err = workflow.Save(data, false); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}
	if *save != "" {
		data, err := os.ReadFile(*save)
		if err != nil {
			return err
		}
		return workflow.Save(data, *replace)
	}
	if *show != "" {
		d, err := workflow.Load(*show)
		if err != nil {
			return err
		}
		return yaml.NewEncoder(os.Stdout).Encode(d)
	}
	ds, err := workflow.List()
	if err != nil {
		return err
	}
	for _, d := range ds {
		fmt.Printf("%-20s %s\n", d.Name, d.Description)
	}
	if len(ds) == 0 {
		fmt.Println("No saved workflows. Run rw workflow --init.")
	} else {
		fmt.Println("Run one: rw run --workflow NAME \"describe the task\" (also with --file, --at or --in, and in rw web)")
	}
	return nil
}

// gatedApprover asks on the terminal for a workflow's approvals in an
// otherwise unattended run (a task file or a scheduled start). Nobody may be
// watching, so each question is also announced: webhooks, then a desktop
// notification. A closed stdin answers no, so nothing runs unapproved.
type gatedApprover struct {
	*termApprover
	alert func(what string)
}

func (a *gatedApprover) announce(what string) {
	if a.alert != nil {
		a.alert(what)
	}
}

func (a *gatedApprover) ApprovePlan(ctx context.Context, task string, p orchestrator.Plan) (orchestrator.Plan, bool) {
	a.announce("approve the plan: " + oneLine(task, 120))
	return a.termApprover.ApprovePlan(ctx, task, p)
}

func (a *gatedApprover) ApprovePlanEstimate(ctx context.Context, task string, p orchestrator.Plan, est func(orchestrator.Plan) orchestrator.PlanEstimate) (orchestrator.Plan, bool) {
	a.announce("approve the plan: " + oneLine(task, 120))
	return a.termApprover.ApprovePlanEstimate(ctx, task, p, est)
}

func (a *gatedApprover) ReviewChanges(ctx context.Context, cs orchestrator.ChangeSet) orchestrator.ChangeDecision {
	a.announce("review the changes of " + cs.StepID)
	return a.termApprover.ReviewChanges(ctx, cs)
}

// waitingNotify is notify.Send; tests replace it.
var waitingNotify = notify.Send

// waiting tells whoever is away that the run waits for an answer.
func (h *headless) waiting(what string) {
	h.webhook(notify.EventWaiting, "Relayweft needs you", what)
	if h.cfg.Notify.Enabled && waitingNotify("Relayweft needs you", what) != nil {
		fmt.Fprint(os.Stderr, notify.Bell())
	}
}
