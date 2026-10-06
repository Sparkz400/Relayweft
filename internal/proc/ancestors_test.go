package proc

import (
	"os"
	"runtime"
	"testing"
)

// The test binary runs under `go test`: its parent is the first ancestor.
func TestAncestors(t *testing.T) {
	switch runtime.GOOS {
	case "windows", "linux", "darwin":
	default:
		t.Skip("not implemented here")
	}
	a := Ancestors()
	if len(a) == 0 {
		t.Fatal("no ancestors")
	}
	if a[0].PID != os.Getppid() {
		t.Errorf("first ancestor %+v, parent pid %d", a[0], os.Getppid())
	}
	if a[0].Name == "" {
		t.Error("no name")
	}
	seen := map[int]bool{os.Getpid(): true}
	for _, x := range a {
		if seen[x.PID] {
			t.Fatalf("pid %d twice: %+v", x.PID, a)
		}
		seen[x.PID] = true
	}
}

func TestProgramName(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/rw": "rw",
		"rw":                "rw",
		`C:\Tools\rw.exe`:   "rw",
		"node":              "node",
	}
	if caseFold {
		cases[`C:\Tools\RW.EXE`] = "rw"
	}
	for in, want := range cases {
		if got := ProgramName(in); got != want {
			t.Errorf("ProgramName(%q) = %q, want %q", in, got, want)
		}
	}
}
