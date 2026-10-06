package affected

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	// A script rw cannot read is not narrowed.
	for _, script := range []string{"jest && eslint .", "mocha", "cross-env CI=1 jest", "vitest"} {
		d := tree(t, map[string]string{"package.json": `{"scripts":{"test":"` + script + `"}}`, "a.js": "x"})
		want(t, d, "npm test", sel(t, d, "npm test", "a.js"), "")
	}
}

func init() {
	// No test runs the real jest: by default it sees the whole folder.
	jestConfig = jestShows(nil)
}

// jestShows makes `jest --showConfig` print one project per roots list
// (folders relative to where jest runs, "." for that folder; nil = one
// project with the folder as root).
func jestShows(roots [][]string, extra ...string) func(context.Context, string, []string) ([]byte, error) {
	if roots == nil {
		roots = [][]string{{"."}}
	}
	return func(_ context.Context, dir string, argv []string) ([]byte, error) {
		if argv[len(argv)-1] != "--showConfig" {
			return nil, errors.New("not --showConfig")
		}
		var configs []map[string]any
		for _, rs := range roots {
			var abs []string
			for _, r := range rs {
				abs = append(abs, filepath.Join(dir, filepath.FromSlash(r)))
			}
			cfg := map[string]any{"rootDir": dir, "roots": abs, "modulePathIgnorePatterns": extra}
			configs = append(configs, cfg)
		}
		out, err := json.Marshal(map[string]any{"configs": configs})
		return append([]byte("> npm noise\n"), out...), err
	}
}

// jest's --findRelatedTests sees only files under its roots: a source
// file outside them (roots: ["test"], as in dayjs and luxon) had no
// related test, and --passWithNoTests made the narrowed run pass.
func TestSelectJestRoots(t *testing.T) {
	defer func(old func(context.Context, string, []string) ([]byte, error)) { jestConfig = old }(jestConfig)
	dir := tree(t, map[string]string{"package.json": `{"scripts":{"test":"jest --ci"}}`, "src/a.js": "x", "test/a.test.js": "x", "lib/b.js": "x"})
	jestConfig = jestShows([][]string{{"test"}})
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.js"), "")
	if p := sel(t, dir, "npm test", "src/a.js"); !strings.Contains(p.Why, "outside jest's roots") {
		t.Errorf("why: %q", p.Why)
	}
	want(t, dir, "npm test", sel(t, dir, "npm test", "test/a.test.js"), "npm test -- --findRelatedTests --passWithNoTests ./test/a.test.js")
	jestConfig = jestShows([][]string{{"src", "test"}})
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.js", "test/a.test.js"), "npm test -- --findRelatedTests --passWithNoTests ./src/a.js ./test/a.test.js")
	want(t, dir, "npm test", sel(t, dir, "npm test", "lib/b.js"), "")
	// Every project must see the file: another project's tests may use it.
	jestConfig = jestShows([][]string{{"src"}, {"lib"}})
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.js"), "")
	// A root above the check folder sees all of it.
	jestConfig = jestShows([][]string{{".."}})
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.js"), "npm test -- --findRelatedTests --passWithNoTests ./src/a.js")
	// modulePathIgnorePatterns hides files from jest too.
	jestConfig = jestShows(nil, `[/\\]src[/\\]`)
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.js"), "")
	want(t, dir, "npm test", sel(t, dir, "npm test", "lib/b.js"), "npm test -- --findRelatedTests --passWithNoTests ./lib/b.js")
	jestConfig = jestShows(nil, `(?<=x)`) // JavaScript-only syntax
	want(t, dir, "npm test", sel(t, dir, "npm test", "lib/b.js"), "")
	// jest cannot tell: full.
	jestConfig = func(context.Context, string, []string) ([]byte, error) { return nil, errors.New("jest: not found") }
	want(t, dir, "npm test", sel(t, dir, "npm test", "lib/b.js"), "")
	jestConfig = func(context.Context, string, []string) ([]byte, error) { return []byte("Usage: jest"), nil }
	want(t, dir, "npm test", sel(t, dir, "npm test", "lib/b.js"), "")
}

