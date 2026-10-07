package affected

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// `vitest related` builds its graph with vite: from each test file it
// follows what vite resolves (aliases, workspace packages, the projects of
// a workspace) and stops at files under node_modules, at virtual modules
// and at anything Node loads itself. Checked on vitest 1.2.2 and 3.2.7
// with real workspaces (docs/bench/2026-10-06-affected-real-projects.md):
// projects (test.projects and vitest.workspace), symlinked workspace
// packages, resolve.alias and the react, react-swc, vue and tsconfig-paths
// plugins find every related test; a plugin's virtual module,
// preserveSymlinks, require() and vi.importActual found none, and a config
// root found none unless the paths are given relative to it. rw narrows
// only when none of those can stand between a test and a changed file, or
// when it can make up for them: paths relative to a root it reads, and the
// files that require() a changed file added to the run (vitest also runs a
// test named there, and the tests that import a file named there).

// vitest before 1.2.2 misses related tests that reach a changed file
// through other files: on a real project, 0.34.6 and 1.2.1 ran no test for
// a module that only other modules import, 1.2.2 ran all three. With
// --passWithNoTests that is a pass.
var vitestFixed = [3]int{1, 2, 2}

// vitestRelated is what a narrowed vitest run needs besides the changed
// files.
type vitestRelated struct {
	// root is the config's root relative to the check folder, as a slash
	// path ("" when there is none): vitest resolves the named files
	// against it.
	root string
	// extra lists dir-relative files that load a changed file where vite
	// does not look (require(), vi.importActual): named too, so vitest
	// runs the tests that import them.
	extra []string
}

// vitestRelatedWorks says what the narrowed run needs, or why `vitest
// related` cannot be trusted here.
func vitestRelatedWorks(c *change) (vitestRelated, string) {
	root, why := vitestConfigs(c)
	if why != "" {
		return vitestRelated{}, why
	}
	vs, why := vitestVersions(c)
	if why != "" {
		return vitestRelated{}, why
	}
	for _, v := range vs {
		if why := vitestVersionWhy(v); why != "" {
			return vitestRelated{}, why
		}
	}
	extra, why := vitestGraph(c)
	return vitestRelated{root: root, extra: extra}, why
}

func vitestVersionWhy(v string) string {
	var n [3]int
	for i, part := range strings.SplitN(strings.SplitN(v, "-", 2)[0], ".", 3) {
		x, err := strconv.Atoi(part)
		if err != nil {
			return "rw cannot read the vitest version " + strconv.Quote(clipStr(v, 40))
		}
		n[i] = x
	}
	// A pre-release of 1.2.2 may lack the fix.
	if slices.Compare(n[:], vitestFixed[:]) < 0 || (n == vitestFixed && strings.Contains(v, "-")) {
		return "vitest " + strconv.Quote(clipStr(v, 40)) + " misses related tests that reach a changed file through other files (fixed in 1.2.2)"
	}
	return ""
}

// vitestVersions reads the versions of the vitest node finds from the
// check folder: in its node_modules or one of a folder above, up to the
// repo; with Yarn Plug'n'Play (no node_modules), every vitest the
// yarn.lock next to .pnp.cjs resolves.
func vitestVersions(c *change) ([]string, string) {
	const unknown = "rw cannot find the vitest version (node_modules/vitest/package.json, or yarn.lock with Plug'n'Play), and vitest before 1.2.2 misses related tests"
	dir := c.dir
	for {
		data, err := os.ReadFile(filepath.Join(dir, "node_modules", "vitest", "package.json"))
		if err == nil {
			var pkg struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &pkg) != nil || pkg.Version == "" {
				return nil, unknown
			}
			return []string{pkg.Version}, ""
		}
		for _, pnp := range []string{".pnp.cjs", ".pnp.js"} {
			if _, err := os.Stat(filepath.Join(dir, pnp)); err != nil {
				continue
			}
			lock, err := os.ReadFile(filepath.Join(dir, "yarn.lock"))
			if err != nil {
				return nil, "Plug'n'Play without a yarn.lock: " + unknown
			}
			if vs := yarnLockVersions(lock, "vitest"); len(vs) > 0 {
				return vs, ""
			}
			return nil, unknown
		}
		up := filepath.Dir(dir)
		if _, ok := relDir(c.root, up); !ok || up == dir {
			return nil, unknown
		}
		dir = up
	}
}

