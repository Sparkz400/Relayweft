package main

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeProgram struct {
	mu    sync.Mutex
	sent  []tea.Msg
	quits int
}

func (f *fakeProgram) Send(m tea.Msg) { f.mu.Lock(); f.sent = append(f.sent, m); f.mu.Unlock() }
func (f *fakeProgram) Quit()          { f.mu.Lock(); f.quits++; f.mu.Unlock() }

func (f *fakeProgram) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent), f.quits
}

// Closing the TUI's window (on Windows CTRL_CLOSE_EVENT arrives as
// SIGTERM) must end the TUI without cancelling the task: bubbletea's own
// handler quit as for a key press, and rw then cancelled the task, so rw
// resume refused it (rw selftest's window-close check found it). Ctrl+C
// as a signal still interrupts.
func TestTUISignals(t *testing.T) {
	sig := make(chan os.Signal)
	done := make(chan struct{})
	var term atomic.Bool
	p := &fakeProgram{}
	go forwardTUISignals(p, sig, &term, done)
	defer close(done)

	sig <- os.Interrupt
	waitFor(t, func() bool { n, _ := p.counts(); return n == 1 })
	if _, ok := p.sent[0].(tea.InterruptMsg); !ok || term.Load() {
		t.Fatalf("Ctrl+C: sent %#v, terminated %v; want an InterruptMsg, not terminated", p.sent[0], term.Load())
	}
	sig <- syscall.SIGTERM
	waitFor(t, func() bool { _, q := p.counts(); return q == 1 })
	if !term.Load() {
		t.Fatal("SIGTERM did not mark the TUI as terminated: its task would be cancelled")
	}
	if n, _ := p.counts(); n != 1 {
		t.Errorf("SIGTERM sent a message besides quitting: %#v", p.sent)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("timed out")
		}
	}
}
