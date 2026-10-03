package orchestrator

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Undo:
//
//   - Every task in a git repo records two snapshot commits of the working
//     tree: before it started and after it ended (also when cancelled or
//     failed). They are kept under refs/switchyard/tasks/<key>/{before,after}
//     so git gc never deletes them; the user's branches are not touched.
//   - Undo applies the change after -> before to the working tree with the
//     same machinery as merges: files the task changed are restored, files it
//     created are removed. A file you edited after the task is 3-way merged,
//     and if that conflicts nothing at all is written.
//   - The state right before an undo is saved as .../undone, so `--redo`
//     (before -> after) puts the task's changes back.
//   - The newest 30 tasks per repo are kept.

const (
	undoRefs   = "refs/switchyard/tasks/"
	undoKeep   = 30
	subjectMax = 72
)

// UndoTask is one recorded task.
type UndoTask struct {
	Key    string
	When   time.Time
	Task   string
	Before string
	After  string
	Undone bool
}

// UndoPlan is what an undo (or redo) would do.
type UndoPlan struct {
	Task    UndoTask
	Changes []string // "M path", "A path" (will be restored), "D path" (will be deleted)
	Edited  []string // files you changed since the task; they get a 3-way merge
}

// recordSnapshot stores a task snapshot ref (best effort).
func (g git) recordSnapshot(key, which, commit string) {
	if commit == "" {
		return
	}
	g.out("update-ref", undoRefs+key+"/"+which, commit)
}

// UndoList returns the recorded tasks of the repo containing dir, newest first.
func UndoList(dir string) ([]UndoTask, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", dir)
	}
	out, err := git{root}.out("for-each-ref", "--format=%(refname)%09%(objectname)%09%(creatordate:unix)%09%(contents:subject)", undoRefs)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*UndoTask{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 4 {
			continue
		}
		rest := strings.TrimPrefix(f[0], undoRefs)
		i := strings.LastIndex(rest, "/")
		if i < 0 {
			continue
		}
		key, which := rest[:i], rest[i+1:]
		t := byKey[key]
		if t == nil {
			t = &UndoTask{Key: key}
			byKey[key] = t
		}
		sec, _ := strconv.ParseInt(f[2], 10, 64)
		switch which {
		case "before":
			t.Before = f[1]
			t.When = time.Unix(sec, 0)
			t.Task = strings.TrimPrefix(f[3], "switchyard before: ")
		case "after":
			t.After = f[1]
		case "undone":
			t.Undone = true
		}
	}
	var list []UndoTask
	for _, t := range byKey {
		if t.Before != "" && t.After != "" {
			list = append(list, *t)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].When.Equal(list[j].When) {
			return list[i].When.After(list[j].When)
		}
		return list[i].Key > list[j].Key
	})
	return list, nil
}

// findTask picks a task by key, or the newest one in the wanted state.
func findTask(dir, key string, undone bool) (UndoTask, string, error) {
	list, err := UndoList(dir)
	if err != nil {
		return UndoTask{}, "", err
	}
	root, _ := repoRoot(dir)
	for _, t := range list {
		if key != "" && t.Key == key {
			return t, root, nil
		}
		if key == "" && t.Undone == undone {
			return t, root, nil
		}
	}
	if key != "" {
		return UndoTask{}, "", fmt.Errorf("no recorded task %q (sy undo --list)", key)
	}
	if undone {
		return UndoTask{}, "", fmt.Errorf("no undone task to redo")
	}
	return UndoTask{}, "", fmt.Errorf("no task to undo in this repo (tasks are recorded when sy runs in a git repo)")
}

// PreviewUndo shows what undoing (redo=false) or redoing a task would change.
func PreviewUndo(dir, key string, redo bool) (UndoPlan, error) {
	t, root, err := findTask(dir, key, redo)
	if err != nil {
		return UndoPlan{}, err
	}
	from, to := t.After, t.Before
	if redo {
		from, to = t.Before, t.After
	}
	g := git{root}
	out, err := g.run(nil, nil, "diff", "--name-status", "--no-renames", "-z", from, to)
	if err != nil {
		return UndoPlan{}, err
	}
	plan := UndoPlan{Task: t}
	fields := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := fields[i][:1], fields[i+1]
		plan.Changes = append(plan.Changes, status+" "+path)
		if clean, err := g.unchangedSince(from, path); err == nil && !clean {
			plan.Edited = append(plan.Edited, path)
		}
	}
	return plan, nil
}

// Undo reverts a task's changes in the working tree (key "" = newest task
// not undone yet). Redo puts them back.
func Undo(dir, key string, redo bool) (UndoPlan, error) {
	plan, err := PreviewUndo(dir, key, redo)
	if err != nil {
		return plan, err
	}
	root, _ := repoRoot(dir)
	g := git{root}
	t := plan.Task
	if redo {
		if err := g.applyDiff(t.Before, t.After); err != nil {
			return plan, err
		}
		g.out("update-ref", "-d", undoRefs+t.Key+"/undone")
		return plan, nil
	}
	// Keep the exact pre-undo state so nothing is ever lost.
	if snap, err := g.snapshot("switchyard before undo of " + t.Key); err == nil {
		g.recordSnapshot(t.Key, "undone", snap)
	}
	if err := g.applyDiff(t.After, t.Before); err != nil {
		g.out("update-ref", "-d", undoRefs+t.Key+"/undone")
		return plan, err
	}
	return plan, nil
}

// trimUndo keeps the newest undoKeep tasks.
func trimUndo(root string) {
	list, err := UndoList(root)
	if err != nil || len(list) <= undoKeep {
		return
	}
	g := git{root}
	for _, t := range list[undoKeep:] {
		for _, w := range []string{"before", "after", "undone"} {
			g.out("update-ref", "-d", undoRefs+t.Key+"/"+w)
		}
	}
}

func subject(prefix, task string) string {
	task = strings.Join(strings.Fields(task), " ")
	if len(task) > subjectMax {
		task = task[:subjectMax] + "..."
	}
	return prefix + task
}
