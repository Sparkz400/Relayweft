package affected

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestJSImports(t *testing.T) {
	src := `import a from './a'
export * from "@scope/pkg/sub"
const x = require('./x')
const y = await vi.importActual<typeof import('./y')>('./y')
const z = await import('./z')
import type { T } from '../t'
`
	got, computed := jsImports([]byte(src))
	var specs []string
	for _, g := range got {
		s := g.spec
		if g.blind {
			s += " (blind)"
		}
		specs = append(specs, s)
	}
	w := []string{"./a", "@scope/pkg/sub", "./x (blind)", "./y (blind)", "./z", "../t"}
	if !slices.Equal(specs, w) || computed {
		t.Errorf("got %q computed %v, want %q", specs, computed, w)
	}
	for _, s := range []string{"require(name)", "await import(`./locale/${l}.js`)", "require('./a' + b)", "vi.importActual(p)"} {
		if _, c := jsImports([]byte(s)); !c {
			t.Errorf("%s: not computed", s)
		}
	}
	for spec, name := range map[string]string{"@a/b/c": "@a/b", "lodash/fp": "lodash", "./x": "", "node:fs": "node:fs", "@a": "@a"} {
		if got := jsPkgName(spec); got != name {
			t.Errorf("jsPkgName(%q) = %q", spec, got)
		}
	}
}

// jestProjects makes `jest --showConfig` print these projects; each has
// roots (relative to where jest runs) and, optionally, more settings.
func jestProjects(projects ...map[string]any) func(context.Context, string, []string) ([]byte, error) {
	return func(_ context.Context, dir string, _ []string) ([]byte, error) {
		var configs []any
		for _, p := range projects {
			cfg := map[string]any{"cwd": dir, "moduleFileExtensions": jestExtensions, "moduleNameMapper": []any{}}
			for k, v := range p {
				switch k {
				case "roots":
					var abs []string
					for _, r := range v.([]string) {
						abs = append(abs, filepath.Join(dir, filepath.FromSlash(r)))
					}
					cfg["roots"] = abs
					cfg["rootDir"] = abs[0]
				case "map":
					// [pattern, path below the check folder or a module name]
					var m []any
					for _, pair := range v.([][2]string) {
						to := pair[1]
						if strings.HasPrefix(to, "@root/") {
							to = filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(to, "@root/")))
						}
						m = append(m, []string{pair[0], to})
					}
					cfg["moduleNameMapper"] = m
				default:
					cfg[k] = v
				}
			}
			configs = append(configs, cfg)
		}
		return json.Marshal(map[string]any{"configs": configs})
	}
}

