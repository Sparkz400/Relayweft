package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

// runEstimate implements `rw run --estimate`: plan the task, print the plan
// with its estimate and stop. Only the planner runs (read-only); nothing
// in the working tree changes.
func runEstimate(c *common, quiet bool, task string) error {
	h, err := startHeadless(c, quiet, nil)
	if err != nil {
		return err
	}
	defer h.close()
	plan, est, err := h.orc.Estimate(h.ctx, task)
	h.drain()
	if err != nil {
		return err
	}
	printPlan(os.Stdout, plan, &est)
	fmt.Println("\nestimate only: nothing was run. Drop --estimate to run it (add --approve to check the plan first).")
	return nil
}

// printEstimateTotal writes the estimate's total and its budget warnings.
func printEstimateTotal(w io.Writer, e orchestrator.PlanEstimate) {
	if se, ok := e.Step(orchestrator.FinalReviewID); ok {
		fmt.Fprintf(w, "  +  final review ~ %s on %s: %s\n", se.Role, se.Route, se.Line())
	}
	fmt.Fprintf(w, "Estimate: %s\n", e.TotalLine())
	fmt.Fprintln(w, "  (median, 25th-75th percentile range; $ is Claude's API-equivalent price; fix rounds and retries come on top)")
	for _, warn := range e.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", warn)
	}
}
