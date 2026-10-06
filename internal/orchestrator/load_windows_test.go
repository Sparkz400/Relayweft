//go:build windows

package orchestrator

import (
	"time"

	"golang.org/x/sys/windows"
)

// loadCPUTimes is the CPU time of this process and of its waited-for
// children (-1: not known).
type loadCPUTimes struct{ self, children time.Duration }

// loadCPU reads this process's user and kernel time. Windows keeps no
// total for finished children.
func loadCPU() loadCPUTimes {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u); err != nil {
		return loadCPUTimes{-1, -1}
	}
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) * 100
	}
	return loadCPUTimes{ft(k) + ft(u), -1}
}
