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
its configuration (0.8-2.9 s, see below).

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

All four are fixed, each with a regression test from a small file tree
that fails without the fix.

1. **jest's roots (silent miss).** `--findRelatedTests` drops a changed
   file that is outside jest's `roots` or matches
   `modulePathIgnorePatterns`, and `--passWithNoTests` makes "no tests" a
   pass. Seen on dayjs; luxon has the same setting. rw now asks jest:
   `<runner> <the script's flags> --showConfig` (in the sandbox when it is
   on; jest's `cwd` maps the container's paths). It narrows only when every
   jest project sees every changed file. This costs 0.8-1.3 s with a warm
   disk cache, 2.9 s cold. `TestSelectJestRoots`, `TestJestShowConfigCommand`.
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
   that is not there fails the run. `TestSelectGradleIncludeLines`.
4. **npx and flags.** After one of its own flags, `npx` reads every later
   flag as npm's: `npx --no jest --config x --showConfig` ran jest without
   either flag (npm 11.17). rw's narrowed vitest command only worked because
   `related` comes first. Both now use `npx --no -- <runner>`.

## Not run here

Everything above ran. Limits:
- jest and pytest ran on Windows only; vitest on Windows (0.34.6) and Linux
  (3.2.7); Gradle on Linux only.
- One project per ecosystem, except jest. Monorepos with several jest or
  vitest projects, Yarn Plug'n'Play (rw cannot find the vitest version and
  runs in full) and Gradle builds with `projectDir` are not covered.
- No agent wrote these changes: the changed files are the commits' own.
