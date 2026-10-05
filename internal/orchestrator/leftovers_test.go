package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparkz400/relayweft/internal/proc"
)

func TestLeftovers(t *testing.T) {
	cache := t.TempDir()
	old := cacheDir
	cacheDir = func() (string, error) { return cache, nil }
	defer func() { cacheDir = old }()
	pd := filepath.Join(cache, "relayweft", "worktrees", "abc123", "pool")
	os.MkdirAll(filepath.Join(pd, "0.trash-1a2b3c4d"), 0o755)
	os.WriteFile(filepath.Join(pd, "0.trash-1a2b3c4d", "f"), make([]byte, 1000), 0o644)
	// Slot 1: an agent (here: the go command that runs this test) still runs.
	os.MkdirAll(filepath.Join(pd, "1"), 0o755)
	os.WriteFile(filepath.Join(pd, "1.pid"), []byte(fmt.Sprintf("%d\n", os.Getppid())), 0o644)
	// Slot 2: same, but a running rw holds the slot, so it is not left behind.
	os.MkdirAll(filepath.Join(pd, "2"), 0o755)
	os.WriteFile(filepath.Join(pd, "2.pid"), []byte(fmt.Sprintf("%d\n", os.Getppid())), 0o644)
	unlock, ok := proc.TryLock(filepath.Join(pd, "2.lock"))
	if !ok {
		t.Fatal("cannot lock slot 2")
	}
	defer unlock()
	// Slot 3: the recorded agent is gone.
	os.MkdirAll(filepath.Join(pd, "3"), 0o755)
	os.WriteFile(filepath.Join(pd, "3.pid"), []byte("999999001 123\n"), 0o644)

	var kinds []string
	for _, l := range Leftovers() {
		if l.Kind == "temp" {
			continue // the machine's real temp folder
		}
		kinds = append(kinds, l.Kind+" "+filepath.Base(l.Path))
		if l.Kind == "trash" && l.Bytes != 1000 {
			t.Errorf("trash size = %d", l.Bytes)
		}
	}
	if got := strings.Join(kinds, ", "); got != "trash 0.trash-1a2b3c4d, orphan-agent 1" {
		t.Fatalf("leftovers = %s", got)
	}
}
