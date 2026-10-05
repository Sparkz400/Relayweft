//go:build unix

package main

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rw stats --merge never waits on a FIFO, named or in a folder.
func TestStatsMergeSkipsFIFO(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "aaaa000000000001.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	captureStats(t)
	done := make(chan struct{})
	var dirErr, fileErr error
	go func() {
		defer close(done)
		dirErr = cmdStats([]string{"--merge", dir})
		fileErr = cmdStats([]string{"--merge", fifo})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rw stats --merge blocks on a FIFO")
	}
	if dirErr != nil {
		t.Errorf("folder with a FIFO: %v", dirErr)
	}
	if fileErr == nil || !strings.Contains(fileErr.Error(), "not a regular file") {
		t.Errorf("a FIFO named on the command line: %v", fileErr)
	}
}
