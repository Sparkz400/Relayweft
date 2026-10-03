package canon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWithin(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg", "a")
	os.MkdirAll(sub, 0o755)
	sibling := root + "-other" // shares the prefix, not the folder
	cases := []struct {
		p    string
		want bool
	}{
		{root, true}, {sub, true}, {filepath.Join(root, "gone", "file.go"), true},
		{sibling, false}, {filepath.Dir(root), false}, {t.TempDir(), false},
	}
	for _, c := range cases {
		if got := Within(root, c.p); got != c.want {
			t.Errorf("Within(%s, %s) = %v", root, c.p, got)
		}
	}
	if !Within(root, filepath.ToSlash(sub)) {
		t.Error("git's forward slashes are not the same folder")
	}
}

// The same folder through a symlink (macOS: /var -> /private/var) or in
// other letter case on Windows is the same folder.
func TestSameThroughAliases(t *testing.T) {
	root := t.TempDir()
	alias := strings.ToUpper(root)
	if runtime.GOOS != "windows" {
		alias = filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
	}
	if !Same(root, alias) || !Within(alias, filepath.Join(root, "x")) {
		t.Fatalf("%s and %s differ", root, alias)
	}
}