// Several jest projects (jest's projects option, a monorepo): each finds
// related tests in its own index only. On a real workspace (jest 29.4.3)
// a change in one package found no test of another package that reached
// it by a relative path, by the package's name or by a moduleNameMapper
// alias; each runs in full. Projects whose files import only what they
// see are narrowed.
func TestJestMonorepoAndAliases(t *testing.T) {
	old := jestConfig
	t.Cleanup(func() { jestConfig = old })
	base := map[string]string{
		"package.json":                 `{"scripts":{"test":"jest"},"workspaces":["packages/*"]}`,
		"packages/core/package.json":   `{"name":"@w/core"}`,
		"packages/core/src/inner.js":   "export const inner = 1\n",
		"packages/core/src/index.js":   "export * from './inner'\n",
		"packages/core/src/a.test.js":  "import { inner } from './index'\n",
		"packages/app/package.json":    `{"name":"@w/app","dependencies":{"@w/core":"1"}}`,
		"packages/app/src/app.js":      "export const app = 1\n",
		"packages/app/src/app.test.js": "import { app } from './app'\n",
		"scripts/build.js":             "console.log(1)\n",
	}
	two := []map[string]any{{"roots": []string{"packages/core"}}, {"roots": []string{"packages/app"}}}
	narrowed := "npm test -- --findRelatedTests --passWithNoTests ./packages/core/src/inner.js"
	run := func(name string, extra map[string]string, projects []map[string]any, change, w string, why ...string) {
		t.Helper()
		files := map[string]string{}
		for k, v := range base {
			files[k] = v
		}
		for k, v := range extra {
			files[k] = v
		}
		dir := tree(t, files)
		jestConfig = jestProjects(projects...)
		p := sel(t, dir, "npm test", change)
		t.Run(name, func(t *testing.T) { want(t, dir, "npm test", p, w) })
		for _, s := range why {
			if !strings.Contains(p.Why, s) {
				t.Errorf("%s: why %q lacks %q", name, p.Why, s)
			}
		}
	}
	run("independent projects", nil, two, "packages/core/src/inner.js", narrowed)
	run("by package name", map[string]string{"packages/app/src/app.js": "import { inner } from '@w/core'\n"}, two, "packages/core/src/inner.js", "", "packages/app/src/app.js imports packages/core/", "does not see")
	run("by relative path", map[string]string{"packages/app/src/app.test.js": "import { inner } from '../../core/src/inner'\n"}, two, "packages/core/src/inner.js", "", "packages/core/src/inner.js")
	alias := []map[string]any{{"roots": []string{"packages/core"}}, {"roots": []string{"packages/app"}, "map": [][2]string{{"^#core$", "@root/packages/core/src/inner.js"}}}}
	run("by alias", map[string]string{"packages/app/src/app.test.js": "import { inner } from '#core'\n"}, alias, "packages/core/src/inner.js", "", "imports packages/core/src/inner.js")
	// Aliases jest's index covers: inside the project, to node_modules,
	// with $1 from the pattern.
	inside := []map[string]any{{"roots": []string{"packages/core"}}, {"roots": []string{"packages/app"}, "map": [][2]string{
		{`\.css$`, "@root/node_modules/identity-obj-proxy/index.js"}, {"^@/(.*)$", "@root/packages/app/src/$1"}}}}
	run("alias inside", map[string]string{"packages/app/src/app.test.js": "import { app } from '@/app'\nimport s from './s.css'\n"}, inside, "packages/core/src/inner.js", narrowed)
	run("alias $1 to another project", map[string]string{"packages/app/src/app.test.js": "import { inner } from '@/inner'\n"},
		[]map[string]any{{"roots": []string{"packages/core"}}, {"roots": []string{"packages/app"}, "map": [][2]string{{"^@/(.*)$", "@root/packages/core/src/$1"}}}},
		"packages/core/src/inner.js", "", "inner.js")
	run("alias outside", map[string]string{"packages/app/src/app.js": "import x from 'x'\n"}, []map[string]any{{"roots": []string{"."}, "map": [][2]string{{"^x$", "/elsewhere/x.js"}}}}, "packages/core/src/inner.js", "")
	run("alias to a workspace package", map[string]string{"packages/app/src/app.js": "import x from 'core-alias'\n"},
		[]map[string]any{{"roots": []string{"packages/core"}}, {"roots": []string{"packages/app"}, "map": [][2]string{{"^core-alias$", "@w/core"}}}}, "packages/core/src/inner.js", "")
	run("JavaScript-only pattern", nil, []map[string]any{{"roots": []string{"."}, "map": [][2]string{{"(?<=a)b", "x"}}}}, "packages/core/src/inner.js", "", "moduleNameMapper")
	// One project over the whole folder sees every package.
	run("one project", map[string]string{"packages/app/src/app.js": "import { inner } from '@w/core'\n"}, []map[string]any{{"roots": []string{"."}}}, "packages/core/src/inner.js", narrowed)
	run("no project sees it", nil, two, "scripts/build.js", "", "no jest project sees")
	for _, extra := range []map[string]any{{"resolver": "./resolver.js"}, {"modulePaths": []string{"shared"}}, {"moduleDirectories": []string{"node_modules", "src"}}} {
		extra["roots"] = []string{"."}
		run("resolver", nil, []map[string]any{extra}, "packages/core/src/inner.js", "")
	}
	run("computed import", map[string]string{"packages/core/src/load.js": "export const load = (n) => require('./' + n)\n"}, two, "packages/core/src/inner.js", "", "computed")
}

const vitestPkg = `{"version":"3.2.7"}`

func vitestTree(t *testing.T, extra map[string]string) string {
	t.Helper()
	files := map[string]string{
		"package.json":                     `{"scripts":{"test":"vitest run"}}`,
		"src/core.ts":                      "export const value = 1\n",
		"src/core.test.ts":                 "import { value } from './core'\n",
		"node_modules/vitest/package.json": vitestPkg,
	}
	for k, v := range extra {
		if v == "" {
			delete(files, k)
		} else {
			files[k] = v
		}
	}
	return tree(t, files)
}

const vitestNarrowed = "npx --no -- vitest related --run --passWithNoTests ./src/core.ts"

