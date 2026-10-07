# Affected tests on real projects (6 Oct 2026)

Until now only the Go selection of `verify.affected` had run on a real
project (this repo). This run covers the other seven: jest, vitest, pytest,
cargo, dotnet, Maven and Gradle, each on a real open-source project.

## Method

The same as the Go measurement in PR #36:

- Take recent bug-fix commits that come with a test.
- Check out the commit, put its non-test files back to the parent (the fix
  is undone, its test stays).
- The changed files are the commit's own diff, as if an agent had written
  it in a fix round.
- Run rw's selection (`affected.Select`, the code `rw run` calls) on that
  tree. Then run the full command and the narrowed command one after the
  other, both through the shell the way rw runs checks.
- Compare the failing tests. "Same" means the two runs fail the same
  tests (or the same build step).

Wall times are for one run each, measured around the command only. They
are noisy: the machine ran other agents' work at the same time. The
selection itself took under 0.4 s, except jest, which now asks jest for
its configuration (0.7-2.9 s, see below).

Where: Windows 11 for jest, pytest, cargo, dotnet and Maven (Docker was not
running at first). The later runs went into Docker containers limited to
2 CPUs and 4 GB: Gradle (`eclipse-temurin:21-jdk`) and the vitest rerun
(`node:24`). All suites ran one at a time.

## Results

| Ecosystem | Project | Commits | Same failures | Wall time, full / narrowed |
|---|---|---|---|---|
| jest | react-hook-form (jest 30.4) | 6 | 6 of 6 | 5.5-6.1 s / 4.7-5.9 s |
| jest | dayjs (jest 22.4) | 1 + a probe | yes, but a silent miss (fixed) | 3.7 s / 1.8 s |
| vitest | superjson (vitest 0.34.6) | 5 | 5 of 5, but misses (fixed) | 1.4-4.1 s / 1.8-2.3 s |
| vitest | superjson with vitest 3.2.7 | 4 | 4 of 4 | 16.5-18.2 s / 11.0-13.7 s |
| pytest | pypa/packaging (pytest 9.1) | 6 | 6 of 6 | 22.5-48.6 s / 0.6-9.3 s |
| cargo | servo/rust-url (cargo 1.98) | 5 | 5 of 5 | 2.3-9.7 s / 1.1-2.3 s |
| dotnet | Spectre.Console (SDK 10.0.401) | 5 | 5 of 5 | 18.0-33.4 s / 15.9-20.3 s |
| Maven | jackson-modules-java8 (Maven 3.9.11, JDK 21) | 5 | 5 of 5 | 16.7-42.3 s / 6.3-17.9 s |
| Gradle | lsp4j (Gradle 8.6, JDK 21) | 3 | 3 of 3 | 90-141 s / 48-74 s |

Every failure of the full run also failed the narrowed run. Three
selection bugs and one command-line bug turned up; they are fixed (see
"Bugs found").

### jest: react-hook-form

`npm test` (the script is `jest --config ./scripts/jest/jest.config.js`,
two jest projects with `roots: ['<rootDir>/src']`). 127 test suites.

| Commit | Selection | Same | Full | Narrowed |
|---|---|---|---|---|
| 000faaf useFieldArray | 28 of 127 suites | yes (1 test) | 5.8 s | 4.7 s |
| 7838f08 edited before ready | full: the fix added `src/utils/isEdited.ts`, which is gone once the fix is undone | yes (3) | 5.8 s | 5.9 s |
| a814cbf reset dirty | 59 of 127 | yes (1) | 5.7 s | 5.9 s |
| 0104fce cloneObject | 59 of 125 | yes (2) | 6.0 s | 6.0 s |
| c12546c validateField | 58 of 124 | yes (2) | 6.1 s | 5.4 s |
| 72d617f useWatch | 36 of 127 | yes (1) | 5.5 s | 5.9 s |

With jest's cache warm, a run is mostly start-up: narrowing saves little
wall time here. The first, cold run took 19.2 s in full and 6.0 s narrowed.

### jest: dayjs (the silent miss)

