package proc

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// stamped: procStamp works here.
const stamped = true

// szomb is a zombie's p_stat (<sys/proc.h>).
const szomb = 5

// procStamp identifies a process instance beyond its pid (its start time
// from sysctl kern.proc.pid, in microseconds), so a recorded pid that was
// reused by another program is not mistaken for a leftover agent. "" when
// the process is gone or a zombie. macOS has no /proc; without this a
// killed rw's agent could not be told from a program that got its pid
// since, and was never stopped.
func procStamp(pid int) string {
	if pid <= 0 {
		return ""
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid || k.Proc.P_stat == szomb {
		return "" // a missing process gives a short answer (EIO)
	}
	t := k.Proc.P_starttime
	if t.Sec == 0 && t.Usec == 0 {
		return ""
	}
	return strconv.FormatInt(int64(t.Sec)*1e6+int64(t.Usec), 10)
}
