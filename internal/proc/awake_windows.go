//go:build windows

package proc

import (
	"runtime"

	"golang.org/x/sys/windows"
)

var procSetThreadExecutionState = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

// startAwake sets the execution state on a thread of its own: the state
// belongs to the calling thread, and goroutines move between threads, so
// one goroutine locked to its thread holds it until stopped.
func startAwake() func() {
	if procSetThreadExecutionState.Find() != nil {
		return nil
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired))
		<-stop
		_, _, _ = procSetThreadExecutionState.Call(uintptr(esContinuous))
	}()
	return func() {
		close(stop)
		<-done
	}
}
