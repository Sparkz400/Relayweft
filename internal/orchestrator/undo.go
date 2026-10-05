package orchestrator

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Undo:
//
//   - Every task in a git repo records two snapshot commits of the working
//     tree: before it started and after it ended (also when cancelled or
//     failed). They are kept under
//     refs/relayweft/tasks/<worktree>/<key>/{before,after} so git gc never
//     deletes them; the user's branches are not touched. <worktree> keeps
//     the history of each working tree (main tree, other worktrees) apart.
//   - The after snapshot's message lists the files the agents reported or
//     merged ("agent files"). A file that changed during the task but is not
//     on that list may be the user's own edit made while the task ran.
//   - Undo applies the change after -> before to the working tree with the
//     same machinery as merges: files the task changed are restored, files it
//     created are removed. A file you edited after the task is 3-way merged,
//     and if that conflicts nothing at all is written.
//   - The state right before an undo is saved as .../undone, so a redo
//     (before -> after) puts the task's changes back.
//   - The newest 30 tasks per working tree are kept. An extra repo's part
//     of a multi-repo task is kept beyond that while the task's primary
//     repo still has its record (undo from the primary needs every part).

const (
	undoRefs       = "refs/relayweft/tasks/"
	undoKeep       = 30
	subjectMax     = 72
	agentFilesMark = "agent-files:"
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
	Changes []string // "M path", "A path", "D path" relative to the repo root
	Edited  []string // files you changed after the task; they get a 3-way merge
	// Unreported are files that changed during the task although no agent
	// reported changing them: possibly your own edits made while it ran.
	// AgentOnly undo leaves them alone.
	Unreported []string
	// Skipped are submodule (gitlink) changes: undo cannot check out
	// another commit of a submodule, so they are left as they are.
	Skipped []string
	// Multi-repo tasks (workspace.go): Repo and Dir name an extra repo's
	// plan; the primary's plan lists them in Others. Missing are extra
	// repos whose folder no longer exists (left out).
	Repo    string
	Dir     string
	Others  []UndoPlan
	Missing []string
}

// TotalChanges counts the changed files of every repo.
func (p UndoPlan) TotalChanges() int {
	n := len(p.Changes)
	for _, o := range p.Others {
		n += len(o.Changes)
	}
	return n
}

// undoPrefix is the ref namespace of one working tree.
func undoPrefix(root string) string {
	h := sha1.Sum([]byte(canonPath(root)))
	return undoRefs + hex.EncodeToString(h[:])[:10] + "/"
}

// recordSnapshot stores a task snapshot ref (best effort). g.dir must be the
// working tree's top level.
func (g git) recordSnapshot(key, which, commit string) {
	if commit == "" {
		return
	}
	g.out("update-ref", undoPrefix(g.dir)+key+"/"+which, commit)
}

func (g git) deleteSnapshot(key, which string) {
	g.out("update-ref", "-d", undoPrefix(g.dir)+key+"/"+which)
}

// UndoList returns the recorded tasks of the working tree containing dir,
// newest first.
func UndoList(dir string) ([]UndoTask, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", dir)
	}
	prefix := undoPrefix(root)
	out, err := git{root}.out("for-each-ref", "--format=%(refname)%09%(objectname)%09%(creatordate:unix)%09%(contents:subject)", prefix)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*UndoTask{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) < 4 {
			continue
		}
		rest := strings.TrimPrefix(f[0], prefix)
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
			t.Task = strings.TrimPrefix(f[3], "relayweft before: ")
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

// findTask picks a task by key, or the newest one in the wanted state
// (undone=true for redo). A keyed task in the wrong state is refused.
func findTask(dir, key string, undone bool) (UndoTask, string, error) {
	list, err := UndoList(dir)
	if err != nil {
		return UndoTask{}, "", err
	}
	root, _ := repoRoot(dir)
	for _, t := range list {
		if key != "" && t.Key == key {
			switch {
			case undone && !t.Undone:
				return UndoTask{}, "", fmt.Errorf("task %s is not undone, nothing to redo", key)
			case !undone && t.Undone:
				return UndoTask{}, "", fmt.Errorf("task %s is already undone (rw undo --redo %s puts it back)", key, key)
			}
			return t, root, nil
		}
		if key == "" && t.Undone == undone {
			return t, root, nil
		}
	}
	if key != "" {
		return UndoTask{}, "", fmt.Errorf("no recorded task %q in this working tree (rw undo --list)", key)
	}
	if undone {
		return UndoTask{}, "", fmt.Errorf("no undone task to redo")
	}
	return UndoTask{}, "", fmt.Errorf("no task to undo in this repo (tasks are recorded when rw runs in a git repo)")
}

