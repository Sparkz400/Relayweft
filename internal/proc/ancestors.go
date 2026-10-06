package proc

import (
	"os"
	"path/filepath"
	"strings"
)

// Ancestor is a process above this one: its pid and the base name of its
// program ("rw.exe", "node", "claude").
type Ancestor struct {
	PID  int
	Name string
}

// maxAncestors bounds the walk (a pid loop must not hang it).
const maxAncestors = 64

// Ancestors returns this process's parent, the parent's parent and so on,
// nearest first. It is best effort: it stops where a parent is gone or
// cannot be read, and returns nil where the system gives no way to tell.
// A parent that started after its child is a pid the system reused, and
// ends the walk.
func Ancestors() []Ancestor { return ancestors(os.Getpid()) }

// ProgramName is a program's base name without .exe, in lower case on
// Windows and macOS (case-insensitive file systems): "rw" for
// C:\Tools\RW.EXE and for /usr/local/bin/rw.
func ProgramName(path string) string {
	base := filepath.Base(strings.ReplaceAll(path, `\`, "/"))
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		base = base[:len(base)-4]
	}
	if caseFold {
		base = strings.ToLower(base)
	}
	return base
}
