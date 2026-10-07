package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/sparkz400/relayweft/internal/orchestrator"
)

func validateBenchSuite(ws *orchestrator.BenchWorkspace, bf benchFile, tasks []benchTask, timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	failed := 0
	for _, t := range tasks {
		if t.Base == "" || t.Tests == nil || t.Tests.From == "" || len(t.Tests.Files) == 0 {
			return fmt.Errorf("%s: validation needs base and tests.from/files", t.Name)
		}
		fmt.Printf("validating %s ... ", t.Name)
		h := historyCandidate{HistoryCommit: orchestrator.HistoryCommit{SHA: t.Tests.From, Parent: t.Base}, tests: t.Tests.Files, check: t.Check}
		why := validateHistory(ctx, ws, h, historyOpts{setup: bf.Setup, timeout: timeout, clean: true})
		if why != "" {
			failed++
			fmt.Println("FAIL:", why)
		} else {
			fmt.Println("PASS: solution passes; base fails with protected tests")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d benchmark tasks failed validation", failed, len(tasks))
	}
	return nil
}