// agentFiles reads the agent-reported files from an after snapshot; ok is
// false for snapshots that carry no list.
func (g git) agentFiles(after string) (map[string]bool, bool) {
	msg, err := g.run(nil, nil, "log", "-1", "--format=%B", after)
	if err != nil {
		return nil, false
	}
	i := strings.Index(msg, agentFilesMark)
	if i < 0 {
		return nil, false
	}
	set := map[string]bool{}
	for _, l := range strings.Split(msg[i+len(agentFilesMark):], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			set[agentKey(l)] = true
		}
	}
	return set, true
}

// agentKey is how an agent-reported path is compared with git's: noteFiles
// records paths through canon.Path, which lower-cases them on Windows
// (whose file names ignore case), so there the comparison ignores case too.
// Without it every file with an upper-case letter looked unreported on
// Windows, and unattended pull requests and rw watch pushes were refused.
func agentKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// PreviewUndo shows what undoing (redo=false) or redoing a task would change.
// For a multi-repo task started here, Others previews its extra repos.
func PreviewUndo(dir, key string, redo bool) (UndoPlan, error) {
	plan, err := previewOne(dir, key, redo)
	if err != nil {
		return plan, err
	}
	root, _ := repoRoot(dir)
	plan.Others, plan.Missing, err = workspaceUndoPlans(root, plan.Task, redo)
	return plan, err
}

// previewOne is PreviewUndo for one repo.
func previewOne(dir, key string, redo bool) (UndoPlan, error) {
	t, root, err := findTask(dir, key, redo)
	if err != nil {
		return UndoPlan{}, err
	}
	from, to := t.After, t.Before
	if redo {
		from, to = t.Before, t.After
	}
	g := git{root}
	entries, err := g.rawDiff(from, to, nil)
	if err != nil {
		return UndoPlan{}, err
	}
	plan := UndoPlan{Task: t}
	agents, known := g.agentFiles(t.After)
	var paths []string
	for _, e := range entries {
		if !e.gitlink() {
			paths = append(paths, e.path)
		}
	}
	// One batch hash for every file (a process per file is slow on Windows).
	ids, _, idErr := g.worktreeIDs(paths)
	for _, e := range entries {
		if e.gitlink() {
			plan.Skipped = append(plan.Skipped, e.path)
			continue
		}
		plan.Changes = append(plan.Changes, e.status+" "+e.path)
		if idErr == nil && ids[e.path] != e.want() {
			plan.Edited = append(plan.Edited, e.path)
		}
		if known && !agents[agentKey(e.path)] {
			plan.Unreported = append(plan.Unreported, e.path)
		}
	}
	return plan, nil
}