// The configuration comes from the runner the test script names, with
// the script's flags. npx reads every flag after `--no jest` as one of
// npm's (npm 11 ran jest without --config and --showConfig), so it gets
// "--".
func TestJestShowConfigCommand(t *testing.T) {
	defer func(old func(context.Context, string, []string) ([]byte, error)) { jestConfig = old }(jestConfig)
	var got []string
	jestConfig = func(ctx context.Context, dir string, argv []string) ([]byte, error) {
		got = append(got, strings.Join(argv, " "))
		return jestShows(nil)(ctx, dir, argv)
	}
	dir := tree(t, map[string]string{"package.json": `{"scripts":{"test":"jest --config ./jest/config.js"}}`, "src/a.js": "x"})
	for _, cmd := range []string{"npm test", "pnpm test", "yarn test", "npx jest -w 2"} {
		if p := sel(t, dir, cmd, "src/a.js"); p.Full {
			t.Errorf("%s: %+v", cmd, p)
		}
	}
	w := []string{"npx --no -- jest --config ./jest/config.js --showConfig", "pnpm exec jest --config ./jest/config.js --showConfig",
		"yarn jest --config ./jest/config.js --showConfig", "npx jest -w 2 --showConfig"}
	if !slices.Equal(got, w) {
		t.Errorf("ran\n%q\nwant\n%q", got, w)
	}
	// In the sandbox it runs there, and jest's paths are the container's:
	// its cwd says where the check folder is.
	var ran []string
	exec := func(_ context.Context, _ string, argv []string) ([]byte, error) {
		ran = append(ran, strings.Join(argv, " "))
		return []byte(`{"configs":[{"cwd":"/work","rootDir":"/work","roots":["/work/src"]}]}`), nil
	}
	p := Select(context.Background(), "npm test", "", Input{Root: dir, Dir: dir, Files: []string{"src/a.js"}, Exec: exec})
	if p.Full || len(ran) != 1 {
		t.Errorf("sandbox: %+v, ran %q", p, ran)
	}
	p = Select(context.Background(), "npm test", "", Input{Root: dir, Dir: dir, Files: []string{"package-lock.json"}, Exec: exec})
	if !p.Full || len(ran) != 1 {
		t.Errorf("a full run asks jest nothing: %+v, ran %q", p, ran)
	}
	exec = func(context.Context, string, []string) ([]byte, error) {
		return []byte(`{"configs":[{"rootDir":"/work","roots":["/work/src"]}]}`), nil
	}
	if p := Select(context.Background(), "npm test", "", Input{Root: dir, Dir: dir, Files: []string{"src/a.js"}, Exec: exec}); !p.Full {
		t.Errorf("an old jest in the sandbox: %+v", p)
	}
}

// vitest before 1.2.2 ran no test for a module that the tests reach only
// through other modules (superjson: 0.34.6 and 1.2.1 found nothing for
// src/transformer.ts, 1.2.2 found three test files).
func TestSelectVitestVersion(t *testing.T) {
	files := map[string]string{"package.json": `{"scripts":{"test":"vitest run"}}`, "src/a.ts": "x"}
	dir := tree(t, files)
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.ts"), "") // no vitest installed
	for v, ok := range map[string]bool{"0.34.6": false, "1.2.1": false, "1.2.2-beta.1": false, "1.2.2": true, "1.10.0": true, "3.2.7": true, "x": false} {
		os.MkdirAll(filepath.Join(dir, "node_modules", "vitest"), 0o755)
		os.WriteFile(filepath.Join(dir, "node_modules", "vitest", "package.json"), []byte(`{"version":"`+v+`"}`), 0o644)
		p := sel(t, dir, "npm test", "src/a.ts")
		if p.Full == ok {
			t.Errorf("vitest %s: %+v", v, p)
		}
	}
	// Hoisted to the top of a workspace.
	root := tree(t, map[string]string{"node_modules/vitest/package.json": `{"version":"3.0.0"}`, "app/package.json": files["package.json"], "app/src/a.ts": "x"})
	p := Select(context.Background(), "npm test", "", Input{Root: root, Dir: filepath.Join(root, "app"), Files: []string{"app/src/a.ts"}})
	if p.Full {
		t.Errorf("hoisted: %+v", p)
	}
	// Above the repo does not count.
	outer := tree(t, map[string]string{"node_modules/vitest/package.json": `{"version":"3.0.0"}`, "repo/package.json": files["package.json"], "repo/src/a.ts": "x"})
	repo := filepath.Join(outer, "repo")
	if p := sel(t, repo, "npm test", "src/a.ts"); !p.Full {
		t.Errorf("outside the repo: %+v", p)
	}
}

