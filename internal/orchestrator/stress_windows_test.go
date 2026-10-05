//go:build windows

package orchestrator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

// handleSlack is how far open handles may rise above the post-warm-up
// count. Windows counts thread, event and other runtime handles that come
// and go with the scheduler, so the bound is looser than for Linux fds.
// Four 30-minute CI runs on windows-latest (Oct 2026) rose at most 6 above
// warm-up in demo mode and 12 in git mode (~290 tasks), so 40 leaves 3x
// headroom while a leak of one handle per ~7 git tasks still exceeds it.
const handleSlack = 40

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
// running this test binary (the fake agent CLI and its grandchild). The
// binary is matched by full path, not by name: `go test` of this package
// in another checkout or session runs an orchestrator.test.exe of its own
// from a different build directory, and must not count as a leak.
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
		if e.ParentProcessID == self {
			n++
			continue
		}
		if strings.ToLower(windows.UTF16ToString(e.ExeFile[:])) == name && sameImage(e.ProcessID, exe) {
			n++
		}
	}
	return n
}

// sameImage reports whether process pid runs the executable at path. A
// process that cannot be queried counts as the same, so a leak is never
// hidden by an access error.
func sameImage(pid uint32, path string) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return true
	}
	return strings.EqualFold(filepath.Clean(windows.UTF16ToString(buf[:size])), filepath.Clean(path))
}

// TestStrayHelperSleep is the process TestSameImage starts; it does
// nothing in a normal run.
func TestStrayHelperSleep(t *testing.T) {
	if os.Getenv("RW_STRAY_HELPER") != "1" {
		t.Skip("helper process for TestSameImage")
	}
	time.Sleep(30 * time.Second)
}

// A copy of this test binary in another directory (a `go test` of this
// package from another checkout) is not this binary, so the stress test
// does not count it as a leftover process; this process itself is.
func TestSameImage(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !sameImage(uint32(os.Getpid()), exe) {
		t.Fatal("sameImage(self) = false")
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), filepath.Base(exe))
	if err := os.WriteFile(other, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(other, "-test.run=^TestStrayHelperSleep$")
	cmd.Env = append(os.Environ(), "RW_STRAY_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if sameImage(uint32(cmd.Process.Pid), exe) {
		t.Fatalf("sameImage(copy in %s) = true", other)
	}
	if !sameImage(uint32(cmd.Process.Pid), other) {
		t.Fatal("sameImage(copy, its own path) = false")
	}
}
