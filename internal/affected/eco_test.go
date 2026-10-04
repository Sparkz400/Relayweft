package affected

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// want checks a plan: "" = full, "-" = nothing to run, else the commands
// joined by " | ". Every narrowed command must be in the agents' allowlist.
func want(t *testing.T, dir, cmd string, p Plan, w string) {
	t.Helper()
	switch {
	case w == "" && !p.Full:
		t.Errorf("%s: want full, got %+v", cmd, p)
	case w == "-" && (p.Full || len(p.Run) != 0):
		t.Errorf("%s: want nothing, got %+v", cmd, p)
	case w != "" && w != "-" && (p.Full || strings.Join(p.Run, " | ") != w):
		t.Errorf("%s:\n want %q\n got %+v", cmd, w, p)
	}
	if p.Why == "" {
		t.Errorf("%s: no reason", cmd)
	}
	checkAllowed(t, dir, cmd, "", p)
}

func TestSelectJest(t *testing.T) {
	dir := tree(t, map[string]string{"package.json": `{"scripts":{"test":"jest --ci"}}`, "src/a.ts": "x", "src/a.test.ts": "x", "src/s.css": "x",
		"src/__snapshots__/a.test.ts.snap": "x", "jest.config.js": "x", "data.json": "{}"})
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.ts", "README.md"), "npm test -- --findRelatedTests --passWithNoTests ./src/a.ts")
	want(t, dir, "pnpm test", sel(t, dir, "pnpm test", "src/a.ts", "data.json"), "pnpm test --findRelatedTests --passWithNoTests ./data.json ./src/a.ts")
	want(t, dir, "yarn test", sel(t, dir, "yarn test", "src/a.test.ts"), "yarn test --findRelatedTests --passWithNoTests ./src/a.test.ts")
	want(t, dir, "npx jest -w 2", sel(t, dir, "npx jest -w 2", "src/a.ts"), "npx jest -w 2 --findRelatedTests --passWithNoTests ./src/a.ts")
	for _, f := range []string{"package.json", "jest.config.js", "src/__snapshots__/a.test.ts.snap", "src/s.css", "src/gone.ts", "tsconfig.json", "node_modules/x/i.js"} {
		want(t, dir, "npm test", sel(t, dir, "npm test", f), "")
	}
	want(t, dir, "npm test", sel(t, dir, "npm test", "docs/x.md"), "-")
	// A script sy cannot read is not narrowed.
	for _, script := range []string{"jest && eslint .", "mocha", "cross-env CI=1 jest", "vitest"} {
		d := tree(t, map[string]string{"package.json": `{"scripts":{"test":"` + script + `"}}`, "a.js": "x"})
		want(t, d, "npm test", sel(t, d, "npm test", "a.js"), "")
	}
}

func TestSelectVitest(t *testing.T) {
	dir := tree(t, map[string]string{"package.json": `{"scripts":{"test":"vitest run --reporter=dot"}}`, "src/a.ts": "x"})
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.ts"), "npx --no vitest related --run --passWithNoTests --reporter=dot ./src/a.ts")
	want(t, dir, "pnpm test", sel(t, dir, "pnpm test", "src/a.ts"), "pnpm exec vitest related --run --passWithNoTests --reporter=dot ./src/a.ts")
	want(t, dir, "npx vitest run", sel(t, dir, "npx vitest run", "src/a.ts"), "npx vitest related --run --passWithNoTests ./src/a.ts")
	pre, hint := Allowed(dir, "npm test", "")
	if len(pre) != 1 || pre[0] != "npx --no vitest related --run --passWithNoTests --reporter=dot" || !strings.Contains(hint, "<files>") {
		t.Errorf("allowed %q hint %q", pre, hint)
	}
}

