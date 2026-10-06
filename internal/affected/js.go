package affected

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sparkz400/relayweft/internal/proc"
)

// jsCmd is a jest or vitest command, run directly or through a package
// manager's test script.
type jsCmd struct {
	kind   string   // "jest" or "vitest" ("" = not one)
	pm     string   // npm, pnpm, yarn ("" = run directly)
	runner []string // how to start the runner directly (npx vitest, pnpm exec vitest, ...)
	flags  []string // the runner's own flags from the command or the script
}

var jestValues = map[string]bool{"-c": true, "--config": true, "-t": true, "--testNamePattern": true, "-w": true, "--maxWorkers": true,
	"--reporters": true, "--testTimeout": true, "--selectProjects": true, "--shard": true, "--coverageReporters": true}

// jsShape recognizes jest and vitest commands: `npx jest`, `jest`,
// `npx vitest run`, `vitest run`, and `npm|pnpm|yarn [run] test` whose
// package.json test script is one of those (flags only).
func jsShape(f []string, dir string) jsCmd {
	pre := func(p ...string) bool {
		return len(f) >= len(p) && strings.Join(f[:len(p)], " ") == strings.Join(p, " ")
	}
	direct := func(f []string) jsCmd {
		for _, p := range [][]string{{"npx", "jest"}, {"jest"}, {"yarn", "jest"}, {"pnpm", "jest"}, {"pnpm", "exec", "jest"}} {
			if len(f) >= len(p) && strings.Join(f[:len(p)], " ") == strings.Join(p, " ") && onlyFlags(f[len(p):], jestValues) {
				return jsCmd{kind: "jest", runner: p, flags: f[len(p):]}
			}
		}
		for _, p := range [][]string{{"npx", "vitest"}, {"vitest"}, {"yarn", "vitest"}, {"pnpm", "vitest"}, {"pnpm", "exec", "vitest"}} {
			if len(f) >= len(p) && strings.Join(f[:len(p)], " ") == strings.Join(p, " ") {
				rest := f[len(p):]
				if len(rest) > 0 && rest[0] == "run" {
					rest = rest[1:]
				} else if !containsStr(rest, "--run") {
					continue // watch mode: never as a check
				}
				if onlyFlags(rest, jestValues) {
					return jsCmd{kind: "vitest", runner: p, flags: rest}
				}
			}
		}
		return jsCmd{}
	}
	if c := direct(f); c.kind != "" {
		return c
	}
	for _, p := range [][]string{{"npm", "test"}, {"npm", "run", "test"}, {"pnpm", "test"}, {"pnpm", "run", "test"}, {"yarn", "test"}, {"yarn", "run", "test"}} {
		if !pre(p...) || len(f) != len(p) {
			continue
		}
		script := testScript(dir)
		c := direct(strings.Fields(script))
		if c.kind == "" || hasShellSyntax(script) {
			return jsCmd{}
		}
		c.pm = p[0]
		// The script's runner, started through the package manager.
		bin := c.runner[len(c.runner)-1]
		switch c.pm {
		case "npm":
			// --no: never download a runner the project does not have.
			// "--": after a flag of its own, npx reads every later flag
			// (--config, --run) as one of npm's.
			c.runner = []string{"npx", "--no", "--", bin}
		case "pnpm":
			c.runner = []string{"pnpm", "exec", bin}
		case "yarn":
			c.runner = []string{"yarn", bin}
		}
		return c
	}
	return jsCmd{}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func testScript(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Scripts["test"])
}

// jsNarrow is the narrowed command without its files: jest gets
// --findRelatedTests (through the script, so its setup is kept), vitest
// runs `vitest related` (a subcommand, which a script cannot take).
func jsNarrow(cmd string, c jsCmd) string {
	if c.kind == "jest" {
		if c.pm == "npm" {
			// npm passes arguments on to the script only after "--"; pnpm
			// and yarn pass them all.
			return cmd + " -- --findRelatedTests --passWithNoTests"
		}
		return cmd + " --findRelatedTests --passWithNoTests"
	}
	return strings.Join(append(append(append([]string(nil), c.runner...), "related", "--run", "--passWithNoTests"), c.flags...), " ")
}

func jsAllowed(f []string, dir string) ([]string, string) {
	c := jsShape(f, dir)
	n := jsNarrow(strings.Join(f, " "), c)
	if c.kind == "jest" {
		return nil, n + " <files>" // starts with the command itself
	}
	return []string{n}, n + " <files>"
}

