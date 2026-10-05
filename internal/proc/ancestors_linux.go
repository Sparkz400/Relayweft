//go:build linux

package proc

import (
	"os"
	"strconv"
	"strings"
)

const caseFold = false

// ancestors reads /proc/<pid>/stat up the chain.
func ancestors(pid int) []Ancestor {
	var out []Ancestor
	// cur is the next process up; below started the one under it.
	_, cur, below, ok := procStat(pid)
	for ok && len(out) < maxAncestors && cur > 0 {
		name, parent, start, found := procStat(cur)
		if !found || start > below {
			break // gone, or a pid reused since
		}
		if exe, err := os.Readlink("/proc/" + strconv.Itoa(cur) + "/exe"); err == nil {
			name = exe
		}
		out = append(out, Ancestor{PID: cur, Name: ProgramName(strings.TrimSuffix(name, " (deleted)"))})
		if parent == cur {
			break
		}
		cur, below = parent, start
	}
	return out
}

// procStat reads a process's name, parent and start time (clock ticks
// after boot) from /proc/<pid>/stat.
func procStat(pid int) (name string, parent int, start uint64, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, 0, false
	}
	s := string(data)
	// The name is in parentheses and may contain spaces and ')'.
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return "", 0, 0, false
	}
	name = s[open+1 : end]
	f := strings.Fields(s[end+1:])
	// f[0] is the state, f[1] the parent, f[19] the start time.
	if len(f) < 20 {
		return "", 0, 0, false
	}
	parent, err1 := strconv.Atoi(f[1])
	start, err2 := strconv.ParseUint(f[19], 10, 64)
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	return name, parent, start, true
}
