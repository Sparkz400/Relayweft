package affected

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Workspaces whose packages each have their own test script and runner
// config: `npm test --workspaces` (npm run test -ws, --if-present) and
// `pnpm -r test` (pnpm --recursive run test), typed or as the root's test
// script. rw runs the test script of the changed packages and of the
// packages that depend on them, a whole package at a time.

// wsCmd is a recursive workspace test command.
type wsCmd struct {
	pm        string // "npm" or "pnpm"
	ifPresent bool
	prefix    []string // npm: "npm test" or "npm run test"
}

// wsShape recognizes a recursive workspace test command, typed or as the
// test script of `npm|pnpm|yarn [run] test`.
func wsShape(f []string, dir string) (wsCmd, bool) {
	if c, ok := wsDirect(f); ok {
		return c, true
	}
	for _, p := range [][]string{{"npm", "test"}, {"npm", "run", "test"}, {"pnpm", "test"}, {"pnpm", "run", "test"}, {"yarn", "test"}, {"yarn", "run", "test"}} {
		if !slices.Equal(f, p) {
			continue
		}
		script := testScript(dir)
		if script == "" || hasShellSyntax(script) {
			return wsCmd{}, false
		}
		return wsDirect(strings.Fields(script))
	}
	return wsCmd{}, false
}

func wsOK(f []string, dir string) bool {
	_, ok := wsShape(f, dir)
	return ok
}

func wsDirect(f []string) (wsCmd, bool) {
	if len(f) < 2 {
		return wsCmd{}, false
	}
	switch f[0] {
	case "npm":
		var c wsCmd
		c.pm = "npm"
		var i int
		switch {
		case f[1] == "test":
			i = 2
		case len(f) > 2 && f[1] == "run" && f[2] == "test":
			i = 3
		default:
			return wsCmd{}, false
		}
		c.prefix = f[:i]
		all := false
		for _, a := range f[i:] {
			switch a {
			case "--workspaces", "-ws", "--workspaces=true":
				all = true
			case "--if-present":
				c.ifPresent = true
			default:
				return wsCmd{}, false
			}
		}
		return c, all
	case "pnpm":
		rec, i := false, 1
		for ; i < len(f) && strings.HasPrefix(f[i], "-"); i++ {
			switch a := f[i]; {
			case a == "-r" || a == "--recursive":
				rec = true
			case a == "--if-present" || a == "--parallel" || a == "--stream" || a == "--no-bail" || a == "--sequential" ||
				strings.HasPrefix(a, "--workspace-concurrency=") || strings.HasPrefix(a, "--reporter="):
			default:
				return wsCmd{}, false
			}
		}
		rest := f[i:]
		if !rec || !(slices.Equal(rest, []string{"test"}) || slices.Equal(rest, []string{"run", "test"})) {
			return wsCmd{}, false
		}
		return wsCmd{pm: "pnpm"}, true
	}
	return wsCmd{}, false
}

// wsRun is the command that runs the test script of these packages.
func (w wsCmd) run(names []string) []string {
	if w.pm == "pnpm" {
		var out []string
		for _, n := range names {
			out = append(out, "pnpm --filter "+n+" run test")
		}
		return out
	}
	args := append([]string(nil), w.prefix...)
	for _, n := range names {
		args = append(args, "--workspace="+n)
	}
	if w.ifPresent {
		args = append(args, "--if-present")
	}
	return []string{strings.Join(args, " ")}
}

func wsAllowed(f []string, dir string) ([]string, string) {
	w, _ := wsShape(f, dir)
	pkgs, why := wsPackages(dir, w)
	if why != "" {
		return nil, ""
	}
	var out []string
	for _, p := range pkgs {
		if wsName(p.Name) {
			out = append(out, w.run([]string{p.Name})[0])
		}
	}
	if w.pm == "pnpm" {
		return out, "pnpm --filter <package> run test"
	}
	return out, strings.Join(w.prefix, " ") + " --workspace=<package> ..."
}

// wsName reports a package name rw passes to the package manager: no
// spaces or commas (Claude's allow rules are comma-joined).
func wsName(n string) bool {
	return n != "" && safeArg(n) && !strings.ContainsAny(n, " ,=")
}

