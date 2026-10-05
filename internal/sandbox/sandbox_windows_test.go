package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The same project reached by its 8.3 short name (C:\Users\RUNNER~1, as
// TEMP is on GitHub's runners), in other letter case, or by its long name
// (what a worktree's .git file holds) gets one home, so sessions are
// shared. Found by CI on Windows.
func TestProjectHomeShortName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "A Long Project Name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(dir)
	buf := make([]uint16, 1024)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	short := windows.UTF16ToString(buf[:n])
	if err != nil || n == 0 || strings.EqualFold(short, dir) {
		t.Skipf("no 8.3 short names on this volume (%v)", err)
	}
	h1, err1 := ProjectHome(dir, "claude")
	h2, err2 := ProjectHome(short, "claude")
	h3, err3 := ProjectHome(strings.ToUpper(dir), "claude")
	if err1 != nil || err2 != nil || err3 != nil || h1 != h2 || h1 != h3 {
		t.Errorf("homes differ: %s (%s), %s (%s), %s", h1, dir, h2, short, h3)
	}
}