// yarnLockVersions lists the versions a yarn.lock (Berry or classic)
// resolves the package name to.
func yarnLockVersions(lock []byte, name string) []string {
	var out []string
	in := false
	sc := bufio.NewScanner(bytes.NewReader(lock))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			// "vitest@npm:^3.2.7, vitest@npm:^3.0.0": (Berry) or
			// vitest@^1.0.0, "vitest@>=1": (classic).
			in = false
			for _, d := range strings.Split(strings.TrimSuffix(line, ":"), ",") {
				d = strings.Trim(strings.TrimSpace(d), `"`)
				at := strings.Index(d[min(1, len(d)):], "@")
				if at >= 0 && d[:at+min(1, len(d))] == name {
					in = true
				}
			}
			continue
		}
		if !in {
			continue
		}
		t := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(t, "version:"); ok && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			out = append(out, strings.Trim(strings.TrimSpace(v), `"`))
		} else if v, ok := strings.CutPrefix(t, "version "); ok && !strings.HasPrefix(line, "   ") {
			out = append(out, strings.Trim(strings.TrimSpace(v), `"`))
		}
	}
	return out
}

var (
	// reViteConfigName names the files vitest reads its configuration and
	// its projects from.
	reViteConfigName = regexp.MustCompile(`^(?:vitest|vite)\.(?:config|workspace)\.[cm]?[jt]s$|^vitest\.workspace\.json$`)
	// What makes `vitest related` miss tests (each seen on a real run):
	// preserveSymlinks keeps workspace packages under node_modules, where
	// the search stops; external modules are loaded by Node. An extends
	// string or a config rw cannot read hides any of these. A root
	// (viteRoot) and plugins (vitePlugins) pass in the forms rw follows.
	reViteUnsafe = []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`\bpreserveSymlinks\b`), "keeps symlinks, so workspace packages stay under node_modules, where vitest's related-test search stops"},
		{regexp.MustCompile(`\bexternal\b`), "externalizes modules, which Node loads outside vitest's related-test search"},
		{regexp.MustCompile(`\bextends\s*:\s*['"` + "`" + `]`), "extends another configuration"},
	}
	reViteProjects = regexp.MustCompile(`\b(?:projects|workspace)\b`)
	reParentPath   = regexp.MustCompile(`['"` + "`" + `]\.\.[/\\]`)
	reViteRoot     = regexp.MustCompile(`\broot\b`)
	reVitePlugins  = regexp.MustCompile(`\bplugins\b`)
)

// The imports a vitest config may have that change nothing rw checks.
var viteConfigImports = []string{"vitest", "vitest/config", "vitest/node", "vite"}

// vitePluginsKnown are the plugins whose graph `vitest related` follows:
// they transform files or resolve aliases to files and serve no virtual
// module a test imports. Each was checked with vitest 3.2.7: a test that
// reaches a changed file through a .tsx, a .vue script or a tsconfig
// alias was found. Any other plugin may serve virtual modules (an inline
// one that did hid its test), so it runs in full.
var vitePluginsKnown = []string{"@vitejs/plugin-react", "@vitejs/plugin-react-swc", "@vitejs/plugin-vue", "vite-tsconfig-paths"}

// viteConfigOrder is where vitest looks for its configuration, in each
// folder from the one it runs in upwards; the first found is loaded.
var viteConfigOrder = []string{"vitest.config.ts", "vitest.config.mts", "vitest.config.cts", "vitest.config.js", "vitest.config.mjs", "vitest.config.cjs",
	"vite.config.ts", "vite.config.mts", "vite.config.cts", "vite.config.js", "vite.config.mjs", "vite.config.cjs"}