// wsPackages lists the workspace's packages (not the root), or says why
// rw cannot.
func wsPackages(dir string, w wsCmd) ([]jsPackage, string) {
	var patterns []string
	switch w.pm {
	case "npm":
		root, ok := readJSPackage(filepath.Join(dir, "package.json"))
		if !ok {
			return nil, "cannot read package.json"
		}
		var list []string
		var obj struct {
			Packages []string `json:"packages"`
		}
		if raw := root.raw["workspaces"]; raw == nil {
			return nil, "package.json defines no workspaces"
		} else if json.Unmarshal(raw, &list) == nil {
			patterns = list
		} else if json.Unmarshal(raw, &obj) == nil {
			patterns = obj.Packages
		} else {
			return nil, "cannot read the workspaces in package.json"
		}
	case "pnpm":
		data, err := os.ReadFile(filepath.Join(dir, "pnpm-workspace.yaml"))
		if err != nil {
			return nil, "pnpm runs it without a pnpm-workspace.yaml in the check folder"
		}
		var ws struct {
			Packages []string `yaml:"packages"`
		}
		if yaml.Unmarshal(data, &ws) != nil {
			return nil, "cannot read pnpm-workspace.yaml"
		}
		patterns = ws.Packages
		if npmrc, err := os.ReadFile(filepath.Join(dir, ".npmrc")); err == nil && strings.Contains(string(npmrc), "include-workspace-root") {
			return nil, ".npmrc may include the workspace root in recursive runs"
		}
	}
	if len(patterns) == 0 {
		return nil, "the workspace lists no packages"
	}
	dirs, why := wsGlob(dir, patterns)
	if why != "" {
		return nil, why
	}
	var out []jsPackage
	for _, d := range dirs {
		p, ok := readJSPackage(filepath.Join(dir, filepath.FromSlash(d), "package.json"))
		if !ok {
			return nil, "cannot read " + d + "/package.json"
		}
		if p.Name == "" {
			return nil, d + "/package.json has no name"
		}
		p.Dir = d
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, "the workspace has no packages"
	}
	return out, ""
}