// vitest follows workspaces, projects and aliases itself (vitest 1.2.2
// and 3.2.7 on a real npm workspace found every related test through
// test.projects, vitest.workspace, symlinked packages and resolve.alias),
// so those narrow, as do the plugins rw knows. What made it miss tests
// runs in full.
func TestVitestLayouts(t *testing.T) {
	cfg := "import { defineConfig } from 'vitest/config'\nimport { fileURLToPath } from 'node:url'\n"
	react := "import react from '@vitejs/plugin-react'\n"
	for name, extra := range map[string]map[string]string{
		"pnpm workspace":       {"pnpm-workspace.yaml": "packages: ['packages/*']"},
		"package workspaces":   {"package.json": `{"scripts":{"test":"vitest run"},"workspaces":["packages/*"]}`},
		"workspace file":       {"vitest.workspace.ts": "export default ['packages/*']", "packages/a/vitest.config.ts": "import { defineProject } from 'vitest/config'\nexport default defineProject({})"},
		"projects":             {"vitest.config.ts": cfg + "export default defineConfig({ test: { projects: ['packages/*'] } })"},
		"alias":                {"vite.config.ts": cfg + "export default defineConfig({ resolve: { alias: { '@core': fileURLToPath(new URL('./src', import.meta.url)) } } })"},
		"tsconfig paths":       {"tsconfig.json": `{"compilerOptions":{"paths":{"@core/*":["./src/*"]}}}`},
		"shared config":        {"vitest.config.ts": cfg + "import shared from './vitest.shared'\nexport default defineConfig(shared)", "vitest.shared.ts": "export default { test: { globals: true } }"},
		"unused project below": {"docs/vite.config.ts": "import vue from '@vitejs/plugin-vue'\nexport default { plugins: [vue()] }"},
		"no plugins":           {"vite.config.ts": "export default { plugins: [] }"},
		"react":                {"vite.config.ts": react + "export default { plugins: [react()] }"},
		"known plugins": {"vite.config.ts": react + "import vue from '@vitejs/plugin-vue'\nimport swc from '@vitejs/plugin-react-swc'\nimport tsconfigPaths from 'vite-tsconfig-paths'\n" +
			"export default defineConfig({\n  plugins: [\n    react({ babel: { plugins: ['x'] } }),\n    vue(),\n    swc(),\n    tsconfigPaths({ root: '..', projects: [\"a\"] }),\n  ],\n})"},
		"named import":          {"vite.config.ts": "import { default as r } from '@vitejs/plugin-react'\nimport { react } from '@vitejs/plugin-react'\nexport default { plugins: [react()] }"},
		"project with react":    {"vitest.config.ts": "export default { test: { projects: ['packages/*'] } }", "packages/a/vitest.config.ts": react + "export default { plugins: [react()] }"},
		"imported with plugins": {"vitest.config.ts": "import x from './more'\nexport default x", "more.ts": react + "export default { plugins: [react()] }"},
	} {
		dir := vitestTree(t, extra)
		t.Run(name, func(t *testing.T) { want(t, dir, "npm test", sel(t, dir, "npm test", "src/core.ts"), vitestNarrowed) })
	}
	for name, c := range map[string]struct{ extra map[string]string }{
		"preserveSymlinks":    {map[string]string{"vite.config.ts": "export default { resolve: { preserveSymlinks: true } }"}},
		"unknown plugin":      {map[string]string{"vite.config.ts": "import svelte from '@sveltejs/vite-plugin-svelte'\nexport default { plugins: [svelte()] }"}},
		"inline plugin":       {map[string]string{"vite.config.ts": "export default { plugins: [{ name: 'virt', load: () => null }] }"}},
		"plugin not imported": {map[string]string{"vite.config.ts": "const react = () => ({ name: 'mine' })\nexport default { plugins: [react()] }"}},
		"plugin spread":       {map[string]string{"vite.config.ts": react + "export default { plugins: [...[react()]] }"}},
		"plugin variable":     {map[string]string{"vite.config.ts": react + "const plugins = [react()]\nexport default { plugins }"}},
		"plugin condition":    {map[string]string{"vite.config.ts": react + "export default { plugins: [process.env.X && react()] }"}},
		"plugin then call":    {map[string]string{"vite.config.ts": react + "export default { plugins: [react().concat(x)] }"}},
		"external":            {map[string]string{"vitest.config.ts": "export default { server: { deps: { external: [/x/] } } }"}},
		"extends":             {map[string]string{"vitest.config.ts": "export default { extends: './base.config.ts' }"}},
		"preset":              {map[string]string{"vitest.config.ts": "import preset from '@acme/vitest-preset'\nexport default preset"}},
		"missing import":      {map[string]string{"vitest.config.ts": "import x from './nothere'\nexport default x"}},
		"imported plugins":    {map[string]string{"vitest.config.ts": "import x from './more'\nexport default x", "more.ts": "import p from 'vite-plugin-pages'\nexport default { plugins: [p()] }"}},
		"computed in config":  {map[string]string{"vitest.config.ts": "export default await import(process.env.CFG)"}},
		"projects above":      {map[string]string{"vitest.workspace.ts": "export default ['../shared/*']"}},
		"project with plugins": {map[string]string{"vitest.config.ts": "export default { test: { projects: ['packages/*'] } }",
			"packages/a/vitest.config.ts": "import pages from 'vite-plugin-pages'\nexport default { plugins: [pages()] }"}},
		"root and projects":   {map[string]string{"vitest.config.ts": "export default { root: 'src', test: { projects: ['packages/*'] } }"}},
		"computed root":       {map[string]string{"vitest.config.ts": "export default { root: process.env.ROOT }"}},
		"two roots":           {map[string]string{"vitest.config.ts": "export default { root: 'src', test: { root: 'lib' } }"}},
		"absolute root":       {map[string]string{"vitest.config.ts": "export default { root: '/srv/app' }"}},
		"root outside":        {map[string]string{"vitest.config.ts": "export default { root: '../..' }"}},
		"template root":       {map[string]string{"vitest.config.ts": "export default { root: `${dir}/src` }"}},
		"root in shared file": {map[string]string{"vitest.config.ts": "import shared from './shared'\nexport default shared", "shared.ts": "export default { root: 'src' }"}},
		"root in vite.config": {map[string]string{"vitest.config.ts": "export default {}", "vite.config.ts": "export default { root: 'src' }"}},
	} {
		dir := vitestTree(t, c.extra)
		t.Run(name, func(t *testing.T) { want(t, dir, "npm test", sel(t, dir, "npm test", "src/core.ts"), "") })
	}
	// A config in a folder above the check folder (vitest looks upwards).
	root := tree(t, map[string]string{"vite.config.ts": "import p from 'vite-plugin-pages'\nexport default { plugins: [p()] }", "app/package.json": `{"scripts":{"test":"vitest run"}}`,
		"app/src/core.ts": "x", "app/node_modules/vitest/package.json": vitestPkg})
	if p := Select(context.Background(), "npm test", "", Input{Root: root, Dir: filepath.Join(root, "app"), Files: []string{"app/src/core.ts"}}); !p.Full || !strings.Contains(p.Why, "vite-plugin-pages") && !strings.Contains(p.Why, "p()") {
		t.Errorf("config above: %+v", p)
	}
	for _, cmd := range []string{"vitest run --config elsewhere/custom.js", "vitest run --config=elsewhere/custom.js", "vitest run -c custom.ts", "vitest run --root=src", "vitest run --project=a"} {
		dir := vitestTree(t, nil)
		want(t, dir, cmd, sel(t, dir, cmd, "src/core.ts"), "")
	}
}