// viteScan is what the configs read so far tell.
type viteScan struct {
	main     string          // the config vitest loads, the only one that may set a root
	seen     map[string]bool // config files read: whether they define projects
	root     string          // the main config's root, absolute ("" = none)
	rootFrom string          // the config that sets it, for reasons
}

// vitestConfigs reads the configurations vitest may load from the check
// folder: those in it and the folders above (vitest looks upwards), and
// with projects or a workspace also those below. It returns the root the
// loaded config sets, relative to the check folder ("" = none), or says
// why a config may hide a related test.
func vitestConfigs(c *change) (string, string) {
	sc := &viteScan{seen: map[string]bool{}}
	projects := false
	for dir := c.dir; ; dir = filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", "cannot inspect the vitest configuration: " + err.Error()
		}
		if sc.main == "" {
			for _, n := range viteConfigOrder {
				if slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == n }) {
					sc.main = filepath.Join(dir, n)
					break
				}
			}
		}
		for _, e := range entries {
			n := e.Name()
			if !reViteConfigName.MatchString(n) {
				continue
			}
			if e.IsDir() {
				return "", n + " is not a configuration file"
			}
			p, why := scanViteConfig(c, filepath.Join(dir, n), sc)
			if why != "" {
				return "", why
			}
			if p || strings.HasPrefix(n, "vitest.workspace.") {
				projects = true
				data, _ := os.ReadFile(filepath.Join(dir, n))
				if reParentPath.Match(data) {
					return "", n + " names files above its folder, which may hold projects rw does not read"
				}
			}
		}
		if dir == c.root || filepath.Dir(dir) == dir {
			break
		}
		if _, ok := relDir(c.root, filepath.Dir(dir)); !ok {
			break
		}
	}
	if projects {
		below, ok := walk(c.dir, maxJSFiles, func(rel string, d os.DirEntry) bool { return reViteConfigName.MatchString(d.Name()) })
		if !ok {
			return "", "too many files to find the vitest projects"
		}
		for _, f := range below {
			if _, why := scanViteConfig(c, c.abs(f), sc); why != "" {
				return "", why
			}
		}
	}
	if sc.root == "" {
		return "", ""
	}
	if projects {
		return "", sc.rootFrom + " sets a root and defines projects, which rw does not follow together"
	}
	rel, err := filepath.Rel(c.dir, sc.root)
	if err != nil || filepath.IsAbs(rel) {
		return "", sc.rootFrom + " sets a root rw cannot reach from the check folder"
	}
	if rel = filepath.ToSlash(rel); rel == "." {
		rel = ""
	}
	return rel, ""
}

