package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// A task's check can run just the task's own tests: {tests} stands for its
// test files (tests.files that a test runner can run and that exist when
// the check starts) and {test_dirs} for their folders ("./cmd/rw", "."),
// e.g. "flutter test {tests}" or "go test {test_dirs}". One failing test
// elsewhere in a big suite then does not fail every run.
const (
	phTests    = "{tests}"
	phTestDirs = "{test_dirs}"
)

func usesTestsPlaceholder(check string) bool {
	return strings.Contains(check, phTests) || strings.Contains(check, phTestDirs)
}

// expandTests fills in a check's placeholders from the task's test files
// as they are in dir.
func expandTests(check, dir string, tests *benchTests) (string, error) {
	if !usesTestsPlaceholder(check) {
		return check, nil
	}
	var files []string
	if tests != nil {
		for _, f := range tests.Files {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err == nil && isTestFile(f) {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		return "", fmt.Errorf("the check uses %s or %s, but the task has no test files", phTests, phTestDirs)
	}
	check = strings.ReplaceAll(check, phTests, quoteAll(files))
	return strings.ReplaceAll(check, phTestDirs, quoteAll(testDirs(files))), nil
}

// testDirs are the folders of files as package paths: "./a/b", or "." for
// the top.
func testDirs(files []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range files {
		d := path.Dir(f)
		if d != "." {
			d = "./" + d
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func quoteAll(list []string) string {
	q := make([]string, len(list))
	for i, s := range list {
		q[i] = argQuote(s)
	}
	return strings.Join(q, " ")
}

// narrowCheck makes a check run only the given test files (a commit's own
// tests, --own-tests), for the test runners it knows: go test ./...,
// flutter/dart test, pytest, jest, vitest and npm/yarn/pnpm test. Each
// "&&" part is narrowed on its own; a part with other arguments, or with
// no test file its runner takes, stays as it is. ok is false when nothing
// was narrowed.
func narrowCheck(check string, files []string) (narrowed string, ok bool) {
	parts := strings.Split(check, "&&")
	for i, p := range parts {
		if n := narrowPart(strings.TrimSpace(p), files); n != "" {
			lead, trail := p[:len(p)-len(strings.TrimLeft(p, " "))], p[len(strings.TrimRight(p, " ")):]
			parts[i] = lead + n + trail
			ok = true
		}
	}
	return strings.Join(parts, "&&"), ok
}

// narrowPart narrows one command, or returns "".
func narrowPart(cmd string, files []string) string {
	f := strings.Fields(cmd)
	// onlyFlags: the runner gets no paths or names of its own.
	onlyFlags := func(rest []string) bool {
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return false
			}
		}
		return true
	}
	pick := func(keep func(string) bool) []string {
		var out []string
		for _, p := range files {
			if isTestFile(p) && keep(p) {
				out = append(out, p)
			}
		}
		return out
	}
	ext := func(exts ...string) func(string) bool {
		return func(p string) bool {
			for _, e := range exts {
				if strings.HasSuffix(strings.ToLower(p), e) {
					return true
				}
			}
			return false
		}
	}
	switch {
	case len(f) >= 2 && f[0] == "go" && f[1] == "test":
		// go test ./... becomes go test <the test files' packages>.
		pkgs := testDirs(pick(ext("_test.go")))
		n := 0
		for i, a := range f {
			if a == "./..." {
				n++
				f[i] = quoteAll(pkgs)
			}
		}
		if n != 1 || len(pkgs) == 0 {
			return ""
		}
		return strings.Join(f, " ")
	case len(f) >= 2 && (f[0] == "flutter" || f[0] == "dart") && f[1] == "test" && onlyFlags(f[2:]):
		// Not integration tests: they need a device.
		dart := pick(func(p string) bool {
			return strings.HasSuffix(p, "_test.dart") && !strings.HasPrefix(p, "integration_test/") && !strings.Contains(p, "/integration_test/")
		})
		return appendFiles(cmd, dart)
	}
	prefix := func(pre ...string) bool {
		return len(f) >= len(pre) && strings.Join(f[:len(pre)], " ") == strings.Join(pre, " ")
	}
	for _, pre := range [][]string{{"python", "-m", "pytest"}, {"python3", "-m", "pytest"}, {"py", "-m", "pytest"}, {"uv", "run", "pytest"},
		{"pytest"}, {"py.test"}} {
		if prefix(pre...) {
			if !onlyFlags(f[len(pre):]) {
				return ""
			}
			return appendFiles(cmd, pick(ext(".py")))
		}
	}
	for _, pre := range [][]string{{"npx", "jest"}, {"jest"}, {"npx", "vitest", "run"}, {"vitest", "run"}, {"yarn", "test"}, {"pnpm", "test"},
		{"npm", "test"}, {"npm", "run", "test"}} {
		if prefix(pre...) {
			rest := f[len(pre):]
			if !onlyFlags(rest) {
				return ""
			}
			js := pick(ext(".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".mts", ".cts"))
			if pre[0] == "npm" && len(js) > 0 && !slices.Contains(rest, "--") {
				// npm passes arguments on to the script only after --.
				cmd += " --"
			}
			return appendFiles(cmd, js)
		}
	}
	return ""
}

func appendFiles(cmd string, files []string) string {
	if len(files) == 0 {
		return ""
	}
	return cmd + " " + quoteAll(files)
}
