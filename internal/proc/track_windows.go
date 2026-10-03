//go:build windows

package proc

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// procStamp identifies a process instance beyond its pid (its creation
// time), so a recorded pid that Windows reused for another program is not
// mistaken for a leftover agent. "" when the process is gone.
func procStamp(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil || code != 259 { // STILL_ACTIVE
		return ""
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return ""
	}
	return strconv.FormatInt(created.Nanoseconds(), 10)
}

// reap only reports whether a recorded agent is still running: sy's job
// object kills agents with sy, so a live one is rare, and the directory is
// skipped rather than killing a process tree from the outside.
func reap(pid int, stamp string) bool {
	cur := procStamp(pid)
	return cur == "" || (stamp != "" && cur != stamp)
}
