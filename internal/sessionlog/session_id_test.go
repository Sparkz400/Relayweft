package sessionlog

import (
	"regexp"
	"testing"
)

// Two rw started in the same second get different session ids (kept
// branches and undo refs are named after them) and different log files.
func TestSessionIDsAreUnique(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir, "/a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(dir, "/b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.Session() == b.Session() || a.Path() == b.Path() {
		t.Fatalf("same session %q / file %q for two writers", a.Session(), a.Path())
	}
	re := regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{4}$`)
	if !re.MatchString(a.Session()) {
		t.Errorf("session id %q does not match %s", a.Session(), re)
	}
	var w *Writer
	if w.Session() != w.Session() {
		t.Error("nil writer's session id changes between calls")
	}
}
