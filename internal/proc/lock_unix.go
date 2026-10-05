//go:build !windows

package proc

import (
	"os"
	"syscall"
)

func openLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
}

func tryLock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

// tryLockShared takes a shared lock (released when f is closed).
func tryLockShared(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil
}
