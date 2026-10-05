package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Relayweft was called Switchyard, with the command sy, up to v0.2.0. Its
// user folder (<user config dir>/switchyard: %APPDATA%\switchyard on
// Windows) holds the config, task states and history, session logs, the
// health and debug logs that `rw health` builds its streak from, learned
// routes, reports and the trust list. rw copies it once to its own folder.
// The worktree pool, MCP and sandbox caches under the user cache dir are
// rebuilt as needed and stay where they are.

// LegacyDirName is the user folder of sy.
const LegacyDirName = "switchyard"

const (
	legacyFileName     = "switchyard.yaml"  // sy's config file
	legacyRepoFileName = ".switchyard.yaml" // sy's per-repo file
)

// LegacyRepoFile returns the .switchyard.yaml that FindRepoFile would
// have found for dir under sy's name, when there is no .relayweft.yaml:
// rw does not read it, so the user should rename it ("" if none).
func LegacyRepoFile(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(d, RepoFileName)); err == nil {
			return ""
		}
		p := filepath.Join(d, legacyRepoFileName)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return ""
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// LegacyLocalFile returns ./switchyard.yaml in dir when dir has no
// ./relayweft.yaml ("" otherwise).
func LegacyLocalFile(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
		return ""
	}
	p := filepath.Join(dir, legacyFileName)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// MigrateSwitchyard copies sy's user folder to rw's on the first start of
// rw: only when rw's folder does not exist yet and sy's does. The copy
// never overwrites anything: it is built next to the target and renamed
// into place, so a second rw starting at the same time, or a failed copy,
// leaves rw's folder absent or complete. sy's folder is left as it was.
//
// It returns the line to tell the user ("" when there was nothing to do).
func MigrateSwitchyard() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", nil // no user config dir: rw cannot have one either
	}
	return migrateSwitchyard(base)
}

func migrateSwitchyard(base string) (string, error) {
	from := filepath.Join(base, LegacyDirName)
	to := filepath.Join(base, "relayweft")
	if _, err := os.Lstat(to); !errors.Is(err, fs.ErrNotExist) {
		return "", nil // rw's folder exists (or cannot be checked): never touch it
	}
	st, err := os.Stat(from)
	if err != nil || !st.IsDir() {
		return "", nil
	}
	tmp, err := os.MkdirTemp(base, "relayweft.migrating-")
	if err != nil {
		return "", fmt.Errorf("copy %s to %s: %w", from, to, err)
	}
	defer os.RemoveAll(tmp) // a no-op once renamed into place
	if err := copyLegacyTree(from, tmp); err != nil {
		return "", fmt.Errorf("copy %s to %s: %w", from, to, err)
	}
	moved, err := migrateTrust(filepath.Join(tmp, "trusted.json"))
	if err != nil {
		return "", fmt.Errorf("copy %s to %s: trusted.json: %w", from, to, err)
	}
	for i := 0; ; i++ {
		err = os.Rename(tmp, to)
		if err == nil {
			break
		}
		if _, serr := os.Lstat(to); serr == nil {
			return "", nil // another rw got there first
		}
		if i == 4 {
			return "", fmt.Errorf("copy %s to %s: %w", from, to, err)
		}
		// A virus scanner often holds a file just written for a moment.
		time.Sleep(time.Duration(100*(i+1)) * time.Millisecond)
	}
	msg := fmt.Sprintf("rw: copied your Switchyard settings, task history and logs from %s to %s (the old folder is unchanged). rw reads .relayweft.yaml, not .switchyard.yaml: rename it in your repos", from, to)
	if moved > 0 {
		msg += fmt.Sprintf("; the trust of %d such file(s) carries over while the content is unchanged.", moved)
	} else {
		msg += " (one that runs commands needs `rw trust` again)."
	}
	return msg, nil
}

// copyLegacyTree copies sy's folder from to the (empty) folder to with
// sy's file names changed to rw's: switchyard.yaml (and its setup backups)
// to relayweft.yaml, and logs/sy-*.log (debug and health logs) to
// logs/rw-*.log. Lock and temporary files are left out: a running sy
// may hold them, and rw creates its own.
func copyLegacyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil || rel == "." {
			return err
		}
		dst := filepath.Join(to, legacyRename(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and the like: not something sy writes
		}
		if name := d.Name(); strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".tmp") {
			return nil
		}
		return copyFile(p, dst)
	})
}

// legacyRename maps a path in sy's folder (relative, OS separators) to
// its name in rw's.
func legacyRename(rel string) string {
	dir, name := filepath.Split(rel)
	switch {
	case dir == "" && strings.HasPrefix(name, legacyFileName):
		name = FileName + strings.TrimPrefix(name, legacyFileName)
	case filepath.Clean(dir) == "logs" && strings.HasPrefix(name, "sy-"):
		name = "rw-" + strings.TrimPrefix(name, "sy-")
	}
	return filepath.Join(dir, name)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Keep the times: rw health and the history sort by them.
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// migrateTrust rewrites the copied trust list (path -> content hash) so a
// repo's .switchyard.yaml, or a folder's switchyard.yaml, that was trusted
// is trusted as .relayweft.yaml (relayweft.yaml) in the same folder, with
// the same hash: renaming the file without changing it keeps the trust,
// any other content does not match. An entry rw's name already has is
// kept. It returns how many entries it moved.
func migrateTrust(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, nil // sy could not read it either; leave it as copied
	}
	moved := 0
	for k, h := range m {
		nk, ok := migrateTrustKey(k)
		if !ok {
			continue
		}
		delete(m, k)
		if _, taken := m[nk]; !taken {
			m[nk] = h
			moved++
		}
	}
	if moved == 0 {
		return 0, nil
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return 0, err
	}
	return moved, os.WriteFile(path, out, 0o644)
}

// migrateTrustKey maps a trust key (an absolute path, lowercased with
// forward slashes: trustKey) of sy's file names to rw's.
func migrateTrustKey(k string) (string, bool) {
	dir, name := "", k
	if i := strings.LastIndex(k, "/"); i >= 0 {
		dir, name = k[:i+1], k[i+1:]
	}
	switch name {
	case legacyRepoFileName:
		return dir + strings.ToLower(RepoFileName), true
	case legacyFileName:
		return dir + strings.ToLower(FileName), true
	}
	return "", false
}
