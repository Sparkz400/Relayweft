// Package affected picks the tests that a task's changed files can affect,
// so a fix round reruns those instead of the whole suite. It knows Go,
// jest/vitest (npm, pnpm, yarn), pytest, cargo, dotnet, Maven and Gradle,
// plus templates from verify.affected_commands. When it cannot tell
// reliably, it says so and the caller runs the full command: a narrowed
// run never replaces the full one.
package affected

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
)

// Off, as verify.affected or as a command's verify.affected_commands
// entry, turns narrowing off.
const Off = "off"

// Input is what changed and where the checks run.
type Input struct {
	Root  string   // the git repo's top folder
	Dir   string   // the folder the checks run in (Root or below)
	Files []string // changed files, slash paths relative to Root (deleted ones too)
	// Exec runs a tool rw uses to select tests (go list, cargo metadata)
	// in dir and returns its standard output. nil runs it here. With the
	// sandbox on it runs in the container: the folder holds the agents'
	// code and configs.
	Exec func(ctx context.Context, dir string, argv []string) ([]byte, error)
}

// Plan says how to run one verify command.
type Plan struct {
	Command string   // the verify command as configured
	Run     []string // the narrowed commands; none (with Full false) when nothing is affected
	Full    bool     // run Command as it is
	Why     string   // why it is full, or what the narrowed run covers
}

func full(cmd, why string) Plan { return Plan{Command: cmd, Full: true, Why: why} }

// Select plans one verify command. template is the command's entry in
// verify.affected_commands: "" detects the narrowed form, Off never
// narrows, anything else is a template with {files}, {packages} and
// {test_files}.
func Select(ctx context.Context, cmd, template string, in Input) Plan {
	if template == Off {
		return full(cmd, "affected_commands turns it off")
	}
	if len(in.Files) == 0 {
		return Plan{Command: cmd, Why: "no file changed"}
	}
	c, why := prepare(in)
	if why != "" {
		return full(cmd, why)
	}
	if template != "" {
		return selectTemplate(ctx, cmd, template, c)
	}
	if hasShellSyntax(cmd) {
		return full(cmd, "rw narrows only a single test command (set verify.affected_commands for this one)")
	}
	f := strings.Fields(cmd)
	switch {
	case goShape(f) >= 0:
		return selectGo(ctx, cmd, f, c)
	case pytestShape(f) > 0:
		return selectPytest(cmd, f, c)
	case jsShape(f, c.dir).kind != "":
		return selectJS(cmd, f, c)
	case cargoShape(f):
		return selectCargo(ctx, cmd, f, c)
	case dotnetShape(f):
		return selectDotnet(cmd, f, c)
	case mavenShape(f):
		return selectMaven(cmd, f, c)
	case gradleShape(f) >= 0:
		return selectGradle(cmd, f, c)
	}
	return full(cmd, "rw does not know how to narrow it (set verify.affected_commands for this one)")
}

// Allowed lists the command prefixes an agent needs, besides cmd itself,
// to run the narrowed form of cmd in dir: every command Select can return
// for it is one of them or starts with one plus a space. hint is the
// narrowed form for the agent's prompt ("go test <packages>"), "" when
// there is none.
func Allowed(dir, cmd, template string) (prefixes []string, hint string) {
	if template == Off {
		return nil, ""
	}
	if template != "" {
		i := firstPlaceholder(template)
		if i < 0 {
			return nil, ""
		}
		// Whole words only: "make test PKGS={packages}" allows "make test".
		pre := template[:i]
		if j := strings.LastIndexAny(pre, " \t"); j >= 0 {
			pre = pre[:j]
		} else {
			pre = ""
		}
		pre = strings.TrimSpace(pre)
		if hasShellSyntax(pre) {
			// An open quote (sh -c "pytest {files}") would allow
			// anything after it.
			return nil, ""
		}
		if pre == "" || pre == cmd || strings.HasPrefix(cmd, pre+" ") {
			// "" would allow any command; a prefix of cmd is allowed already.
			if pre == "" {
				return nil, ""
			}
			return nil, template
		}
		return []string{pre}, template
	}
	if hasShellSyntax(cmd) {
		return nil, ""
	}
	f := strings.Fields(cmd)
	switch {
	case goShape(f) >= 0:
		i := goShape(f)
		pre := strings.Join(f[:i], " ")
		rest := strings.Join(f[i+1:], " ")
		return []string{pre}, strings.TrimSpace(pre + " <packages> " + rest)
	case pytestShape(f) > 0:
		return nil, cmd + " <test files>"
	case jsShape(f, dir).kind != "":
		return jsAllowed(f, dir)
	case cargoShape(f):
		if i := cargoArgsEnd(f); i < len(f) {
			pre := strings.Join(f[:i], " ")
			return []string{pre}, pre + " -p <crate> " + strings.Join(f[i:], " ")
		}
		return nil, cmd + " -p <crate>"
	case dotnetShape(f):
		return nil, cmd + " <test project>"
	case mavenShape(f):
		return nil, cmd + " -pl <modules> -am -amd"
	case gradleShape(f) >= 0:
		return gradleAllowed(f, dir)
	}
	return nil, ""
}

