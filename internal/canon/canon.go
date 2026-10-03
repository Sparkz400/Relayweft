// Package canon normalizes paths for comparison, so the same folder reached
// in different ways (C:/x from git vs C:\x from Go, a macOS /var symlink, a
// Windows 8.3 short name, other letter case on Windows) compares equal.
package canon

import (
	"path/filepath"
	"runtime"
	"strings"
)

// Path normalizes a path for comparison: git prints C:/x where Go uses
// C:\x, git resolves symlinks (macOS /var -> /private/var; on Windows
// EvalSymlinks also expands 8.3 short names), and Windows paths are
// case-insensitive.
func Path(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	p = evalExisting(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// evalExisting resolves symlinks in p's nearest existing ancestor and keeps
// the rest, so a file an agent deleted still canonicalizes like its folder
// (macOS: /var/... -> /private/var/...).
func evalExisting(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(evalExisting(parent), filepath.Base(p))
}

// Same reports whether two paths name the same file or folder.
func Same(a, b string) bool { return Path(a) == Path(b) }

// Within reports whether p is root itself or inside it.
func Within(root, p string) bool {
	rel, err := filepath.Rel(Path(root), Path(p))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
