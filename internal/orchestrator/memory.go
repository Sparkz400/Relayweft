package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Project memory: notes later tasks in a repo get in their prompts
// (handoff.go). A finished task records what it did and the files it
// changed; the user adds, edits, pins and removes notes (rw memory, the web
// view).
//
// The notes file stays plain text. A note is its body, then metadata lines:
//
//	  refs: a.go#3f2a9c1b04de, b.go#..., +3 more
//	  pinned: yes
//
// refs are the note's source files with a fingerprint of their content when
// the note was recorded or last confirmed. A note whose source files have all
// changed or gone no longer describes the code and leaves the prompts. A
// pinned note is a project convention: every task gets it, it never ages
// out, and a changed source only marks it for the user to re-confirm.
//
// Each task gets its pinned notes, the newest notes, and the notes that match
// the task's words and file names; unrelated notes stay out of the prompt.

const (
	notesPinned  = 8     // pinned notes per prompt
	notesRecent  = 2     // newest notes every task gets, related or not
	notesRefsMax = 12    // source files recorded per automatic note
	notesBudget  = 16384 // bytes of notes per prompt: at least one note of noteMax
	noteMax      = 8192  // bytes of one note's text
	noteMatchMin = 2     // relevance score that makes a note related
)

// notesMaxAge drops dated, unpinned notes older than this from the prompts.
const notesMaxAge = 60 * 24 * time.Hour

const (
	refsPrefix  = "  refs: "
	filesPrefix = "  files: " // notes recorded before refs had fingerprints
	pinnedLine  = "  pinned: yes"
)

// MemoryEntry is a stored project note and whether it enters the next prompt.
type MemoryEntry struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Pinned   bool           `json:"pinned"`
	Sources  []MemorySource `json:"sources"`
	More     int            `json:"more_sources,omitempty"`
	Included bool           `json:"included"`
	Reason   string         `json:"reason"`
}

// MemorySource is a file a note refers to. State is "current", "changed",
// "gone", or "unchecked" for a reference without a fingerprint (recorded
// before rw kept them, or written by hand): only its deletion counts.
type MemorySource struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

type MemoryView struct {
	Revision string        `json:"revision"`
	Path     string        `json:"path"`
	Task     string        `json:"task,omitempty"`
	Entries  []MemoryEntry `json:"entries"`
	Prompt   string        `json:"prompt"`
}

// MemoryChange edits project memory. An empty ID adds a note. For an
// existing note, Text replaces its text ("" keeps it), Pin pins or unpins it,
// Refs (non-nil) replaces its source files and Refresh records their current
// content: the user confirmed the note still holds.
type MemoryChange struct {
	Revision string   `json:"revision"`
	ID       string   `json:"id,omitempty"`
	Text     string   `json:"text,omitempty"`
	Delete   bool     `json:"delete,omitempty"`
	Pin      *bool    `json:"pin,omitempty"`
	Refs     []string `json:"refs,omitempty"`
	Refresh  bool     `json:"refresh,omitempty"`
}

var ErrMemoryChanged = errors.New("project memory changed; reload it before editing")