// change is Input made relative to the folder the checks run in.
type change struct {
	root, dir string
	exec      func(ctx context.Context, dir string, argv []string) ([]byte, error) // Input.Exec
	files     []string                                                             // changed files under dir, slash paths relative to dir
	outside   []string                                                             // changed files outside dir, relative to root
}

func prepare(in Input) (*change, string) {
	rel, ok := relDir(in.Root, in.Dir)
	if !ok {
		return nil, "the check folder is not inside the repo"
	}
	c := &change{root: in.Root, dir: in.Dir, exec: in.Exec}
	for _, f := range in.Files {
		if strings.Contains(f, `\`) {
			// git writes slashes; a backslash is part of a file name.
			return nil, unsafeWhy(f)
		}
		f = path.Clean(f)
		if f == "." || strings.HasPrefix(f, "../") || path.IsAbs(f) {
			return nil, fmt.Sprintf("changed file %q is not inside the repo", f)
		}
		switch {
		case rel == ".":
			c.files = append(c.files, f)
		case hasPrefixFold(f, rel+"/"):
			c.files = append(c.files, f[len(rel)+1:])
		default:
			c.outside = append(c.outside, f)
		}
	}
	for _, f := range c.outside {
		if !isDoc(f) {
			return nil, fmt.Sprintf("%s changed outside the check folder", f)
		}
	}
	return c, ""
}

// relDir is dir relative to root as a slash path ("." for the root).
func relDir(root, dir string) (string, bool) {
	try := func(r, d string) (string, bool) {
		rel, err := filepath.Rel(r, d)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", false
		}
		return filepath.ToSlash(rel), true
	}
	if root == "" || dir == "" {
		return "", false
	}
	if rel, ok := try(root, dir); ok {
		return rel, true
	}
	// Symlinks, junctions or another spelling of the same folder.
	r, err1 := filepath.EvalSymlinks(root)
	d, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return "", false
	}
	return try(r, d)
}

func hasPrefixFold(s, prefix string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
	}
	return strings.HasPrefix(s, prefix)
}

// abs is a changed file's path on disk.
func (c *change) abs(f string) string { return filepath.Join(c.dir, filepath.FromSlash(f)) }

func (c *change) exists(f string) bool {
	_, err := os.Stat(c.abs(f))
	return err == nil
}

// isDoc reports files no test runner reads: docs, images, licences.
func isDoc(p string) bool {
	base := strings.ToLower(path.Base(p))
	switch path.Ext(base) {
	case ".md", ".markdown", ".rst", ".adoc", ".txt", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp":
		return !strings.HasPrefix(base, "requirements") // requirements.txt is a dependency list
	}
	for _, n := range []string{"license", "licence", "notice", "authors", "codeowners", "changelog"} {
		if strings.HasPrefix(base, n) {
			return true
		}
	}
	return false
}

// isTestFile reports test files by the usual naming of each ecosystem.
func isTestFile(p string) bool {
	l := strings.ToLower(p)
	base := path.Base(l)
	ext := path.Ext(base)
	name := strings.TrimSuffix(base, ext)
	switch {
	case strings.Contains(base, ".test.") || strings.Contains(base, ".spec."):
		return true
	case ext == ".py" && (strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test")):
		return true
	case (ext == ".go" || ext == ".rb" || ext == ".exs" || ext == ".dart") && (strings.HasSuffix(name, "_test") || strings.HasSuffix(name, "_spec")):
		return true
	case (ext == ".java" || ext == ".kt" || ext == ".cs" || ext == ".fs" || ext == ".scala") && (strings.HasSuffix(name, "test") || strings.HasSuffix(name, "tests")):
		return true
	case strings.Contains("/"+l, "/__tests__/"):
		return true
	}
	return false
}

// hasShellSyntax reports a command line that is more than one plain
// command with arguments: rw then cannot tell where to add the tests.
func hasShellSyntax(cmd string) bool {
	return strings.ContainsAny(cmd, "&|;<>()$`\"'%!^*?[]{}~\n\r")
}

// Quoting. Changed file names come from agents, so a name is passed to the
// shell (sh -c, or cmd.exe /c on Windows) only when it cannot break out of
// its quotes there: letters, digits, spaces and . _ - / + @ = , : only
// (ASCII only on Windows), and
// never a leading "-" that a test runner would read as a flag. Anything
// else makes the selection unsure, so the full command runs.

// safeArg reports whether s can be passed to a shell (see quote).
func safeArg(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'):
		case strings.ContainsRune(" ._-/+@=,:", r):
		case r >= 0x80 && runtime.GOOS != "windows" && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			// Not on Windows: a child that parses its command line in the
			// ANSI code page (the Java launcher behind mvn.cmd, gradlew.bat)
			// maps some letters to ASCII, U+02BA to a double quote.
		default:
			return false
		}
	}
	return true
}