// vitest resolves the files `vitest related` gets against its root
// (3.2.7: with root 'src', ./src/core.ts found no test and ./core.ts
// found it), so rw names them relative to a root it reads.
func TestVitestRoot(t *testing.T) {
	url := "import { fileURLToPath } from 'node:url'\n"
	for name, c := range map[string]struct {
		cfg, want string
	}{
		"string":          {"export default { root: 'src' }", "./core.ts ./other.ts"},
		"dot string":      {"export default { root: './src/' }", "./core.ts ./other.ts"},
		"test root":       {"import { defineConfig } from 'vitest/config'\nexport default defineConfig({ test: { root: \"src\" } })", "./core.ts ./other.ts"},
		"dirname":         {"import path from 'node:path'\nexport default { root: path.resolve(__dirname, 'src') }", "./core.ts ./other.ts"},
		"join":            {"import { join } from 'node:path'\nexport default { root: join(import.meta.dirname, 'src') }", "./core.ts ./other.ts"},
		"url":             {url + "export default { root: fileURLToPath(new URL('./src', import.meta.url)) }", "./core.ts ./other.ts"},
		"config folder":   {"export default { root: __dirname }", "./src/core.ts ./src/other.ts"},
		"cwd":             {"export default { root: process.cwd() }", "./src/core.ts ./src/other.ts"},
		"file above root": {"export default { root: 'src/inner' }", "../core.ts ../other.ts"},
	} {
		dir := vitestTree(t, map[string]string{"vitest.config.ts": c.cfg, "src/other.ts": "export const o = 1\n", "src/inner/x.test.ts": "import '../core'\n"})
		t.Run(name, func(t *testing.T) {
			p := sel(t, dir, "npm test", "src/core.ts", "src/other.ts")
			want(t, dir, "npm test", p, "npx --no -- vitest related --run --passWithNoTests "+c.want)
		})
	}
	// A string root resolves against the folder vitest runs in, not the
	// config's; a config above the check folder.
	root := tree(t, map[string]string{"vite.config.ts": "export default { root: 'src' }", "app/package.json": `{"scripts":{"test":"vitest run"}}`,
		"app/src/core.ts": "x", "app/node_modules/vitest/package.json": vitestPkg})
	p := Select(context.Background(), "npm test", "", Input{Root: root, Dir: filepath.Join(root, "app"), Files: []string{"app/src/core.ts"}})
	want(t, filepath.Join(root, "app"), "npm test", p, "npx --no -- vitest related --run --passWithNoTests ./core.ts")
	root = tree(t, map[string]string{"vite.config.ts": "export default { root: __dirname }", "app/package.json": `{"scripts":{"test":"vitest run"}}`,
		"app/src/core.ts": "x", "app/node_modules/vitest/package.json": vitestPkg})
	p = Select(context.Background(), "npm test", "", Input{Root: root, Dir: filepath.Join(root, "app"), Files: []string{"app/src/core.ts"}})
	want(t, filepath.Join(root, "app"), "npm test", p, "npx --no -- vitest related --run --passWithNoTests ./app/src/core.ts")
	// The root vitest loads is the nearest config, vitest.config before
	// vite.config: one in a folder above it is not loaded.
	root = tree(t, map[string]string{"vite.config.ts": "export default { root: 'src' }", "app/vitest.config.ts": "export default {}", "app/package.json": `{"scripts":{"test":"vitest run"}}`,
		"app/src/core.ts": "x", "app/node_modules/vitest/package.json": vitestPkg})
	p = Select(context.Background(), "npm test", "", Input{Root: root, Dir: filepath.Join(root, "app"), Files: []string{"app/src/core.ts"}})
	want(t, filepath.Join(root, "app"), "npm test", p, "")
}

