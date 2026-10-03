package proc

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot.lock")
	unlock, ok := TryLock(path)
	if !ok {
		t.Fatal("first lock failed")
	}
	if _, ok := TryLock(path); ok {
		t.Fatal("second lock succeeded while the first is held")
	}
	unlock()
	unlock2, ok := TryLock(path)
	if !ok {
		t.Fatal("lock failed after unlock")
	}
	unlock2()
}

func TestCleanPathList(t *testing.T) {
	cases := []struct {
		in, want string
		bad      []string
	}{
		{`C:\a;C:\b`, `C:\a;C:\b`, nil},
		{`C:\a;"C:\b c";C:\d`, `C:\a;"C:\b c";C:\d`, nil},
		{`C:\a;"C:\semi;colon";C:\d`, `C:\a;"C:\semi;colon";C:\d`, nil},
		{`C:\CMake\bin;C:\Program Files\PowerShell\7";C:\Git\cmd`,
			`C:\CMake\bin;C:\Program Files\PowerShell\7;C:\Git\cmd`,
			[]string{`C:\Program Files\PowerShell\7"`}},
		{`"C:\open;C:\Git\cmd`, `C:\open;C:\Git\cmd`, []string{`"C:\open`}},
		{`C:\x\";"C:\ok";C:\y`, `C:\x\;"C:\ok";C:\y`, []string{`C:\x\"`}},
	}
	for _, c := range cases {
		got, bad := cleanPathList(c.in)
		if got != c.want || !reflect.DeepEqual(bad, c.bad) {
			t.Errorf("cleanPathList(%q) = %q, %q; want %q, %q", c.in, got, bad, c.want, c.bad)
		}
	}
}
