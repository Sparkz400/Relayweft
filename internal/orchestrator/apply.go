package orchestrator

import (
	"bufio"
	"crypto/sha1"
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
)

// Applying a change to the user's working tree:
//
//   - The change is read with `git diff-tree -r -z` (modes and blob ids of
//     both sides). Gitlinks (submodules, nested repositories) are skipped
//     and reported: there is no file to write.
//   - A file is only overwritten when it still has its content at `from`.
//     All files are hashed in one `git hash-object --stdin-paths` (symlinks
//     by their target, as git stores them). Files the user edited are 3-way
//     merged in memory; if that conflicts nothing at all is written.
//   - Right before each batch of writes, every file's stat data is compared
//     with what was hashed; a file edited in between aborts the apply.
//   - Writes go through `git restore --source=<to>` (checkout filters, line
//     endings, LFS) with the paths on stdin, so any number of files fits
//     (Windows limits command lines to 32K).
//   - Writes are all-or-nothing: if any step fails (read-only or locked
//     file, full disk), everything already written is put back, but only
//     files that still hold exactly what was written, so an edit the user
//     made meanwhile is never undone.

const (
	modeNone    = "000000"
	modeGitlink = "160000"
	modeLink    = "120000"
	applyChunk  = 64
)

// rawEntry is one line of `git diff-tree -r -z` (raw format).
type rawEntry struct {
	oldMode, newMode string
	oldID, newID     string
	status           string
	path             string
}

func (e rawEntry) gitlink() bool { return e.oldMode == modeGitlink || e.newMode == modeGitlink }

// want is the id the working-tree file has at the old side ("" = absent).
func (e rawEntry) want() string {
	if e.oldMode == modeNone {
		return ""
	}
	return e.oldID
}

func parseRaw(out string) []rawEntry {
	f := strings.Split(out, "\x00")
	var list []rawEntry
	for i := 0; i+1 < len(f); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(f[i], ":"))
		if len(meta) < 5 {
			continue
		}
		list = append(list, rawEntry{oldMode: meta[0], newMode: meta[1], oldID: meta[2], newID: meta[3], status: meta[4][:1], path: f[i+1]})
	}
	return list
}

// rawDiff lists the files that differ between two commits, limited to only
// when given (filtered here: a pathspec per file breaks long lists).
func (g git) rawDiff(from, to string, only []string) ([]rawEntry, error) {
	out, err := g.run(nil, nil, "diff-tree", "-r", "-z", "--no-renames", from, to)
	if err != nil {
		return nil, err
	}
	list := parseRaw(out)
	if len(only) == 0 {
		return list, nil
	}
	keep := map[string]bool{}
	for _, p := range only {
		keep[p] = true
	}
	var sel []rawEntry
	for _, e := range list {
		if keep[e.path] {
			sel = append(sel, e)
		}
	}
	return sel, nil
}

// fileStat is what a working-tree file looked like when it was checked.
type fileStat struct {
	exists bool
	size   int64
	mtime  time.Time
	mode   os.FileMode
}

func statOf(full string) fileStat {
	fi, err := os.Lstat(full)
	if err != nil {
		return fileStat{}
	}
	return fileStat{exists: true, size: fi.Size(), mtime: fi.ModTime(), mode: fi.Mode()}
}

func (a fileStat) same(b fileStat) bool {
	return a.exists == b.exists && a.size == b.size && a.mtime.Equal(b.mtime) && a.mode == b.mode
}

func (g git) full(p string) string { return filepath.Join(g.dir, filepath.FromSlash(p)) }