func memoryRevision(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func noteID(i int, e string) string { return fmt.Sprintf("%d-%s", i, memoryRevision([]byte(e))[:16]) }

type noteRef struct{ path, sum string }

type note struct {
	body   string
	refs   []noteRef
	more   int // changed files beyond the recorded refs
	pinned bool
}

func parseNote(s string) note {
	var n note
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
meta:
	for len(lines) > 1 {
		switch l := lines[len(lines)-1]; {
		case l == pinnedLine:
			n.pinned = true
		case strings.HasPrefix(l, refsPrefix):
			n.refs, n.more = parseRefs(l[len(refsPrefix):])
		case n.refs == nil && strings.HasPrefix(l, filesPrefix):
			n.refs, n.more = parseRefs(l[len(filesPrefix):])
		default:
			break meta
		}
		lines = lines[:len(lines)-1]
	}
	n.body = strings.TrimSpace(strings.Join(lines, "\n"))
	return n
}

func parseRefs(s string) (refs []noteRef, more int) {
	for _, p := range strings.Split(s, ", ") {
		p = strings.TrimSpace(p)
		if m, ok := strings.CutPrefix(p, "+"); ok {
			more, _ = strconv.Atoi(strings.TrimSuffix(m, " more"))
			continue
		}
		r := noteRef{path: p}
		if i := strings.LastIndexByte(p, '#'); i > 0 {
			r = noteRef{path: p[:i], sum: p[i+1:]}
		}
		if r.path != "" {
			refs = append(refs, r)
		}
	}
	return refs, more
}

func (n note) String() string {
	s := n.body
	if len(n.refs) > 0 || n.more > 0 {
		parts := make([]string, 0, len(n.refs)+1)
		for _, r := range n.refs {
			parts = append(parts, r.path+"#"+r.sum)
		}
		if n.more > 0 {
			parts = append(parts, fmt.Sprintf("+%d more", n.more))
		}
		s += "\n" + refsPrefix + strings.Join(parts, ", ")
	}
	if n.pinned {
		s += "\n" + pinnedLine
	}
	return s
}

// date is an automatic note's date ("- 2006-01-02: task"), zero for others.
func (n note) date() time.Time {
	if len(n.body) >= 12 && strings.HasPrefix(n.body, "- ") {
		if d, err := time.Parse("2006-01-02", n.body[2:12]); err == nil {
			return d
		}
	}
	return time.Time{}
}

// sources fingerprints repo files, reading each at most once per call.
type sources struct {
	root string
	sums map[string]string
}

func newSources(root string) *sources { return &sources{root: root, sums: map[string]string{}} }

// sum is the file's content fingerprint, "" if it is not a readable file.
func (s *sources) sum(p string) string {
	if v, ok := s.sums[p]; ok {
		return v
	}
	v := ""
	if data, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(p))); err == nil {
		h := sha256.Sum256(data)
		v = hex.EncodeToString(h[:6])
	}
	s.sums[p] = v
	return v
}

func (s *sources) state(r noteRef) string {
	switch cur := s.sum(r.path); {
	case cur == "":
		return "gone"
	case r.sum == "":
		return "unchecked"
	case cur != r.sum:
		return "changed"
	}
	return "current"
}

// pick is one note and the decision whether it enters the prompt.
type pick struct {
	n        note
	states   []string // per ref
	included bool
	reason   string
}

func (p pick) moved() (out []string) {
	for i, st := range p.states {
		if st == "changed" || st == "gone" {
			out = append(out, p.n.refs[i].path)
		}
	}
	return out
}

// selectNotes decides which notes enter the prompt of a task ("" when there
// is none yet: the newest notes fill the slots) and why.
func selectNotes(root, task string, entries []string, now time.Time) []pick {
	src := newSources(root)
	picks := make([]pick, len(entries))
	for i, e := range entries {
		p := &picks[i]
		p.n = parseNote(e)
		for _, r := range p.n.refs {
			p.states = append(p.states, src.state(r))
		}
		moved := p.moved()
		switch d := p.n.date(); {
		case p.n.pinned:
		case !d.IsZero() && now.Sub(d) > notesMaxAge:
			p.reason = "older than 60 days"
		case len(p.n.refs) > 0 && len(moved) == len(p.n.refs):
			p.reason = "its source files changed since it was recorded (" + listClip(moved, 4) + "); re-confirm it to use it again"
		}
	}
	budget := notesBudget
	take := func(p *pick, why string) {
		size := len(renderNote(*p)) + 1
		if size > budget {
			p.reason = fmt.Sprintf("does not fit the %d-byte memory budget", notesBudget)
			return
		}
		budget -= size
		p.included, p.reason = true, why
	}
	// Pinned conventions first, newest first.
	pinned := 0
	for i := len(picks) - 1; i >= 0; i-- {
		p := &picks[i]
		if !p.n.pinned {
			continue
		}
		if pinned == notesPinned {
			p.reason = fmt.Sprintf("only %d pinned notes enter prompts; unpin one", notesPinned)
			continue
		}
		pinned++
		why := "pinned convention: every task gets it"
		if moved := p.moved(); len(moved) > 0 {
			why = "pinned, but its source files changed since it was confirmed (" + listClip(moved, 4) + "); agents are told to check it, re-confirm or edit it"
		}
		take(p, why)
	}
	// The newest notes keep a task in touch with the work just before it.
	var rest []int
	recent, left := 0, notesInclude
	for i := len(picks) - 1; i >= 0; i-- {
		p := &picks[i]
		if p.n.pinned || p.reason != "" {
			continue
		}
		if recent < notesRecent || task == "" && left > 0 {
			recent++
			left--
			why := "one of the newest notes"
			if task == "" {
				why = "one of the newest current notes (other notes are chosen by how well they match the task)"
			}
			take(p, why)
			continue
		}
		rest = append(rest, i)
	}
	if task == "" {
		for _, i := range rest {
			picks[i].reason = fmt.Sprintf("only the %d newest current notes enter when there is no task to match", notesInclude)
		}
		return picks
	}
	// The rest by relevance, best first; ties go to the newer note.
	tt := terms(task)
	score := map[int]int{}
	hits := map[int][]string{}
	for _, i := range rest {
		score[i], hits[i] = relevance(tt, picks[i].n)
	}
	sort.SliceStable(rest, func(a, b int) bool { return score[rest[a]] > score[rest[b]] })
	for _, i := range rest {
		p := &picks[i]
		switch {
		case score[i] < noteMatchMin:
			p.reason = "not related to this task"
		case left == 0:
			p.reason = fmt.Sprintf("related (%s), but %d notes already fill the slots", listClip(hits[i], 4), notesInclude)
		default:
			left--
			take(p, "related to this task: "+listClip(hits[i], 4))
		}
	}
	return picks
}

