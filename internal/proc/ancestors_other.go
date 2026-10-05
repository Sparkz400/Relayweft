//go:build !windows && !linux && !darwin

package proc

const caseFold = false

// ancestors is not implemented here (BSDs): `rw mcp` relies on the
// RW_AGENT marker alone.
func ancestors(pid int) []Ancestor { return nil }