// blobID is the id git gives a blob with this content.
func (g git) blobID(format string, data []byte) string {
	h := sha1.New()
	if format == "sha256" {
		h = sha256.New()
	}
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// worktreeIDs returns the blob id each working-tree path would get ("" when
// missing, "?" for a directory or anything else that is not a file) and its
// stat data, taken before hashing so a later edit shows up as a change.
func (g git) worktreeIDs(paths []string) (map[string]string, map[string]fileStat, error) {
	ids := map[string]string{}
	stats := map[string]fileStat{}
	var batch, single []string
	format := ""
	for _, p := range paths {
		full := g.full(p)
		st := statOf(full)
		stats[p] = st
		switch {
		case !st.exists:
			ids[p] = ""
		case st.mode&os.ModeSymlink != 0:
			// hash-object follows links; git stores the target.
			target, err := os.Readlink(full)
			if err != nil {
				return nil, nil, err
			}
			if format == "" {
				if format, _ = g.out("rev-parse", "--show-object-format"); format == "" {
					format = "sha1"
				}
			}
			ids[p] = g.blobID(format, []byte(filepath.ToSlash(target)))
		case st.mode.IsRegular():
			// --stdin-paths reads lines and unquotes "..." lines.
			if strings.ContainsAny(p, "\n\r") || strings.HasPrefix(p, `"`) {
				single = append(single, p)
			} else {
				batch = append(batch, p)
			}
		default:
			ids[p] = "?"
		}
	}
	if len(batch) > 0 {
		out, err := g.run(nil, []byte(strings.Join(batch, "\n")+"\n"), "hash-object", "--stdin-paths")
		if err != nil {
			return nil, nil, err
		}
		sc := bufio.NewScanner(strings.NewReader(out))
		i := 0
		for sc.Scan() && i < len(batch) {
			ids[batch[i]] = strings.TrimSpace(sc.Text())
			i++
		}
		if i != len(batch) {
			return nil, nil, fmt.Errorf("git hash-object returned %d ids for %d files", i, len(batch))
		}
	}
	for _, p := range single {
		id, err := g.out("hash-object", "--", p)
		if err != nil {
			return nil, nil, err
		}
		ids[p] = id
	}
	return ids, stats, nil
}

// applyHook lets tests act between the steps of an apply ("checked" after
// the working tree was compared, "wrote" after each batch of writes).
var applyHook func(stage string)

// applyDiff brings the working tree from commit `from` to commit `to`; see
// the top of this file.
func (g git) applyDiff(from, to string, only ...string) error {
	_, err := g.applyDiffReport(from, to, only...)
	return err
}

// applyDiffReport is applyDiff that also returns the gitlinks (submodule
// changes) it skipped.
func (g git) applyDiffReport(from, to string, only ...string) (skipped []string, err error) {
	entries, err := g.rawDiff(from, to, only)
	if err != nil {
		return nil, err
	}
	var files []rawEntry
	var paths []string
	for _, e := range entries {
		if e.gitlink() {
			skipped = append(skipped, e.path)
			continue
		}
		files = append(files, e)
		paths = append(paths, e.path)
	}
	ids, stats, err := g.worktreeIDs(paths)
	if err != nil {
		return skipped, err
	}
	var write, remove, touched []rawEntry
	for _, e := range files {
		switch {
		case ids[e.path] != e.want():
			touched = append(touched, e)
		case e.newMode == modeNone:
			remove = append(remove, e)
		default:
			write = append(write, e)
		}
	}
	// Merge the files the user touched first, in memory, so a conflict
	// leaves the working tree exactly as it was.
	var conflicts []string
	merged := map[string]*mergedFile{}
	for _, e := range touched {
		if e.oldMode == modeLink || e.newMode == modeLink || ids[e.path] == "?" {
			conflicts = append(conflicts, e.path)
			continue
		}
		orig, rerr := os.ReadFile(g.full(e.path))
		out, err := g.mergeFile(from, to, e.path)
		if err != nil || rerr != nil {
			conflicts = append(conflicts, e.path)
			continue
		}
		merged[e.path] = &mergedFile{content: out, orig: orig, stat: stats[e.path]}
	}
	if len(conflicts) > 0 {
		return skipped, errYourEdits{conflicts}
	}
	if applyHook != nil {
		applyHook("checked")
	}
	// Refuse up front what cannot be written (read-only, immutable, locked
	// by another program on Windows) rather than fail halfway.
	check := func(p string) error {
		st := stats[p]
		if !st.exists || !st.mode.IsRegular() {
			return nil
		}
		f, err := os.OpenFile(g.full(p), os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("cannot write %s: %w", p, err)
		}
		f.Close()
		return nil
	}
	for _, e := range write {
		if err := check(e.path); err != nil {
			return skipped, err
		}
	}
	for p := range merged {
		if err := check(p); err != nil {
			return skipped, err
		}
	}
	a := &applier{g: g, from: from, to: to, stats: stats}
	if err := a.apply(write, remove, merged); err != nil {
		if rerr := a.rollback(); rerr != nil {
			return skipped, fmt.Errorf("%w; putting back what was written failed: %v", err, rerr)
		}
		return skipped, fmt.Errorf("%w (nothing was changed)", err)
	}
	return skipped, nil
}

type mergedFile struct {
	content string
	orig    []byte
	stat    fileStat
	done    bool
}

// errYourEdits: files the user changed overlap with the change; nothing
// was written (a resolve step may merge them, resolve.go).
type errYourEdits struct{ paths []string }

func (e errYourEdits) Error() string {
	return "you changed " + strings.Join(e.paths, ", ") + " while agents were working and the edits overlap"
}

// errEditedDuringApply: the user changed a file between the check and the
// write.
type errEditedDuringApply struct{ paths []string }

func (e errEditedDuringApply) Error() string {
	return "you changed " + strings.Join(e.paths, ", ") + " while the change was being applied"
}

// applier writes a change and remembers what it did, for rollback.
type applier struct {
	g        git
	from, to string
	stats    map[string]fileStat
	written  []rawEntry // passed to git restore (maybe partly written)
	removed  []rawEntry
	merged   map[string]*mergedFile
	newDirs  []string // directories created for new files
}

func (a *applier) recheck(paths []string) error {
	var changed []string
	for _, p := range paths {
		if !a.stats[p].same(statOf(a.g.full(p))) {
			changed = append(changed, p)
		}
	}
	if len(changed) > 0 {
		return errEditedDuringApply{changed}
	}
	return nil
}

func (a *applier) apply(write, remove []rawEntry, merged map[string]*mergedFile) error {
	a.merged = merged
	for i := 0; i < len(write); i += applyChunk {
		chunk := write[i:min(i+applyChunk, len(write))]
		var paths []string
		for _, e := range chunk {
			paths = append(paths, e.path)
		}
		if err := a.recheck(paths); err != nil {
			return err
		}
		for _, e := range chunk {
			if e.oldMode == modeNone {
				a.noteNewDirs(e.path)
			}
		}
		a.written = append(a.written, chunk...)
		if err := a.g.restorePaths(a.to, paths); err != nil {
			return err
		}
		if applyHook != nil {
			applyHook("wrote")
		}
	}
	for _, e := range remove {
		if err := a.recheck([]string{e.path}); err != nil {
			return err
		}
		if err := os.Remove(a.g.full(e.path)); err != nil && !os.IsNotExist(err) {
			return err
		}
		a.removed = append(a.removed, e)
		removeEmptyParents(a.g.dir, e.path)
	}
	var mpaths []string
	for p := range merged {
		mpaths = append(mpaths, p)
	}
	sort.Strings(mpaths)
	for _, p := range mpaths {
		m := merged[p]
		if !m.stat.same(statOf(a.g.full(p))) {
			return errEditedDuringApply{[]string{p}}
		}
		if err := os.WriteFile(a.g.full(p), []byte(m.content), 0o644); err != nil {
			return err
		}
		m.done = true
	}
	return nil
}

// noteNewDirs records the parent directories of p that do not exist yet.
func (a *applier) noteNewDirs(p string) {
	for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
		if _, err := os.Lstat(a.g.full(d)); err == nil {
			return
		}
		a.newDirs = append(a.newDirs, d)
	}
}