// Where vitest's related-test search does not look: Node loads require()d
// files and vi.importActual itself (vitest 3.2.7 found no test through
// either). rw names the files that load a changed file that way, and
// vitest finds their tests (and runs a test named there). A computed
// import cannot be followed, and the search stops at files under
// node_modules.
func TestVitestGraph(t *testing.T) {
	blind := map[string]string{
		"src/leaf.cjs":        "module.exports = { leaf: 5 }\n",
		"src/mid.cjs":         "const { leaf } = require('./leaf.cjs')\nmodule.exports = { mid: leaf }\n",
		"src/mid.test.ts":     "import m from './mid.cjs'\n",
		"src/other.cjs":       "module.exports = 1\n",
		"src/other.ts":        "export const other = 1\n",
		"src/actual.ts":       "export const actual = 1\n",
		"src/act.test.ts":     "const a = await vi.importActual('./actual')\n",
		"src/reached.ts":      "export const reached = 1\n",
		"src/through.cjs":     "module.exports = require('./uses.js')\n",
		"src/uses.js":         "export { reached } from './reached'\n",
		"src/through.test.ts": "const t = require('./through.cjs')\n",
		"src/lodash.cjs":      "module.exports = require('lodash')\n",
		"src/fs.cjs":          "module.exports = require('node:fs')\n",
	}
	blind["package.json"] = `{"scripts":{"test":"vitest run"},"dependencies":{"lodash":"4"}}`
	dir := vitestTree(t, blind)
	related := "npx --no -- vitest related --run --passWithNoTests "
	for f, w := range map[string]string{
		"src/leaf.cjs":   "./src/leaf.cjs ./src/mid.cjs",
		"src/actual.ts":  "./src/act.test.ts ./src/actual.ts",
		"src/reached.ts": "./src/reached.ts ./src/through.cjs ./src/through.test.ts",
		// What only import statements reach adds nothing.
		"src/other.ts": "./src/other.ts",
		"src/mid.cjs":  "./src/mid.cjs",
		"src/uses.js":  "./src/through.cjs ./src/through.test.ts ./src/uses.js",
	} {
		p := sel(t, dir, "npm test", f)
		want(t, dir, "npm test", p, related+w)
		if strings.Contains(w, " ") && f != "src/other.ts" && f != "src/mid.cjs" && !strings.Contains(p.Why, "require() or vi.importActual") {
			t.Errorf("%s: why %q", f, p.Why)
		}
	}
	// A file reached through require() that imports an alias or a
	// virtual module: rw cannot tell where it leads.
	for name, extra := range map[string]map[string]string{
		"alias":    {"src/uses.js": "export { reached } from '@/reached'\n"},
		"virtual":  {"src/uses.js": "import 'virtual:pages'\nexport { reached } from './reached'\n"},
		"outside":  {"src/uses.js": "export { reached } from '../../reached'\n"},
		"required": {"src/through.test.ts": "const t = require('../../x.cjs')\n"},
	} {
		b := maps.Clone(blind)
		maps.Copy(b, extra)
		dir := vitestTree(t, b)
		t.Run(name, func(t *testing.T) { want(t, dir, "npm test", sel(t, dir, "npm test", "src/reached.ts"), "") })
	}
	// With a root the added files are named relative to it too.
	b := maps.Clone(blind)
	b["vitest.config.ts"] = "export default { root: 'src' }"
	dir = vitestTree(t, b)
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/leaf.cjs"), related+"./leaf.cjs ./mid.cjs")

	dir = vitestTree(t, map[string]string{"src/load.ts": "export const load = (n: string) => import(`./locale/${n}.ts`)\n"})
	want(t, dir, "npm test", sel(t, dir, "npm test", "src/core.ts"), "")

	// A workspace package required by name is loaded by Node too.
	ws := map[string]string{
		"package.json":               `{"scripts":{"test":"vitest run"},"workspaces":["packages/*"]}`,
		"packages/core/package.json": `{"name":"@w/core"}`,
		"packages/core/src/a.ts":     "export const a = 1\n",
		"src/core.test.ts":           "const { a } = require('@w/core')\n",
	}
	dir = vitestTree(t, ws)
	want(t, dir, "npm test", sel(t, dir, "npm test", "packages/core/src/a.ts"), related+"./packages/core/src/a.ts ./src/core.test.ts")
	ws["src/core.test.ts"] = "import { a } from '@w/core'\n"
	dir = vitestTree(t, ws)
	want(t, dir, "npm test", sel(t, dir, "npm test", "packages/core/src/a.ts"), related+"./packages/core/src/a.ts")
	// Installed as a copy (npm install-links, pnpm injected): its files
	// are under node_modules, where vitest stops.
	ws["node_modules/@w/core/package.json"] = `{"name":"@w/core"}`
	dir = vitestTree(t, ws)
	p := sel(t, dir, "npm test", "packages/core/src/a.ts")
	want(t, dir, "npm test", p, "")
	if !strings.Contains(p.Why, "copy") {
		t.Errorf("copied: %q", p.Why)
	}
	// A link to the package's folder is what workspaces install.
	delete(ws, "node_modules/@w/core/package.json")
	dir = vitestTree(t, ws)
	os.MkdirAll(filepath.Join(dir, "node_modules", "@w"), 0o755)
	if err := os.Symlink(filepath.Join(dir, "packages", "core"), filepath.Join(dir, "node_modules", "@w", "core")); err != nil {
		t.Logf("no symlink: %v", err)
		return
	}
	want(t, dir, "npm test", sel(t, dir, "npm test", "packages/core/src/a.ts"), "npx --no -- vitest related --run --passWithNoTests ./packages/core/src/a.ts")
}

