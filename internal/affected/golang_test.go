package affected

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goTree is a module where b imports a, d's test imports b, c stands
// alone, e embeds e.txt and f's test reads "fixture.json" by name.
func goTree(t *testing.T) string {
	t.Helper()
	return tree(t, map[string]string{
		"go.mod":              "module example.com/m\n\ngo 1.22\n",
		"a/a.go":              "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go":         "package a\n",
		"b/b.go":              "package b\n\nimport \"example.com/m/a\"\n\nfunc B() int { return a.A() }\n",
		"c/c.go":              "package c\n\nfunc C() {}\n",
		"d/d.go":              "package d\n",
		"d/d_test.go":         "package d\n\nimport (\n\t\"testing\"\n\t\"example.com/m/b\"\n)\n\nfunc TestD(t *testing.T) { _ = b.B() }\n",
		"e/e.go":              "package e\n\nimport _ \"embed\"\n\n//go:embed e.txt\nvar E string\n",
		"e/e.txt":             "hi\n",
		"f/f.go":              "package f\n",
		"f/f_test.go":         "package f\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { _ = \"fixture.json\" }\n",
		"fixture.json":        "{}\n",
		"a/testdata/x.golden": "x\n",
		"g/g.go":              "package g\n",
		"README.md":           "# m\n",
		"ci/build.sh":         "echo\n",
	})
}

func TestSelectGo(t *testing.T) {
	dir := goTree(t)
	cases := []struct {
		files []string
		want  string // "" = full; "-" = nothing to run
	}{
		{[]string{"a/a.go"}, "go test ./a ./b ./d"},
		{[]string{"c/c.go"}, "go test ./c"},
		{[]string{"d/d_test.go"}, "go test ./d"},
		{[]string{"a/a_test.go"}, "go test ./a"},                         // a test change stays in its package
		{[]string{"e/e.txt"}, "go test ./e"},                             // embedded
		{[]string{"fixture.json"}, "go test ./f"},                        // named by f's test
		{[]string{"README.md"}, "-"},                                     // docs
		{[]string{"go.mod"}, ""},                                         // dependencies
		{[]string{"a/testdata/x.golden"}, "go test ./a"},                 // test data belongs to its package
		{[]string{"a/testdata/gen.go"}, "go test ./a"},                   // go ignores testdata, .go files too
		{[]string{"x/testdata/y"}, ""},                                   // test data outside a package
		{[]string{"ci/build.sh"}, ""},                                    // not Go, named nowhere
		{[]string{"gone/x.go"}, ""},                                      // a package that no longer exists
		{[]string{"a/a.go", "c/c.go", "e/e.go", "f/f.go", "g/g.go"}, ""}, // every package
	}
	for _, c := range cases {
		p := sel(t, dir, "go test ./...", c.files...)
		switch {
		case c.want == "" && !p.Full:
			t.Errorf("%v: want full, got %+v", c.files, p)
		case c.want == "-" && (p.Full || len(p.Run) != 0):
			t.Errorf("%v: want nothing, got %+v", c.files, p)
		case c.want != "" && c.want != "-" && (p.Full || len(p.Run) != 1 || p.Run[0] != c.want):
			t.Errorf("%v: want %q, got %+v", c.files, c.want, p)
		}
		if p.Why == "" {
			t.Errorf("%v: no reason", c.files)
		}
		checkAllowed(t, dir, "go test ./...", "", p)
	}
	// Flags around the pattern are kept; vet and build narrow the same way.
	p := sel(t, dir, "go test -race -count=1 ./... -run TestD", "a/a.go")
	if p.Full || p.Run[0] != "go test -race -count=1 ./a ./b ./d -run TestD" {
		t.Errorf("flags: %+v", p)
	}
	checkAllowed(t, dir, "go test -race -count=1 ./... -run TestD", "", p)
	if p := sel(t, dir, "go vet ./...", "c/c.go"); p.Full || p.Run[0] != "go vet ./c" {
		t.Errorf("vet: %+v", p)
	}
	// go build is not narrowed: "go build <pattern>" in the allowlist would
	// let agents pass -toolexec or -o. -C, -modfile and -mod change the
	// module the packages come from.
	for _, cmd := range []string{"go build ./...", "go test -C sub ./...", "go test -C=sub ./...", "go test -modfile=x.mod ./...", "go vet -mod=mod ./..."} {
		if p := sel(t, dir, cmd, "c/c.go"); !p.Full {
			t.Errorf("%s: %+v", cmd, p)
		}
		if pre, _ := Allowed(dir, cmd, ""); len(pre) != 0 {
			t.Errorf("%s: allowed %q", cmd, pre)
		}
	}
	if !strings.Contains(sel(t, dir, "go test ./...", "a/a.go").Why, "3 of 7 packages") {
		t.Errorf("reason: %q", sel(t, dir, "go test ./...", "a/a.go").Why)
	}
}

