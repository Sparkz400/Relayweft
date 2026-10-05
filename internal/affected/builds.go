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
				return full(cmd, fmt.Sprintf("%s references %s, which sy cannot find", p, m[1]))
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
		return nil, fmt.Sprintf("plain dotnet test runs what is in the folder, and sy narrows only one solution (found %d)", len(slns))
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
		return nil, slns[0] + " lists no projects sy can read"
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
	reGradleQuoted  = regexp.MustCompile(`["']([^"']+)["']`)
	reGradleProject = regexp.MustCompile(`project\(\s*(?:path\s*[:=]\s*)?["'](:[^"']*)["']`)
	reGradleUnsure  = regexp.MustCompile(`projectDir|includeBuild|includeFlat|\.each\b|forEach|for\s*\(|file\(|rootProject\.children|projects\.\w`)
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
	if reGradleUnsure.Match(data) {
		return nil, name + " places projects in a way sy does not follow"
	}
	projects := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "include") {
			continue
		}
		for _, m := range reGradleQuoted.FindAllStringSubmatch(t, -1) {
			p := ":" + strings.TrimPrefix(m[1], ":")
			if !plainGradleName(p) {
				// Task names become command arguments and allow rules
				// (comma-joined for Claude): letters, digits, . _ - : only.
				return nil, fmt.Sprintf("%s names project %q, which sy does not pass on", name, m[1])
			}
			projects[p] = strings.ReplaceAll(strings.TrimPrefix(p, ":"), ":", "/")
		}
	}
	if len(projects) == 0 {
		return nil, "the project has no subprojects"
	}
	return projects, ""
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
		if b, err := os.ReadFile(c.abs(n)); err == nil && (reGradleProject.Match(b) || reGradleUnsure.Match(b)) {
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
				return full(cmd, path.Join(d, n)+" names projects in a way sy does not follow")
			}
			for _, m := range reGradleProject.FindAllSubmatch(b, -1) {
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
