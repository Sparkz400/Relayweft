//go:build windows

package proc

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const caseFold = true

// ancestors walks a snapshot of the process table. Windows keeps a dead
// parent's pid in its children, and may give that pid to a new program:
// a "parent" created after its child is not the parent.
func ancestors(pid int) []Ancestor {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	type entry struct {
		parent int
		name   string
	}
	table := map[int]entry{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		table[int(pe.ProcessID)] = entry{parent: int(pe.ParentProcessID), name: windows.UTF16ToString(pe.ExeFile[:])}
	}
	var out []Ancestor
	child, childStart := pid, created(pid)
	for len(out) < maxAncestors {
		e, ok := table[child]
		if !ok || e.parent <= 0 || e.parent == child {
			break
		}
		p, ok := table[e.parent]
		if !ok {
			break // the parent is gone
		}
		start := created(e.parent)
		if start == 0 || childStart != 0 && start > childStart {
			break // gone or not readable, or a reused pid
		}
		out = append(out, Ancestor{PID: e.parent, Name: ProgramName(p.name)})
		child, childStart = e.parent, start
	}
	return out
}

// created is a process's creation time (0 when it cannot be read).
func created(pid int) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	return c.Nanoseconds()
}
