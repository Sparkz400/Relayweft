# Selective affected-test validation, 7 October 2026

This extends the [earlier layout checks](2026-10-06-affected-real-projects.md).
The real runners use disposable local workspaces with a passing baseline,
then a deliberately broken dependency. These are small integration projects,
not published monorepos or agent-written fixes.

## Implemented

- Exclude Yarn's generated `.pnp.cjs`, `.pnp.js` and `.pnp.loader.mjs`
  from the application import graph. Previously a real Vitest PnP install
  ran in full because rw tried to interpret its generated loader.
- Run in full when Yarn's loader, data, configuration, or anything under
  `.yarn/` changes, including patches and plugins.
- Run Jest and Vitest in full when a local PnP workspace declares peer
  dependencies. Yarn gives it a virtual path; both runners missed a test
  importing that workspace when given the changed file's physical path.
  With `--passWithNoTests`, both incorrectly exited successfully. The full
  run failed the expected assertion. rw now explains this fallback.
- Read literal Gradle project dependencies with whitespace before the
  parenthesis and named `path:` / `path =` arguments. Previously these
  could omit a consumer's tests.
- Run in full for computed, interpolated, unknown or otherwise unread
  Gradle project references, duplicate project folders, init scripts and
  attached project/build/settings path options. No change is made to the
  configured command on fallback.
- Reject malformed package names, scripts and dependency maps. Their
  parse errors previously discarded information needed for selection;
  they now cause a full run. Windows project-folder case collisions are
  covered by a separate regression.

## Real runner evidence

Windows 11; Node 24.19.0; Yarn 4.6.0; Jest 29.7.0; Vitest 3.2.7;
Gradle 8.14; Microsoft JDK 21.0.12.1. Package versions are pinned in the
checked-in [live tests](../../internal/affected/live_test.go).

| Layout | Full run with bug | Selected run with bug |
|---|---|---|
| Yarn PnP, Jest, plain workspace package | Consumer assertion fails; unrelated suite passes | Same assertion fails; unrelated suite omitted |
| Yarn PnP, Vitest, plain workspace package | Consumer assertion fails; unrelated suite passes | Same assertion fails; unrelated suite omitted |
| Yarn PnP, Jest, workspace with a peer dependency | Consumer assertion fails | Full-suite fallback catches the assertion; the pre-fix narrowed probe passed with no tests |
| Yarn PnP, Vitest, workspace with a peer dependency | Consumer assertion fails | Full-suite fallback catches the assertion; the pre-fix narrowed probe passed with no tests |
| Gradle Groovy settings, `file(...)` and `new File(settingsDir, ...)` project folders; `project (path: ':core')` dependency | `ConsumerTest.detectsCoreBug` fails; unrelated task runs | `:consumer:test :core:test` catches the same failure; unrelated task omitted |
| Gradle Kotlin settings, `file(...)` and `File(rootDir, ...)` folders; Kotlin `project (path = ":core")` dependency | Same consumer failure; unrelated task runs | Same failure in the selected tasks; unrelated task omitted |
| Gradle Groovy computed dependency, `project(target)` | Consumer assertion fails | Full-suite fallback catches the assertion |

The plain PnP checks also restore the dependency and require the narrowed
run to pass. Every Gradle run uses `--rerun-tasks --continue`, so cached
results and early task cancellation cannot hide the unrelated control.
The core Gradle project intentionally has no tests: missing its consumer
would produce a false pass.

## Automated checks

- The affected package and the orchestrator's affected-first, disabled,
  cancellation and command-allowlist tests pass on Windows.
- The new loader, virtual-workspace and Gradle regressions were also run
  against the saved pre-change source using a Go overlay. They failed
  before the fixes and pass with them.
- `go test -race ./internal/affected -count=1 -timeout 5m` passes in the
  Linux Go 1.26 container. Native Windows checks use Go 1.27.0.
- `go vet ./...` passes for Windows and with `GOOS=linux` and
  `GOOS=darwin`. Cross-vet does not establish native runtime behavior.
- `go build ./cmd/rw` passes, with the executable written to the external
  validation folder rather than replacing the workspace's `rw.exe`.
- `golangci-lint run ./internal/affected` reports zero issues in the
  Linux v2.14.0 container.
- The broader Windows `go test -p 4 ./... -count=1 -timeout 20m` did
  **not** complete successfully: all other packages passed, but the
  orchestrator package reached its aggregate 20-minute timeout while
  `TestResolveReviewedAsConflict/accepted` had been running for seven
  seconds. This is not a full-suite pass, and does not establish that
  the interrupted test itself hung. The CLI package passed in 970 seconds.
  `go test ./internal/orchestrator -run '^TestResolveReviewedAsConflict$'
  -count=1 -timeout 5m` then passed separately in 16.9 seconds.

## Repeat the checks

The normal suite uses fixtures and makes no package installs. Run:

```text
go test ./internal/affected -count=1
```

The real checks are opt-in and download packages into disposable test
workspaces (the tools may reuse their user caches). Put Yarn 4.6.0 on
`PATH`, and Gradle 8.14 with JDK 21 on `PATH` / `JAVA_HOME`, then in
PowerShell:

```powershell
$env:RW_AFFECTED_LIVE_YARN = '1'
$env:RW_AFFECTED_LIVE_GRADLE = '1'
go test ./internal/affected -run '^TestLive' -v -count=1 -timeout 15m
```

The flags can be enabled separately. A temporary `yarn.cmd` containing
`@corepack yarn@4.6.0 %*` also works on Windows. The tests assert the
baseline, failure identity, fallback or narrowing, and command allowlist;
an install failure or runner startup failure does not count as validation.

## Nx, Turbo and further configurations

Automatic Nx/Turbo narrowing remains deferred, with regression coverage
for direct commands and package scripts. Their task semantics need their
own real-run comparisons before reuse of a package dependency graph.

- [Nx affected](https://nx.dev/docs/features/ci-features/affected) uses
  its project graph. A future adapter should pass rw's exact changed
  files to the native graph query, verify that every changed file and
  configured target is accounted for, and retain full execution on a
  graph error or unknown schema. Replacing a configured `run-many` target
  must also preserve its configuration and dependency tasks.
- [Turbo run](https://turborepo.dev/docs/reference/run) supports native
  task dry-runs. Its default `--affected` compares Git revisions, which
  is not necessarily rw's fix-round file set. A future adapter also needs
  to account for explicit task edges and
  [global inputs](https://turborepo.dev/docs/reference/configuration#globaldependencies),
  rather than filtering solely by package ownership.
- Custom Jest resolvers, unknown Vitest plugins and configuration forms,
  Yarn `workspaces foreach`, and dynamic Gradle layouts retain the full
  command. Explicit `verify.affected_commands` templates remain available
  for a repository's own validated selection command.

Not validated here: published monorepos, native macOS runner execution,
other Yarn/Gradle versions, Nx/Turbo native narrowing, or exhaustive
combinations of plugins, peer dependencies and composite builds.
