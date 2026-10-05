package proc

import (
	"os"
	"strconv"
	"strings"
)

// stamped: procStamp works here.
const stamped = true

// procStamp identifies a process instance beyond its pid (its start time
// in /proc/<pid>/stat), so a recorded pid that was reused by another
// program is not mistaken for a leftover agent. "" when the process is
// gone or a zombie.
func procStamp(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state); start time is field 22
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return ""
	}
	return f[19]
}