// wsGlob expands workspace patterns ("packages/*", "apps/**", "tools/cli",
// "!**/test/**") to the folders below dir that hold a package.json.
func wsGlob(dir string, patterns []string) ([]string, string) {
	match := map[string]bool{}
	for _, raw := range patterns {
		neg := strings.HasPrefix(raw, "!")
		p := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(raw, "!"), "./"), "/")
		p = strings.TrimSuffix(p, "/")
		if p == "" || strings.Contains(p, `\`) || strings.HasPrefix(p, "../") || p == ".." {
			return nil, fmt.Sprintf("rw does not follow the workspace pattern %q", raw)
		}
		if neg {
			for d := range match {
				if globMatch(strings.Split(p, "/"), strings.Split(d, "/")) {
					delete(match, d)
				}
			}
			continue
		}
		found, why := globDirs(dir, ".", strings.Split(p, "/"))
		if why != "" {
			return nil, why
		}
		for _, d := range found {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(d), "package.json")); err == nil {
				match[d] = true
			}
		}
	}
	delete(match, ".")
	return sortedKeys(match), ""
}

// globDirs lists the folders below dir/at that match the pattern's
// segments.
func globDirs(dir, at string, segs []string) ([]string, string) {
	if len(segs) == 0 {
		return []string{at}, ""
	}
	seg := segs[0]
	if _, err := path.Match(seg, ""); err != nil {
		return nil, "rw does not follow the workspace pattern segment " + seg
	}
	if !strings.ContainsAny(seg, "*?[") {
		next := path.Join(at, seg)
		if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(next))); err != nil || !st.IsDir() {
			return nil, ""
		}
		return globDirs(dir, next, segs[1:])
	}
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(at)))
	if err != nil {
		return nil, ""
	}
	var out []string
	if seg == "**" {
		// Zero folders, or one and still "**".
		sub, why := globDirs(dir, at, segs[1:])
		if why != "" {
			return nil, why
		}
		out = append(out, sub...)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "node_modules" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		next := path.Join(at, e.Name())
		if seg == "**" {
			sub, why := globDirs(dir, next, segs)
			if why != "" {
				return nil, why
			}
			out = append(out, sub...)
			continue
		}
		if ok, _ := path.Match(seg, e.Name()); ok {
			sub, why := globDirs(dir, next, segs[1:])
			if why != "" {
				return nil, why
			}
			out = append(out, sub...)
		}
	}
	return out, ""
}

// globMatch matches a folder's segments against a pattern's ("**" spans
// any number of folders).
func globMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if globMatch(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, _ := path.Match(pat[0], segs[0])
	return ok && globMatch(pat[1:], segs[1:])
}

// selectWorkspace narrows a recursive workspace test run to the packages
// with changed files and the packages that depend on them: by their
// package.json dependencies, by relative imports between packages and by
// imports of another package's name. A bare import rw cannot place (an
// alias, a dependency the package does not declare), a file of the
// workspace root, configuration and lockfiles make it full.
func selectWorkspace(cmd string, w wsCmd, c *change) Plan {
	for _, file := range c.files {
		base := strings.ToLower(path.Base(file))
		parts := strings.Split(file, "/")
		switch {
		case yarnRuntimeFile(file):
			return full(cmd, file+" changed (dependencies or configuration)")
		case containsStr(parts, "node_modules"):
			return full(cmd, file+" is a dependency")
		case base == "package.json" || base == "package-lock.json" || base == "yarn.lock" || base == "pnpm-lock.yaml" || base == "pnpm-workspace.yaml" ||
			base == ".npmrc" || base == "bun.lock" || base == "bun.lockb" || strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, ".babelrc") ||
			strings.HasPrefix(base, ".env") || base == ".swcrc" || strings.Contains(base, ".config.") || strings.Contains(base, "preset"):
			// Other packages may read a package's configuration by path
			// (a jest preset, an extended tsconfig).
			return full(cmd, file+" changed (dependencies or configuration)")
		}
	}
	pkgs, why := wsPackages(c.dir, w)
	if why != "" {
		return full(cmd, why)
	}
	var dirs []string
	byDir := map[string]jsPackage{}
	byName := map[string]jsPackage{}
	for _, p := range pkgs {
		if !wsName(p.Name) {
			return full(cmd, unsafeWhy(p.Name))
		}
		dirs = append(dirs, p.Dir)
		byDir[p.Dir] = p
		byName[p.Name] = p
	}
	root, _ := readJSPackage(c.abs("package.json"))
	var start []string
	for _, file := range c.files {
		switch d := owner(file, dirs); {
		case d != "":
			start = append(start, byDir[d].Name)
		case isDoc(file):
		default:
			return full(cmd, file+" belongs to the workspace root, which every package may use")
		}
	}
	if len(start) == 0 {
		return Plan{Command: cmd, Why: "no package is affected by the " + c.changedWhy()}
	}
	t, why := readJSTree(c)
	if why != "" {
		return full(cmd, why)
	}
	if len(t.computed) > 0 {
		return full(cmd, t.computed[0]+" imports a computed path, so rw cannot tell which packages it uses")
	}
	deps := map[string][]string{}
	for _, p := range pkgs {
		for n := range p.Deps {
			if _, ok := byName[n]; ok {
				deps[p.Name] = append(deps[p.Name], n)
			}
		}
	}
	for _, f := range t.files {
		from := owner(f, dirs)
		for _, imp := range t.imports[f] {
			spec := imp.spec
			var to string
			switch {
			case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
				target := path.Join(path.Dir(f), spec)
				if target == ".." || strings.HasPrefix(target, "../") {
					return full(cmd, fmt.Sprintf("%s imports %s, outside the check folder", f, spec))
				}
				to = owner(target, dirs)
				if to == "" && from != "" {
					return full(cmd, fmt.Sprintf("%s imports %s, a file of the workspace root", f, spec))
				}
				if to == "" {
					continue
				}
				to = byDir[to].Name
			default:
				name := jsPkgName(spec)
				if p, ok := byName[name]; ok {
					to = p.Name
					break
				}
				if from == "" || isNodeBuiltin(name) || root.Deps[name] || byDir[from].Deps[name] {
					continue
				}
				return full(cmd, fmt.Sprintf("%s imports %s, which is not a dependency of its package (an alias rw cannot follow?)", f, spec))
			}
			if from != "" && to != byDir[from].Name {
				deps[byDir[from].Name] = append(deps[byDir[from].Name], to)
			}
		}
	}
	set := reverseClosure(start, deps)
	var names []string
	runs := 0 // the packages the full command runs
	for _, p := range pkgs {
		// pnpm and npm --if-present skip a package without a test script;
		// plain npm fails on it, as the full run does.
		if p.Scripts["test"] == "" && (w.pm == "pnpm" || w.ifPresent) {
			continue
		}
		runs++
		if set[p.Name] {
			names = append(names, p.Name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return Plan{Command: cmd, Why: fmt.Sprintf("no affected package has a test script (%s)", list(sortedKeys(set), 4))}
	}
	if len(names) >= runs {
		return full(cmd, "every package is affected by the "+c.changedWhy())
	}
	return Plan{Command: cmd, Run: w.run(names), Why: fmt.Sprintf("the tests of %s (%d of %d packages), affected by the %s", list(names, 4), len(names), len(pkgs), c.changedWhy())}
}
