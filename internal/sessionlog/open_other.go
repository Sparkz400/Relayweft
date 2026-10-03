//go:build !unix

package sessionlog

import "os"

// openNoBlock opens path for reading (no FIFOs to wait for here).
func openNoBlock(path string) (*os.File, error) { return os.Open(path) }