// scanViteConfig checks one config and the files it imports; projects
// reports a config that defines projects or a workspace.
func scanViteConfig(c *change, file string, sc *viteScan) (projects bool, why string) {
	if p, ok := sc.seen[file]; ok {
		return p, ""
	}
	sc.seen[file] = false
	defer func() {
		if why == "" {
			sc.seen[file] = projects
		}
	}()
	name := filepath.Base(file)
	if rel, ok := relDir(c.root, file); ok {
		name = rel
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return false, "cannot read " + name
	}
	imports, computed := jsImports(data)
	if computed {
		return false, name + " loads a computed path, which rw cannot read"
	}
	// The plugin lists are cut out before the other checks: their options
	// are the plugins' own (vite-tsconfig-paths takes a root of its own).
	data, plugins, why := vitePlugins(data)
	if why != "" {
		return false, name + " " + why
	}
	for _, u := range reViteUnsafe {
		if u.re.Match(data) {
			return false, name + " " + u.why
		}
	}
	if reViteRoot.Match(data) {
		if file != sc.main {
			return false, name + " sets a root, against which vitest resolves the changed files, and is not the one configuration vitest loads"
		}
		root, why := viteRoot(c, file, data)
		if why != "" {
			return false, name + " " + why
		}
		sc.root, sc.rootFrom = root, name
	}
	projects = reViteProjects.Match(data)
	for _, imp := range imports {
		switch {
		case strings.HasPrefix(imp.spec, "./") || strings.HasPrefix(imp.spec, "../"):
			target := ""
			base := filepath.Join(filepath.Dir(file), filepath.FromSlash(imp.spec))
			if _, ok := relDir(c.root, base); !ok {
				return false, fmt.Sprintf("%s imports %s, outside the repo", name, imp.spec)
			}
			for _, cand := range jsImportCandidates(filepath.ToSlash(base)) {
				if st, err := os.Stat(filepath.FromSlash(cand)); err == nil && !st.IsDir() {
					target = filepath.FromSlash(cand)
					break
				}
			}
			if target == "" {
				return false, fmt.Sprintf("%s imports %s, which rw cannot find", name, imp.spec)
			}
			p, why := scanViteConfig(c, target, sc)
			if why != "" {
				return false, why
			}
			projects = projects || p
		case isNodeBuiltin(imp.spec) || slices.Contains(viteConfigImports, imp.spec) || plugins[imp.spec]:
		default:
			return false, fmt.Sprintf("%s imports %s, a configuration rw does not read", name, imp.spec)
		}
	}
	return projects, ""
}

// reViteRootForms are the roots rw reads. vite resolves a string against
// the folder vitest runs in (checked: run from a folder below the config,
// root 'src' found no test). The other forms start from the config's own
// folder or the one vitest runs in. Group 1 names the start, group 2 the
// path under it.
var reViteRootForms = []*regexp.Regexp{
	regexp.MustCompile(`\broot\s*:\s*()['"]([^'"\n]*)['"]`),
	regexp.MustCompile(`\broot\s*:\s*(?:path\.)?(?:resolve|join)\(\s*(__dirname|import\.meta\.dirname)\s*(?:,\s*['"]([^'"\n]*)['"]\s*)?\)`),
	regexp.MustCompile(`\broot\s*:\s*(__dirname|import\.meta\.dirname)()\s*[,}\n]`),
	regexp.MustCompile(`\broot\s*:\s*fileURLToPath\(\s*new URL\(\s*(?:()['"]([^'"\n]*)['"])\s*,\s*import\.meta\.url\s*\)\s*\)`),
	regexp.MustCompile(`\broot\s*:\s*(process\.cwd)\(\)()`),
}

// viteRoot reads the root a config sets (data has its plugin lists cut
// out) as an absolute folder inside the repo, or says why it cannot.
func viteRoot(c *change, file string, data []byte) (string, string) {
	const unread = "sets a root rw cannot read, and vitest resolves the changed files against it"
	if len(reViteRoot.FindAllIndex(data, -1)) != 1 {
		return "", unread
	}
	for i, re := range reViteRootForms {
		m := re.FindSubmatch(data)
		if m == nil {
			continue
		}
		dir, rel := filepath.Dir(file), string(m[2])
		if i == 0 || i == 4 {
			dir = c.dir
		}
		if strings.ContainsAny(rel, `\$`) || strings.HasPrefix(rel, "/") || filepath.IsAbs(filepath.FromSlash(rel)) {
			return "", unread
		}
		root := filepath.Join(dir, filepath.FromSlash(rel))
		if _, ok := relDir(c.root, root); !ok {
			return "", "sets a root outside the repo"
		}
		return root, ""
	}
	return "", unread
}

var (
	// reImportBinding is an import with names: group 1 the default, group
	// 2 the named list, group 3 the module.
	reImportBinding = regexp.MustCompile(`\bimport\s+(?:([A-Za-z_$][\w$]*)\s*,?\s*)?(?:\{([^}]*)\})?\s*from\s*['"]([^'"\n]+)['"]`)
	// reJSCall is the start of a call: name(.
	reJSCall = regexp.MustCompile(`^([A-Za-z_$][\w$]*)\s*\(`)
)