// rollback puts back what apply wrote. A file is only touched while it
// still holds exactly what apply put there.
func (a *applier) rollback() error {
	var problems []string
	for p, m := range a.merged {
		if !m.done {
			continue
		}
		cur, err := os.ReadFile(a.g.full(p))
		if err != nil || string(cur) != m.content {
			problems = append(problems, p)
			continue
		}
		if err := os.WriteFile(a.g.full(p), m.orig, 0o644); err != nil {
			problems = append(problems, p)
		}
	}
	var restore []string
	if len(a.written) > 0 {
		var paths []string
		for _, e := range a.written {
			paths = append(paths, e.path)
		}
		ids, _, err := a.g.worktreeIDs(paths)
		if err != nil {
			return err
		}
		for _, e := range a.written {
			cur := ids[e.path]
			switch {
			case cur == e.want() && (e.oldID != e.newID || e.oldMode == e.newMode):
				// never written (or already back)
			case cur == e.newID || (cur == "" && e.oldMode != modeNone):
				// ours (or deleted halfway by restore): put the old one back
				if e.oldMode == modeNone {
					if err := os.Remove(a.g.full(e.path)); err != nil && !os.IsNotExist(err) {
						problems = append(problems, e.path)
					}
					removeEmptyParents(a.g.dir, e.path)
				} else {
					restore = append(restore, e.path)
				}
			default:
				problems = append(problems, e.path) // someone else wrote it meanwhile
			}
		}
	}
	for _, e := range a.removed {
		if _, err := os.Lstat(a.g.full(e.path)); os.IsNotExist(err) {
			restore = append(restore, e.path)
		} else {
			problems = append(problems, e.path)
		}
	}
	if err := a.g.restorePaths(a.from, restore); err != nil {
		problems = append(problems, restore...)
	}
	sort.Slice(a.newDirs, func(i, j int) bool { return strings.Count(a.newDirs[i], "/") > strings.Count(a.newDirs[j], "/") })
	for _, d := range a.newDirs {
		os.Remove(a.g.full(d)) // only if empty
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New("could not put back " + strings.Join(problems, ", "))
	}
	return nil
}