func listClip(s []string, n int) string {
	if len(s) > n {
		return strings.Join(s[:n], ", ") + fmt.Sprintf(" +%d more", len(s)-n)
	}
	return strings.Join(s, ", ")
}

// noteStop are words too common in tasks and notes to tie them together.
var noteStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for with that this from into onto when then than also only
		all any each some not are was were has have had its it's can could should would will must
		add adds added fix fixes fixed make makes use uses used using update updates updated change changes
		changed new old file files code test tests task tasks result done run runs get set now one two
		via per out but more less like just what which who how why where there their them they you your
		our does did don't let make sure keep please need needs want`) {
		noteStop[w] = true
	}
}

// terms are the lower-case words of 3 or more characters in s, with the
// parts of CamelCase and snake_case identifiers and of file paths.
func terms(s string) map[string]bool {
	out := map[string]bool{}
	word := func(w string) {
		w = strings.ToLower(w)
		if len([]rune(w)) >= 3 && !noteStop[w] && strings.TrimFunc(w, unicode.IsDigit) != "" {
			out[w] = true
		}
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		word(f)
		for _, part := range strings.Split(f, "_") {
			rs := []rune(part)
			start := 0
			for i := 1; i < len(rs); i++ {
				if unicode.IsUpper(rs[i]) && unicode.IsLower(rs[i-1]) {
					word(string(rs[start:i]))
					start = i
				}
			}
			if start > 0 || len(rs) < len([]rune(f)) {
				word(string(rs[start:]))
			}
		}
	}
	return out
}

// relevance scores a note for a task: each task word in the note's text
// counts 1, in the name or folder of one of its source files 2.
func relevance(task map[string]bool, n note) (int, []string) {
	body := terms(n.body)
	files := map[string]bool{}
	for _, r := range n.refs {
		dir, base := path.Split(r.path)
		for w := range terms(strings.TrimSuffix(base, path.Ext(base)) + " " + path.Base(dir)) {
			files[w] = true
		}
	}
	score := 0
	var hits []string
	for w := range task {
		switch {
		case files[w]:
			score += 2
		case body[w]:
			score++
		default:
			continue
		}
		hits = append(hits, w)
	}
	sort.Strings(hits)
	return score, hits
}

// renderNote is a note as agents read it.
func renderNote(p pick) string {
	s := p.n.body
	if !strings.HasPrefix(s, "- ") {
		s = "- " + strings.ReplaceAll(s, "\n", "\n  ")
	}
	if len(p.n.refs) > 0 || p.n.more > 0 {
		parts := make([]string, 0, len(p.n.refs)+1)
		for i, r := range p.n.refs {
			switch p.states[i] {
			case "changed":
				parts = append(parts, r.path+" (changed since)")
			case "gone":
				parts = append(parts, r.path+" (deleted since)")
			default:
				parts = append(parts, r.path)
			}
		}
		if p.n.more > 0 {
			parts = append(parts, fmt.Sprintf("+%d more", p.n.more))
		}
		s += "\n  files: " + strings.Join(parts, ", ")
	}
	if p.n.pinned && len(p.moved()) > 0 {
		s += "\n  (these files changed since the user confirmed this; check it still holds before relying on it)"
	}
	return s
}