func TestSelectPytest(t *testing.T) {
	dir := tree(t, map[string]string{
		"pkg/__init__.py":        "",
		"pkg/core.py":            "def f(): pass\n",
		"pkg/api.py":             "from . import core\nfrom .core import f\n",
		"pkg/cli.py":             "import pkg.api as api\n",
		"pkg/lonely.py":          "x = 1\n",
		"tests/test_api.py":      "from pkg.api import (\n    f,\n)\n",
		"tests/test_cli.py":      "import pkg.cli\n",
		"tests/test_other.py":    "import os\n",
		"tests/helpers.py":       "import json\n",
		"tests/test_helpers.py":  "import helpers\n",
		"src/lib/mod.py":         "",
		"tests/test_src.py":      "from lib import mod\n",
		"tests/conftest.py":      "",
		"tests/data/sample.json": "{}",
	})
	cmd := "python -m pytest -q"
	want(t, dir, cmd, sel(t, dir, cmd, "pkg/core.py"), cmd+" ./tests/test_api.py ./tests/test_cli.py")
	want(t, dir, cmd, sel(t, dir, cmd, "pkg/cli.py"), cmd+" ./tests/test_cli.py")
	want(t, dir, cmd, sel(t, dir, cmd, "tests/test_other.py"), cmd+" ./tests/test_other.py")
	want(t, dir, cmd, sel(t, dir, cmd, "tests/helpers.py"), cmd+" ./tests/test_helpers.py") // rootdir-style import
	want(t, dir, cmd, sel(t, dir, cmd, "src/lib/mod.py"), cmd+" ./tests/test_src.py")       // src layout
	want(t, dir, cmd, sel(t, dir, cmd, "pkg/__init__.py"), cmd+" ./tests/test_api.py ./tests/test_cli.py")
	want(t, dir, cmd, sel(t, dir, cmd, "docs/x.md"), "-")
	for _, f := range []string{"tests/conftest.py", "pyproject.toml", "tests/data/sample.json", "requirements.txt", "pkg/lonely.py"} {
		want(t, dir, cmd, sel(t, dir, cmd, f), "")
	}
	// pytest with its own paths or a test selection is not narrowed.
	for _, c := range []string{"pytest tests/unit", "pytest -q tests"} {
		want(t, dir, c, sel(t, dir, c, "pkg/core.py"), "")
	}
	want(t, dir, "pytest -k fast", sel(t, dir, "pytest -k fast", "pkg/cli.py"), "pytest -k fast ./tests/test_cli.py")
}

func TestSelectCargo(t *testing.T) {
	dir := tree(t, map[string]string{"Cargo.toml": "[workspace]\n", "core/src/lib.rs": "", "web/src/lib.rs": "", "cli/src/main.rs": "", "tools/x.rs": ""})
	meta := `{"packages":[
 {"name":"core","manifest_path":"` + jsonPath(dir, "core/Cargo.toml") + `","dependencies":[{"name":"serde"}]},
 {"name":"web","manifest_path":"` + jsonPath(dir, "web/Cargo.toml") + `","dependencies":[{"name":"core","path":"` + jsonPath(dir, "core") + `"}]},
 {"name":"cli","manifest_path":"` + jsonPath(dir, "cli/Cargo.toml") + `","dependencies":[]}]}`
	old := cargoMetadata
	defer func() { cargoMetadata = old }()
	cargoMetadata = func(ctx context.Context, d string) ([]byte, error) { return []byte(meta), nil }
	want(t, dir, "cargo test", sel(t, dir, "cargo test", "core/src/lib.rs"), "cargo test -p core -p web")
	want(t, dir, "cargo test", sel(t, dir, "cargo test", "cli/src/main.rs"), "cargo test -p cli")
	want(t, dir, "cargo test --release -- --nocapture", sel(t, dir, "cargo test --release -- --nocapture", "cli/src/main.rs"), "cargo test --release -p cli -- --nocapture")
	want(t, dir, "cargo test", sel(t, dir, "cargo test", "core/src/lib.rs", "cli/src/main.rs"), "")
	for _, f := range []string{"Cargo.lock", "web/Cargo.toml", "tools/x.rs"} {
		want(t, dir, "cargo test", sel(t, dir, "cargo test", f), "")
	}
	want(t, dir, "cargo test -p core", sel(t, dir, "cargo test -p core", "cli/src/main.rs"), "")
	cargoMetadata = func(ctx context.Context, d string) ([]byte, error) { return nil, errors.New("cargo: not found") }
	want(t, dir, "cargo test", sel(t, dir, "cargo test", "cli/src/main.rs"), "")
}