// vitePlugins checks every plugin list in a config: each entry must call
// a plugin rw knows, imported by name. It returns data with those lists
// blanked out and the plugin packages the config may import.
func vitePlugins(data []byte) ([]byte, map[string]bool, string) {
	locs := reVitePlugins.FindAllIndex(data, -1)
	if len(locs) == 0 {
		return data, nil, ""
	}
	const unknown = "uses the vite plugin %s, which rw does not know; vitest's related-test search does not follow a plugin's virtual modules"
	known := map[string]string{} // local name -> package
	for _, m := range reImportBinding.FindAllSubmatch(data, -1) {
		pkg := string(m[3])
		if !slices.Contains(vitePluginsKnown, pkg) {
			continue
		}
		if len(m[1]) > 0 {
			known[string(m[1])] = pkg
		}
		for _, n := range strings.Split(string(m[2]), ",") {
			if f := strings.Fields(n); len(f) > 0 && f[0] != "type" {
				known[f[len(f)-1]] = pkg
			}
		}
	}
	out := slices.Clone(data)
	used := map[string]bool{}
	end := 0
	for _, loc := range locs {
		if loc[0] < end {
			continue // inside a list already read: react({ babel: { plugins: [...] } })
		}
		// plugins: [ ... ]; anything else (a variable, a spread, a
		// shorthand) is not read.
		i := skipSpace(data, loc[1])
		if i < len(data) && data[i] == ':' {
			i = skipSpace(data, i+1)
		} else {
			i = len(data)
		}
		if i >= len(data) || data[i] != '[' {
			return nil, nil, "uses vite plugins in a form rw cannot read; vitest's related-test search does not follow a plugin's virtual modules"
		}
		close, items := jsList(data, i)
		if close < 0 {
			return nil, nil, "has a plugin list rw cannot read"
		}
		for _, it := range items {
			it = strings.TrimSpace(it)
			if it == "" {
				continue
			}
			m := reJSCall.FindStringSubmatch(it)
			if m == nil || jsClosing(it, len(m[0])-1) != len(it)-1 {
				return nil, nil, fmt.Sprintf(unknown, strconv.Quote(clipStr(it, 40)))
			}
			pkg, ok := known[m[1]]
			if !ok {
				return nil, nil, fmt.Sprintf(unknown, m[1]+"()")
			}
			used[pkg] = true
		}
		for k := loc[0]; k <= close; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
		end = close + 1
	}
	return out, used, ""
}

func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\r' || data[i] == '\n') {
		i++
	}
	return i
}

// jsList reads the list that opens at data[open] ('['): the index of its
// closing bracket and its items, split at its own commas. Strings and
// nested brackets are skipped; -1 when it does not close.
func jsList(data []byte, open int) (int, []string) {
	depth, start := 0, open+1
	var items []string
	for i := open; i < len(data); i++ {
		switch ch := data[i]; ch {
		case '\'', '"', '`':
			j := jsStringEnd(data, i)
			if j < 0 {
				return -1, nil
			}
			i = j
		case '[', '(', '{':
			depth++
		case ']', ')', '}':
			depth--
			if depth == 0 {
				if ch != ']' {
					return -1, nil
				}
				return i, append(items, string(data[start:i]))
			}
		case ',':
			if depth == 1 {
				items = append(items, string(data[start:i]))
				start = i + 1
			}
		}
	}
	return -1, nil
}

