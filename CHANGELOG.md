# Changelog

All notable changes to Relayweft are in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before
1.0.0, a minor version (0.x.0) may change or remove things.

Up to 0.2.0 the project was called Switchyard and its command was `sy`.
The entries for those versions use the names of the time.

<!--
How to add an entry (every pull request with a user-visible change):
- Add one line under [Unreleased], in the right subsection: Added, Changed,
  Deprecated, Removed, Fixed or Security (in that order). Create the
  subsection if it is missing.
- Write for users, not as a commit message: what changed for them, in one
  short sentence. Use rw's command and config names.
- End the line with the pull request link, for example
  ([#45](https://github.com/Sparkz400/Relayweft/pull/45)).
- Delete the "Nothing yet" line when you add the first entry.
- Tests, CI and refactors without a visible effect need no entry.
At release time, [Unreleased] becomes the new version (packaging/README.md).
-->

## [Unreleased]

### Added

- Day planner: `rw run --file tasks.txt --fill` spends both subscriptions' 5-hour usage windows overnight. Each task leans on the provider whose window resets first, rw waits for the next reset when every window is full, and a task a limit stopped resumes after it. `--until` and `--fresh-at` keep the morning free and both windows full; `rw dayplan` prints the plan. See `docs/dayplan.md`.

### Fixed

- Homebrew makes the downloaded `rw` binary executable before running it to generate completion scripts during installation. ([#62](https://github.com/Sparkz400/Relayweft/pull/62))
- The package repository publisher requests release asset bytes with a single `Accept` header; conflicting JSON and binary headers made the first signed feed deployment download metadata and fail its checksum check. ([#62](https://github.com/Sparkz400/Relayweft/pull/62))

## [0.4.0] - 2026-10-06

### Added

- Bitbucket Cloud: pull requests, issues as tasks, review follow-ups and team queues with `BITBUCKET_TOKEN`. See `docs/bitbucket.md`. ([#55](https://github.com/Sparkz400/Relayweft/pull/55))
- A searchable documentation site with command and config references at https://sparkz400.github.io/Relayweft/. ([#52](https://github.com/Sparkz400/Relayweft/pull/52))
- Signed `.deb`, `.rpm` and `.apk` packages and apt, dnf and apk repositories on the documentation site. See `packaging/README.md` for installation and key verification. ([#54](https://github.com/Sparkz400/Relayweft/pull/54))
- Azure DevOps: `rw pr`, work items as tasks (`--issue`, `--issues label:<tag>`, `--team`), `rw watch` and `rw review --post` work on dev.azure.com, `*.visualstudio.com` and Azure DevOps Server (`AZURE_DEVOPS_HOST`), with `AZURE_DEVOPS_TOKEN`. See `docs/azure-devops.md`. ([#57](https://github.com/Sparkz400/Relayweft/pull/57))
- Resolve steps: when two agents' changes conflict, or an agent's change overlaps with your own uncommitted edits, an agent merges both in a pool worktree (never in your folder); rw checks for leftover markers and runs `verify.commands` before it lands, and asks first for your own edits. `orchestrator.conflicts: auto|resolve|ask|fail`, `max_resolve_rounds`, `resolve_role`, `/conflicts` in the TUI; see docs/conflicts.md. ([#56](https://github.com/Sparkz400/Relayweft/pull/56))
- `rw mcp`: an MCP server, so Claude Code or Codex can hand a multi-step task to rw from inside their own session and follow it (status, plan approval, change review, result, undo). It works only in the folder it was started in, and refuses tasks under rw's own agents. Set-up: `docs/mcp.md`; `rw doctor` checks it. ([#58](https://github.com/Sparkz400/Relayweft/pull/58))
- `rw completion bash|zsh|fish|powershell`: Tab completion for subcommands, flags, provider names, models and task ids (Windows PowerShell 5.1 and PowerShell 7 too). The .deb/.rpm/.apk packages, Homebrew and the AUR package install it. ([#49](https://github.com/Sparkz400/Relayweft/pull/49))
- `docs/config.md`: every config key with its type, default, description and whether a repo's `.relayweft.yaml` needs `rw trust` for it; generated from the code, and CI fails when it is out of date. ([#49](https://github.com/Sparkz400/Relayweft/pull/49))
- `CONTRIBUTING.md`, a pull request template and a Code of Conduct. ([#45](https://github.com/Sparkz400/Relayweft/pull/45))

### Security

- Releases are attested and signed: GitHub build provenance for every asset, a CycloneDX SBOM per binary (`rw-<os>-<arch>.cdx.json`, attested), and a keyless Sigstore signature of `checksums.txt` (`checksums.txt.sigstore.json`). `rw update` prints the `gh attestation verify` command after installing a signed release. See `packaging/README.md`, "Verifying a release". ([#47](https://github.com/Sparkz400/Relayweft/pull/47))
- CI checks formatting (gofmt), runs golangci-lint and runs govulncheck on every change and weekly. ([#48](https://github.com/Sparkz400/Relayweft/pull/48))

### Fixed

- Affected test selection falls back to the full suite when Jest does not index the changed files or Vitest is older than 1.2.2; it reads multiline Gradle includes and passes runner flags correctly through `npx`. ([#59](https://github.com/Sparkz400/Relayweft/pull/59))
- On Linux and macOS, killing rw now also stops its agents and their children in the same process group; resume keeps their half-done work. ([#53](https://github.com/Sparkz400/Relayweft/pull/53))
- A user edit merged with an agent's change could lose the end of the agent's file when the disk was full; it now counts as a conflict and leaves the file as it was. ([#48](https://github.com/Sparkz400/Relayweft/pull/48))
- `rw bench` blamed "another rw bench" for a workspace folder it could not create. ([#48](https://github.com/Sparkz400/Relayweft/pull/48))
- Commit messages reach git on stdin, so very long task texts work on Windows. ([#48](https://github.com/Sparkz400/Relayweft/pull/48))
- A logged-out or missing agent CLI no longer counts as a usage-limit hit in the Dashboard, `rw stats` and the `rw stats --json` export (format 1.1: `unavailable`), and `rw run --when-reset` no longer waits for its 12 hours. ([#60](https://github.com/Sparkz400/Relayweft/pull/60))
- With several rw (or an editor's git) on one repo, a merge no longer fails when another git holds the repo's lock for a moment, and a file another rw removes no longer makes the snapshot for undo fail; a task whose start or end cannot be recorded says that undo will not work. ([#60](https://github.com/Sparkz400/Relayweft/pull/60))
- After `rw resume`, `rw undo --agent-files-only` and `rw pr` also cover the files of the steps from before the interruption. ([#60](https://github.com/Sparkz400/Relayweft/pull/60))

## [0.3.0] - 2026-10-05

**Switchyard is now Relayweft, and `sy` is now `rw`.** `sy update` cannot
install this release. Users of v0.1.0 and v0.2.0 reinstall once. On its
first start, rw copies the Switchyard folder (config, history, logs, learned
routes, trust) to `relayweft`. See [the release notes](packaging/release-notes/v0.3.0.md)
and the README section "Upgrading from Switchyard (`sy`)".

### Added

- `rw setup`, a guided first run: it finds the agent CLIs, checks versions and logins without using quota, writes the config and offers a read-only first task. `rw`, `rw run` and `rw web` start it when there is no config. ([#30](https://github.com/Sparkz400/Relayweft/pull/30))
- Best of N for hard steps (`routing.best_of`, off by default; `b` in the plan view): a writing step runs on two to four routes, and the checks or the reviewer pick the change that lands. ([#34](https://github.com/Sparkz400/Relayweft/pull/34))
- Affected tests in fix rounds: with `max_fix_rounds` 2 or more, a fix round first runs only the tests the changes affect. The full checks still run before the final review. ([#36](https://github.com/Sparkz400/Relayweft/pull/36))
- A container sandbox for agents (`sandbox:`, docker or podman, off by default): agents, verify commands, `after_*` hooks and bench checks run with only the step's folder writable. See [docs/sandbox.md](docs/sandbox.md). ([#35](https://github.com/Sparkz400/Relayweft/pull/35))
- A Dashboard in `rw web`: tasks, cost, routes, learned routes, limits and the health streak over 7, 30 or 90 days. ([#33](https://github.com/Sparkz400/Relayweft/pull/33))
- A JetBrains plugin (`editors/jetbrains`) for IntelliJ IDEA, PyCharm, GoLand, WebStorm, Rider and the other JetBrains IDEs (2025.2+), with plan approval and hunk review in the IDE's diff viewer. It is not on the Marketplace yet. ([#37](https://github.com/Sparkz400/Relayweft/pull/37))
- Homebrew (`brew install relayweft` from this repo's tap), `.deb`, `.rpm` and `.apk` packages on each release, and an AUR package (`relayweft-bin`). ([#32](https://github.com/Sparkz400/Relayweft/pull/32))
- A step that was running when rw died continues its agent's own session on `rw resume`, in the folder it ran in, with its half-done edits. ([#25](https://github.com/Sparkz400/Relayweft/pull/25))
- The half-done edits of an interrupted step are saved on a branch (`rw/<task>/<step>-unfinished`) before its worktree is freed after 7 days or by `rw clean`. `rw history` and `rw resume` say where they are. ([#29](https://github.com/Sparkz400/Relayweft/pull/29))
- A Forgejo/Gitea Actions workflow (`ci/forgejo-workflow.yml`) runs issue tasks in CI. ([#24](https://github.com/Sparkz400/Relayweft/pull/24))
- `rw bench --from-history --own-tests`, and `{tests}`/`{test_dirs}` in a check, run only a commit's own tests. ([#27](https://github.com/Sparkz400/Relayweft/pull/27))

### Changed

- Renamed to Relayweft: the command is `rw`, the config files are `relayweft.yaml` and `.relayweft.yaml`, environment variables are `RW_*`, task branches are `rw/…`, and release assets are `rw-<os>-<arch>`. The VS Code extension is `sparkz400.relayweft`, the JetBrains plugin `io.github.sparkz400.relayweft`, and the GitHub Action `Sparkz400/Relayweft`. ([#44](https://github.com/Sparkz400/Relayweft/pull/44))
- A follow-up to a Claude or Codex agent that ran in a pool worktree resumes it in that worktree. ([#25](https://github.com/Sparkz400/Relayweft/pull/25), [#29](https://github.com/Sparkz400/Relayweft/pull/29))
- The worktree pool keeps its size: a step cancelled before its agent changed anything holds no worktree, and a full pool gives up the oldest held worktree after saving its edits on a branch. ([#40](https://github.com/Sparkz400/Relayweft/pull/40), [#41](https://github.com/Sparkz400/Relayweft/pull/41), [#43](https://github.com/Sparkz400/Relayweft/pull/43))
- `rw tune` flags a route only on a clear majority, which accounts for the number of runs (3 failures in 5 runs, 4 in 10, 7 in 20), and counts an escalated step once. ([#31](https://github.com/Sparkz400/Relayweft/pull/31))
- Model tiers count routine words only in a step's title, so "do not rename" in a prompt no longer moves a logic fix to the fast model. ([#31](https://github.com/Sparkz400/Relayweft/pull/31))
- `rw update` leaves a binary that a package manager installed alone and prints that package manager's command instead. ([#32](https://github.com/Sparkz400/Relayweft/pull/32))
- `rw doctor` also checks Claude Code's login. ([#30](https://github.com/Sparkz400/Relayweft/pull/30))

### Fixed

- macOS: the agent of a killed rw kept running, and its worktree could not be resumed. The next rw now stops it (after checking the process's start time), and the step continues in its own worktree. ([#29](https://github.com/Sparkz400/Relayweft/pull/29))
- Linux: Ctrl+C in `rw app` or `rw web` closed the browser it had started, with all its windows. ([#28](https://github.com/Sparkz400/Relayweft/pull/28))
- Linux notifications: a NUL byte made them fail, and text like `Vec<T>` or `&amp;` was read as markup. ([#28](https://github.com/Sparkz400/Relayweft/pull/28))
- `rw run --issue` or `--issues` with `--pr` exited 0 when the push or the pull request failed, so a CI job stayed green. ([#26](https://github.com/Sparkz400/Relayweft/pull/26))
- Running an issue again after its pull request was closed failed on the existing branch. The new run gets a fresh branch name. ([#26](https://github.com/Sparkz400/Relayweft/pull/26))
- `rw bench`: `git log --all` in a run could show the solution of a history task. Every run now starts in a fresh repository with only the starting commit's files. ([#27](https://github.com/Sparkz400/Relayweft/pull/27))

### Security

- Release binaries are built with Go 1.26.8, which fixes 23 standard-library vulnerabilities that rw reaches with Go 1.26.0 (in `net/http`, `html/template`, `crypto/x509`, `net/url` and `os`). ([#46](https://github.com/Sparkz400/Relayweft/pull/46))
- Every git command rw runs on your machine in a folder an agent wrote to runs with `core.fsmonitor=false` and without submodule recursion, so files an agent writes cannot make git run code. ([#35](https://github.com/Sparkz400/Relayweft/pull/35))
- In a Forgejo or Gitea Actions job, the runner's `GITHUB_TOKEN` (the job's own token) is never sent to GitHub, and `GITEA_RUNNER_REGISTRATION_TOKEN` is kept from agents. ([#24](https://github.com/Sparkz400/Relayweft/pull/24))
- An untrusted `./relayweft.yaml` or repo `.relayweft.yaml` may lower `routing.best_of` and make the sandbox stricter, but needs `rw trust` to raise or loosen them. ([#34](https://github.com/Sparkz400/Relayweft/pull/34), [#35](https://github.com/Sparkz400/Relayweft/pull/35))
- The container sandbox had two independent security reviews before release. [docs/sandbox.md](docs/sandbox.md) says what it protects and what it does not. ([#35](https://github.com/Sparkz400/Relayweft/pull/35))

## [0.2.0] - 2026-10-04

Released as Switchyard (`sy`).

### Added

- Scheduled runs (`sy run --at`, `--in`, `--when-reset`), budgets per task and per day, `sy pr`, GitHub issues as tasks (`--issue`, `--issues`), `sy report`, MCP servers and multi-repo tasks. ([#10](https://github.com/Sparkz400/Relayweft/pull/10))
- `sy watch` and `sy review`, learned routes (`sy tune --apply`), cost estimates before plan approval, team budgets and stats export, the repo's own conventions as context, and a VS Code extension. ([#11](https://github.com/Sparkz400/Relayweft/pull/11))
- GitLab and Gitea/Forgejo (Codeberg too) for `sy pr`, issues, `sy watch` and `sy review`. ([#12](https://github.com/Sparkz400/Relayweft/pull/12), [#15](https://github.com/Sparkz400/Relayweft/pull/15))
- Webhook notifications (Slack, Discord, ntfy, plain JSON) and `sy notify --test`. ([#12](https://github.com/Sparkz400/Relayweft/pull/12))
- `sy selftest`, the automated part of the Windows test pass, with a scripted agent. ([#12](https://github.com/Sparkz400/Relayweft/pull/12))
- `sy bench --from-history` builds a benchmark from your own commits, and bench results can feed learned routes. ([#12](https://github.com/Sparkz400/Relayweft/pull/12))
- More providers (beta): named providers with a `kind`, a `generic` kind for any CLI described in config, presets for Gemini CLI, Qwen Code, DeepSeek and Ollama, and `standby` for a free local model. See [docs/providers.md](docs/providers.md). ([#16](https://github.com/Sparkz400/Relayweft/pull/16))
- Cost-aware model tiers (`routing.tiers: auto`, off by default). ([#18](https://github.com/Sparkz400/Relayweft/pull/18))
- Issue tasks in CI: a GitHub Action and a GitLab CI job, plus `sy history --json`. ([#19](https://github.com/Sparkz400/Relayweft/pull/19))
- `sy health`: a health log, a hang watchdog and a Health panel in `sy web`. ([#20](https://github.com/Sparkz400/Relayweft/pull/20))
- Team mode: `--team` lets several machines work through one issue label. ([#21](https://github.com/Sparkz400/Relayweft/pull/21))
- A Scoop manifest, and `SECURITY.md` with private vulnerability reporting. ([#6](https://github.com/Sparkz400/Relayweft/pull/6))

### Fixed

- Windows: closing `sy app` also closed your other Edge windows. `sy app` now exits about 5 seconds after its window closes. ([#12](https://github.com/Sparkz400/Relayweft/pull/12))
- Windows: Claude's PowerShell tool was refused for `verify.commands`, and refused tool calls were not shown. ([#12](https://github.com/Sparkz400/Relayweft/pull/12))
- `sy update` failed when the previous update's `.old` file was still running. ([#12](https://github.com/Sparkz400/Relayweft/pull/12))
- A task that ended during planning could leave git still adding pool worktrees to the repo. ([#14](https://github.com/Sparkz400/Relayweft/pull/14))

### Security

- A `./switchyard.yaml` that came with a cloned repository could run programs (for example from `sy doctor`) without `sy trust`. Its command settings now need trust, like a repo's `.switchyard.yaml`. ([#12](https://github.com/Sparkz400/Relayweft/pull/12))
- A crafted `.switchyard.yaml` could get past `sy trust` with YAML aliases or merge keys. ([#10](https://github.com/Sparkz400/Relayweft/pull/10))
- Windows: agent arguments follow the Windows quoting rules, which closes an argument-injection path. ([#10](https://github.com/Sparkz400/Relayweft/pull/10))
- Agents, verify commands, hooks and bench checks start without the forge and CI tokens. ([#19](https://github.com/Sparkz400/Relayweft/pull/19))

## [0.1.0] - 2026-10-03

The first release, as Switchyard (`sy`).

### Added

- The terminal app and its engine: a planner splits a task, a rule router (with an optional judge model) picks Codex or Claude for each step, and agents run in parallel in git worktrees. Their work is merged back without touching your index or branch, with review checkpoints and a fix round. `sy --demo` shows it with fake agents. ([#1](https://github.com/Sparkz400/Relayweft/pull/1))
- A usage limit, a logged-out CLI or a missing CLI moves the work to the other provider. ([#1](https://github.com/Sparkz400/Relayweft/pull/1))
- `sy doctor`, `sy models`, `sy init` and `sy stats`. ([#1](https://github.com/Sparkz400/Relayweft/pull/1))
- Fast big and Git LFS repos: a worktree pool, faster snapshots and `sy clean`. ([#2](https://github.com/Sparkz400/Relayweft/pull/2))
- A multi-line prompt that handles pastes, and reliable cancel. ([#3](https://github.com/Sparkz400/Relayweft/pull/3))
- Load limits, a disk guard, debug and crash logs with `sy bugreport`, `sy undo`, `sy bench`, switching provider before Claude's limit, and the cost of each task. ([#4](https://github.com/Sparkz400/Relayweft/pull/4))
- Plan approval, change review by file or hunk, verify commands, follow-ups (`@agent`), history and resume, a task queue, notifications, `sy tune`, per-repo `.switchyard.yaml` with `sy trust`, hooks, `sy web` and `sy app`, and `sy update`. ([#5](https://github.com/Sparkz400/Relayweft/pull/5))
- Release binaries for Windows, Linux and macOS with checksums, under the MIT license. ([#5](https://github.com/Sparkz400/Relayweft/pull/5))

[Unreleased]: https://github.com/Sparkz400/Relayweft/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/Sparkz400/Relayweft/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/Sparkz400/Relayweft/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Sparkz400/Relayweft/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Sparkz400/Relayweft/releases/tag/v0.1.0