dayjs sets `roots: ["test"]`; luxon does the same. jest's
`--findRelatedTests` only knows files under its roots. A changed
`src/plugin/timezone/index.js` had no related test:
`npx jest --findRelatedTests --passWithNoTests ./src/plugin/timezone/index.js`
printed "No tests found" and exited 0. Three test suites import that file.
A fix round that changed only the source would have passed its narrowed
run without running one test.

Commit dad46e6 (timezone plugin) was still caught, because its test file
was in the change: 2 tests failed in both runs (3.7 s full, 1.8 s
narrowed). After the fix rw runs dayjs and luxon in full when a source
file changes.

### vitest: superjson

`npm test` (`vitest run`). 7 test files.

| Commit | 0.34.6: selection | 3.2.7: selection | Same | 3.2.7 full / narrowed |
|---|---|---|---|---|
| aaa65e3 BigInt64Array | 2 of 7 files | 5 of 7 | yes | 18.2 s / 13.7 s |
| faf164b Error cause | 1 of 7 | 3 of 7 | yes | 16.5 s / 11.0 s |
| bccc082 NaN in typed arrays | 1 of 7 | not run | yes (0.34.6) | - |
| 4054f3f escape mapping | 4 of 7 | 4 of 7 | yes | 16.9 s / 12.5 s |
| 5debdda nth key | 2 of 7 | 4 of 7 | yes | 16.7 s / 12.4 s |

The project pins vitest 0.34.6. Its `vitest related` misses tests that
reach a changed file through other files: for `src/transformer.ts` alone it
found no test (there are three). The table's runs still matched, because
the test that catches each fix was itself in the change. Trying versions:
0.34.6, 1.0.0, 1.1.0, 1.2.0 and 1.2.1 find nothing; 1.2.2, 1.3.0, 1.6.1,
2.0.0 and 3.2.7 find all three.

For the 3.2.7 column, vitest was swapped in (`npm i --no-save`) and run in
Docker. Under 3.2.7, six old tests in `index.test.ts` fail in both runs;
they are counted in "same".

### pytest: pypa/packaging

`python -m pytest -q`. 31 test files; property tests are deselected by the
project's settings in both runs.

| Commit | Selection | Same | Full | Narrowed |
|---|---|---|---|---|
| 0625596 wheel build tag | 23 test files | yes (2 tests) | 22.5 s | 4.9 s |
| 2602dd0 URL token | 6 | yes (6) | 23.6 s | 3.5 s |
| 55cbf1b unbounded ranges | 22 | yes (4) | 25.7 s | 5.7 s |
| 4e79787 empty platforms | 24 | yes (3) | 48.6 s | 9.3 s |
| 2d873eb metadata folding | 1 | yes (12) | 46.5 s | 0.8 s |
| 2c87a33 LicenseRef case | 2 | yes (3) | 23.5 s | 0.6 s |

The slow tests (tags, manylinux, musllinux) sit in leaf modules, so most
narrowed runs skip them. 2d873eb also changed `CHANGELOG.rst`, which rw
ignores as a doc.

### cargo: servo/rust-url

`cargo test` in a virtual workspace of 6 crates. The changed files were
touched before each run, so each run rebuilt them.

| Commit | Selection | Same | Full | Narrowed |
|---|---|---|---|---|
| 00a6ce5 file: drive letters | url, url_debug_tests | yes (3 WPT cases) | 2.3 s | 1.3 s |
| 25137be caret in path | url, url_debug_tests | yes (1) | 2.5 s | 1.1 s |
| 7eccac9 issue 974 | url, url_debug_tests | yes (1) | 7.6 s | 1.3 s |
| 467ef63 xn--55555577 | idna, url, url_debug_tests | yes (1) | 9.7 s | 2.3 s |
| e654efb data URL % | data-url | yes (5) | 5.2 s | 0.9 s |

`cargo test` stops at the first failing test binary, so both runs report
the failures of the first crate that fails. 9771ab5 also changed
`.github/workflows/main.yml`: rw ran in full, as it should (a file outside
every crate). It is left out: at that commit the idna tests fail on their
own.

