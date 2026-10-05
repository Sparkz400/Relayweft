//go:build !windows && !linux && !darwin

package proc

// stamped: no way to tell a process instance from a later one with the
// same pid here, so leftover agents are never killed (the slot is skipped).
const stamped = false

func procStamp(pid int) string { return "" }
