package orchestrator

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Context hand-off: agents start with no memory, so every planner and step
// prompt gets
//   - a repo map (top-level layout from git ls-files), so agents spend
//     fewer tokens finding their way around;
//   - project memory: the user's pinned conventions and the notes from
//     earlier tasks in this repo that match the task (memory.go);
// and edit steps also get what read-only steps of the same task found,
// even when the planner did not declare the dependency.

const (
	repoMapMax   = 2500 // bytes
	notesKeep    = 20   // entries per repo
	notesInclude = 6    // unpinned entries per prompt
)

// repoMap summarizes a repo's tracked files: root files and, per top-level
// directory, the file count and the most common extensions.
func repoMap(root string) string {
	s, err := git{root}.run(nil, nil, "ls-files", "-z")
	if err != nil || s == "" {
		return ""
	}
	type dir struct {
		n    int
		exts map[string]int
	}
	dirs := map[string]*dir{}
	var rootFiles []string
	total := 0
	for _, f := range strings.Split(strings.TrimRight(s, "\x00"), "\x00") {
		total++
		top, rest, nested := strings.Cut(f, "/")
		if !nested {
			rootFiles = append(rootFiles, f)
			continue
		}
		d := dirs[top]
		if d == nil {
			d = &dir{exts: map[string]int{}}
			dirs[top] = d
		}
		d.n++
		if e := path.Ext(rest); e != "" {
			d.exts[e]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d tracked files.\n", total)
	if len(rootFiles) > 0 {
		if len(rootFiles) > 30 {
			rootFiles = append(rootFiles[:30], "...")
		}
		b.WriteString("Root files: " + strings.Join(rootFiles, ", ") + "\n")
	}
	names := make([]string, 0, len(dirs))
	for n := range dirs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return dirs[names[i]].n > dirs[names[j]].n })
	for i, n := range names {
		d := dirs[n]
		exts := make([]string, 0, len(d.exts))
		for e := range d.exts {
			exts = append(exts, e)
		}
		sort.Slice(exts, func(i, j int) bool {
			return d.exts[exts[i]] > d.exts[exts[j]] || d.exts[exts[i]] == d.exts[exts[j]] && exts[i] < exts[j]
		})
		if len(exts) > 3 {
			exts = exts[:3]
		}
		line := fmt.Sprintf("%s/ %d files", n, d.n)
		if len(exts) > 0 {
			line += " (" + strings.Join(exts, " ") + ")"
		}
		if b.Len()+len(line) > repoMapMax {
			fmt.Fprintf(&b, "... and %d more directories\n", len(names)-i)
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// notesPath is the per-repo notes file.
func notesPath(root string) string {
	h := sha1.Sum([]byte(canonPath(root)))
	return filepath.Join(stateDir(), "notes", hex.EncodeToString(h[:6])+".md")
}

// noteEntry separates entries in the notes file.
const noteSep = "\n<!-- rw-note -->\n"

func splitNotes(s string) []string {
	var out []string
	for _, e := range strings.Split(s, noteSep) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func oneLineClip(s string, n int) string {
	return clip(strings.Join(strings.Fields(s), " "), n)
}

// handoff is the context block for planner and step prompts.
func (t *task) handoff() string {
	var b strings.Builder
	if t.repoMap != "" {
		b.WriteString("\nREPOSITORY MAP (tracked files):\n" + t.repoMap)
	}
	b.WriteString(t.repoNotes)
	return b.String()
}

// changedFiles lists the files that differ from the task's start snapshot.
func (t *task) changedFiles() []string {
	if !t.useGit || t.start == "" {
		return nil
	}
	// The work tree is the agents': never look into its submodules.
	s, err := git{t.root}.out("diff", "--name-only", "--ignore-submodules=all", t.start)
	if err != nil || s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
