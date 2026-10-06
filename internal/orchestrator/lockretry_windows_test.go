//go:build windows

package orchestrator

import (
	"testing"

	"golang.org/x/sys/windows"
)

// holdUnreadable keeps path open without sharing, as a virus scanner or
// another program may, so git cannot read it until release.
func holdUnreadable(t *testing.T, path string) (release func()) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { windows.CloseHandle(h) }
}
