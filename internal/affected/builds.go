package affected

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// dotnet, Maven and Gradle: the project or module of each changed file and
// the ones that depend on it (by project references), else everything.

const maxBuildFiles = 200000

var dotnetValues = map[string]bool{"-c": true, "--configuration": true, "-f": true, "--framework": true, "--filter": true, "-l": true, "--logger": true,
	"-v": true, "--verbosity": true, "-r": true, "--runtime": true, "-s": true, "--settings": true, "--results-directory": true, "-a": true,
	"--arch": true, "--os": true, "--collect": true, "--blame-hang-timeout": true}

// dotnetShape matches `dotnet test` with flags only (no project of its own).
func dotnetShape(f []string) bool {
	return len(f) >= 2 && f[0] == "dotnet" && f[1] == "test" && onlyFlags(f[2:], dotnetValues)
}

var (
	reProjectRef = regexp.MustCompile(`(?i)<ProjectReference\s+Include\s*=\s*"([^"]+)"`)
	reTestSdk    = regexp.MustCompile(`(?i)Microsoft\.NET\.Test\.Sdk|<IsTestProject>\s*true|Sdk\s*=\s*"MSTest\.Sdk`)
)

// selectDotnet narrows `dotnet test` to the test projects that reference a
// changed project, directly or not (one run per test project). Solution
// files, shared build settings and files outside every project make it full.
func selectDotnet(cmd string, f []string, c *change) Plan {
	for _, file := range c.files {
		base := strings.ToLower(path.Base(file))
		ext := path.Ext(base)
		if ext == ".sln" || ext == ".slnx" || ext == ".props" || ext == ".targets" || base == "global.json" || base == "nuget.config" {
			return full(cmd, file+" changed (shared build settings)")
		}
	}
	projs, ok := walk(c.dir, maxBuildFiles, func(rel string, d os.DirEntry) bool {
		switch strings.ToLower(path.Ext(rel)) {
		case ".csproj", ".fsproj", ".vbproj":
			return true
		}
		return false
	})
	if !ok || len(projs) == 0 {
		return full(cmd, "could not find the projects")
	}
	// Plain `dotnet test` runs the solution in the folder: only its test
	// projects count, never one it leaves out.
	inSln, why := solutionProjects(c.dir)
	if why != "" {
		return full(cmd, why)
	}
	deps := map[string][]string{}
	isTest := map[string]bool{}
	byDir := map[string][]string{}
	var dirs []string
	known := map[string]bool{}
	for _, p := range projs {
		known[foldKey(p)] = true
	}
	for _, p := range projs {
		data, err := os.ReadFile(c.abs(p))
		if err != nil {
			return full(cmd, "could not read "+p)
		}
		name := strings.ToLower(strings.TrimSuffix(path.Base(p), path.Ext(p)))
		isTest[p] = reTestSdk.Match(data) || strings.HasSuffix(name, "test") || strings.HasSuffix(name, "tests")
		for _, m := range reProjectRef.FindAllStringSubmatch(string(data), -1) {
			ref := path.Clean(path.Join(path.Dir(p), strings.ReplaceAll(m[1], `\`, "/")))
			if !known[foldKey(ref)] {
				return full(cmd, fmt.Sprintf("%s references %s, which rw cannot find", p, m[1]))
			}
			deps[p] = append(deps[p], findFold(projs, ref))
		}
		d := path.Dir(p)
		if byDir[d] == nil {
			dirs = append(dirs, d)
		}
		byDir[d] = append(byDir[d], p)
	}
	var start []string
	for _, file := range c.files {
		d := owner(file, dirs)
		switch {
		case d != "":
			start = append(start, byDir[d]...)
		case isDoc(file):
		default:
			return full(cmd, file+" is outside every project")
		}
	}
	set := reverseClosure(start, deps)
	var tests, all []string
	for _, p := range projs {
		if isTest[p] && inSln[foldKey(p)] {
			all = append(all, p)
			if set[p] {
				tests = append(tests, p)
			}
		}
	}
	if len(tests) == 0 {
		return Plan{Command: cmd, Why: "no test project is affected by the " + c.changedWhy()}
	}
	if len(tests) == len(all) {
		return full(cmd, "every test project is affected by the "+c.changedWhy())
	}
	var run []string
	for _, p := range sortedSet(tests) {
		a := dotSlash(p)
		if !safeArg(a) {
			return full(cmd, unsafeWhy(a))
		}
		run = append(run, cmd+" "+quote(a))
	}
	return Plan{Command: cmd, Run: run, Why: fmt.Sprintf("%d of %d test projects, affected by the %s", len(tests), len(all), c.changedWhy())}
}

var (
	reSlnProject  = regexp.MustCompile(`(?m)^Project\("[^"]*"\)\s*=\s*"[^"]*"\s*,\s*"([^"]+\.(?:cs|fs|vb)proj)"`)
	reSlnxProject = regexp.MustCompile(`<Project\s+Path\s*=\s*"([^"]+\.(?:cs|fs|vb)proj)"`)
)

// solutionProjects reads the projects (folded slash paths relative to
// dir) of the one solution file in dir, or says why it cannot.
func solutionProjects(dir string) (map[string]bool, string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "could not read the check folder"
	}
	var slns []string
	for _, e := range entries {
		switch strings.ToLower(path.Ext(e.Name())) {
		case ".sln", ".slnx":
			slns = append(slns, e.Name())
		}
	}
	if len(slns) != 1 {
		return nil, fmt.Sprintf("plain dotnet test runs what is in the folder, and rw narrows only one solution (found %d)", len(slns))
	}
	data, err := os.ReadFile(dir + "/" + slns[0])
	if err != nil {
		return nil, "could not read " + slns[0]
	}
	re := reSlnProject
	if strings.EqualFold(path.Ext(slns[0]), ".slnx") {
		re = reSlnxProject
	}
	out := map[string]bool{}
	for _, m := range re.FindAllSubmatch(data, -1) {
		out[foldKey(path.Clean(strings.ReplaceAll(string(m[1]), `\`, "/")))] = true
	}
	if len(out) == 0 {
		return nil, slns[0] + " lists no projects rw can read"
	}
	return out, ""
}

func findFold(list []string, p string) string {
	for _, x := range list {
		if foldKey(x) == foldKey(p) {
			return x
		}
	}
	return p
}

// Maven.

var mavenValues = map[string]bool{"-P": true, "--activate-profiles": true, "-T": true, "--threads": true, "-s": true, "--settings": true,
	"-gs": true, "--global-settings": true, "-t": true, "--toolchains": true, "-l": true, "--log-file": true}

func isMaven(s string) bool {
	switch strings.ToLower(s) {
	case "mvn", "mvn.cmd", "mvnw", "mvnw.cmd", "./mvnw", `.\mvnw`, `.\mvnw.cmd`:
		return true
	}
	return false
}

// mavenShape matches `mvn [flags] test` (any goals that run the test
// phase) without a module selection of its own.
func mavenShape(f []string) bool {
	if len(f) < 2 || !isMaven(f[0]) {
		return false
	}
	tests := false
	for i := 1; i < len(f); i++ {
		a := f[i]
		switch {
		case a == "-pl" || a == "--projects" || a == "-f" || a == "--file" || a == "-rf" || a == "--resume-from" ||
			a == "-am" || a == "-amd" || a == "--also-make" || a == "--also-make-dependents":
			return false
		case strings.HasPrefix(a, "-"):
			if !strings.Contains(a, "=") && mavenValues[a] {
				i++
			}
		case a == "test" || a == "verify" || a == "package" || a == "install":
			tests = true
		case a == "clean" || a == "compile" || a == "test-compile" || a == "validate":
		default:
			return false
		}
	}
	return tests
}

var (
	reMvnModule  = regexp.MustCompile(`<module>\s*([^<\s]+)\s*</module>`)
	reMvnModules = regexp.MustCompile(`<modules>`)
)

// selectMaven narrows to the reactor modules of the changed files (-pl)
// plus what they need and what depends on them (-am -amd). Any pom.xml,
// .mvn and files of the root module make it full.
func selectMaven(cmd string, f []string, c *change) Plan {
	for _, file := range c.files {
		if path.Base(file) == "pom.xml" || strings.HasPrefix(file, ".mvn/") {
			return full(cmd, file+" changed (build settings)")
		}
	}
	var modules []string
	var visit func(dir string, depth int) string
	visit = func(dir string, depth int) string {
		data, err := os.ReadFile(c.abs(path.Join(dir, "pom.xml")))
		if err != nil {
			return "could not read " + path.Join(dir, "pom.xml")
		}
		if depth > 20 {
			return "the modules nest too deep"
		}
		if len(reMvnModules.FindAllIndex(data, -1)) > 1 {
			return path.Join(dir, "pom.xml") + " sets modules in profiles"
		}
		for _, m := range reMvnModule.FindAllStringSubmatch(string(data), -1) {
			sub := path.Clean(path.Join(dir, m[1]))
			if strings.HasSuffix(sub, ".xml") {
				return path.Join(dir, "pom.xml") + " names a module by its pom file"
			}
			if sub == "." || strings.HasPrefix(sub, "../") {
				return path.Join(dir, "pom.xml") + " has a module outside its folder"
			}
			modules = append(modules, sub)
			if why := visit(sub, depth+1); why != "" {
				return why
			}
		}
		return ""
	}
	if why := visit(".", 0); why != "" {
		return full(cmd, why)
	}
	if len(modules) == 0 {
		return full(cmd, "the project has one module")
	}
	set := map[string]bool{}
	for _, file := range c.files {
		d := owner(file, modules)
		switch {
		case d != "":
			set[d] = true
		case isDoc(file):
		default:
			return full(cmd, file+" belongs to the root module")
		}
	}
	if len(set) == 0 {
		return Plan{Command: cmd, Why: "no module is affected by the " + c.changedWhy()}
	}
	names := sortedKeys(set)
	for _, n := range names {
		if !safeArg(n) || strings.Contains(n, ",") {
			return full(cmd, unsafeWhy(n))
		}
	}
	return Plan{Command: cmd, Run: []string{cmd + " -pl " + quote(strings.Join(names, ",")) + " -am -amd"},
		Why: fmt.Sprintf("modules %s with what they need and what depends on them, for the %s", list(names, 4), c.changedWhy())}
}

// Gradle.

var gradleValues = map[string]bool{"-x": true, "--exclude-task": true, "-I": true, "--init-script": true, "--max-workers": true,
	"--console": true, "--warning-mode": true}

func isGradle(s string) bool {
	switch strings.ToLower(s) {
	case "gradle", "gradle.bat", "gradlew", "gradlew.bat", "./gradlew", `.\gradlew`, `.\gradlew.bat`:
		return true
	}
	return false
}

// gradleShape returns the index of the single "test" task in `gradle
// [flags] test`, or -1.
func gradleShape(f []string) int {
	if len(f) < 2 || !isGradle(f[0]) {
		return -1
	}
	at := -1
	for i := 1; i < len(f); i++ {
		a := f[i]
		switch {
		case strings.HasPrefix(a, "-I") || a == "--init-script" || strings.HasPrefix(a, "--init-script="):
			return -1 // init scripts can add dependencies outside this build
		case strings.HasPrefix(a, "-p") || strings.HasPrefix(a, "-b") || strings.HasPrefix(a, "-c"):
			return -1 // short options also accept an attached path
		case a == "-p" || a == "--project-dir" || a == "-b" || a == "--build-file" || a == "-c" || a == "--settings-file" ||
			strings.HasPrefix(a, "--project-dir=") || strings.HasPrefix(a, "--build-file=") || strings.HasPrefix(a, "--settings-file="):
			return -1
		case strings.HasPrefix(a, "-"):
			if !strings.Contains(a, "=") && gradleValues[a] {
				i++
			}
		case a == "test" && at < 0:
			at = i
		default:
			return -1
		}
	}
	return at
}

var (
	// Every place that looks like an include statement (not includeBuild
	// or includeFlat), wherever it is.
	reGradleIncludeWord = regexp.MustCompile(`\binclude\s*[('"]`)
	reGradleProject     = regexp.MustCompile(`\bproject\s*\(\s*(?:path\s*[:=]\s*)?["'](:[A-Za-z0-9_.:-]+)["']\s*\)`)
	reGradleProjectCall = regexp.MustCompile(`\b(?:project|findProject)\b`)
	reGradleUnsure      = regexp.MustCompile(`projectDir|includeBuild|includeFlat|\.each\b|forEach|for\s*\(|\bfile\s*\(|rootProject\.children|projects\.\w|\bapply\s*(?:\(|from\b)`)
)

// gradleProjects reads the subprojects (":a:b" -> "a/b") from the settings
// file, or says why it cannot.
func gradleProjects(dir string) (map[string]string, string) {
	var data []byte
	var name string
	for _, n := range []string{"settings.gradle.kts", "settings.gradle"} {
		if b, err := os.ReadFile(dir + "/" + n); err == nil {
			data, name = b, n
			break
		}
	}
	if name == "" {
		return nil, "the project has no settings.gradle"
	}
	projects := map[string]string{}
	src := strings.TrimPrefix(string(data), bom)
	st, why := gradleIncludes(src)
	if why != "" {
		return nil, name + " " + why
	}
	// What is left besides the projectDir lines rw read must not place
	// projects in any other way.
	rest := []byte(src)
	for _, sp := range st.spans {
		for i := sp[0]; i < sp[1]; i++ {
			if rest[i] != '\n' {
				rest[i] = ' '
			}
		}
	}
	if reGradleUnsure.Match(rest) || reGradleSettingsUnsure.Match(rest) {
		return nil, name + " places projects in a way rw does not follow"
	}
	// Every include rw did not read (in a comment or a string, after a
	// dot) makes it unsure.
	if len(reGradleIncludeWord.FindAllStringIndex(src, -1)) != len(st.includes) {
		return nil, name + " includes projects in a way rw does not follow"
	}
	for _, names := range st.includes {
		for _, n := range names {
			p := ":" + strings.TrimPrefix(n, ":")
			if !plainGradleName(p) {
				// Task names become command arguments and allow rules
				// (comma-joined for Claude): letters, digits, . _ - : only.
				return nil, fmt.Sprintf("%s names project %q, which rw does not pass on", name, n)
			}
			projects[p] = strings.ReplaceAll(strings.TrimPrefix(p, ":"), ":", "/")
		}
	}
	for p, d := range st.dirs {
		if _, ok := projects[p]; !ok {
			return nil, fmt.Sprintf("%s sets the folder of %s, which it does not include", name, p)
		}
		projects[p] = d
	}
	if len(projects) == 0 {
		return nil, "the project has no subprojects"
	}
	byDir := map[string]string{}
	for _, p := range sortedKeys(projects) {
		d := projects[p]
		// Do not pick an arbitrary owner when two projects share a folder.
		for oldDir, old := range byDir {
			if hasPrefixFold(d+"/", oldDir+"/") && len(d) == len(oldDir) {
				return nil, fmt.Sprintf("%s places %s and %s in the same folder", name, old, p)
			}
		}
		byDir[d] = p
	}
	return projects, ""
}

// reGradleProjectDir is a settings line that moves one project:
// project(':a').projectDir = file('x') (Groovy or Kotlin), or
// new File(rootDir, 'x'), File(settingsDir, "x"). Group 1 is the project,
// group 2 or 3 the folder relative to the settings file.
var reGradleProjectDir = regexp.MustCompile(`^project\s*\(\s*["'](:[^"'$\\\n]+)["']\s*\)\s*\.\s*projectDir\s*=\s*(?:file\s*\(\s*["']([^"'$\\\n]+)["']\s*\)|(?:new\s+)?File\s*\(\s*(?:rootDir|settingsDir|rootProject\.projectDir)\s*,\s*["']([^"'$\\\n]+)["']\s*\))[ \t]*(;|\r?\n|$|//|/\*)`)

// reGradleSettingsUnsure: anything else in a settings script that names or
// changes a project (its name, its build file) and that rw does not read.
var reGradleSettingsUnsure = regexp.MustCompile(`\b(?:project|findProject)\s*\(|buildFileName|\bprojectDescriptor`)

// gradleSettings is what rw read from a settings script.
type gradleSettings struct {
	includes [][]string
	dirs     map[string]string // project -> its folder, from projectDir lines
	spans    [][2]int          // where those lines are
}

// bom is UTF-8's byte order mark, which some editors put first in a file.
const bom = "\xef\xbb\xbf"

// gradleIncludes reads the include statements of a settings script,
// skipping comments and strings, or says why it cannot. An include inside
// a block may not run (mockito includes its Android projects only with an
// SDK), and a task of a project that is not there fails the run.
func gradleIncludes(src string) (gradleSettings, string) {
	var out gradleSettings
	out.dirs = map[string]string{}
	depth := 0
	ident := func(b byte) bool {
		return b == '_' || b == '$' || b == '.' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	for i := 0; i < len(src); {
		rest := src[i:]
		switch {
		case strings.HasPrefix(rest, "//"):
			i += lineEnd(rest)
			continue
		case strings.HasPrefix(rest, "/*"):
			j := strings.Index(rest[2:], "*/")
			if j < 0 {
				return out, "has an unclosed comment"
			}
			i += j + 4
			continue
		case strings.HasPrefix(rest, `"""`) || strings.HasPrefix(rest, "'''"):
			j := strings.Index(rest[3:], rest[:3])
			if j < 0 {
				return out, "has an unclosed string"
			}
			i += j + 6
			continue
		case rest[0] == '"' || rest[0] == '\'':
			j := 1
			for j < len(rest) && rest[j] != rest[0] {
				if rest[j] == '\\' {
					j++
				}
				j++
			}
			i += j + 1
			continue
		case rest[0] == '{' || rest[0] == '(' || rest[0] == '[':
			depth++
		case rest[0] == '}' || rest[0] == ')' || rest[0] == ']':
			depth--
		case strings.HasPrefix(rest, "include") && (i == 0 || !ident(src[i-1])) && (len(rest) == 7 || !ident(rest[7])):
			if depth > 0 {
				return out, "includes projects inside a block, which rw cannot evaluate"
			}
			names, n, ok := gradleIncludeArgs(rest[7:])
			if !ok {
				return out, "includes projects in a way rw does not follow"
			}
			out.includes = append(out.includes, names)
			i += 7 + n
			continue
		case strings.HasPrefix(rest, "project") && (i == 0 || !ident(src[i-1])):
			m := reGradleProjectDir.FindStringSubmatch(rest)
			if m == nil {
				break // the check of what is left makes it unsure
			}
			if depth > 0 {
				return out, "sets a project's folder inside a block, which rw cannot evaluate"
			}
			d := m[2] + m[3]
			clean := path.Clean(d)
			if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(d, ":") {
				return out, fmt.Sprintf("places %s outside the build folder (%s)", m[1], d)
			}
			if _, dup := out.dirs[m[1]]; dup {
				return out, "sets the folder of " + m[1] + " twice"
			}
			out.dirs[m[1]] = clean
			n := len(m[0])
			if m[4] == "//" || m[4] == "/*" {
				n -= 2 // the comment is read as one
			}
			out.spans = append(out.spans, [2]int{i, i + n})
			i += n
			continue
		}
		i++
	}
	return out, ""
}

// lineEnd is the index of the first newline in s, or len(s).
func lineEnd(s string) int {
	if k := strings.IndexByte(s, '\n'); k >= 0 {
		return k
	}
	return len(s)
}

// gradleIncludeArgs reads the project names of one include statement and
// how much of s it took; s starts right after the word. It follows string
// literals only, on one line or several: `include 'a', 'b'`,
// `include 'a',\n 'b'`, `include(\n "a", // core\n "b",\n)`.
func gradleIncludeArgs(s string) ([]string, int, bool) {
	i := 0
	skip := func(newlines bool) {
		for i < len(s) {
			switch {
			case s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || newlines && s[i] == '\n':
				i++
			case newlines && strings.HasPrefix(s[i:], "//"):
				i += lineEnd(s[i:])
			case newlines && strings.HasPrefix(s[i:], "/*"):
				j := strings.Index(s[i+2:], "*/")
				if j < 0 {
					return
				}
				i += j + 4
			default:
				return
			}
		}
	}
	skip(false)
	paren := i < len(s) && s[i] == '('
	if paren {
		i++
	}
	var names []string
	for {
		// Inside parentheses and after a comma the next name may be on
		// the next line.
		skip(paren || len(names) > 0)
		if paren && len(names) > 0 && i < len(s) && s[i] == ')' {
			return names, i + 1, true // a trailing comma
		}
		if i >= len(s) || (s[i] != '\'' && s[i] != '"') {
			return nil, 0, false // a variable, a list, a spread
		}
		q := s[i]
		j := strings.IndexByte(s[i+1:], q)
		if j < 0 {
			return nil, 0, false
		}
		lit := s[i+1 : i+1+j]
		if strings.ContainsAny(lit, "$\\\n") {
			return nil, 0, false // an interpolated or escaped name
		}
		names = append(names, lit)
		i += j + 2
		skip(paren)
		if i < len(s) && s[i] == ',' {
			i++
			continue
		}
		if paren {
			if i < len(s) && s[i] == ')' {
				return names, i + 1, true
			}
			return nil, 0, false
		}
		// Without parentheses the statement ends with the line, a
		// semicolon or a comment.
		if rest := s[i:]; rest == "" || rest[0] == '\n' || rest[0] == ';' || strings.HasPrefix(rest, "//") || strings.HasPrefix(rest, "/*") {
			return names, i, true
		}
		return nil, 0, false
	}
}

func plainGradleName(p string) bool {
	for _, r := range p {
		if !(r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-:", r))) {
			return false
		}
	}
	return p != ":"
}

func gradleAllowed(f []string, dir string) ([]string, string) {
	at := gradleShape(f)
	projects, why := gradleProjects(dir)
	if why != "" {
		return nil, ""
	}
	launcher := strings.Join(f[:at], " ")
	var out []string
	for _, p := range sortedKeys(projects) {
		if safeArg(p) {
			out = append(out, launcher+" "+p+":test")
		}
	}
	return out, launcher + " <:project:test ...> " + strings.Join(f[at+1:], " ")
}

// selectGradle narrows `gradle test` to the test tasks of the subprojects
// of the changed files and those that depend on them (project(":x")
// dependencies). Build scripts, settings and files of the root project make
// it full.
func selectGradle(cmd string, f []string, c *change) Plan {
	for _, file := range c.files {
		base := strings.ToLower(path.Base(file))
		if strings.HasSuffix(base, ".gradle") || strings.HasSuffix(base, ".gradle.kts") || base == "gradle.properties" ||
			strings.HasPrefix(file, "gradle/") || strings.HasPrefix(file, "buildSrc/") || strings.HasSuffix(base, ".versions.toml") {
			return full(cmd, file+" changed (build settings)")
		}
	}
	projects, why := gradleProjects(c.dir)
	if why != "" {
		return full(cmd, why)
	}
	for _, n := range []string{"build.gradle.kts", "build.gradle"} {
		if b, err := os.ReadFile(c.abs(n)); err == nil && (reGradleProjectCall.Match(b) || reGradleUnsure.Match(b)) {
			return full(cmd, n+" adds project dependencies for every subproject")
		}
	}
	deps := map[string][]string{}
	var dirs []string
	byDir := map[string]string{}
	for p, d := range projects {
		dirs = append(dirs, d)
		byDir[d] = p
		for _, n := range []string{"build.gradle.kts", "build.gradle"} {
			b, err := os.ReadFile(c.abs(path.Join(d, n)))
			if err != nil {
				continue
			}
			if reGradleUnsure.Match(b) {
				return full(cmd, path.Join(d, n)+" names projects in a way rw does not follow")
			}
			if reGradleProjectCall.Match(reGradleProject.ReplaceAll(b, nil)) {
				return full(cmd, path.Join(d, n)+" has a project dependency rw cannot resolve")
			}
			for _, m := range reGradleProject.FindAllSubmatch(b, -1) {
				if _, ok := projects[string(m[1])]; !ok {
					return full(cmd, path.Join(d, n)+" refers to a project rw cannot place: "+string(m[1]))
				}
				deps[p] = append(deps[p], string(m[1]))
			}
		}
	}
	var start []string
	for _, file := range c.files {
		d := owner(file, dirs)
		switch {
		case d != "":
			start = append(start, byDir[d])
		case isDoc(file):
		default:
			return full(cmd, file+" belongs to the root project")
		}
	}
	if len(start) == 0 {
		return Plan{Command: cmd, Why: "no project is affected by the " + c.changedWhy()}
	}
	set := reverseClosure(start, deps)
	var tasks []string
	for _, p := range sortedKeys(set) {
		if _, ok := projects[p]; !ok {
			continue
		}
		if !safeArg(p) {
			return full(cmd, unsafeWhy(p))
		}
		tasks = append(tasks, p+":test")
	}
	if len(tasks) >= len(projects) {
		return full(cmd, "every project is affected by the "+c.changedWhy())
	}
	at := gradleShape(f)
	out := strings.Join(append(append(append([]string(nil), f[:at]...), tasks...), f[at+1:]...), " ")
	return Plan{Command: cmd, Run: []string{out}, Why: fmt.Sprintf("%d of %d projects, affected by the %s", len(tasks), len(projects), c.changedWhy())}
}
