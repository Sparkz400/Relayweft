package affected

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// What JavaScript and TypeScript files import, read from their text. The
// runners build their own graphs; rw reads the imports only to find where
// a runner's graph stops (jest's index, vitest's require() blind spot, a
// package boundary) and runs in full when a change may lie beyond.

// jsImport is one import in a file.
type jsImport struct {
	spec string
	// blind: loaded where vitest's related-test search does not look:
	// require() (Node loads it, not vite) and vi.importActual/importMock.
	blind bool
}

var (
	// reJSImport finds an import of a literal path: import, export ...
	// from, require, dynamic import, jest.requireActual/requireMock,
	// vi.importActual/importMock. Group 1 is the call (when there is one),
	// group 2 the path.
	reJSImport = regexp.MustCompile(`(?:\bfrom\s*|\b(require|import|requireActual|requireMock|importActual|importMock)\s*(?:<[^\n]*?>)?\s*\(\s*|\bimport\s+)['"` + "`" + `]([^'"` + "`" + `\n]+)['"` + "`]")
	// reJSComputed finds require() or import() of a computed path, which no
	// one can follow: a variable, a template with ${...}, a concatenation.
	reJSComputed = regexp.MustCompile(`\b(?:require|import|importActual|importMock|requireActual)\s*\(\s*(?:[^'"` + "`" + `\s)]|` + "`" + `[^` + "`" + `]*\$\{|['"][^'"\n]*['"]\s*\+)`)
)

// jsImports lists a file's imports; computed is true when the file loads a
// path rw cannot read.
func jsImports(data []byte) (out []jsImport, computed bool) {
	for _, m := range reJSImport.FindAllSubmatch(data, -1) {
		call := string(m[1])
		out = append(out, jsImport{spec: string(m[2]), blind: call == "require" || call == "importActual" || call == "importMock"})
	}
	return out, reJSComputed.Match(data)
}

// jsPkgName is the package a bare import names ("@a/b/c" -> "@a/b",
// "lodash/fp" -> "lodash"), or "" for a relative or absolute path.
func jsPkgName(spec string) string {
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
		return ""
	}
	parts := strings.SplitN(spec, "/", 3)
	if strings.HasPrefix(spec, "@") {
		if len(parts) < 2 {
			return spec
		}
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// isNodeBuiltin reports Node's own modules ("fs", "node:test").
func isNodeBuiltin(name string) bool {
	if strings.HasPrefix(name, "node:") {
		return true
	}
	return slices.Contains(nodeBuiltins, name)
}

var nodeBuiltins = []string{"assert", "async_hooks", "buffer", "child_process", "cluster", "console", "constants", "crypto", "dgram",
	"diagnostics_channel", "dns", "domain", "events", "fs", "http", "http2", "https", "inspector", "module", "net", "os", "path",
	"perf_hooks", "process", "punycode", "querystring", "readline", "repl", "stream", "string_decoder", "sys", "timers", "tls",
	"trace_events", "tty", "url", "util", "v8", "vm", "wasi", "worker_threads", "zlib"}

// jsPackage is a package.json below the check folder.
type jsPackage struct {
	Name    string
	Dir     string // slash path relative to the check folder ("." for it)
	Scripts map[string]string
	Deps    map[string]bool // what it declares: dependencies, dev, peer and optional
	raw     map[string]json.RawMessage
}

func readJSPackage(file string) (jsPackage, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return jsPackage{}, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return jsPackage{}, false
	}
	p := jsPackage{Deps: map[string]bool{}, raw: raw}
	if raw["name"] != nil && json.Unmarshal(raw["name"], &p.Name) != nil {
		return jsPackage{}, false
	}
	if raw["scripts"] != nil && json.Unmarshal(raw["scripts"], &p.Scripts) != nil {
		return jsPackage{}, false
	}
	for _, k := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		var m map[string]json.RawMessage
		if raw[k] != nil && json.Unmarshal(raw[k], &m) != nil {
			return jsPackage{}, false
		}
		for n := range m {
			p.Deps[n] = true
		}
	}
	return p, true
}

