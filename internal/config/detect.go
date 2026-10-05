package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DetectVerify guesses a repo's own checks from its build files, for
// `rw init`. It returns nil when it finds nothing it is sure about.
func DetectVerify(dir string) []string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	glob := func(pattern string) bool {
		m, _ := filepath.Glob(filepath.Join(dir, pattern))
		return len(m) > 0
	}
	var out []string
	if has("go.mod") {
		out = append(out, "go build ./...", "go test ./...")
	}
	if has("Cargo.toml") {
		out = append(out, "cargo test")
	}
	if has("package.json") {
		if cmd := npmTest(dir); cmd != "" {
			out = append(out, cmd)
		}
	}
	if has("pytest.ini") || has("conftest.py") || fileHas(filepath.Join(dir, "pyproject.toml"), "[tool.pytest") ||
		fileHas(filepath.Join(dir, "setup.cfg"), "[tool:pytest]") || fileHas(filepath.Join(dir, "tox.ini"), "[pytest]") {
		out = append(out, "python -m pytest -q")
	}
	if glob("*.sln") || glob("*.csproj") || glob("*.fsproj") {
		out = append(out, "dotnet test")
	}
	switch {
	case has("pom.xml"):
		out = append(out, "mvn -q test")
	case has("gradlew") || has("gradlew.bat"):
		out = append(out, gradleWrapper()+" test")
	case has("build.gradle") || has("build.gradle.kts"):
		out = append(out, "gradle test")
	}
	return out
}

// npmTest returns the test command of a package.json with a real test
// script ("" for none or npm's placeholder).
func npmTest(dir string) string {
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
	t := pkg.Scripts["test"]
	if t == "" || strings.Contains(t, "no test specified") {
		return ""
	}
	tool := "npm"
	switch {
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		tool = "pnpm"
	case exists(filepath.Join(dir, "yarn.lock")):
		tool = "yarn"
	case exists(filepath.Join(dir, "bun.lockb")) || exists(filepath.Join(dir, "bun.lock")):
		tool = "bun"
	}
	if tool == "npm" {
		return "npm test"
	}
	return tool + " test"
}

func gradleWrapper() string {
	if filepath.Separator == '\\' {
		return `.\gradlew.bat`
	}
	return "./gradlew"
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileHas(path, s string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), s)
}

// WithVerify returns the default config text with the verify commands
// filled in (the commented layout is kept).
func WithVerify(yamlText []byte, cmds []string) []byte {
	if len(cmds) == 0 {
		return yamlText
	}
	q := make([]string, len(cmds))
	for i, c := range cmds {
		q[i] = strconv.Quote(c)
	}
	old := "  commands: []"
	s := string(yamlText)
	i := strings.Index(s, old)
	if i < 0 {
		return yamlText
	}
	// Keep the trailing comment column.
	end := strings.IndexByte(s[i:], '\n')
	if end < 0 {
		end = len(s) - i
	}
	line := "  commands: [" + strings.Join(q, ", ") + "]   # detected by rw init"
	return []byte(s[:i] + line + s[i+end:])
}