### dotnet: Spectre.Console

`dotnet test` in `src/` (one solution, `Spectre.Console.slnx`, with two test
projects that each target net8.0, net9.0 and net10.0).

| Commit | Selection | Same | Full | Narrowed |
|---|---|---|---|---|
| a5d49bc selection prompt default | Spectre.Console.Tests (1 of 2) | yes (3 tests) | 26.9 s | 18.7 s |
| 3ba1023 table measurer loop | 1 of 2 | yes (1) | 33.4 s | 20.3 s |
| bbf51e4 grid expansion | 1 of 2 | yes (1) | 18.0 s | 16.5 s |
| a334da9 markup escaping | 1 of 2 | yes (1) | 20.4 s | 17.3 s |
| 33bcbc4 SplitLines | 1 of 2 | yes (1) | 23.6 s | 15.9 s |

The other test project (Spectre.Console.Ansi.Tests) is small, so the build
takes most of the time either way.

### Maven: jackson-modules-java8

`mvn -B test`, three modules (parameter-names, datatypes, datetime).

| Commit | Selection | Same | Full | Narrowed |
|---|---|---|---|---|
| 236faaf negative instants | `-pl datetime -am -amd` | yes (1 test) | 16.7 s | 13.5 s |
| 9bc7cbf Optional (#372) | `-pl datatypes -am -amd` | yes (1) | 21.0 s | 6.3 s |
| 6283ad1 one-based month | `-pl datetime -am -amd` | yes (2) | 42.3 s | 12.7 s |
| adf848b negative timestamp strings | `-pl datetime -am -amd` | yes (2) | 21.6 s | 17.9 s |
| ec2fa91 OffsetDateTime formatter | `-pl datetime -am -amd` | yes: the test does not compile without the fix, in both runs | 27.2 s | 17.0 s |

Two adjustments, both outside rw's selection:
- The old commits name a parent snapshot (`jackson-base 2.21.0-SNAPSHOT`)
  that no repository has any more. The root pom's parent was switched to the
  release of the same version.
- Most of these commits also edit `release-notes/VERSION-2.x`. With it in
  the change rw runs in full (a file of the root module), which is safe but
  says nothing. The runs above use the commits' code and test files.

af80336 is left out: at that commit the build fails on an old plugin.

### Gradle: lsp4j

`./gradlew test` in Docker (2 CPUs, 4 GB). Each run started a fresh Gradle
(no daemon), with the earlier runs' test results deleted, so no test task
was up to date.

| Commit | Selection | Same | Full | Narrowed |
|---|---|---|---|---|
| 57eeaa4 inline value adapter | `:org.eclipse.lsp4j:test` (1 of 6 projects) | yes (3 tests) | 90.1 s | 52.3 s |
| ec0979b resource operation adapter | `:org.eclipse.lsp4j:test` (1 of 7) | yes (2) | 129.4 s | 48.3 s |
| 4b6d14b message producer | full: every project uses jsonrpc | yes (1) | 141.1 s | 74.0 s |

## Bugs found

All are fixed, each with a regression test from a small file tree that
fails without the fix. An independent review of the fixes found more ways
for jest and Gradle to miss tests; those are fixed too (marked "review").

1. **jest's index (silent miss).** `--findRelatedTests` only knows the
   files jest indexes: under its `roots`, with an extension in
   `moduleFileExtensions`, not matching `modulePathIgnorePatterns`. It drops
   any other changed file, and `--passWithNoTests` makes "no tests" a pass.
   Seen on dayjs; luxon has the same setting. rw now asks jest:
   `<runner> <the script's flags> --showConfig` (in the sandbox when it is
   on; jest's `cwd` maps the container's paths). It narrows only when every
   jest project indexes every changed file. Review: jest also follows
   imports only through indexed files, so rw runs in full when an indexed
   file imports, by a relative path, a file jest does not index (a test
   that imports `../index.js` outside `roots: ["<rootDir>/src"]`). And below
   the check folder jest compares paths case-sensitively (`<rootDir>/Src`
   does not hold `src/a.js`). With react-hook-form this costs 0.7-0.9 s
   warm, 2.1 s cold; luxon 2.9 s cold. `TestSelectJestRoots`,
   `TestSelectJestIndex`, `TestJestShowConfigCommand`.
2. **vitest before 1.2.2 (miss).** Older `vitest related` misses tests
   that reach a changed file through other files. rw reads the vitest
   version from `node_modules` (up to the repo's top folder) and runs in
   full below 1.2.2 or when it cannot tell. `TestSelectVitestVersion`.
3. **Gradle includes over several lines (miss).** `include(\n "a",\n "b"\n)`
   and `include 'a',\n 'b'` are common (mockito, ArchUnit, Hamcrest).
   rw read only the first line, so it did not know the other projects: a
   change in a project ran its tests but not those of the projects using
   it. In those three projects another check made rw run in full anyway, so
   no wrong run was seen on a real project; the regression test builds one.
   Includes are now read across lines. Includes rw cannot read (names from
   variables, includes inside an `if` block such as mockito's Android
   projects, which exist only with an SDK) run in full: a task of a project
   that is not there fails the run. Review: rw now reads the settings
   script with its comments, strings and blocks, so it also finds an
   include after a byte order mark or after another statement on the line
   (`include(":a"); include(":b")`), and an include inside a block runs in
   full even when it starts at the line's beginning. An include rw sees but
   did not read (in a comment, a string or after a dot) runs in full.
   `TestSelectGradleIncludeLines`.
4. **npx and flags.** After one of its own flags, `npx` reads every later
   flag as npm's: `npx --no jest --config x --showConfig` ran jest without
   either flag (npm 11.17). rw's narrowed vitest command only worked because
   `related` comes first. Both now use `npx --no -- <runner>`.

## Monorepos, aliases, Plug'n'Play and `projectDir` (follow-up, 6 Oct)

The first version of this record left four layouts open. Each one now
either narrows, with the case checked against the real runner, or runs in
full with a reason in the activity log. Fixture tests:
`internal/affected/layouts_test.go`.

How it was checked: small workspaces built locally (Windows 11, npm 11.17,
node 24) with vitest 3.2.7 and 1.2.2, jest 29.4.3 and pnpm 10.34.6. First
each runner's own related-test search was probed for what it finds and
what it misses. Then a bug was put into one file and rw's selection ran
with the full and the narrowed command, as in the method above.

**What the runners do.** `vitest related` (1.2.2 and 3.2.7) found every
related test through `test.projects`, a `vitest.workspace` file, workspace
packages imported by name (npm links them) and `resolve.alias`. It found
nothing through a config `root` or `test.root` (the changed paths resolve
against it), a plugin's virtual module, `resolve.preserveSymlinks`, a
`require()` chain (Node loads those files, not vite), `vi.importActual`, or
a computed `import()`. jest's `--findRelatedTests` with `projects` found
no test of one project that reached another project's changed file: by a
relative path, by the package's name, or by a `moduleNameMapper` alias.
Each project searches only its own index.

| Layout | Now | Real run |
|---|---|---|
| vitest `test.projects`, `vitest.workspace`, npm/pnpm workspaces, `resolve.alias`, tsconfig `paths` | Narrows with `vitest related`. rw reads every config vitest may load (the check folder and the folders above it, since vitest looks upwards; with projects, the configs below too) and the files they import. It runs in full on `preserveSymlinks`, plugins other than react, react-swc, vue and tsconfig-paths, `external`, `extends: '<file>'`, a config import it does not read (a shared preset package), a project list that points above its folder, and `--config`, `--project` or `--root` on the command line. | 3.2.7, projects + alias + import by package name: bug in `core/src/inner.ts`, full 3 failing, narrowed the same 3 (1 of 4 test files left out). Same on 1.2.2 with `vitest.workspace.ts`. In a package folder (`packages/app`, root config above): 1 failing, same. |
| vitest and `require()` / `vi.importActual` / computed imports | rw reads the imports of every JS/TS file in the check folder. It runs in full when any file imports a computed path. (Until 7 Oct it also ran in full when a changed file was reachable from a `require()` or `vi.importActual` target; see the follow-up below.) It also runs in full when a workspace package is installed as a copy under `node_modules` (npm `install-links`, pnpm injected packages), where vitest stops; it follows symlinks and Windows junctions to tell. | Plan on the real workspace: full for a `.cjs` file reached through `require()`, full with `preserveSymlinks`; narrowed again without them. |
| Yarn Plug'n'Play | With `.pnp.cjs` (or `.pnp.js`) and no `node_modules/vitest`, rw reads every vitest version in the `yarn.lock` next to it (Berry or classic). It narrows only if all are 1.2.2 or later, and runs in full without a lockfile or a vitest entry. | Not run: Yarn is not installed here and was not downloaded. The lockfile format follows Yarn 4; the fixture tests use it. |
| Several jest projects, `moduleNameMapper` | A changed file needs one project that sees it, not all. For each project, every file it sees may import only files it sees: relative imports, workspace packages by name (all their files), and `moduleNameMapper` targets. rw applies the first matching pattern with `$1` like jest, and allows targets under `node_modules`. It runs in full on a pattern Go cannot read, a target outside the check folder, a computed import, `resolver`, `modulePaths` or custom `moduleDirectories`. | jest 29.4.3, two projects. With cross-project imports (by name, alias and relative path): full, and the real run had missed all three tests. Independent projects: bug in core, full 1 failing, narrowed the same 1. An app alias `^@app/(.*)$`: 1 failing, same. |
| Workspaces where each package has its own runner config: `npm test --workspaces` (`-ws`, `--if-present`), `pnpm -r test`, or a root test script that is one of them | Package granularity. rw runs the changed packages and those that depend on them: `npm test --workspace=a --workspace=b`, or `pnpm --filter a run test` per package. Dependencies come from `package.json`, relative imports between packages, and imports of a package's name. It runs in full on a file of the workspace root, `package.json`, lockfiles, `.npmrc`, tsconfig, `*.config.*` or preset files, and on a bare import that is neither a declared dependency (of the package or the root) nor a workspace package. Such an import may be an alias into another package. It also runs in full on `include-workspace-root` with pnpm. Yarn's `workspaces foreach` and turbo/nx still run in full. | npm, 3 packages: bug in app, `--workspace=@vw/app`, 1 failing in both runs. Bug in core: app + core, 2 failing in both. pnpm 10.34.6 (`Scope: 3 of 4 workspace projects`): the same 2 failing with `pnpm --filter`. |
| Gradle `project(':x').projectDir = file('dir')` | rw reads the plain forms: `file('dir')`, `File(rootDir, "dir")`, `new File(settingsDir, 'dir')`, Groovy or Kotlin, at the top level of the settings script. They map that folder to the project. It runs in full on one inside a block, a folder outside the build, a computed folder, a project that is not included, a second assignment, any other `project(...)` call in the settings (`buildFileName`, names) and `rootProject.children.each`. | Not run on a real Gradle build (Docker was busy with another benchmark); fixture tests only. |

Two more fixes came out of the probes. A vitest config `root` had made
narrowed runs find no test, and `--passWithNoTests` made that a pass. Earlier
code did not check for it, and an interim fallback caught it only by
accident. jest's `moduleNameMapper` used to make every jest run full.

## Config `root`, plugins and `require()` chains (follow-up, 7 Oct)

The three vitest layouts that ran in full above now narrow. Each was
probed against the real runner first, then checked with a bug in one file,
full and narrowed. Windows 11, node 24, vitest 3.2.7 (vite 7) and 1.2.2.

**What vitest does.**
- `vitest related` resolves the files it gets against the config's root.
  With `root: 'src'`, `./src/core.ts` and the absolute path found no test.
  `./core.ts` found it. A string root is resolved against the folder vitest
  runs in, not against the config's folder.
- It also runs a test file named on the command line, and the tests that
  import a named file. `related b.cjs` found no test for `a.test.ts`, which
  imports `a.cjs`, which `require()`s `b.cjs`. `related b.cjs a.cjs` found it.
- With `@vitejs/plugin-react`, `@vitejs/plugin-react-swc`,
  `@vitejs/plugin-vue` (`<script setup>` and plain `<script>`) and
  `vite-tsconfig-paths`, it found the test that reaches a changed file
  through a `.tsx`, a `.vue` script or a tsconfig alias. An inline plugin
  that serves a virtual module re-exporting the changed file hid its test.

**What rw does now.**
- **Root.** rw reads the root of the config vitest loads (the nearest
  `vitest.config.*`, then `vite.config.*`). Accepted forms: a string,
  `path.resolve`/`join(__dirname | import.meta.dirname, '...')`,
  `__dirname`, `fileURLToPath(new URL('...', import.meta.url))` and
  `process.cwd()`. rw names the files relative to that root.
- **Plugins.** Every `plugins: [...]` list must call one of those four
  plugins, imported by name; their options are not read.
- **`require()` chains.** rw adds every file that loads a changed file
  (directly or through other files) with `require()` or `vi.importActual`,
  so vitest finds its tests. At most 100 such files are added.

These cases still run in full:
- a root in another file, a computed or absolute root, two roots, or a
  root together with projects;
- any other plugin, a spread or variable plugin list;
- a file reached through `require()` that imports an alias, a virtual
  module or a file outside the check folder.

| Case | vitest 3.2.7: full / narrowed | vitest 1.2.2: full / narrowed |
|---|---|---|
| Bug in `b.cjs`, required by `a.cjs` (imported by a test) and by a test through `createRequire` | 2 failed of 4 / 2 failed of 2 (`related ./src/a.cjs ./src/b.cjs ./src/direct.test.ts`) | 2 failed / 2 failed |
| Bug in `actual.ts`, loaded by a test with `vi.importActual` | 1 failed of 4 / 1 failed of 1 | 1 failed of 4 / 1 failed of 1 |
| Plain import (control) | 1 failed of 4 / 1 failed of 1 | not rerun |
| `root: 'src'`, bug in `src/core.ts` | 1 failed of 3 / 1 failed of 1 (`related ./core.ts`) | 1 failed of 3 / 1 failed of 1 |
| `root: 'src'`, bug in `leaf.cjs` behind `require()` | 1 failed of 3 / 1 failed of 1 (`related ./leaf.cjs ./mid.cjs`) | 1 failed of 3 / 1 failed of 1 |
| react + tsconfig-paths plugins, bug in `src/lib/deep.ts` reached through `@/lib/deep` in a `.tsx` | 1 failed of 2 / 1 failed of 1 | 1 failed of 2 / 1 failed of 1 |

These were small local workspaces, not published projects. Fixture tests:
`TestVitestLayouts`, `TestVitestRoot` and `TestVitestGraph` in
`internal/affected/layouts_test.go`.

## Not run here

Everything above ran, except where a row says otherwise. Limits:

The later [7 October PnP and Gradle checks](2026-10-07-affected-layouts.md)
close the real-install gaps in the historical table above. They also found
and fixed false passes with virtual PnP workspaces and unread Gradle
dependencies. The limits below describe this earlier run only.

- jest and pytest ran on Windows only; vitest on Windows (0.34.6, and
  1.2.2/3.2.7 in the follow-up) and Linux (3.2.7); Gradle on Linux only.
- One project per ecosystem, except jest. The monorepo follow-up used small
  local workspaces, not published monorepos.
- Not covered: Yarn Plug'n'Play against a real Yarn install, Gradle
  `projectDir` against a real Gradle build, Yarn `workspaces foreach`,
  turbo and nx (full runs), and jest `moduleDirectories`/`modulePaths`/
  custom resolvers (full runs). vitest configs with plugins other than
  react, react-swc, vue and tsconfig-paths still run in full: their
  virtual modules cannot be followed from outside vite.
- The superjson vitest 3.2.7 runs above predate the config checks. A
  superjson config with plugins or an unread import would now run in full.
- No agent wrote these changes: the changed files are the commits' own.