// Yarn Plug'n'Play installs no node_modules: the vitest version comes from
// the yarn.lock next to .pnp.cjs.
func TestVitestPlugNPlay(t *testing.T) {
	berry := func(versions ...string) string {
		s := "# This file is generated by running \"yarn install\" inside your project.\n\n__metadata:\n  version: 8\n  cacheKey: 10c0\n\n"
		for _, v := range versions {
			s += "\"vitest@npm:^" + v + "\":\n  version: " + v + "\n  resolution: \"vitest@npm:" + v + "\"\n  dependencies:\n    vite: \"npm:^5.0.0\"\n  languageName: node\n  linkType: hard\n\n"
		}
		return s + "\"vite@npm:^5.0.0\":\n  version: 5.4.0\n  languageName: node\n  linkType: hard\n"
	}
	narrowed := "yarn vitest related --run --passWithNoTests ./src/core.ts"
	for name, c := range map[string]struct {
		lock string
		want string
	}{
		"fixed":        {berry("3.2.7"), narrowed},
		"old":          {berry("1.1.0"), ""},
		"two versions": {berry("3.2.7", "1.0.0"), ""},
		"no vitest":    {berry(), ""},
		"no lock":      {"", ""},
	} {
		dir := vitestTree(t, map[string]string{"node_modules/vitest/package.json": "", ".pnp.cjs": "#!/usr/bin/env node\n", "yarn.lock": c.lock})
		t.Run(name, func(t *testing.T) { want(t, dir, "yarn test", sel(t, dir, "yarn test", "src/core.ts"), c.want) })
	}
	classic := "# yarn lockfile v1\n\n\nvitest@^1.0.0, \"vitest@>=1\":\n  version \"1.6.1\"\n  resolved \"https://x\"\n  dependencies:\n    vite \"^5.0.0\"\n\n\"@vitest/expect@1.6.1\":\n  version \"1.6.1\"\n"
	if got := yarnLockVersions([]byte(classic), "vitest"); !slices.Equal(got, []string{"1.6.1"}) {
		t.Errorf("classic: %q", got)
	}
	if got := yarnLockVersions([]byte(berry("3.2.7")), "vite"); !slices.Equal(got, []string{"5.4.0"}) {
		t.Errorf("berry vite: %q", got)
	}
}

