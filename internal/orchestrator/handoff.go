package orchestrator

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Context hand-off: agents start with no memory, so every planner and step
// prompt gets
//   - a repo map (top-level layout from git ls-files), so agents spend
//     fewer tokens finding their way around;
//   - notes from earlier tasks in this repo (what was done, which files),
//     kept per repo next to the task states;
// and edit steps also get what read-only steps of the same task found,
// even when the planner did not declare the dependency.

const (
	repoMapMax   = 2500 // bytes
	notesKeep    = 20   // entries per repo
	notesInclude = 6    // entries shown to the planner
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
const noteSep = "\n<!-- sy-note -->\n"

// repoNotes returns the newest notes for a repo ("" if none).
func repoNotes(root string) string {
	data, err := os.ReadFile(notesPath(root))
	if err != nil {
		return ""
	}
	entries := splitNotes(string(data))
	if len(entries) > notesInclude {
		entries = entries[len(entries)-notesInclude:]
	}
	return strings.Join(entries, "\n")
}

func splitNotes(s string) []string {
	var out []string
	for _, e := range strings.Split(s, noteSep) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// addRepoNote records a finished task for later tasks in the same repo.
func addRepoNote(root, task, summary string, files []string) {
	p := notesPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	data, _ := os.ReadFile(p)
	entries := splitNotes(string(data))
	if len(files) > 12 {
		files = append(files[:12:12], fmt.Sprintf("+%d more", len(files)-12))
	}
	e := fmt.Sprintf("- %s: %s\n  result: %s", time.Now().Format("2006-01-02"), oneLineClip(task, 160), oneLineClip(summary, 200))
	if len(files) > 0 {
		e += "\n  files: " + strings.Join(files, ", ")
	}
	entries = append(entries, e)
	if len(entries) > notesKeep {
		entries = entries[len(entries)-notesKeep:]
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, []byte(strings.Join(entries, noteSep)+"\n"), 0o644) == nil {
		os.Rename(tmp, p)
	}
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
	if t.repoNotes != "" {
		b.WriteString("\nEARLIER SWITCHYARD TASKS IN THIS REPO (newest last; the code may have changed since):\n" + t.repoNotes + "\n")
	}
	return b.String()
}

// changedFiles lists the files that differ from the task's start snapshot.
func (t *task) changedFiles() []string {
	if !t.useGit || t.start == "" {
		return nil
	}
	s, err := git{t.root}.out("diff", "--name-only", t.start)
	if err != nil || s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