// jsTree is the JavaScript side of the check folder: its code files with
// their imports and its packages.
type jsTree struct {
	c        *change
	files    []string // code files, dir-relative
	imports  map[string][]jsImport
	computed []string // files that load a computed path
	packages []jsPackage
	byName   map[string]jsPackage
}

// readJSTree reads the check folder, or says why it cannot.
func readJSTree(c *change) (*jsTree, string) {
	t := &jsTree{c: c, imports: map[string][]jsImport{}, byName: map[string]jsPackage{}}
	var pkgs []string
	all, ok := walk(c.dir, maxJSFiles, func(rel string, d os.DirEntry) bool {
		// Yarn's generated loaders implement dependency resolution, not
		// application imports. Changes to them require a full run.
		if yarnRuntimeFile(rel) {
			return false
		}
		if d.Name() == "package.json" {
			pkgs = append(pkgs, rel)
		}
		return jsCode[strings.ToLower(path.Ext(rel))]
	})
	if !ok {
		return nil, "too many files to read what the tests import"
	}
	t.files = all
	for _, f := range all {
		data, err := os.ReadFile(c.abs(f))
		if err != nil {
			return nil, "could not read " + f
		}
		var computed bool
		t.imports[f], computed = jsImports(data)
		if computed {
			t.computed = append(t.computed, f)
		}
	}
	for _, f := range pkgs {
		p, ok := readJSPackage(c.abs(f))
		if !ok {
			return nil, "cannot read " + f
		}
		p.Dir = path.Dir(f)
		t.packages = append(t.packages, p)
		if p.Name != "" {
			if old, dup := t.byName[p.Name]; dup && old.Dir != p.Dir {
				return nil, "two packages are named " + p.Name
			}
			t.byName[p.Name] = p
		}
	}
	return t, ""
}

func yarnRuntimeFile(file string) bool {
	for _, part := range strings.Split(file, "/") {
		if part == ".yarn" {
			return true
		}
	}
	switch path.Base(file) {
	case ".pnp.cjs", ".pnp.js", ".pnp.loader.mjs", ".pnp.data.json", ".yarnrc.yml", ".yarnrc":
		return true
	}
	return false
}

// Yarn resolves a workspace with peers through a virtual path. Jest 29
// and Vitest 3 then find no related tests for its physical path, even
// though a full run fails. The check folder can itself be a workspace.
// Do not infer path equivalence without the runner's resolved graph.
func pnpVirtualWorkspace(c *change, t *jsTree) string {
	pnp := false
	for dir := c.dir; ; dir = filepath.Dir(dir) {
		for _, name := range []string{".pnp.cjs", ".pnp.js"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				pnp = true
			}
		}
		up := filepath.Dir(dir)
		if pnp || up == dir {
			break
		}
		if _, ok := relDir(c.root, up); !ok {
			break
		}
	}
	if pnp {
		for _, p := range t.packages {
			var peers map[string]json.RawMessage
			if raw := p.raw["peerDependencies"]; raw != nil && (json.Unmarshal(raw, &peers) != nil || len(peers) != 0) {
				return "Yarn Plug'n'Play may give " + p.Dir + " a virtual path for its peer dependencies, which related tests cannot reliably match"
			}
		}
	}
	return ""
}

// resolveRel lists the files a relative import from f can load (none when
// none is there), or ok false when it leaves the check folder.
func (t *jsTree) resolveRel(f, spec string) ([]string, bool) {
	target := path.Join(path.Dir(f), spec)
	if target == ".." || strings.HasPrefix(target, "../") {
		return nil, false
	}
	return t.existing(target), true
}

// existing lists the files an import of target can load: every candidate
// that is there, as rw does not rank them the way each resolver does.
func (t *jsTree) existing(target string) []string {
	var out []string
	for _, cand := range jsImportCandidates(target) {
		if st, err := os.Stat(t.c.abs(cand)); err == nil && !st.IsDir() {
			out = append(out, cand)
		}
	}
	return out
}

// under lists the code files in the folder d.
func (t *jsTree) under(d string) []string {
	var out []string
	for _, f := range t.files {
		if d == "." || strings.HasPrefix(f, d+"/") {
			out = append(out, f)
		}
	}
	return out
}
