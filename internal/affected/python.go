package affected

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

var pytestValues = map[string]bool{"-k": true, "-m": true, "-p": true, "-n": true, "--maxfail": true, "--tb": true, "--durations": true,
	"-W": true, "-o": true, "--timeout": true, "--color": true, "--import-mode": true}

// pytestShape returns how many words start a pytest command (python -m
// pytest, pytest, uv run pytest, ...) followed by flags only, or 0.
func pytestShape(f []string) int {
	for _, p := range [][]string{{"python", "-m", "pytest"}, {"python3", "-m", "pytest"}, {"py", "-m", "pytest"}, {"uv", "run", "pytest"},
		{"poetry", "run", "pytest"}, {"pytest"}, {"py.test"}} {
		if len(f) >= len(p) && strings.Join(f[:len(p)], " ") == strings.Join(p, " ") && onlyFlags(f[len(p):], pytestValues) {
			return len(p)
		}
	}
	return 0
}

const maxPyFiles = 20000

var (
	rePyImport = regexp.MustCompile(`^\s*import\s+(.+)`)
	rePyFrom   = regexp.MustCompile(`^\s*from\s+(\.*)([\w.]*)\s+import\s+(.*)`)
)

// pyAffected finds the test files that import a changed module, directly
// or through other modules (by the module names a file could be imported
// as), or says why it cannot: conftest.py, settings, data files and a
// changed module that no test reaches.
func pyAffected(c *change) ([]string, string) {
	var changed []string
	for _, file := range c.files {
		base := strings.ToLower(path.Base(file))
		switch {
		case base == "conftest.py":
			return nil, file + " changed (shared fixtures)"
		case base == "pytest.ini" || base == "pyproject.toml" || base == "setup.cfg" || base == "setup.py" || base == "tox.ini" ||
			strings.HasPrefix(base, "requirements") || strings.HasPrefix(base, "pipfile") || base == "poetry.lock" || base == "uv.lock":
			return nil, file + " changed (settings or dependencies)"
		case strings.HasSuffix(base, ".py"):
			changed = append(changed, file)
		case isDoc(file):
		default:
			return nil, file + " is not Python code, so sy cannot tell which tests read it"
		}
	}
	if len(changed) == 0 {
		return nil, ""
	}
	files, ok := walk(c.dir, maxPyFiles*5, func(rel string, d os.DirEntry) bool { return strings.HasSuffix(rel, ".py") })
	if !ok || len(files) > maxPyFiles {
		return nil, "too many files to read the imports of"
	}
	// Every name a file could be imported as: its path from the top and
	// from each folder below (rootdir-relative, src layout, test folders
	// on sys.path). More names only add tests, never drop one.
	byName := map[string][]string{}
	addNames := func(file string) {
		mod := strings.TrimSuffix(file, ".py")
		mod = strings.TrimSuffix(mod, "/__init__")
		parts := strings.Split(mod, "/")
		for i := range parts {
			n := strings.Join(parts[i:], ".")
			byName[n] = append(byName[n], file)
		}
	}
	present := map[string]bool{}
	for _, file := range files {
		present[file] = true
		addNames(file)
	}
	for _, file := range changed {
		if !present[file] {
			if c.exists(file) {
				return nil, file + " is in a folder sy does not read (build output, a virtualenv, ...)"
			}
			addNames(file) // deleted: who imported it?
		}
	}
	deps := map[string][]string{}
	for _, file := range files {
		data, err := os.ReadFile(c.abs(file))
		if err != nil {
			return nil, "could not read " + file
		}
		deps[file] = pyImports(file, string(data), byName)
	}
	reached := reverseClosure(changed, deps)
	var tests []string
	for file := range reached {
		if present[file] && isTestFile(file) {
			tests = append(tests, file)
		}
	}
	for _, file := range changed {
		if isTestFile(file) || !present[file] {
			continue
		}
		hit := false
		for t := range reverseClosure([]string{file}, deps) {
			if t != file && present[t] && isTestFile(t) {
				hit = true
				break
			}
		}
		if !hit {
			return nil, fmt.Sprintf("no test imports %s, so sy cannot tell which tests cover it", file)
		}
	}
	return sortedSet(tests), ""
}

// selectPytest narrows pytest to the affected test files.
func selectPytest(cmd string, f []string, c *change) Plan {
	tests, why := pyAffected(c)
	if why != "" {
		return full(cmd, why)
	}
	if len(tests) == 0 {
		return Plan{Command: cmd, Why: "no test is affected by the " + c.changedWhy()}
	}
	args := make([]string, 0, len(tests))
	for _, t := range tests {
		args = append(args, dotSlash(t))
	}
	q, bad := quoteAll(args)
	if bad != "" {
		return full(cmd, unsafeWhy(bad))
	}
	return Plan{Command: cmd, Run: []string{cmd + " " + q}, Why: fmt.Sprintf("%s importing the %s", plural(len(tests), "test file", "test files"), c.changedWhy())}
}

func sortedSet(list []string) []string {
	m := map[string]bool{}
	for _, x := range list {
		m[x] = true
	}
	return sortedKeys(m)
}

// pyImports returns the files that file imports, by name. Importing a.b.c
// also runs a/__init__.py and a/b/__init__.py.
func pyImports(file, src string, byName map[string][]string) []string {
	var out []string
	add := func(mod string) {
		parts := strings.Split(mod, ".")
		for i := 1; i <= len(parts); i++ {
			out = append(out, byName[strings.Join(parts[:i], ".")]...)
		}
	}
	pkg := strings.Split(path.Dir(file), "/")
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if m := rePyFrom.FindStringSubmatch(line); m != nil {
			names := m[3]
			if strings.HasPrefix(strings.TrimSpace(names), "(") {
				for !strings.Contains(names, ")") && i+1 < len(lines) {
					i++
					names += " " + lines[i]
				}
			}
			base := m[2]
			if dots := len(m[1]); dots > 0 {
				// Relative to the file's package.
				up := pkg
				if dots-1 <= len(up) {
					up = up[:len(up)-(dots-1)]
				}
				prefix := strings.Join(up, ".")
				if prefix == "." {
					prefix = ""
				}
				base = strings.Trim(prefix+"."+base, ".")
			}
			if base != "" {
				add(base)
			}
			for _, n := range strings.FieldsFunc(names, func(r rune) bool { return r == ',' || r == '(' || r == ')' || r == ' ' || r == '\t' || r == '\\' }) {
				if n == "as" || n == "*" || strings.HasPrefix(n, "#") {
					continue
				}
				if base == "" {
					add(n)
				} else {
					out = append(out, byName[base+"."+n]...)
				}
			}
			continue
		}
		if m := rePyImport.FindStringSubmatch(line); m != nil {
			for _, part := range strings.Split(m[1], ",") {
				w := strings.Fields(strings.SplitN(part, "#", 2)[0])
				if len(w) > 0 {
					add(w[0])
				}
			}
		}
	}
	return out
}