// jsClosing is the index of the bracket that closes the one at s[open],
// or -1.
func jsClosing(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '\'', '"', '`':
			j := jsStringEnd([]byte(s), i)
			if j < 0 {
				return -1
			}
			i = j
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// jsStringEnd is the index of the quote that ends the string opening at
// data[i], or -1.
func jsStringEnd(data []byte, i int) int {
	q := data[i]
	for j := i + 1; j < len(data); j++ {
		switch data[j] {
		case '\\':
			j++
		case q:
			return j
		case '\n':
			if q != '`' {
				return -1
			}
		}
	}
	return -1
}

// maxVitestExtra caps the files added for require() and vi.importActual,
// which go on one command line.
const maxVitestExtra = 100

// vitestGraph lists the files that load a changed file where vitest's
// related-test search does not look: through require() or vi.importActual
// (Node loads those files, not vite), directly or through other files.
// Named with the changed files, they make vitest run the tests that import
// them, and a test among them runs itself (vitest 3.2.7: `related b.cjs`
// found no test of a.cjs, which requires b.cjs; `related b.cjs a.cjs`
// found it). It says why it cannot tell: a computed import, a workspace
// package installed as a copy under node_modules, or a file reached
// through require() that imports what rw cannot place.
func vitestGraph(c *change) ([]string, string) {
	t, why := readJSTree(c)
	if why != "" {
		return nil, why
	}
	if why := pnpVirtualWorkspace(c, t); why != "" {
		return nil, why
	}
	if len(t.computed) > 0 {
		return nil, t.computed[0] + " imports a computed path, which vitest's related-test search cannot follow"
	}
	if why := copiedPackages(c, t); why != "" {
		return nil, why
	}
	// targets lists the files an import from f can load: a relative path,
	// or every file of a workspace package. ok is false when rw cannot
	// place it: outside the check folder, or a bare name that is no
	// package the tree declares or installs (an alias, a virtual module).
	installed := jsInstalled(c, t)
	targets := func(f string, imp jsImport) (tg []string, ok bool, where string) {
		if strings.HasPrefix(imp.spec, "./") || strings.HasPrefix(imp.spec, "../") {
			tg, ok := t.resolveRel(f, imp.spec)
			return tg, ok, "outside the check folder"
		}
		name := jsPkgName(imp.spec)
		if p, ok := t.byName[name]; ok {
			return t.under(p.Dir), true, ""
		}
		if name == "" || isNodeBuiltin(imp.spec) || installed(name) {
			return nil, true, ""
		}
		return nil, false, "which is neither a file nor a package rw finds"
	}
	// Where the blind imports lead: every file a require() or
	// vi.importActual target reaches. vite reads none of their imports, so
	// rw must place each one.
	reached := map[string]string{} // file -> the blind import it comes from
	var queue []string
	for _, f := range t.files {
		for _, imp := range t.imports[f] {
			if !imp.blind {
				continue
			}
			tg, ok, where := targets(f, imp)
			if !ok {
				return nil, fmt.Sprintf("%s loads %s with require() or vi.importActual, %s; vitest's related-test search does not follow it", f, imp.spec, where)
			}
			for _, x := range tg {
				if _, done := reached[x]; !done {
					reached[x] = f + " loads " + imp.spec
					queue = append(queue, x)
				}
			}
		}
	}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, imp := range t.imports[x] {
			tg, ok, where := targets(x, imp)
			if !ok {
				return nil, fmt.Sprintf("%s (%s with require() or vi.importActual) imports %s, %s", x, reached[x], imp.spec, where)
			}
			for _, y := range tg {
				if _, done := reached[y]; !done {
					reached[y] = reached[x]
					queue = append(queue, y)
				}
			}
		}
	}
	if len(reached) == 0 {
		return nil, ""
	}
	// The files that reach a changed file, by any import rw reads.
	users := map[string][]string{} // file -> the files that import it
	for _, f := range t.files {
		for _, imp := range t.imports[f] {
			tg, _, _ := targets(f, imp)
			for _, x := range tg {
				users[x] = append(users[x], f)
			}
		}
	}
	reach := map[string]bool{}
	queue = queue[:0]
	for _, f := range c.files {
		reach[f] = true
		queue = append(queue, f)
	}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		for _, u := range users[x] {
			if !reach[u] {
				reach[u] = true
				queue = append(queue, u)
			}
		}
	}
	// A file that loads one of them blindly is where vitest's search
	// breaks off: name it, and vitest finds the tests that import it.
	changed := map[string]bool{}
	for _, f := range c.files {
		changed[f] = true
	}
	var extra []string
	for _, f := range sortedKeys(reach) {
		if changed[f] {
			continue
		}
		for _, imp := range t.imports[f] {
			if !imp.blind {
				continue
			}
			tg, _, _ := targets(f, imp)
			if slices.ContainsFunc(tg, func(x string) bool { return reach[x] }) {
				extra = append(extra, f)
				break
			}
		}
	}
	if len(extra) > maxVitestExtra {
		return nil, fmt.Sprintf("%d files reach a changed file through require() or vi.importActual, too many to name to vitest", len(extra))
	}
	return extra, ""
}

