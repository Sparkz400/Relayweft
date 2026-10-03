//go:build unix

package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO, a symbolic link or a folder in the team folder is skipped with a
// warning, and reading never waits on a FIFO.
func TestTeamDirSkipsSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	other := "2222222222222222"
	if err := WriteTeamFile(dir, BuildExport(exportRecs(now), ExportOptions{Machine: other, Now: now})); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "3333333333333333.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	os.Symlink(TeamFile(dir, other), filepath.Join(dir, "4444444444444444.json"))
	os.Mkdir(filepath.Join(dir, "5555555555555555.json"), 0o755)

	done := make(chan struct{})
	var exps []Export
	var warns []string
	var rerr, ferr error
	go func() {
		defer close(done)
		exps, warns, rerr = ReadTeamDir(dir, "1111111111111111", now)
		_, ferr = ReadExport(fifo) // named directly: must not wait for a writer
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reading the team folder blocks on a FIFO")
	}
	if rerr != nil || len(exps) != 1 || exps[0].Machine != other {
		t.Fatalf("read %+v, %v", exps, rerr)
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"3333333333333333.json: not a regular file", "4444444444444444.json: a symbolic link", "5555555555555555.json: a folder"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if ferr == nil || !strings.Contains(ferr.Error(), "not a regular file") {
		t.Errorf("ReadExport of a FIFO: %v", ferr)
	}
}