// restorePaths writes paths as they are in source, through git's checkout
// filters. The paths go in on stdin.
func (g git) restorePaths(source string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := g.run(nil, nulList(paths), "restore", "--source="+source, "--worktree", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

// removeEmptyParents removes the directories above the removed file p that
// are now empty, up to (not including) root, as git does.
func removeEmptyParents(root, p string) {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if os.Remove(filepath.Join(root, filepath.FromSlash(d))) != nil {
			return
		}
	}
}

// writeMergeInput writes mergeFile's base and theirs files (a variable so
// a test can make it fail).
var writeMergeInput = os.WriteFile

// mergeFile 3-way merges one file the user edited while an agent changed it:
// base = the file at `from`, theirs = the file at `to`, ours = the working
// tree. Blobs are read with --filters so line endings match the checkout.
// It returns the merged content; an error means the edits conflict.
func (g git) mergeFile(from, to, path string) (string, error) {
	full := g.full(path)
	theirs, err := g.run(nil, nil, "cat-file", "--filters", to+":"+path)
	if err != nil {
		return "", err // deleted by the agent but edited by the user: conflict
	}
	base, _ := g.run(nil, nil, "cat-file", "--filters", from+":"+path)
	tmp, err := os.MkdirTemp("", "rw-merge-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	bp, tp := filepath.Join(tmp, "base"), filepath.Join(tmp, "theirs")
	// A cut-off input (disk full) would merge without a conflict and drop
	// the rest of the file.
	if err := writeMergeInput(bp, []byte(base), 0o644); err != nil {
		return "", err
	}
	if err := writeMergeInput(tp, []byte(theirs), 0o644); err != nil {
		return "", err
	}
	if _, err := os.Stat(full); err != nil {
		return "", err
	}
	merged, err := g.run(nil, nil, "merge-file", "-p", full, bp, tp)
	if err != nil {
		return "", err // exit status > 0 means conflicts
	}
	return merged, nil
}