// Workspaces where each package has its own test script and runner
// config, run with npm --workspaces or pnpm -r: rw runs the changed
// packages and those that depend on them.
func TestWorkspaceRuns(t *testing.T) {
	files := map[string]string{
		"package.json":                `{"name":"root","private":true,"workspaces":["packages/*","tools/*"],"devDependencies":{"vitest":"3"},"scripts":{"test":"npm test --workspaces"}}`,
		"pnpm-workspace.yaml":         "packages:\n  - 'packages/*'\n  - 'tools/*'\n  - '!tools/ignored'\n",
		"packages/core/package.json":  `{"name":"@w/core","scripts":{"test":"vitest run"}}`,
		"packages/core/src/a.ts":      "export const a = 1\n",
		"packages/core/src/a.test.ts": "import { a } from './a'\nimport { test } from 'vitest'\n",
		"packages/app/package.json":   `{"name":"@w/app","scripts":{"test":"vitest run"},"dependencies":{"@w/core":"workspace:*","react":"18"}}`,
		"packages/app/src/app.ts":     "import { a } from '@w/core'\nimport React from 'react'\nimport { readFileSync } from 'node:fs'\n",
		"packages/util/package.json":  `{"name":"@w/util","scripts":{"test":"jest"}}`,
		"packages/util/src/u.ts":      "export const u = 1\n",
		"packages/docs/package.json":  `{"name":"@w/docs"}`,
		"tools/cli/package.json":      `{"name":"cli","scripts":{"test":"vitest run"}}`,
		"tools/cli/main.ts":           "export {}\n",
		"tools/ignored/package.json":  `{"name":"ignored","scripts":{"test":"exit 1"}}`,
		"scripts/release.js":          "console.log(1)\n",
	}
	dir := tree(t, files)
	npm := "npm test --workspaces"
	want(t, dir, npm, sel(t, dir, npm, "packages/core/src/a.ts"), "npm test --workspace=@w/app --workspace=@w/core")
	want(t, dir, npm, sel(t, dir, npm, "packages/util/src/u.ts", "README.md"), "npm test --workspace=@w/util")
	want(t, dir, "npm run test -ws --if-present", sel(t, dir, "npm run test -ws --if-present", "packages/app/src/app.ts"), "npm run test --workspace=@w/app --if-present")
	want(t, dir, npm, sel(t, dir, npm, "README.md"), "-")
	// The root's test script is the recursive run.
	want(t, dir, "npm test", sel(t, dir, "npm test", "packages/util/src/u.ts"), "npm test --workspace=@w/util")
	for _, pn := range []string{"pnpm -r test", "pnpm --recursive run test", "pnpm -r --if-present test"} {
		want(t, dir, pn, sel(t, dir, pn, "packages/core/src/a.ts"), "pnpm --filter @w/app run test | pnpm --filter @w/core run test")
		want(t, dir, pn, sel(t, dir, pn, "tools/cli/main.ts"), "pnpm --filter cli run test")
		// A package without a test script runs nothing.
		want(t, dir, pn, sel(t, dir, pn, "packages/docs/x.ts"), "-")
	}
	pre, hint := Allowed(dir, "pnpm -r test", "")
	if !slices.Contains(pre, "pnpm --filter @w/core run test") || !strings.Contains(hint, "<package>") {
		t.Errorf("allowed %q hint %q", pre, hint)
	}
	for _, f := range []string{"scripts/release.js", "package.json", "packages/core/package.json", "pnpm-lock.yaml", "packages/core/vitest.config.ts", "tsconfig.base.json", "packages/core/node_modules/x/i.js"} {
		want(t, dir, npm, sel(t, dir, npm, f), "")
	}
	// A pnpm workspace that runs the root package too.
	files[".npmrc"] = "include-workspace-root=true\n"
	d := tree(t, files)
	want(t, d, "pnpm -r test", sel(t, d, "pnpm -r test", "packages/util/src/u.ts"), "")
	delete(files, ".npmrc")

	// Imports rw cannot place make it full; imports between packages add
	// the importer.
	for name, c := range map[string]struct {
		file, body, change, want string
	}{
		"alias":      {"packages/util/src/u.ts", "import x from '@/x'\n", "packages/util/src/u.ts", ""},
		"undeclared": {"packages/util/src/u.ts", "import l from 'lodash'\n", "packages/util/src/u.ts", ""},
		"root file":  {"packages/util/src/u.ts", "import r from '../../../scripts/release.js'\n", "packages/util/src/u.ts", ""},
		"computed":   {"packages/util/src/u.ts", "const m = await import(name)\n", "packages/util/src/u.ts", ""},
		"relative":   {"packages/util/src/u.ts", "import { a } from '../../core/src/a'\n", "packages/core/src/a.ts", "npm test --workspace=@w/app --workspace=@w/core --workspace=@w/util"},
		"by name":    {"tools/cli/main.ts", "import { u } from '@w/util'\n", "packages/util/src/u.ts", "npm test --workspace=@w/util --workspace=cli"},
		"hoisted":    {"packages/util/src/u.ts", "import { test } from 'vitest'\n", "packages/util/src/u.ts", "npm test --workspace=@w/util"},
	} {
		f := map[string]string{}
		for k, v := range files {
			f[k] = v
		}
		f[c.file] = c.body
		d := tree(t, f)
		t.Run(name, func(t *testing.T) { want(t, d, npm, sel(t, d, npm, c.change), c.want) })
	}
	// Every package affected: full.
	f := map[string]string{}
	for k, v := range files {
		f[k] = v
	}
	f["packages/util/src/u.ts"] = "import { a } from '@w/core'\n"
	f["tools/cli/main.ts"] = "import { a } from '@w/core'\n"
	d = tree(t, f)
	want(t, d, "pnpm -r test", sel(t, d, "pnpm -r test", "packages/core/src/a.ts"), "")
	// Without a workspace, or with a command rw does not know: full.
	plain := tree(t, map[string]string{"package.json": `{"name":"x"}`, "a.ts": "x"})
	want(t, plain, npm, sel(t, plain, npm, "a.ts"), "")
	want(t, plain, "pnpm -r test", sel(t, plain, "pnpm -r test", "a.ts"), "")
	want(t, dir, "pnpm -r --filter x test", sel(t, dir, "pnpm -r --filter x test", "packages/core/src/a.ts"), "")
}

