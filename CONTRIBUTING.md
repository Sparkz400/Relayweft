# Contributing to Relayweft

Relayweft (`rw`) is a Go CLI that runs Codex, Claude Code and other agent
CLIs on your machine and applies their changes to your repository. A bug
here can cost someone their work, their quota or a frozen PC. So the rules
below come first, and every change follows them.

`rw` reads this file and the pull request template as this repo's
conventions when it plans and reviews work here (ROADMAP 4.20). Keep both
short, clear and imperative.

## Rules

These are the standing quality rules for every change.

- **A bug fix needs a test.** Add a regression test that fails without the fix. Check that it does.
- **Windows first.** Every PR passes CI on Windows, Linux and macOS. A change to processes, paths or the console also gets a Windows-specific test.
- **No silent failure.** Every error reaches the TUI log and the session log with a next step, such as "run `rw doctor`" or "/limit reset".
- **Safe by default.** Never commit to the user's branch, never touch their index, never overwrite their concurrent edits, and keep the option to undo.
- **Pin and record CLI versions.** When Codex or Claude Code changes, record new output fixtures (`internal/runner/testdata`) before you raise the tested version in `rw doctor`.
- **Adversarial review before merge.** Changes to the orchestrator, git, process or runner code get a second review pass focused on concurrency, Windows and failure paths. See [Adversarial review](#adversarial-review).
- **No quota in tests.** Tests use fake or scripted CLIs and recorded output, never a real model.

## Build and test

You need Go (the version in `go.mod`) and Git 2.38 or newer.

```sh
go build ./cmd/rw
go vet ./...
go test -p 4 ./...
```

- `-p 4` keeps the load sane: many tests run real git. On Windows the
  orchestrator tests take about 10 minutes, so add `-timeout 20m`.
- CI also runs `go test -race ./...` on Linux and macOS.
- Vet the other systems too: `GOOS=linux go vet ./...`, `GOOS=darwin go vet ./...`
  and `GOOS=windows go vet ./...` (in PowerShell: `$env:GOOS="linux"; go vet ./...`).
- `rw selftest` runs the Windows real-use checks with a scripted agent (no
  quota). `--sandbox only` runs the container sandbox checks and needs
  docker or podman. `go test` runs it as `TestSelftest`.
- Long runs, by hand or in their CI workflows:
  - stress test: `RW_STRESS_DURATION=3m go test -run '^TestStress$' -v ./internal/orchestrator` (`stress.yml` runs 30 minutes);
  - fuzzing: `go test -run='^$' -fuzz='^FuzzClaudeParser$' -fuzztime=30s ./internal/runner` (`fuzz.yml` has every target).

**VS Code extension** (`editors/vscode`, Node 22):

```sh
npm ci
npm run typecheck
npm test
npm run test:integration   # downloads VS Code and drives it
```

**JetBrains plugin** (`editors/jetbrains`, JDK 21; `gradlew.bat` on Windows):

```sh
./gradlew test buildPlugin
./gradlew verifyPlugin
```

[editors/jetbrains/README.md](editors/jetbrains/README.md) explains the
real-rw and UI tests.

## Pull requests

- Branch from `main`. One topic per PR. Never push to `main`.
- Title: what the change does, in one short sentence. Add the ROADMAP item
  in brackets when there is one, for example "(ROADMAP 2.5)".
- Fill in the [pull request template](.github/pull_request_template.md).
  Its sections are the project's PR style:
  - **Summary:** what changed and why, for someone who has not seen the issue.
  - **Design:** how it works, and the choices you made against the alternatives.
  - **Tests:** each new test, what it covers, and that it fails without the fix. Say which OS you ran the suite on.
  - **Not verified:** what you could not run for real. Never leave this out; write "nothing" if so.
  - **ROADMAP update:** the rows and lines to change in [ROADMAP.md](ROADMAP.md).
  - **CHANGELOG entry:** the line you added to [CHANGELOG.md](CHANGELOG.md).
- Add review findings and their fixes to the description under "Review fixes".
- Get CI green on all three systems before you ask for a merge.

### CHANGELOG

Add one line under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md) for
every user-visible change. The comment at the top of that file says how.
Tests, CI and refactors without a visible effect need no entry.

### ROADMAP

Update [ROADMAP.md](ROADMAP.md) in the same PR: the item's status,
"Gaps closed" or "Remaining gaps", and "Verified" or "Not verified yet".
When several PRs are open at once, put the text in the "ROADMAP update"
section instead, and a docs PR folds it in.

## Adversarial review

Changes to the orchestrator (`internal/orchestrator`), git handling, process
code (`internal/proc`), the runners (`internal/runner`) or the sandbox
(`internal/sandbox`) get a second review before merge.

- The reviewer did not write the change: a person, or an agent with a fresh context.
- Its job is to break the change: races and lock order, Windows paths and processes, cancel, crash and kill at every step, full disks, and lost work.
- For trust, tokens, the sandbox or `rw web`, it also reviews security.
- Fix each finding with a regression test, or write down in the PR why it stays.
- A large change gets a second pass over the fixes.

## Writing style

Docs, PR descriptions, comments and messages use plain English in short
sentences. Say what happens and what to do next. Use the real command and
file names. No marketing words.

## Releases

[packaging/README.md](packaging/README.md) says how to cut a release. In
short: move `[Unreleased]` in the CHANGELOG to the new version, tag it, then
render and commit the manifests.

## Security and conduct

Report security problems privately, as [SECURITY.md](SECURITY.md) says,
not in a public issue. Everyone here follows the
[Code of Conduct](CODE_OF_CONDUCT.md).