// Test data another package reaches into is shared: the run is full.
func TestSelectGoSharedTestData(t *testing.T) {
	dir := goTree(t)
	os.WriteFile(filepath.Join(dir, "c", "c_test.go"), []byte("package c\n\nvar golden = \"../a/testdata/x.golden\"\n"), 0o644)
	if p := sel(t, dir, "go test ./...", "a/testdata/x.golden"); !p.Full || !strings.Contains(p.Why, "example.com/m/c") {
		t.Errorf("shared test data: %+v", p)
	}
}

// A package that only builds with a tag is seen when the command sets it.
func TestSelectGoBuildTags(t *testing.T) {
	dir := tree(t, map[string]string{
		"go.mod":       "module example.com/m\n\ngo 1.22\n",
		"a/a.go":       "package a\n",
		"b/b.go":       "//go:build integration\n\npackage b\n\nimport _ \"example.com/m/a\"\n",
		"b/b_plain.go": "package b\n",
		"c/c.go":       "package c\n",
	})
	if p := sel(t, dir, "go test ./...", "a/a.go"); p.Full || p.Run[0] != "go test ./a" {
		t.Errorf("no tag: %+v", p)
	}
	if p := sel(t, dir, "go test -tags integration ./...", "a/a.go"); p.Full || p.Run[0] != "go test -tags integration ./a ./b" {
		t.Errorf("tag: %+v", p)
	}
}

// The checks may run in a module below the repo's top folder.
func TestSelectGoInSubfolder(t *testing.T) {
	root := goTree(t)
	sub := filepath.Join(root, "svc")
	os.MkdirAll(filepath.Join(sub, "x"), 0o755)
	os.MkdirAll(filepath.Join(sub, "y"), 0o755)
	os.WriteFile(filepath.Join(sub, "go.mod"), []byte("module example.com/svc\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(sub, "x", "x.go"), []byte("package x\n"), 0o644)
	os.WriteFile(filepath.Join(sub, "y", "y.go"), []byte("package y\n"), 0o644)
	p := Select(context.Background(), "go test ./...", "", Input{Root: root, Dir: sub, Files: []string{"svc/x/x.go"}})
	if p.Full || len(p.Run) != 1 || p.Run[0] != "go test ./x" {
		t.Errorf("subfolder: %+v", p)
	}
	if p := Select(context.Background(), "go test ./...", "", Input{Root: root, Dir: sub, Files: []string{"a/a.go"}}); !p.Full {
		t.Errorf("code outside the module's folder: %+v", p)
	}
}

// A package that does not load (a syntax error an agent left) still runs
// when it is affected; elsewhere its imports cannot be trusted.
func TestSelectGoBrokenPackage(t *testing.T) {
	dir := goTree(t)
	os.WriteFile(filepath.Join(dir, "c", "c.go"), []byte("package c\n\nimport (\n\t\"example.com/m/a\"\nfunc C() {}\n"), 0o644)
	if p := sel(t, dir, "go test ./...", "c/c.go"); p.Full || p.Run[0] != "go test ./c" {
		t.Errorf("broken and changed: %+v", p)
	}
	if p := sel(t, dir, "go test ./...", "e/e.go"); !p.Full || !strings.Contains(p.Why, "example.com/m/c") {
		t.Errorf("broken elsewhere: %+v", p)
	}
}

func TestGoAllowedAndHint(t *testing.T) {
	pre, hint := Allowed("", "go test -race ./...", "")
	if len(pre) != 1 || pre[0] != "go test -race" || hint != "go test -race <packages>" {
		t.Errorf("allowed %q hint %q", pre, hint)
	}
	if pre, hint := Allowed("", "go test ./... | tee x", ""); pre != nil || hint != "" {
		t.Errorf("shell syntax: %q %q", pre, hint)
	}
	if pre, _ := Allowed("", "go test ./...", Off); pre != nil {
		t.Errorf("off: %q", pre)
	}
}

// go rejects a folder whose name is no import path (a space): the listing
// is not complete, so the run is full.
func TestSelectGoUnloadablePackage(t *testing.T) {
	dir := goTree(t)
	os.MkdirAll(filepath.Join(dir, "my dir"), 0o755)
	os.WriteFile(filepath.Join(dir, "my dir", "g.go"), []byte("package g\n"), 0o644)
	for _, f := range []string{"c/c.go", "my dir/g.go"} {
		if p := sel(t, dir, "go test ./...", f); !p.Full {
			t.Errorf("%s: %+v", f, p)
		}
	}
}
