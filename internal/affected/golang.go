package affected

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/sparkz400/switchyard/internal/proc"
)

// goShape returns the index of the single "./..." in `go test|vet ...
// ./...`, or -1. go build is not narrowed: it is quick with the build
// cache, and "go build <pattern>" would let agents pass -toolexec or -o.
// -C, -modfile and -mod change which module the packages come from, so
// those run in full.
func goShape(f []string) int {
	if len(f) < 3 || f[0] != "go" || (f[1] != "test" && f[1] != "vet") {
		return -1
	}
	at := -1
	for i, a := range f[2:] {
		for _, flag := range []string{"-C", "-modfile", "-mod"} {
			if a == flag || strings.HasPrefix(a, flag+"=") {
				return -1
			}
		}
		if a == "./..." {
			if at >= 0 {
				return -1
			}
			at = i + 2
		}
		if a == "-args" && at < 0 {
			return -1 // the pattern would be passed to the test binary
		}
	}
	return at
}

// goPkg is one package as `go list` sees it.
type goPkg struct {
	dir     string   // relative to the check folder, slash path
	err     string   // a load error other than "no Go files for this platform"
	imports []string // import paths, test imports included in tests
	tests   []string
	embeds  []string // embedded and other non-Go files of the package, relative to its folder
	goFiles []string // every .go file of the package, relative to it
}

// goList runs `go list` in dir (tests swap it).
var goList = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", append([]string{"list"}, args...)...)
	proc.Prepare(cmd)
	cmd.Env = proc.WithoutSecrets(os.Environ())
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(lastLine(stderr.String())))
	}
	return out, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// goListFormat prints one line per package: import path, folder, load
// error, imports, test imports, non-Go files (embedded ones included) and
// Go files, separated by \x1f; lists are separated by \x1e.
var goListFormat = strings.Join([]string{"{{.ImportPath}}", "{{.Dir}}", "{{with .Error}}{{.Err}}{{end}}",
	joins("Imports"), joins("TestImports", "XTestImports"),
	joins("EmbedFiles", "TestEmbedFiles", "XTestEmbedFiles", "CFiles", "CXXFiles", "HFiles", "SFiles", "MFiles", "FFiles", "SwigFiles", "SysoFiles", "IgnoredOtherFiles"),
	joins("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles", "IgnoredGoFiles"),
}, "\x1f")

func joins(fields ...string) string {
	var parts []string
	for _, f := range fields {
		parts = append(parts, `{{join .`+f+` "\x1e"}}`)
	}
	return strings.Join(parts, "\x1e")
}

// loadGo lists the packages of ./... in dir, keyed by import path. The
// build tags of the verify command are used, so the graph is the one the
// command sees.
func loadGo(ctx context.Context, dir string, f []string) (map[string]*goPkg, error) {
	args := []string{"-e"}
	for i := 2; i < len(f); i++ {
		switch a := f[i]; {
		case strings.HasPrefix(a, "-tags="):
			args = append(args, a)
		case a == "-tags" && i+1 < len(f):
			args = append(args, "-tags="+f[i+1])
		}
	}
	out, err := goList(ctx, dir, append(args, "-f", goListFormat, "./...")...)
	if err != nil {
		return nil, err
	}
	base := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		base = r
	}
	pkgs := map[string]*goPkg{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
		parts := strings.Split(strings.TrimRight(line, "\r"), "\x1f")
		if len(parts) != 7 {
			return nil, fmt.Errorf("unexpected go list line %q", line)
		}
		if parts[1] == "" {
			// Not loadable at all (an import path go rejects).
			pkgs[parts[0]] = &goPkg{err: "cannot load: " + parts[2]}
			continue
		}
		rel, ok := relDir(dir, parts[1])
		if !ok {
			if rel, ok = relDir(base, parts[1]); !ok {
				return nil, fmt.Errorf("package %s is outside %s", parts[0], dir)
			}
		}
		p := &goPkg{dir: rel, imports: split1e(parts[3]), tests: split1e(parts[4]), embeds: split1e(parts[5]), goFiles: split1e(parts[6])}
		if e := parts[2]; e != "" && !strings.Contains(e, "build constraints exclude all Go files") && !strings.Contains(e, "no Go files") {
			p.err = e
		}
		pkgs[parts[0]] = p
	}
	return pkgs, nil
}