// jsInstalled reports the packages the tree declares (in a package.json
// in or above the check folder, up to the repo) or installs under a
// node_modules there.
func jsInstalled(c *change, t *jsTree) func(name string) bool {
	declared := map[string]bool{}
	for _, p := range t.packages {
		for d := range p.Deps {
			declared[d] = true
		}
	}
	var dirs []string
	for _, p := range t.packages {
		dirs = append(dirs, c.abs(p.Dir))
	}
	for dir := c.dir; ; dir = filepath.Dir(dir) {
		if dir != c.dir {
			if p, ok := readJSPackage(filepath.Join(dir, "package.json")); ok {
				for d := range p.Deps {
					declared[d] = true
				}
			}
		}
		dirs = append(dirs, dir)
		up := filepath.Dir(dir)
		if dir == c.root || up == dir {
			break
		}
		if _, ok := relDir(c.root, up); !ok {
			break
		}
	}
	return func(name string) bool {
		if declared[name] {
			return true
		}
		for _, d := range dirs {
			if _, err := os.Stat(filepath.Join(d, "node_modules", filepath.FromSlash(name))); err == nil {
				return true
			}
		}
		return false
	}
}

// copiedPackages says why a workspace package installed as a copy (npm
// install-links, pnpm injected packages) may hide tests, or "": vitest
// stops at files under node_modules.
func copiedPackages(c *change, t *jsTree) string {
	if len(t.byName) == 0 {
		return ""
	}
	dirs := map[string]bool{}
	for _, p := range t.packages {
		dirs[c.abs(p.Dir)] = true
	}
	for dir := c.dir; ; dir = filepath.Dir(dir) {
		dirs[dir] = true
		up := filepath.Dir(dir)
		if dir == c.root || up == dir {
			break
		}
		if _, ok := relDir(c.root, up); !ok {
			break
		}
	}
	for _, d := range sortedKeys(dirs) {
		for _, name := range sortedKeys(t.byName) {
			p := filepath.Join(d, "node_modules", filepath.FromSlash(name))
			real, ok := linkTarget(p)
			if !ok {
				continue
			}
			rel := filepath.ToSlash(real)
			if r, ok := relDir(c.root, real); ok {
				rel = r
			}
			if slices.Contains(strings.Split(strings.ToLower(rel), "/"), "node_modules") {
				where, _ := relDir(c.root, p)
				return fmt.Sprintf("workspace package %s is installed as a copy (%s), and vitest's related-test search stops under node_modules", name, where)
			}
		}
	}
	return ""
}

// linkTarget follows symlinks and Windows junctions (which
// filepath.EvalSymlinks leaves alone) from p; ok is false when p is not
// there.
func linkTarget(p string) (string, bool) {
	for range 16 {
		st, err := os.Lstat(p)
		if err != nil {
			return "", false
		}
		if st.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
			return p, true
		}
		l, err := os.Readlink(p)
		if err != nil {
			return p, true
		}
		if !filepath.IsAbs(l) {
			l = filepath.Join(filepath.Dir(p), l)
		}
		p = filepath.Clean(l)
	}
	return p, true
}
