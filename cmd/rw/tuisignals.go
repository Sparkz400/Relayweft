package main

import (
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// The TUI handles signals itself instead of bubbletea.
//
// bubbletea quits on SIGTERM as if the user had quit, and rw then cancelled
// the running task. But SIGTERM is not the user cancelling: on Windows it
// is the console window being closed (CTRL_CLOSE_EVENT; Windows ends the
// process about 5 seconds later), a logoff or a shutdown; elsewhere kill or
// a service manager. A cancelled task is over: rw history listed it as
// cancelled, and rw resume refused it (without --force, which starts the
// interrupted step over). Closing the window lost the task.
//
// Now SIGTERM ends the TUI without cancelling anything. rw exits, the job
// object (Windows) or the wrapper's process group (elsewhere) stops the
// agents, and the task is left interrupted, exactly as when rw is killed:
// rw resume continues it, and its agent's session. Ctrl+C arriving as a
// signal (input that is not a terminal) interrupts as before.

// tuiProgram is what the signals are forwarded to (a *tea.Program).
type tuiProgram interface {
	Send(tea.Msg)
	Quit()
}

// forwardTUISignals forwards the signals from sig to p until done is
// closed. SIGTERM sets terminated.
func forwardTUISignals(p tuiProgram, sig <-chan os.Signal, terminated *atomic.Bool, done <-chan struct{}) {
	for {
		select {
		case s := <-sig:
			if s == syscall.SIGTERM {
				terminated.Store(true)
				p.Quit()
				continue
			}
			p.Send(tea.InterruptMsg{})
		case <-done:
			return
		}
	}
}

// handleTUISignals takes over the TUI's signals (create p with
// tea.WithoutSignalHandler). terminated reports whether rw was told to end
// (SIGTERM); stop ends the forwarding.
func handleTUISignals(p tuiProgram) (terminated func() bool, stop func()) {
	var term atomic.Bool
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go forwardTUISignals(p, sig, &term, done)
	return term.Load, func() {
		signal.Stop(sig)
		close(done)
	}
}