func split1e(s string) []string {
	var out []string
	for _, x := range strings.Split(s, "\x1e") {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// goSel is the Go packages a change affects.
type goSel struct {
	pkgs      map[string]*goPkg // every package of ./...
	run       map[string]bool   // the affected ones, by import path
	mentioned []string          // non-Go files placed by a mention in code
}

// goAffected finds the packages of the changed files and every package
// that imports them (tests included), or says why it cannot: go.mod,
// go.sum, test data another package reads and files sy cannot place. f is the verify command,
// for its build tags.
func goAffected(ctx context.Context, f []string, c *change) (*goSel, string) {
	for _, file := range c.files {
		base := path.Base(file)
		switch {
		case base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum":
			return nil, file + " changed"
		case strings.HasPrefix(file, "vendor/"):
			return nil, file + " is vendored code"
		}
	}
	pkgs, err := loadGo(ctx, c.dir, f)
	if err != nil {
		return nil, "go list failed: " + clipStr(err.Error(), 200)
	}
	byDir := map[string]string{}
	for ip, p := range pkgs {
		byDir[foldKey(p.dir)] = ip
	}
	pkgOf := func(dir string) (string, bool) {
		ip, ok := byDir[foldKey(dir)]
		return ip, ok
	}
	direct := map[string]bool{}   // packages whose code changed
	testOnly := map[string]bool{} // packages whose tests changed (no other package can import a test)
	var mentioned []string
	sources := map[string][]byte{}
	read := func(ip, name string) []byte {
		k := ip + "/" + name
		if b, ok := sources[k]; ok {
			return b
		}
		b, _ := os.ReadFile(c.abs(path.Join(pkgs[ip].dir, name)))
		sources[k] = b
		return b
	}
	var dataOwners []string // packages whose test data changed
	for _, file := range c.files {
		dir := path.Dir(file)
		if parts := strings.Split(file, "/"); slices.Contains(parts, "testdata") {
			// Test data belongs to the package around its testdata folder
			// (go ignores the folder itself, .go files included).
			owner := strings.Join(parts[:slices.Index(parts, "testdata")], "/")
			if owner == "" {
				owner = "."
			}
			ip, ok := pkgOf(owner)
			if !ok {
				return nil, file + " is test data outside a package"
			}
			testOnly[ip] = true
			dataOwners = append(dataOwners, ip)
			continue
		}
		if strings.HasSuffix(file, ".go") {
			ip, ok := pkgOf(dir)
			if !ok {
				return nil, fmt.Sprintf("%s is not in a package of ./...", file)
			}
			if strings.HasSuffix(file, "_test.go") {
				testOnly[ip] = true
			} else {
				direct[ip] = true
			}
			continue
		}
		// Embedded by a package?
		placed := false
		for ip, p := range pkgs {
			for _, e := range p.embeds {
				if foldKey(path.Join(p.dir, e)) == foldKey(file) {
					direct[ip], placed = true, true
				}
			}
		}
		if placed {
			continue
		}
		// Named in a package's code (a test reading a file by name)?
		name := path.Base(file)
		for ip, p := range pkgs {
			for _, g := range p.goFiles {
				if bytes.Contains(read(ip, g), []byte(name)) {
					placed = true
					if strings.HasSuffix(g, "_test.go") {
						testOnly[ip] = true
					} else {
						direct[ip] = true
					}
				}
			}
		}
		switch {
		case placed:
			mentioned = append(mentioned, file)
		case isDoc(file):
		default:
			return nil, fmt.Sprintf("%s is neither Go code nor named by any package, so sy cannot tell which tests read it", file)
		}
	}
	// Another package reaching into a testdata folder ("../x/testdata")
	// shares it: sy cannot tell which files it reads.
	if len(dataOwners) > 0 {
		for ip, p := range pkgs {
			if slices.Contains(dataOwners, ip) {
				continue
			}
			for _, g := range p.goFiles {
				for _, line := range bytes.Split(read(ip, g), []byte("\n")) {
					if bytes.Contains(line, []byte("testdata")) && bytes.Contains(line, []byte("..")) {
						return nil, fmt.Sprintf("test data changed and %s reads test data of another package (%s)", ip, g)
					}
				}
			}
		}
	}
	var start []string
	for ip := range direct {
		start = append(start, ip)
	}
	deps := map[string][]string{}
	for ip, p := range pkgs {
		deps[ip] = p.imports
	}
	code := reverseClosure(start, deps) // packages whose code may behave differently
	run := map[string]bool{}
	for ip := range testOnly {
		run[ip] = true
	}
	for ip := range code {
		if _, ok := pkgs[ip]; ok {
			run[ip] = true
		}
	}
	for ip, p := range pkgs {
		for _, t := range p.tests {
			if code[t] {
				run[ip] = true
			}
		}
	}
	for ip := range pkgs {
		if p := pkgs[ip]; p.err != "" && !run[ip] {
			// Its imports may be incomplete, so it might need a run.
			return nil, fmt.Sprintf("go list reports an error in %s: %s", ip, clipStr(p.err, 160))
		}
	}
	return &goSel{pkgs: pkgs, run: run, mentioned: mentioned}, ""
}

// selectGo narrows `go test ./...` (or vet, build) to the affected packages.
func selectGo(ctx context.Context, cmd string, f []string, c *change) Plan {
	sel, why := goAffected(ctx, f, c)
	if why != "" {
		return full(cmd, why)
	}
	pkgs, run, mentioned := sel.pkgs, sel.run, sel.mentioned
	if len(run) == 0 {
		return Plan{Command: cmd, Why: "no Go package is affected by the " + c.changedWhy()}
	}
	if len(run) == len(pkgs) {
		return full(cmd, "every package is affected by the "+c.changedWhy())
	}
	var dirs []string
	for _, ip := range sortedKeys(run) {
		dirs = append(dirs, dotSlash(pkgs[ip].dir))
	}
	slices.Sort(dirs)
	q, bad := quoteAll(dirs)
	if bad != "" {
		return full(cmd, unsafeWhy(bad))
	}
	i := goShape(f)
	out := strings.Join(append(append(append([]string(nil), f[:i]...), q), f[i+1:]...), " ")
	why = fmt.Sprintf("%d of %d packages, affected by the %s", len(run), len(pkgs), c.changedWhy())
	if len(mentioned) > 0 {
		why += " (" + list(mentioned, 3) + " named in code)"
	}
	return Plan{Command: cmd, Run: []string{out}, Why: why}
}

// foldKey compares paths the way the file system does: without case on
// Windows and macOS.
func foldKey(p string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
