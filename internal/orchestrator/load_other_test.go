//go:build !windows

package orchestrator

import (
	"syscall"
	"time"
)

// loadCPUTimes is the CPU time of this process and of its waited-for
// children (-1: not known).
type loadCPUTimes struct{ self, children time.Duration }

// loadCPU reads getrusage for this process and its finished children (the
// agents and git).
func loadCPU() loadCPUTimes {
	get := func(who int) time.Duration {
		var ru syscall.Rusage
		if err := syscall.Getrusage(who, &ru); err != nil {
			return -1
		}
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	return loadCPUTimes{get(syscall.RUSAGE_SELF), get(syscall.RUSAGE_CHILDREN)}
}