// Undo reverts a task's changes in the working tree (key "" = newest task
// not undone yet); redo puts them back. With agentOnly, files no agent
// reported (possibly edits you made while the task ran) are left alone.
//
// A multi-repo task is undone in every repo it changed: nothing is written
// unless every repo's preview works, and if one repo fails the repos done
// before it are put back.
func Undo(dir, key string, redo, agentOnly bool) (UndoPlan, error) {
	plan, err := PreviewUndo(dir, key, redo)
	if err != nil {
		return plan, err
	}
	all := append([]UndoPlan{plan}, plan.Others...)
	dirs := append([]string{dir}, make([]string, len(plan.Others))...)
	only := make([][]string, len(all))
	for i, p := range all {
		if i > 0 {
			dirs[i] = p.Dir
		}
		if only[i], err = agentOnlyPaths(p, agentOnly); err != nil {
			if i > 0 {
				err = fmt.Errorf("repo %s: %w", p.Repo, err)
			}
			return plan, err
		}
	}
	for i, p := range all {
		if err := undoRepo(dirs[i], p.Task, redo, only[i]); err != nil {
			if i > 0 {
				err = fmt.Errorf("repo %s: %w", p.Repo, err)
			}
			// Put the repos done so far back, and say which could not be.
			var stuck []string
			for j := i - 1; j >= 0; j-- {
				if berr := undoRepo(dirs[j], all[j].Task, !redo, only[j]); berr != nil {
					stuck = append(stuck, fmt.Sprintf("%s is still %s (putting it back failed: %v)", repoLabel(all[j]), doneWord(redo), berr))
				} else {
					stuck = append(stuck, repoLabel(all[j])+" was put back")
				}
			}
			if len(stuck) > 0 {
				err = fmt.Errorf("%w; %s; %s and the repos after it were not changed", err, strings.Join(stuck, "; "), repoLabel(p))
			} else {
				err = fmt.Errorf("%w; nothing was changed", err)
			}
			return plan, err
		}
	}
	if !redo {
		// An undone task is not resumed: free the pool worktrees its
		// interrupted steps held.
		releaseHolds(key)
	}
	return plan, nil
}

// repoLabel names a repo of an undo plan in messages.
func repoLabel(p UndoPlan) string {
	if p.Repo == "" {
		return "repo " + PrimaryRepo
	}
	return "repo " + p.Repo
}

func doneWord(redo bool) string {
	if redo {
		return "redone"
	}
	return "undone"
}

// agentOnlyPaths is the path list of an --agent-files-only undo (nil = all).
func agentOnlyPaths(plan UndoPlan, agentOnly bool) ([]string, error) {
	var only []string
	if agentOnly && len(plan.Unreported) > 0 {
		skip := map[string]bool{}
		for _, p := range plan.Unreported {
			skip[p] = true
		}
		for _, c := range plan.Changes {
			if p := c[2:]; !skip[p] {
				only = append(only, p)
			}
		}
		if len(only) == 0 {
			return nil, fmt.Errorf("every changed file is unreported; nothing to do with --agent-files-only")
		}
	}
	return only, nil
}

// undoRepo is undoOne; tests replace it to make a repo fail.
var undoRepo = undoOne

// undoOne undoes (or redoes) task t in the repo containing dir.
func undoOne(dir string, t UndoTask, redo bool, only []string) error {
	root, _ := repoRoot(dir)
	g := git{root}
	if redo {
		if err := g.applyDiff(t.Before, t.After, only...); err != nil {
			return err
		}
		g.deleteSnapshot(t.Key, "undone")
		return nil
	}
	// Keep the exact pre-undo state so nothing is ever lost.
	if snap, err := g.snapshot("relayweft before undo of " + t.Key); err == nil {
		g.recordSnapshot(t.Key, "undone", snap)
	}
	if err := g.applyDiff(t.After, t.Before, only...); err != nil {
		g.deleteSnapshot(t.Key, "undone")
		return err
	}
	return nil
}

// trimUndo keeps the newest undoKeep tasks of a working tree. An extra
// repo's part of a multi-repo task is kept beyond that for as long as the
// task's primary repo keeps its record.
func trimUndo(root string) {
	list, err := UndoList(root)
	if err != nil || len(list) <= undoKeep {
		return
	}
	g := git{root}
	for _, t := range list[undoKeep:] {
		if p := g.primaryOf(t.Before); p != "" && canonPath(p) != canonPath(root) {
			if _, err := (git{p}).out("rev-parse", "--verify", "-q", undoPrefix(p)+t.Key+"/before"); err == nil {
				continue
			}
		}
		for _, w := range []string{"before", "after", "undone"} {
			g.deleteSnapshot(t.Key, w)
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

// afterMessage is the after snapshot's message with the agent file list.
func afterMessage(task string, files map[string]bool) string {
	var list []string
	for f := range files {
		list = append(list, f)
	}
	sort.Strings(list)
	return subject("relayweft after: ", task) + "\n\n" + agentFilesMark + "\n" + strings.Join(list, "\n") + "\n"
}