var jsCode = map[string]bool{".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true, ".mts": true, ".cts": true}

// selectJS narrows a jest or vitest run to the tests related to the
// changed source files. Dependencies, configs, snapshots, mocks, deleted
// and non-code files make it full.
func selectJS(ctx context.Context, cmd string, f []string, c *change) Plan {
	js := jsShape(f, c.dir)
	var files []string
	for _, file := range c.files {
		base := strings.ToLower(path.Base(file))
		ext := path.Ext(base)
		parts := strings.Split(file, "/")
		switch {
		case containsStr(parts, "node_modules"):
			return full(cmd, file+" is a dependency")
		case base == "package.json" || base == "package-lock.json" || base == "yarn.lock" || base == "pnpm-lock.yaml" || base == "bun.lock" || base == "bun.lockb" ||
			strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, ".babelrc") || strings.HasPrefix(base, ".env") || base == ".swcrc" ||
			strings.Contains(base, ".config."):
			return full(cmd, file+" changed (dependencies or configuration)")
		case containsStr(parts, "__snapshots__") || containsStr(parts, "__mocks__") || containsStr(parts, "__fixtures__"):
			return full(cmd, file+" is a snapshot, mock or fixture")
		case jsCode[ext] || ext == ".json":
			if !c.exists(file) {
				return full(cmd, file+" was deleted, so the tests that used it cannot be found")
			}
			files = append(files, dotSlash(file))
		case isDoc(file):
		default:
			return full(cmd, file+" is not JavaScript or TypeScript, so rw cannot tell which tests use it")
		}
	}
	if len(files) == 0 {
		return Plan{Command: cmd, Why: "no test is affected by the " + c.changedWhy()}
	}
	q, bad := quoteAll(sortedSet(files))
	if bad != "" {
		return full(cmd, unsafeWhy(bad))
	}
	var why string
	if js.kind == "jest" {
		why = jestSeesAll(ctx, js, c, files)
	} else {
		why = vitestRelatedWorks(c)
	}
	if why != "" {
		return full(cmd, why)
	}
	return Plan{Command: cmd, Run: []string{jsNarrow(cmd, js) + " " + q}, Why: js.kind + " tests related to the " + c.changedWhy()}
}

// jest only finds related tests among the files it indexes: under its
// roots, with an extension in moduleFileExtensions, not in
// modulePathIgnorePatterns. A changed file it does not index is dropped
// without a word, and --passWithNoTests turns "no tests" into a pass. Many
// projects set roots: ["test"] or ["<rootDir>/src"]. rw asks jest for its
// resolved configuration (it runs the project's jest config, as the tests
// do) and runs the full command unless every project sees every changed
// file and every file that could link a test to one.

// jestConfig runs `jest --showConfig` in dir (tests swap it). argv holds
// only words that passed hasShellSyntax, so the shell sees them as typed.
var jestConfig = func(ctx context.Context, dir string, argv []string) ([]byte, error) {
	cmd := proc.Shell(ctx, strings.Join(argv, " "))
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, clipStr(lastLine(stderr.String()), 200))
	}
	return out, nil
}

// jestShowConfig is the command that prints the configuration the test
// command uses: its runner with its flags.
func jestShowConfig(js jsCmd) []string {
	return append(append(append([]string(nil), js.runner...), js.flags...), "--showConfig")
}

// jestSeesAll says why jest's --findRelatedTests could miss tests of the
// changed files (dir-relative, "./" in front), or "".
func jestSeesAll(ctx context.Context, js jsCmd, c *change, files []string) string {
	argv := jestShowConfig(js)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // a config that waits for something
	defer cancel()
	var out []byte
	var err error
	if c.exec != nil {
		out, err = c.exec(ctx, c.dir, argv)
	} else {
		out, err = jestConfig(ctx, c.dir, argv)
	}
	if err != nil {
		return "jest --showConfig failed, so rw cannot tell which files jest sees: " + clipStr(err.Error(), 200)
	}
	var shown struct {
		Configs []jestProject `json:"configs"`
	}
	i := bytes.IndexByte(out, '{')
	if i < 0 || json.NewDecoder(bytes.NewReader(out[i:])).Decode(&shown) != nil || len(shown.Configs) == 0 {
		return "could not read jest --showConfig"
	}
	for k := range shown.Configs {
		p := &shown.Configs[k]
		if p.Cwd == "" { // where jest ran: the check folder, in the sandbox too
			if c.exec != nil {
				return "this jest does not say which folder it ran in"
			}
			p.Cwd = c.dir
		}
		for _, s := range p.ModulePathIgnorePatterns {
			re, err := regexp.Compile(s)
			if err != nil {
				return "rw cannot read jest's modulePathIgnorePatterns"
			}
			p.ignore = append(p.ignore, re)
		}
	}
	for _, f := range files {
		f = strings.TrimPrefix(f, "./")
		for _, p := range shown.Configs {
			if why := p.unseen(f); why != "" {
				return fmt.Sprintf("jest does not see %s (%s), so it cannot find the tests that use it", f, why)
			}
		}
	}
	// jest follows imports only through the files it sees: a test that
	// imports ../index.js, outside roots: ["<rootDir>/src"], which imports
	// a changed src/a.js, is not related to src/a.js. So no file jest sees
	// may import, by a relative path, a file it does not see.
	unseen := func(f string) string {
		for _, p := range shown.Configs {
			if why := p.unseen(f); why != "" {
				return why
			}
		}
		return ""
	}
	code, ok := walk(c.dir, maxJSFiles, func(rel string, d os.DirEntry) bool { return jsCode[strings.ToLower(path.Ext(rel))] })
	if !ok {
		return "too many files to check what jest sees"
	}
	for _, f := range code {
		if unseen(f) != "" {
			continue
		}
		data, err := os.ReadFile(c.abs(f))
		if err != nil {
			return "could not read " + f
		}
		for _, m := range reJSRelImport.FindAllSubmatch(data, -1) {
			target := path.Join(path.Dir(f), string(m[1]))
			if target == ".." || strings.HasPrefix(target, "../") {
				return fmt.Sprintf("%s imports %s, outside the check folder", f, m[1])
			}
			for _, cand := range jsImportCandidates(target) {
				if st, err := os.Stat(c.abs(cand)); err != nil || st.IsDir() {
					continue
				}
				if why := unseen(cand); why != "" {
					return fmt.Sprintf("%s imports %s, which jest does not see (%s), so it may miss tests that reach a change through it", f, cand, why)
				}
			}
		}
	}
	return ""
}

