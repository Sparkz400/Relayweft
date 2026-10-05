package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Recent (shell completion, on every Tab) reads only the newest state
// files, whatever is lying around, and filters by folder.
func TestRecentReadsOnlyTheNewest(t *testing.T) {
	old := stateDir
	dir := t.TempDir()
	stateDir = func() string { return dir }
	t.Cleanup(func() { stateDir = old })

	project := t.TempDir()
	other := t.TempDir()
	base := time.Now().Add(-time.Hour)
	write := func(i int, in string) string {
		id := fmt.Sprintf("t%03d", i)
		s := TaskState{ID: id, Task: "task " + id, Dir: in, Status: "done", Created: base.Add(time.Duration(i) * time.Second)}
		data, _ := json.Marshal(s)
		p := filepath.Join(dir, id+".json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		mod := base.Add(time.Duration(i) * time.Second)
		os.Chtimes(p, mod, mod)
		return id
	}
	for i := 0; i < recentReads+30; i++ {
		in := project
		if i%2 == 1 {
			in = other
		}
		write(i, in)
	}
	// Not a state: ignored.
	os.WriteFile(filepath.Join(dir, "zzz.json"), []byte("{not json"), 0o644)

	all := Recent("", 0)
	if len(all) != recentReads-1 { // the broken file is among the newest
		t.Fatalf("read %d states, want %d", len(all), recentReads-1)
	}
	if all[0].ID != fmt.Sprintf("t%03d", recentReads+29) {
		t.Errorf("newest first: got %s", all[0].ID)
	}
	mine := Recent(project, 5)
	if len(mine) != 5 {
		t.Fatalf("got %d, want 5", len(mine))
	}
	for _, s := range mine {
		if !SamePath(s.Dir, project) {
			t.Errorf("%s is from %s", s.ID, s.Dir)
		}
	}
	if mine[0].Created.Before(mine[4].Created) {
		t.Error("not newest first")
	}
}
