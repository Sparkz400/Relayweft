//go:build darwin

package proc

import (
	"bytes"

	"golang.org/x/sys/unix"
)

const caseFold = true

// ancestors asks the kernel (sysctl kern.proc.pid) up the chain. The name
// is the kernel's short one (16 characters), enough for "rw".
func ancestors(pid int) []Ancestor {
	var out []Ancestor
	self, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil
	}
	// cur is the next process up; below started the one under it.
	cur, below := int(self.Eproc.Ppid), self.Proc.P_starttime.Nano()
	for len(out) < maxAncestors && cur > 0 {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", cur)
		if err != nil || kp.Proc.P_pid != int32(cur) {
			break // gone
		}
		start := kp.Proc.P_starttime.Nano()
		if start > below {
			break // a pid reused since
		}
		name := kp.Proc.P_comm[:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		out = append(out, Ancestor{PID: cur, Name: ProgramName(string(name))})
		parent := int(kp.Eproc.Ppid)
		if parent == cur {
			break
		}
		cur, below = parent, start
	}
	return out
}