// notesPrompt is the project memory block of a prompt ("" if no notes).
func notesPrompt(picks []pick) string {
	var pinned, other []string
	for _, p := range picks {
		switch {
		case !p.included:
		case p.n.pinned:
			pinned = append(pinned, renderNote(p))
		default:
			other = append(other, renderNote(p))
		}
	}
	var b strings.Builder
	if len(pinned) > 0 {
		b.WriteString("\nPROJECT CONVENTIONS PINNED BY THE USER (follow them unless the task says otherwise):\n" + strings.Join(pinned, "\n") + "\n")
	}
	if len(other) > 0 {
		b.WriteString("\nEARLIER RELAYWEFT TASKS AND NOTES IN THIS REPO (newest last; the code may have changed since, files marked changed no longer match the note):\n" + strings.Join(other, "\n") + "\n")
	}
	return b.String()
}

// repoNotes is the project memory block for a task's prompts ("" if none).
func repoNotes(root, task string) string {
	data, err := os.ReadFile(notesPath(root))
	if err != nil {
		return ""
	}
	return notesPrompt(selectNotes(root, task, splitNotes(string(data)), time.Now()))
}

// addRepoNote records a finished task for later tasks in the same repo, with
// the changed files' content now, so a later change to them retires the note.
func addRepoNote(root, task, summary string, files []string) error {
	n := note{body: fmt.Sprintf("- %s: %s\n  result: %s", time.Now().Format("2006-01-02"), oneLineClip(task, 160), oneLineClip(summary, 200))}
	src := newSources(root)
	for _, f := range files {
		if len(n.refs) == notesRefsMax {
			n.more++
			continue
		}
		// A deleted file leaves nothing to compare; a path that would break
		// the refs line is left out.
		if sum := src.sum(f); sum != "" && !strings.ContainsAny(f, ",\n") {
			n.refs = append(n.refs, noteRef{f, sum})
		}
	}
	return changeNotes(root, func(entries []string, _ []byte) ([]string, error) {
		return evictNotes(append(entries, n.String())), nil
	})
}

// evictNotes drops the oldest unpinned notes beyond notesKeep.
func evictNotes(entries []string) []string {
	for len(entries) > notesKeep {
		i := 0
		for i < len(entries) && parseNote(entries[i]).pinned {
			i++
		}
		if i == len(entries) {
			break
		}
		entries = append(entries[:i], entries[i+1:]...)
	}
	return entries
}

// ProjectMemory reads only this repository's stored notes and shows which
// enter the prompts of task (or of a task not yet known, for ""). Repo
// instructions, learned routes and the current task's discoveries are
// separate context.
func ProjectMemory(dir, task string) (MemoryView, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return MemoryView{}, err
	}
	p := notesPath(root)
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return MemoryView{}, err
	}
	v := MemoryView{Revision: memoryRevision(data), Path: p, Task: task, Entries: []MemoryEntry{}}
	entries := splitNotes(string(data))
	picks := selectNotes(root, task, entries, time.Now())
	for i, pk := range picks {
		e := MemoryEntry{ID: noteID(i, entries[i]), Text: pk.n.body, Pinned: pk.n.pinned, Sources: []MemorySource{}, More: pk.n.more, Included: pk.included, Reason: pk.reason}
		for j, r := range pk.n.refs {
			e.Sources = append(e.Sources, MemorySource{Path: r.path, State: pk.states[j]})
		}
		v.Entries = append(v.Entries, e)
	}
	v.Prompt = notesPrompt(picks)
	return v, nil
}

