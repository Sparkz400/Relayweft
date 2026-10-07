package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Reports live outside the disposable workspace. Each checkpoint replaces the
// JSON first; an interrupted Markdown refresh cannot lose machine-readable rows.
type benchEvidence struct {
	path, commit string
	file         benchFile
}

func newBenchEvidence(commit string, bf benchFile) (*benchEvidence, error) {
	f, err := os.CreateTemp(".", "bench-results-"+time.Now().Format("20060102-150405")+"-*.json")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return nil, err
	}
	e := &benchEvidence{path: path, commit: commit, file: bf}
	return e, e.save(nil)
}

func (e *benchEvidence) runDir(n int) (string, error) {
	p := e.path[:len(e.path)-len(".json")] + fmt.Sprintf("/run-%03d", n)
	return p, os.MkdirAll(p, 0700)
}

func (e *benchEvidence) save(rs []benchResult) error {
	if err := writeBenchResults(e.path, e.commit, e.file, rs); err != nil {
		return fmt.Errorf("save benchmark checkpoint: %w", err)
	}
	md := "# rw bench results\n\nCommit " + e.commit + "\n\nUpdated after each run and before its check. A running/checking row means the process stopped before a final result was saved. Wall time includes setup, preflight, agents and checks. Usage marked incomplete is a lower bound. Artifact paths are relative to this report.\n\n```\n" + benchReport(rs, e.file.Modes) + "```\n"
	return benchAtomicWrite(e.path[:len(e.path)-len(".json")]+".md", []byte(md))
}

// Write to a sibling and sync before rename, including on Windows. A failed
// replacement leaves the previous valid checkpoint intact and stops the bench.
func benchAtomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".bench-checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
