package orchestrator

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestProjectMemoryEditRemoveAndRevision(t *testing.T) {
	root := gitRepo(t)
	v, err := ProjectMemory(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, Text: "Keep the public API compatible."}); err != nil {
		t.Fatal(err)
	}
	v, _ = ProjectMemory(root, "")
	if len(v.Entries) != 1 || !v.Entries[0].Included || v.Prompt != repoNotes(root, "") {
		t.Fatalf("%+v", v)
	}
	old := v
	if err = ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, ID: v.Entries[0].ID, Text: "Preserve backwards compatibility."}); err != nil {
		t.Fatal(err)
	}
	if err = ChangeProjectMemory(root, MemoryChange{Revision: old.Revision, ID: old.Entries[0].ID, Text: "lost update"}); !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("stale edit accepted: %v", err)
	}
	v, _ = ProjectMemory(root, "")
	if !strings.Contains(v.Prompt, "backwards") {
		t.Fatal(v.Prompt)
	}
	if err = ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, ID: v.Entries[0].ID, Delete: true}); err != nil {
		t.Fatal(err)
	}
	v, _ = ProjectMemory(root, "")
	if len(v.Entries) != 0 || repoNotes(root, "") != "" {
		t.Fatal("deleted note still enters prompt")
	}
	if err = ChangeProjectMemory(root, MemoryChange{Revision: v.Revision, Text: "a\n<!-- rw-note -->\nb"}); err == nil {
		t.Fatal("separator injection accepted")
	}
}

func TestProjectMemoryConcurrentNotes(t *testing.T) {
	root := gitRepo(t)
	const count = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; _ = addRepoNote(root, fmt.Sprintf("task %d", i), "done", nil) }(i)
	}
	close(start)
	wg.Wait()
	v, err := ProjectMemory(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Entries) != count {
		t.Fatalf("lost concurrent notes: got %d, want %d", len(v.Entries), count)
	}
	included := 0
	for _, e := range v.Entries {
		if e.Included {
			included++
		}
	}
	if included != notesInclude {
		t.Fatalf("included %d notes", included)
	}
	if v.Prompt != repoNotes(root, "") {
		t.Fatal("preview differs from actual prompt context")
	}
}

func TestRecoveryState(t *testing.T) {
	root := gitRepo(t)
	s := &TaskState{ID: "recover-fixture", Dir: root, Task: "repair", Status: "running", Kept: []string{"rw/fixture/conflict"}, Results: map[string]StepState{"one": {OK: true}, "two": {Err: "conflict"}}, Plan: &Plan{Subtasks: []Subtask{{ID: "one"}, {ID: "two"}}}}
	if err := s.saveErr(); err != nil {
		t.Fatal(err)
	}
	items, err := Recovery(root)
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %+v", err, items)
	}
	i := items[0]
	if i.Status != "interrupted" || !i.CanResume || i.CanUndo || i.Done != 1 || len(i.Branches) != 1 || len(i.Conflicts) != 1 {
		t.Fatalf("%+v", i)
	}
	unlock, ok := s.lock()
	if !ok {
		t.Fatal("lock")
	}
	defer unlock()
	items, _ = Recovery(root)
	if items[0].CanResume || items[0].CanUndo {
		t.Fatal("running task exposes recovery actions")
	}
	if err := s.noteBranch("rw/fixture/alternative"); err != nil {
		t.Fatal(err)
	}
	items, err = Recovery(root)
	if err != nil || len(items[0].Branches) != 2 {
		t.Fatal("preserved alternative missing from recovery", err)
	}
}
