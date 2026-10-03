package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogCrashRecover(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	defer Close()
	Logf("spawn agent=%s", "a1")
	Logf("multi\nline")
	data, _ := os.ReadFile(filepath.Join(dir, debugFile))
	if !strings.Contains(string(data), "spawn agent=a1") || !strings.Contains(string(data), "multi\n    line") {
		t.Fatalf("debug log = %q", data)
	}
	var got string
	func() {
		defer Recover("test goroutine", func(p string) { got = p })
		panic("boom")
	}()
	if got == "" {
		t.Fatal("no crash log written")
	}
	crash, _ := os.ReadFile(got)
	for _, want := range []string{"panic: boom", "test goroutine", "spawn agent=a1", "diag_test.go"} {
		if !strings.Contains(string(crash), want) {
			t.Errorf("crash log misses %q", want)
		}
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, debugFile)
	os.WriteFile(path, make([]byte, maxSize+1), 0o644)
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	defer Close()
	Logf("fresh")
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() <= maxSize {
		t.Fatalf("old log not rotated: %v", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "fresh") || len(data) > 1000 {
		t.Fatalf("new log = %d bytes", len(data))
	}
}