func jsonPath(dir, rel string) string {
	p := dir + "/" + rel
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(strings.ReplaceAll(p, "/", `\`), `\`, `\\`)
	}
	return p
}

func TestSelectDotnet(t *testing.T) {
	testProj := `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup><PackageReference Include="Microsoft.NET.Test.Sdk" Version="17" />` +
		`<ProjectReference Include="..\..\src\%s\%s.csproj" /></ItemGroup></Project>`
	dir := tree(t, map[string]string{
		"App.sln":                              "",
		"src/Core/Core.csproj":                 `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"src/Core/A.cs":                        "",
		"src/Web/Web.csproj":                   `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup><ProjectReference Include="..\Core\Core.csproj" /></ItemGroup></Project>`,
		"src/Web/W.cs":                         "",
		"src/Other/Other.csproj":               `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"tests/Core.Tests/Core.Tests.csproj":   strings.ReplaceAll(strings.Replace(testProj, "%s", "Core", 1), "%s", "Core"),
		"tests/Web.Tests/Web.Tests.csproj":     strings.ReplaceAll(strings.Replace(testProj, "%s", "Web", 1), "%s", "Web"),
		"tests/Other.Tests/Other.Tests.csproj": strings.ReplaceAll(strings.Replace(testProj, "%s", "Other", 1), "%s", "Other"),
		"Directory.Build.props":                "",
	})
	cmd := "dotnet test -c Release"
	want(t, dir, cmd, sel(t, dir, cmd, "src/Web/W.cs"), cmd+" ./tests/Web.Tests/Web.Tests.csproj")
	want(t, dir, cmd, sel(t, dir, cmd, "src/Core/A.cs"), cmd+" ./tests/Core.Tests/Core.Tests.csproj | "+cmd+" ./tests/Web.Tests/Web.Tests.csproj")
	want(t, dir, cmd, sel(t, dir, cmd, "src/Core/A.cs", "src/Other/Other.csproj"), "")
	for _, f := range []string{"App.sln", "Directory.Build.props", "global.json", "build/x.cs"} {
		want(t, dir, cmd, sel(t, dir, cmd, f), "")
	}
	want(t, dir, "dotnet test App.sln", sel(t, dir, "dotnet test App.sln", "src/Web/W.cs"), "")
}

func TestSelectMaven(t *testing.T) {
	dir := tree(t, map[string]string{
		"pom.xml":                      "<project><modules><module>core</module><module>web</module></modules></project>",
		"core/pom.xml":                 "<project></project>",
		"core/src/main/java/A.java":    "",
		"web/pom.xml":                  "<project><modules><module>api</module></modules></project>",
		"web/api/pom.xml":              "<project></project>",
		"web/api/src/B.java":           "",
		"src/test/resources/x/pom.xml": "",
	})
	want(t, dir, "mvn -q test", sel(t, dir, "mvn -q test", "web/api/src/B.java"), "mvn -q test -pl web/api -am -amd")
	want(t, dir, "mvn -q test", sel(t, dir, "mvn -q test", "core/src/main/java/A.java", "web/api/src/B.java"), "mvn -q test -pl "+quote("core,web/api")+" -am -amd")
	for _, f := range []string{"core/pom.xml", "src/Main.java", ".mvn/wrapper/x"} {
		want(t, dir, "mvn -q test", sel(t, dir, "mvn -q test", f), "")
	}
	want(t, dir, "mvn -pl core test", sel(t, dir, "mvn -pl core test", "core/src/main/java/A.java"), "")
	want(t, dir, "mvn deploy", sel(t, dir, "mvn deploy", "core/src/main/java/A.java"), "")
}

func TestSelectGradle(t *testing.T) {
	dir := tree(t, map[string]string{
		"settings.gradle":           "rootProject.name = 'x'\ninclude 'core', 'web'\ninclude(\":tools:cli\")\n",
		"build.gradle":              "plugins { id 'java' }\n",
		"core/build.gradle":         "",
		"core/src/main/java/A.java": "",
		"web/build.gradle.kts":      "dependencies { implementation(project(\":core\")) }\n",
		"web/src/W.java":            "",
		"tools/cli/build.gradle":    "",
		"tools/cli/src/C.java":      "",
	})
	cmd := "./gradlew test --info"
	want(t, dir, cmd, sel(t, dir, cmd, "core/src/main/java/A.java"), "./gradlew :core:test :web:test --info")
	want(t, dir, cmd, sel(t, dir, cmd, "tools/cli/src/C.java"), "./gradlew :tools:cli:test --info")
	want(t, dir, cmd, sel(t, dir, cmd, "core/src/main/java/A.java", "tools/cli/src/C.java"), "")
	for _, f := range []string{"core/build.gradle", "gradle/libs.versions.toml", "src/Root.java", "gradle.properties"} {
		want(t, dir, cmd, sel(t, dir, cmd, f), "")
	}
	pre, _ := Allowed(dir, cmd, "")
	if strings.Join(pre, "|") != "./gradlew :core:test|./gradlew :tools:cli:test|./gradlew :web:test" {
		t.Errorf("allowed %q", pre)
	}
	want(t, dir, "./gradlew check", sel(t, dir, "./gradlew check", "web/src/W.java"), "")
	odd := tree(t, map[string]string{"settings.gradle": "include 'a'\nproject(':a').projectDir = file('x')\n", "x/A.java": ""})
	want(t, odd, "gradle test", sel(t, odd, "gradle test", "x/A.java"), "")
}
