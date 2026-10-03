//go:build windows

package proc

import (
	"os"

	"golang.org/x/sys/windows"
)

func tryLock(f *os.File) bool {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol) == nil
}
