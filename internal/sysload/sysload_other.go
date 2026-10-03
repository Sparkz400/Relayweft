//go:build !linux && !windows

package sysload

// CPU and memory are unknown here (macOS would need cgo or sysctl parsing);
// unknown never blocks.
func cpuTimes() (busy, total uint64, ok bool) { return 0, 0, false }
func memory() (free, total uint64, ok bool)   { return 0, 0, false }