func TestWorkspaceGlob(t *testing.T) {
	dir := tree(t, map[string]string{"a/package.json": "{}", "b/c/package.json": "{}", "b/c/d/package.json": "{}", "b/node_modules/x/package.json": "{}", "e/package.json": "{}", "e/test/package.json": "{}"})
	got, why := wsGlob(dir, []string{"a", "b/**", "e/**", "!**/test/**", "!**/test"})
	if why != "" || !slices.Equal(got, []string{"a", "b/c", "b/c/d", "e"}) {
		t.Errorf("got %q %q", got, why)
	}
	if _, why := wsGlob(dir, []string{"../x/*"}); why == "" {
		t.Error("a pattern above the folder")
	}
}

func TestGradleCustomLayoutsAndExternalDependencies(t *testing.T) {
	for _, script := range []string{
		"apply from: '../dependencies.gradle'",
		`apply(from = "../dependencies.gradle.kts")`,
		"dependencies { implementation(projects.core) }",
	} {
		dir := tree(t, map[string]string{
			"settings.gradle":    "include 'core', 'consumer', 'other'",
			"core/src/Core.java": "", "consumer/build.gradle": script,
			"dependencies.gradle": "dependencies { implementation project(':core') }",
		})
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/src/Core.java"), "")
	}
	for _, settings := range []string{
		"include 'core'; includeBuild('../shared')",
		"includeFlat 'core', 'consumer'",
	} {
		dir := tree(t, map[string]string{"settings.gradle": settings, "core/Core.java": ""})
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/Core.java"), "")
	}
}

// project(':x').projectDir = file('...') moves a project: rw reads the
// plain forms and runs in full on any other.
func TestGradleProjectDir(t *testing.T) {
	files := map[string]string{
		"core/build.gradle":           "",
		"core/src/A.java":             "",
		"components/web/build.gradle": "dependencies { implementation project(':core') }\n",
		"components/web/src/W.java":   "",
		"tools/cli/build.gradle":      "",
		"tools/cli/src/C.java":        "",
	}
	for _, settings := range []string{
		"include 'core', 'web', 'cli'\nproject(':web').projectDir = file('components/web')\nproject(':cli').projectDir = file('tools/cli')\n",
		"include(\"core\", \"web\", \"cli\")\nproject(\":web\").projectDir = File(rootDir, \"components/web\")\nproject(\":cli\").projectDir = file(\"tools/cli\") // the CLI\n",
		"include 'core', 'web', 'cli'\nproject(':web').projectDir = new File(settingsDir, 'components/web'); project(':cli').projectDir = file('./tools/cli/')\n",
		"rootProject.name = 'x'\r\ninclude 'core'\r\ninclude 'web'\r\ninclude 'cli'\r\nproject(':web').projectDir = file('components/web')\r\nproject(':cli').projectDir = file('tools/cli')\r\n",
	} {
		files["settings.gradle"] = settings
		dir := tree(t, files)
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/src/A.java"), "gradle :core:test :web:test")
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "components/web/src/W.java"), "gradle :web:test")
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "tools/cli/src/C.java"), "gradle :cli:test")
		// The default folder is no project's any more.
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "web/src/W.java"), "")
		pre, _ := Allowed(dir, "gradle test", "")
		if !slices.Equal(pre, []string{"gradle :cli:test", "gradle :core:test", "gradle :web:test"}) {
			t.Errorf("allowed %q", pre)
		}
	}
	for _, settings := range []string{
		"include 'core', 'web'\nif (x) {\n  project(':web').projectDir = file('components/web')\n}\n",
		"include 'core', 'web'\nproject(':web').projectDir = file('../web')\n",
		"include 'core', 'web'\nproject(':web').projectDir = file(\"$base/web\")\n",
		"include 'core', 'web'\nproject(':web').projectDir = file(webDir)\n",
		"include 'core', 'web'\nproject(':nope').projectDir = file('components/web')\n",
		"include 'core', 'web'\nproject(':web').projectDir = file('components/web')\nproject(':web').projectDir = file('x')\n",
		"include 'core', 'web'\nproject(':web').buildFileName = 'web.gradle'\n",
		"include 'core', 'web'\nrootProject.children.each { it.projectDir = file(\"components/${it.name}\") }\n",
		"include 'core', 'web'\nproject(':web').projectDir = file('components/web') + 'x'\n",
		"include 'core', 'web'\nproject(':web').projectDir = file('/abs/web')\n",
		"include 'core', 'web'\n// project(':web').projectDir = file('components/web')\n",
	} {
		files["settings.gradle"] = settings
		dir := tree(t, files)
		want(t, dir, "gradle test", sel(t, dir, "gradle test", "core/src/A.java"), "")
	}
}
