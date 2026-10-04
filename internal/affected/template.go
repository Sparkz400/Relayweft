package affected

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
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
// files, {packages} the affected Go packages (else the changed files'
// folders), {test_files} the affected test files (Go, pytest, else the
// changed test files). All are relative to the check folder.
func selectTemplate(ctx context.Context, cmd, tmpl string, c *change) Plan {
	if firstPlaceholder(tmpl) < 0 {
		return full(cmd, "its affected_commands entry has no {files}, {packages} or {test_files}")
	}
	out := tmpl
	for _, ph := range placeholders {
		if !strings.Contains(tmpl, ph) {
			continue
		}
		args, why := c.placeholder(ctx, ph)
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

func (c *change) placeholder(ctx context.Context, ph string) ([]string, string) {
	var out []string
	switch ph {
	case "{files}":
		for _, f := range c.files {
			if c.exists(f) {
				out = append(out, dotSlash(f))
			}
		}
	case "{packages}":
		if c.goModule() {
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
		case c.goModule():
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
		case slices.ContainsFunc(c.files, func(f string) bool { return strings.HasSuffix(f, ".py") }):
			tests, why := pyAffected(c)
			if why != "" {
				return nil, why
			}
			for _, t := range tests {
				out = append(out, dotSlash(t))
			}
		default:
			for _, f := range c.files {
				if isTestFile(f) && c.exists(f) {
					out = append(out, dotSlash(f))
				}
			}
		}
	}
	return sortedSet(out), ""
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
