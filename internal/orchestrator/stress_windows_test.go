//go:build windows

package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

// handleSlack is how far open handles may rise above the post-warm-up
// count. Windows counts thread, event and other runtime handles that come
// and go with the scheduler, so the bound is looser than for Linux fds; a
// real leak (one handle per task) still exceeds it in a 15-minute run.
const handleSlack = 100

// openHandles counts this process's open kernel handles.
func openHandles() int {
	var n uint32
	r, _, _ := procGetProcessHandleCount.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return -1
	}
	return int(n)
}

// strayProcesses counts live children of this process and other processes
// running this test binary (the fake agent CLI and its grandchild).
func strayProcesses() int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return -1
	}
	defer windows.CloseHandle(snap)
	self := uint32(os.Getpid())
	exe, _ := os.Executable()
	name := strings.ToLower(filepath.Base(exe))
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	n := 0
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == self {
			continue
		}
		if e.ParentProcessID == self || strings.ToLower(windows.UTF16ToString(e.ExeFile[:])) == name {
			n++
		}
	}
	return n
}
