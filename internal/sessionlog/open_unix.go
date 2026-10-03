//go:build unix

package sessionlog

import (
	"os"
	"syscall"
)

// openNoBlock opens path for reading without waiting for a FIFO's writer
// (O_NONBLOCK; regular files read as usual). The caller still checks the
// file is regular before reading.
func openNoBlock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
