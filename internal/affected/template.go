package affected

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Templates (verify.affected_commands).

var placeholders = []string{"{files}", "{packages}", "{test_files}"}

// firstPlaceholder is the index of the first placeholder in t, or -1.
func firstPlaceholder(t string) int {
	at := -1
	for _, p := range placeholders {
		if i := strings.Index(t, p); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	return at
}

// selectTemplate fills a template's placeholders: {files} the changed
// files, {packages} the affected Go packages for a go command (else the
// changed files' folders), {test_files} the affected test files (Go for a
// go command, pytest for a pytest one, else the changed test files and
// the tests named after a changed file). All are relative to the check
// folder. The template's own command picks the selection, not the files
// in the repo, so a pytest template in a repo with a go.mod still works.
func selectTemplate(ctx context.Context, cmd, tmpl string, c *change) Plan {
	if firstPlaceholder(tmpl) < 0 {
		return full(cmd, "its affected_commands entry has no {files}, {packages} or {test_files}")
	}
	out := tmpl
	for _, ph := range placeholders {
		if !strings.Contains(tmpl, ph) {
			continue
		}
		args, why := c.placeholder(ctx, ph, tmpl)
		if why != "" {
			return full(cmd, why)
		}
		if len(args) == 0 {
			return Plan{Command: cmd, Why: "nothing for " + ph + " in the " + c.changedWhy()}
		}
		q, bad := quoteAll(args)
		if bad != "" {
			return full(cmd, unsafeWhy(bad))
		}
		out = strings.ReplaceAll(out, ph, q)
	}
	return Plan{Command: cmd, Run: []string{out}, Why: "affected_commands, for the " + c.changedWhy()}
}

func (c *change) placeholder(ctx context.Context, ph, tmpl string) ([]string, string) {
	words := strings.Fields(tmpl)
	isGo := len(words) > 0 && words[0] == "go" && c.goModule()
	isPy := strings.Contains(tmpl, "pytest")
	var out []string
	switch ph {
	case "{files}":
		for _, f := range c.files {
			if c.exists(f) {
				out = append(out, dotSlash(f))
			}
		}
	case "{packages}":
		if isGo {
			sel, why := goAffected(ctx, []string{"go", "test", "./..."}, c)
			if why != "" {
				return nil, why
			}
			for ip := range sel.run {
				out = append(out, dotSlash(sel.pkgs[ip].dir))
			}
			break
		}
		for _, f := range c.files {
			if !isDoc(f) {
				out = append(out, dotSlash(path.Dir(f)))
			}
		}
	case "{test_files}":
		switch {
		case isGo:
			sel, why := goAffected(ctx, []string{"go", "test", "./..."}, c)
			if why != "" {
				return nil, why
			}
			for ip := range sel.run {
				p := sel.pkgs[ip]
				for _, g := range p.goFiles {
					if strings.HasSuffix(g, "_test.go") {
						out = append(out, dotSlash(path.Join(p.dir, g)))
					}
				}
			}
		case isPy:
			tests, why := pyAffected(c)
			if why != "" {
				return nil, why
			}
			for _, t := range tests {
				out = append(out, dotSlash(t))
			}
		default:
			// The changed test files, and the test files named after a
			// changed file (lib/a.rb: a_spec.rb, test_a.py, a.test.ts).
			stems := map[string]bool{}
			for _, f := range c.files {
				switch {
				case isTestFile(f) && c.exists(f):
					out = append(out, dotSlash(f))
				case !isDoc(f):
					stems[stem(f)] = true
				}
			}
			if len(stems) > 0 {
				tests, ok := walk(c.dir, maxBuildFiles, func(rel string, d os.DirEntry) bool { return isTestFile(rel) })
				if !ok {
					return nil, "too many files to look for tests in"
				}
				for _, t := range tests {
					if stems[testStem(t)] {
						out = append(out, dotSlash(t))
					}
				}
			}
		}
	}
	return sortedSet(out), ""
}

// stem is a file's name without folder and extension, lower case.
func stem(p string) string {
	base := strings.ToLower(path.Base(p))
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return base
}

// testStem is the name of the file a test file is named after:
// a_spec.rb, a_test.go, test_a.py, a.test.ts, ATests.cs -> "a".
func testStem(p string) string {
	s := stem(p)
	for _, suf := range []string{"_test", "_spec", "tests", "test"} {
		if t := strings.TrimSuffix(s, suf); t != s && t != "" {
			return t
		}
	}
	return strings.TrimPrefix(s, "test_")
}

// goModule reports whether the check folder is in a Go module.
func (c *change) goModule() bool {
	for d := c.dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return true
		}
		up := filepath.Dir(d)
		if up == d || len(up) < len(c.root) {
			return false
		}
		d = up
	}
}