// quote quotes a safe argument for the platform's shell: bare when it has
// only plain characters, else in double quotes on Windows (cmd.exe) and
// single quotes elsewhere (sh).
func quote(s string) string {
	plain := true
	for _, r := range s {
		if !(r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/+:", r))) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	if runtime.GOOS == "windows" {
		return `"` + s + `"`
	}
	return "'" + s + "'"
}

// quoteAll quotes args, or returns the first one that is not safe.
func quoteAll(args []string) (string, string) {
	q := make([]string, len(args))
	for i, a := range args {
		if !safeArg(a) {
			return "", a
		}
		q[i] = quote(a)
	}
	return strings.Join(q, " "), ""
}

// unsafeWhy is the reason for a full run when a name cannot be passed on.
func unsafeWhy(name string) string {
	return fmt.Sprintf("%q has characters rw does not pass to a shell", name)
}

// dotSlash makes a dir-relative path start with "./" (or be "."), so a
// test runner never reads it as a flag and Go reads it as a folder.
func dotSlash(p string) string {
	if p == "." || p == "" {
		return "."
	}
	return "./" + p
}

// list shortens a list of names for a reason.
func list(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + fmt.Sprintf(" and %d more", len(names)-n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// changedWhy names what the narrowed run is based on.
func (c *change) changedWhy() string {
	names := make([]string, len(c.files))
	for i, f := range c.files {
		names[i] = f
		if strings.ContainsFunc(f, unicode.IsControl) {
			names[i] = fmt.Sprintf("%q", f) // keep log lines whole
		}
	}
	return plural(len(c.files), "changed file", "changed files") + ": " + list(names, 4)
}

// onlyFlags reports whether args has only flags, letting a value follow
// the flags in takesValue ("-k expr").
func onlyFlags(args []string, takesValue map[string]bool) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return false
		}
		if !strings.HasPrefix(a, "-") {
			return false
		}
		if !strings.Contains(a, "=") && takesValue[a] {
			i++
		}
	}
	return true
}

// sortedKeys returns m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// walk lists files under dir (slash paths relative to it) whose name
// keep accepts, skipping VCS, dependency and build folders. It stops with
// ok false past max files.
func walk(dir string, max int, keep func(rel string, d os.DirEntry) bool) (files []string, ok bool) {
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir {
				switch name := d.Name(); name {
				case ".git", "node_modules", "vendor", "target", "bin", "obj", "build", "dist", "out", "__pycache__", "venv", ".venv", "site-packages", ".tox", ".gradle":
					return filepath.SkipDir
				default:
					if strings.HasPrefix(name, ".") {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if n++; n > max {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if keep(rel, d) {
			files = append(files, rel)
		}
		return nil
	})
	return files, err == nil && n <= max
}

// owner returns the longest folder in dirs that holds f ("" = none);
// dirs and f are slash paths relative to the same folder ("." is the top).
func owner(f string, dirs []string) string {
	best, bestLen := "", -1
	for _, d := range dirs {
		var in bool
		if d == "." {
			in = true
		} else {
			in = f == d || strings.HasPrefix(f, d+"/")
		}
		if in && len(d) > bestLen {
			best, bestLen = d, len(d)
		}
	}
	return best
}

// reverseClosure returns start plus everything that reaches it through
// deps (deps[a] lists what a depends on).
func reverseClosure(start []string, deps map[string][]string) map[string]bool {
	users := map[string][]string{}
	for a, ds := range deps {
		for _, d := range ds {
			users[d] = append(users[d], a)
		}
	}
	seen := map[string]bool{}
	queue := append([]string(nil), start...)
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		if seen[x] {
			continue
		}
		seen[x] = true
		queue = append(queue, users[x]...)
	}
	return seen
}