const maxJSFiles = 50000

// reJSRelImport finds an import of a relative path (import, export ...
// from, require, dynamic import); group 1 is the path.
var reJSRelImport = regexp.MustCompile(`(?:\bfrom\s*|\brequire\s*\(\s*|\bimport\s*\(\s*|\bimport\s+)['"` + "`" + `](\.\.?/[^'"` + "`" + `\n]*)`)

// jsImportCandidates are the files an import of target can load.
func jsImportCandidates(target string) []string {
	out := []string{target}
	for ext := range jsCode {
		out = append(out, target+ext, target+"/index"+ext)
	}
	return append(out, target+".json")
}

// jestProject is one project of `jest --showConfig`.
type jestProject struct {
	Cwd                      string   `json:"cwd"`
	Roots                    []string `json:"roots"`
	ModulePathIgnorePatterns []string `json:"modulePathIgnorePatterns"`
	ModuleFileExtensions     []string `json:"moduleFileExtensions"`
	ignore                   []*regexp.Regexp
}

// unseen says why jest's index of this project leaves out the
// dir-relative slash path f, or "".
func (p jestProject) unseen(f string) string {
	seen := false
	for _, r := range p.Roots {
		if underRoot(p.Cwd, r, f) {
			seen = true
			break
		}
	}
	if !seen {
		return "outside its roots"
	}
	if ext := strings.TrimPrefix(path.Ext(f), "."); !slices.Contains(p.ModuleFileExtensions, ext) {
		return "." + ext + " is not in its moduleFileExtensions"
	}
	abs := strings.TrimRight(p.Cwd, `/\`) + "/" + f
	if strings.Contains(p.Cwd, `\`) {
		abs = strings.ReplaceAll(abs, "/", `\`)
	}
	for _, re := range p.ignore {
		if re.MatchString(abs) {
			return "it matches modulePathIgnorePatterns"
		}
	}
	return ""
}

// underRoot reports whether the dir-relative slash path f lies under
// root; root and base (the folder jest ran in) are absolute, as jest
// prints them (Windows or slash paths). Below the check folder jest's
// index is case-sensitive: roots ["<rootDir>/Src"] do not hold src/a.js.
func underRoot(base, root, f string) bool {
	norm := func(p string) string { return strings.TrimRight(strings.ReplaceAll(p, `\`, "/"), "/") + "/" }
	b, r := norm(base), norm(root)
	switch {
	case hasPrefixFold(b, r):
		return true // the root holds the whole check folder
	case hasPrefixFold(r, b):
		return strings.HasPrefix(f+"/", r[len(b):]) // r[len(b):] is "src/" or "packages/a/"
	}
	return false
}

// vitest before 1.2.2 misses related tests that reach a changed file
// through other files: on a real project, 0.34.6 and 1.2.1 ran no test for
// a module that only other modules import, 1.2.2 ran all three. With
// --passWithNoTests that is a pass.
var vitestFixed = [3]int{1, 2, 2}

// vitestRelatedWorks says why `vitest related` cannot be trusted here, or "".
func vitestRelatedWorks(c *change) string {
	v, ok := vitestVersion(c)
	if !ok {
		return "rw cannot find the vitest version (node_modules/vitest/package.json), and vitest before 1.2.2 misses related tests"
	}
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

// vitestVersion reads the version of the vitest node finds from the check
// folder: in its node_modules or in one of a folder above, up to the repo.
func vitestVersion(c *change) (string, bool) {
	dir := c.dir
	for {
		data, err := os.ReadFile(filepath.Join(dir, "node_modules", "vitest", "package.json"))
		if err == nil {
			var pkg struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &pkg) != nil || pkg.Version == "" {
				return "", false
			}
			return pkg.Version, true
		}
		up := filepath.Dir(dir)
		if _, ok := relDir(c.root, up); !ok || up == dir {
			return "", false
		}
		dir = up
	}
}
