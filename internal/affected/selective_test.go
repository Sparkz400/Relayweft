package affected

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestJSPackageRejectsUnreadDependencyMetadata(t *testing.T) {
	for _, contents := range []string{
		`null`, `{"name":42}`, `{"scripts":[]}`, `{"dependencies":["core"]}`,
		`{"devDependencies":"core"}`, `{"peerDependencies":false}`, `{"optionalDependencies":42}`,
	} {
		dir := vitestTree(t, map[string]string{"packages/core/package.json": contents})
		if _, ok := readJSPackage(filepath.Join(dir, "packages/core/package.json")); ok {
			t.Errorf("accepted unread package metadata: %s", contents)
		}
		plan := sel(t, dir, "yarn test", "src/core.ts")
		if !plan.Full || !strings.Contains(plan.Why, "cannot read") {
			t.Errorf("unread metadata should fall back: %+v", plan)
		}
	}
}

func TestPnPVirtualWorkspacesRunFull(t *testing.T) {
	for _, loader := range []string{".pnp.cjs", ".pnp.js"} {
		dir := vitestTree(t, map[string]string{
			loader:                       "// generated loader",
			"packages/core/package.json": `{"name":"core","peerDependencies":{"react":"*"}}`,
		})
		for _, command := range []string{"yarn test", "yarn jest"} {
			plan := Select(context.Background(), command, "", Input{Root: dir, Dir: dir, Files: []string{"src/core.ts"}, Exec: jestProjects(map[string]any{"roots": []string{"."}})})
			if !plan.Full || !strings.Contains(plan.Why, "virtual path") {
				t.Errorf("%s: %+v", command, plan)
			}
		}
	}
	// The check folder itself may be a workspace beneath the PnP root.
	dir := tree(t, map[string]string{
		".pnp.cjs":                             "// generated loader",
		"app/package.json":                     `{"name":"app","scripts":{"test":"vitest run"},"peerDependencies":{"react":"*"}}`,
		"app/node_modules/vitest/package.json": `{"version":"3.2.7"}`,
		"app/src/core.ts":                      "export const value = 1;",
	})
	for _, command := range []string{"yarn test", "yarn jest"} {
		plan := Select(context.Background(), command, "", Input{Root: dir, Dir: dir + "/app", Files: []string{"app/src/core.ts"}, Exec: jestProjects(map[string]any{"roots": []string{"."}})})
		if !plan.Full || !strings.Contains(plan.Why, "virtual path") {
			t.Errorf("nested %s: %+v", command, plan)
		}
	}
}

func TestTaskOrchestratorsRemainFull(t *testing.T) {
	for _, command := range []string{"nx run-many -t test", "npx nx run-many --target=test --all", "turbo run test", "pnpm exec turbo run test"} {
		dir := tree(t, map[string]string{"package.json": `{"scripts":{"test":"` + command + `"}}`, "packages/core/src/a.ts": ""})
		for _, invocation := range []string{command, "npm test", "pnpm test", "yarn test"} {
			want(t, dir, invocation, sel(t, dir, invocation, "packages/core/src/a.ts"), "")
		}
	}
}

func TestPnPLoadersAreNotApplicationImports(t *testing.T) {
	dir := vitestTree(t, map[string]string{
		".pnp.cjs":                "const key = 'runtime'; require(key);\n",
		".pnp.loader.mjs":         "import './.pnp.cjs';\n",
		".yarn/releases/yarn.cjs": "require(dynamicPath);\n",
	})
	command := "yarn test"
	want(t, dir, command, sel(t, dir, command, "src/core.ts"), "yarn vitest related --run --passWithNoTests ./src/core.ts")
	for _, file := range []string{".pnp.cjs", ".pnp.js", ".pnp.loader.mjs", ".pnp.data.json", ".yarnrc.yml", ".yarnrc", ".yarn/plugins/plugin.cjs", ".yarn/patches/dep.patch"} {
		p := sel(t, dir, command, file)
		if !p.Full || !strings.Contains(p.Why, "dependencies or configuration") {
			t.Errorf("runtime change %s must run in full: %+v", file, p)
		}
	}
}

func TestGradleDependencyCalls(t *testing.T) {
	for _, tc := range []struct{ name, dependency, narrowed string }{
		{"space", "implementation project (':core')", "gradle :consumer:test :core:test"},
		{"newline", "implementation project\n(':core')", "gradle :consumer:test :core:test"},
		{"named groovy", "implementation project (path: ':core')", "gradle :consumer:test :core:test"},
		{"named kotlin", `implementation(project (path = ":core"))`, "gradle :consumer:test :core:test"},
		{"variable", "def target = ':core'; implementation project(target)", ""},
		{"command syntax", "def dep = project ':core'; implementation dep", ""},
		{"command variable", "def dep = project target; implementation dep", ""},
		{"concatenated", "implementation project(':co' + 're')", ""},
		{"interpolated", `implementation project(":${target}")`, ""},
		{"findProject", "implementation findProject(':core')", ""},
		{"unknown", "implementation project(':missing')", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tree(t, map[string]string{
				"settings.gradle":       "include 'core', 'consumer', 'other'\n",
				"core/A.java":           "",
				"consumer/build.gradle": "dependencies { " + tc.dependency + " }\n",
			})
			want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/A.java"), tc.narrowed)
		})
	}
}

func TestGradleAmbiguousProjectDirs(t *testing.T) {
	dir := tree(t, map[string]string{
		"settings.gradle": "include 'a', 'b', 'other'\nproject(':a').projectDir = file('shared')\nproject(':b').projectDir = file('shared')\n",
		"shared/A.java":   "",
	})
	want(t, dir, "gradle test", sel(t, dir, "gradle test", "shared/A.java"), "")
}

func TestGradleProjectDirWindowsCaseCollision(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows project folders are case insensitive")
	}
	dir := tree(t, map[string]string{
		"settings.gradle": "include 'a', 'b', 'other'\nproject(':a').projectDir = file('Shared')\nproject(':b').projectDir = file('shared')\n",
		"shared/A.java":   "",
	})
	want(t, dir, "gradle test", sel(t, dir, "gradle test", "shared/A.java"), "")
}

func TestGradleInitScriptRunsFull(t *testing.T) {
	dir := tree(t, map[string]string{"settings.gradle": "include 'a', 'b'\n", "a/A.java": ""})
	for _, command := range []string{"gradle -I setup.gradle test", "gradle test --init-script=setup.gradle", "gradle -Isetup.gradle test", "gradle -pother test", "gradle -bother.gradle test", "gradle -cother.gradle test"} {
		want(t, dir, command, sel(t, dir, command, "a/A.java"), "")
	}
}