func TestSelectVitest(t *testing.T) {
	dir := tree(t, map[string]string{"package.json": `{"scripts":{"test":"vitest run --reporter=dot"}}`, "src/a.ts": "x",
		"node_modules/vitest/package.json": `{"version":"3.2.7"}`})
	// npx reads flags after `--no vitest` as npm's unless "--" ends its own.
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/a.ts"), "npx --no -- vitest related --run --passWithNoTests --reporter=dot ./src/a.ts")
	want(t, dir, "pnpm test", sel(t, dir, "pnpm test", "src/a.ts"), "pnpm exec vitest related --run --passWithNoTests --reporter=dot ./src/a.ts")
	want(t, dir, "npx vitest run", sel(t, dir, "npx vitest run", "src/a.ts"), "npx vitest related --run --passWithNoTests ./src/a.ts")
	pre, hint := Allowed(dir, "npm test", "")
	if len(pre) != 1 || pre[0] != "npx --no -- vitest related --run --passWithNoTests --reporter=dot" || !strings.Contains(hint, "<files>") {
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

// Plain `cargo test` in a workspace whose root is a package tests only the
// default members: a narrowed run must not test the others.
func TestSelectCargoDefaultMembers(t *testing.T) {
	dir := tree(t, map[string]string{"Cargo.toml": "[package]\n", "src/main.rs": "", "core/src/lib.rs": "", "web/src/lib.rs": "", "cli/src/lib.rs": ""})
	pkg := func(name, rel, deps string) string {
		return `{"id":"` + name + `-id","name":"` + name + `","manifest_path":"` + jsonPath(dir, rel+"Cargo.toml") + `","dependencies":[` + deps + `]}`
	}
	dep := func(rel string) string {
		return `{"name":"x","path":"` + strings.TrimSuffix(jsonPath(dir, rel+"/"), `\\`) + `"}`
	}
	pkgs := `"packages":[` + pkg("app", "", dep("core")) + "," + pkg("core", "core/", "") + "," + pkg("web", "web/", dep("core")) + "," + pkg("cli", "cli/", "") + "]"
	old := cargoMetadata
	defer func() { cargoMetadata = old }()
	meta := `{` + pkgs + `,"workspace_default_members":["app-id","core-id","cli-id"]}`
	cargoMetadata = func(ctx context.Context, d string) ([]byte, error) { return []byte(meta), nil }
	// web depends on core but is no default member: not tested.
	want(t, dir, "cargo test", sel(t, dir, "cargo test", "core/src/lib.rs"), "cargo test -p app -p core")
	want(t, dir, "cargo test", sel(t, dir, "cargo test", "web/src/lib.rs"), "-")
	want(t, dir, "cargo test --workspace", sel(t, dir, "cargo test --workspace", "web/src/lib.rs"), "cargo test --workspace -p web")
	// An older cargo does not list them: a root package means full.
	meta = `{` + pkgs + `}`
	want(t, dir, "cargo test", sel(t, dir, "cargo test", "cli/src/lib.rs"), "")
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
		// Extra.Tests is not in the solution: plain dotnet test never runs it.
		"App.sln": "Microsoft Visual Studio Solution File, Format Version 12.00\n" +
			`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Core", "src\Core\Core.csproj", "{1}"` + "\nEndProject\n" +
			`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Web", "src\Web\Web.csproj", "{2}"` + "\nEndProject\n" +
			`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Other", "src\Other\Other.csproj", "{3}"` + "\nEndProject\n" +
			`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Core.Tests", "tests\Core.Tests\Core.Tests.csproj", "{4}"` + "\nEndProject\n" +
			`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Web.Tests", "tests\Web.Tests\Web.Tests.csproj", "{5}"` + "\nEndProject\n" +
			`Project("{FAE04EC0-301F-11D3-BF4B-00C04F79EFBC}") = "Other.Tests", "tests\Other.Tests\Other.Tests.csproj", "{6}"` + "\nEndProject\n",
		"src/Core/Core.csproj":                 `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"src/Core/A.cs":                        "",
		"src/Web/Web.csproj":                   `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup><ProjectReference Include="..\Core\Core.csproj" /></ItemGroup></Project>`,
		"src/Web/W.cs":                         "",
		"src/Other/Other.csproj":               `<Project Sdk="Microsoft.NET.Sdk"></Project>`,
		"tests/Core.Tests/Core.Tests.csproj":   strings.ReplaceAll(strings.Replace(testProj, "%s", "Core", 1), "%s", "Core"),
		"tests/Web.Tests/Web.Tests.csproj":     strings.ReplaceAll(strings.Replace(testProj, "%s", "Web", 1), "%s", "Web"),
		"tests/Other.Tests/Other.Tests.csproj": strings.ReplaceAll(strings.Replace(testProj, "%s", "Other", 1), "%s", "Other"),
		"tests/Extra.Tests/Extra.Tests.csproj": strings.ReplaceAll(strings.Replace(testProj, "%s", "Web", 1), "%s", "Web"),
		"Directory.Build.props":                "",
	})
	cmd := "dotnet test -c Release"
	want(t, dir, cmd, sel(t, dir, cmd, "src/Web/W.cs"), cmd+" ./tests/Web.Tests/Web.Tests.csproj")
	want(t, dir, cmd, sel(t, dir, cmd, "src/Core/A.cs"), cmd+" ./tests/Core.Tests/Core.Tests.csproj | "+cmd+" ./tests/Web.Tests/Web.Tests.csproj")
	// Every test project of the solution is affected.
	want(t, dir, cmd, sel(t, dir, cmd, "src/Core/A.cs", "src/Other/Other.csproj"), "")
	for _, f := range []string{"App.sln", "Directory.Build.props", "global.json", "build/x.cs"} {
		want(t, dir, cmd, sel(t, dir, cmd, f), "")
	}
	want(t, dir, "dotnet test App.sln", sel(t, dir, "dotnet test App.sln", "src/Web/W.cs"), "")
	// No solution, or two: plain dotnet test runs what is in the folder.
	os.WriteFile(filepath.Join(dir, "Second.slnx"), []byte(`<Solution><Project Path="src/Web/Web.csproj" /></Solution>`), 0o644)
	want(t, dir, cmd, sel(t, dir, cmd, "src/Web/W.cs"), "")
	os.Remove(filepath.Join(dir, "App.sln"))
	os.WriteFile(filepath.Join(dir, "Second.slnx"), []byte(`<Solution><Project Path="src/Web/Web.csproj" /><Project Path="tests/Web.Tests/Web.Tests.csproj" /><Project Path="tests/Core.Tests/Core.Tests.csproj" /></Solution>`), 0o644)
	want(t, dir, cmd, sel(t, dir, cmd, "src/Web/W.cs"), cmd+" ./tests/Web.Tests/Web.Tests.csproj")
	os.Remove(filepath.Join(dir, "Second.slnx"))
	want(t, dir, cmd, sel(t, dir, cmd, "src/Web/W.cs"), "")
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

// An include over several lines (mockito's settings.gradle.kts, ArchUnit's
// settings.gradle) named projects that rw did not see: a change in core
// ran :core:test, never the tests of the projects that use core.
func TestSelectGradleIncludeLines(t *testing.T) {
	files := map[string]string{
		"core/src/A.java":     "",
		"plugin/build.gradle": "dependencies { implementation project(':core') }\n",
		"plugin/src/P.java":   "",
		"other/src/O.java":    "",
	}
	for _, settings := range []string{
		"include(\"core\")\ninclude(\n    \"plugin\",\n    \"other\",\n)\n",
		"include 'core',\n        'plugin', 'other'\n",
		"include(\":core\", \":plugin\",\n  \":other\")\n",
	} {
		files["settings.gradle"] = settings
		dir := tree(t, files)
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/src/A.java"), "gradle :core:test :plugin:test")
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "other/src/O.java"), "gradle :other:test")
	}
	// Includes rw cannot read run in full: names from variables or
	// loops, and includes inside a block, which may not run (mockito
	// includes its Android projects only with an SDK; a task of a project
	// that is not there fails the run).
	for _, settings := range []string{
		"include(\"core\")\ninclude(*extra)\n",
		"include 'core'\ninclude names\n",
		"include(\"core\")\nif (file(\"local.properties\").exists()) {\n    include(\"plugin\")\n}\n",
		"include(\"core\")\nif (hasSdk) {\n\tinclude 'plugin'\n}\n",
		"include \"core\", \"$name\"\n",
		"include 'core' + 'x'\n",
		"include(\"core\", \"plugin\"\n",
	} {
		files["settings.gradle"] = settings
		dir := tree(t, files)
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/src/A.java"), "")
	}
	// includeBuild and includeFlat are not include statements (and run in full).
	files["settings.gradle"] = "include 'core', 'plugin', 'other'\nincludeBuild '../lib'\n"
	dir := tree(t, files)
	want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/src/A.java"), "")
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
	// The include sits on its own line after the settings: a Groovy
	// comment is fine.
	d := tree(t, map[string]string{"settings.gradle": "include 'core', 'web' // all\n", "core/A.java": "", "web/build.gradle": "dependencies { implementation project(':core') }\n"})
	want(t, d, "gradle test", sel(t, d, "gradle test", "web/W.java"), "gradle :web:test")
	odd := tree(t, map[string]string{"settings.gradle": "include 'a'\nproject(':a').projectDir = file('x')\n", "x/A.java": ""})
	want(t, odd, "gradle test", sel(t, odd, "gradle test", "x/A.java"), "")
	// A project name with a space or comma would split the task argument
	// or the comma-joined allow rules.
	for _, name := range []string{"a b", "a,b"} {
		d := tree(t, map[string]string{"settings.gradle": "include 'core', '" + name + "'\n", "core/build.gradle": "", "core/A.java": ""})
		want(t, d, "gradle test", sel(t, d, "gradle test", "core/A.java"), "")
		if pre, _ := Allowed(d, "gradle test", ""); len(pre) != 0 {
			t.Errorf("%q: allowed %q", name, pre)
		}
	}
}