// ChangeProjectMemory uses a revision to prevent a browser or CLI from replacing
// notes a task just recorded.
func ChangeProjectMemory(dir string, c MemoryChange) error {
	root, err := repoRoot(dir)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(strings.ReplaceAll(c.Text, "\r\n", "\n"))
	if len(c.Text) > noteMax || strings.Contains(c.Text, "<!-- rw-note -->") || strings.ContainsRune(c.Text, 0) {
		return fmt.Errorf("a note must contain 1 to %d bytes and cannot contain the note separator or NUL", noteMax)
	}
	if n := parseNote(text); n.body != text {
		return errors.New("a note's text cannot end with refs:, files: or pinned: lines; set sources and pinning separately")
	}
	var refs []noteRef
	if c.Refs != nil {
		if refs, err = noteRefs(root, c.Refs); err != nil {
			return err
		}
	}
	return changeNotes(root, func(entries []string, data []byte) ([]string, error) {
		if c.Revision == "" || c.Revision != memoryRevision(data) {
			return nil, ErrMemoryChanged
		}
		pinned := 0
		for _, e := range entries {
			if parseNote(e).pinned {
				pinned++
			}
		}
		pin := func(n *note) error {
			if c.Pin == nil {
				return nil
			}
			if *c.Pin && !n.pinned && pinned >= notesPinned {
				return fmt.Errorf("at most %d notes can be pinned; unpin one first", notesPinned)
			}
			n.pinned = *c.Pin
			return nil
		}
		if c.ID == "" {
			switch {
			case c.Delete:
				return nil, errors.New("choose a note to remove")
			case text == "":
				return nil, fmt.Errorf("a note must contain 1 to %d bytes", noteMax)
			case len(entries) >= notesKeep:
				return nil, fmt.Errorf("project memory holds at most %d notes; remove one first", notesKeep)
			}
			n := note{body: text, refs: refs}
			if err := pin(&n); err != nil {
				return nil, err
			}
			return append(entries, n.String()), nil
		}
		for i, e := range entries {
			if c.ID != noteID(i, e) {
				continue
			}
			if c.Delete {
				return append(entries[:i], entries[i+1:]...), nil
			}
			if text == "" && c.Pin == nil && c.Refs == nil && !c.Refresh {
				return nil, errors.New("nothing to change: give new text, sources, a pin change or a refresh")
			}
			n := parseNote(e)
			if text != "" {
				n.body = text
			}
			if c.Refs != nil {
				n.refs, n.more = refs, 0
			} else if c.Refresh {
				src := newSources(root)
				var kept []noteRef
				for _, r := range n.refs {
					if sum := src.sum(r.path); sum != "" {
						kept = append(kept, noteRef{r.path, sum})
					}
				}
				n.refs = kept
			}
			if err := pin(&n); err != nil {
				return nil, err
			}
			entries[i] = n.String()
			return entries, nil
		}
		return nil, errors.New("note no longer exists; reload project memory")
	})
}

// noteRefs checks source files the user named and fingerprints them now.
// Paths are repo-relative or inside the repo.
func noteRefs(root string, paths []string) ([]noteRef, error) {
	src := newSources(root)
	var refs []noteRef
	seen := map[string]bool{}
	for _, p := range paths {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		rel := p
		if filepath.IsAbs(p) {
			r, err := filepath.Rel(canonPath(root), canonPath(p))
			if err != nil {
				return nil, fmt.Errorf("source %s is not inside %s", p, root)
			}
			rel = r
		}
		rel = path.Clean(filepath.ToSlash(rel))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.ContainsAny(rel, ",\n#") {
			return nil, fmt.Errorf("source %s must be a file inside the repo without commas or #", p)
		}
		sum := src.sum(rel)
		if sum == "" {
			return nil, fmt.Errorf("source %s is not a readable file in the repo", p)
		}
		if !seen[rel] {
			seen[rel] = true
			refs = append(refs, noteRef{rel, sum})
		}
	}
	if len(refs) > notesRefsMax*2 {
		return nil, fmt.Errorf("a note can name at most %d source files", notesRefsMax*2)
	}
	return refs, nil
}

// Every read-modify-write, including automatic notes, shares this OS lock.
func changeNotes(root string, change func([]string, []byte) ([]string, error)) error {
	p := notesPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	unlock, ok := lockRetry(p + ".lock")
	if !ok {
		return errors.New("project memory is busy; retry after the other task finishes")
	}
	defer unlock()
	data, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	entries, err := change(splitNotes(string(data)), data)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(strings.Join(entries, noteSep)+"\n"))
}
