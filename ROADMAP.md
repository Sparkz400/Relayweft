# Relayweft Roadmap

**Goal:** a tool you trust with real work every day. It must be **reliable first, useful second, smart third, pretty fourth.**

The phases are ordered. A phase starts only when the previous phase's **exit criteria** are met. Features never jump ahead of reliability.

> **Note (October 2026):** Phases 2 to 4 were built ahead of Phase 1's exit criteria, at your request. They have automated coverage and several recorded real-use checks, detailed below. Phase 1's remaining verification (1.1, 1.6) and its exit criteria still come first. Until they are met, treat Phase 2 to 4 features as a beta.

> **Renamed (October 2026):** up to v0.2.0 the project was called Switchyard and its command was `sy`. From v0.3.0 it is Relayweft and the command is `rw`, because "Switchyard" was taken on the VS Code Marketplace. The history below says `sy` wherever `sy` was what ran.

---

## Where we are (October 2026)

Each numbered item has three independent statuses in its phase's evidence table:

- **Implemented:** `Yes` means the described capability or test harness exists; `Partial` names the unfinished scope.
- **Automatically tested:** `Covered` links the checked-in tests; executed workflow results are linked where recorded. It does not mean every scenario is covered or that those tests were rerun for this documentation update.
- **Verified in real use:** `Recorded` names a completed real-system check; `Partial` names both the checked scope and remaining gaps; `Pending` means no such evidence is recorded here. `N/A` applies to automated-test or review deliverables. Checks using scripted agents, headless browsers or containers retain those qualifications.

Implementation, automated coverage and real-use evidence do not substitute for one another or close the phase exit criteria. The dated records below provide the evidence and limits for the tables.

### Local implementation batch — 6 October 2026

The following additions are implemented in this checkout, after v0.4.0. They
do not close the sustained-use, hosted-service or release approval gates.

| Area | Implemented | Verification and remaining evidence |
|---|---|---|
| Benchmark follow-up | Durable per-run evidence, Claude command preflight, concurrent budget reservations and incomplete-usage reporting | [Implementation and checks](docs/bench/2026-10-06-benchmark-followup.md). The [real four-mode rerun](docs/bench/2026-10-06-pilot-rerun.md) retained replayable evidence, passed all three exact-command preflights and kept reported usage below 200k per mode. Required work still exceeded the remaining budget; alternate Claude commands still met denials. |
| Realistic benchmark | [Ten real multi-file tasks](docs/bench/realistic.md), protected checks, fair starts, repetitions, routed tiers and JSON comparisons | All 10 known solutions pass and all 10 bases fail on Windows; scripted matrix tests pass. Both one-task four-mode pilots scored 0/4. The [rerun](docs/bench/2026-10-06-pilot-rerun.md) took 30m 53s; routed modes stopped on budget admission. The broader comparison and benefit criterion remain open. |
| Reliability | Concurrent notes, project-bound recovery, saved-branch reporting, cancellation/provider fixes, and undo/resume file attribution | Full Windows tests and the Linux race suite pass; both systems pass 30-minute stress assertions plus final-fix checks. The Windows long-run wrapper was stopped during Go's post-test cache processing; the final shorter run exited normally. Real Codex and Claude hard-kill/resume/undo/redo preserve user edits, branch and index with no child leftovers. Sustained daily use remains open. |
| Affected tests | Monorepos, aliases, Yarn Plug'n'Play and Gradle `projectDir`. vitest projects, workspaces and aliases narrow. Several jest projects and `moduleNameMapper` narrow when each project's index is closed. `npm test --workspaces` and `pnpm -r test` narrow by package. Gradle reads plain `projectDir` lines and literal project references with whitespace or named arguments. vitest with a config `root`, the react, react-swc, vue or tsconfig-paths plugins, or `require()`/`vi.importActual` chains narrows too. Generated PnP loaders are excluded from application imports; loader changes and PnP workspaces with peers run in full. Unread layouts and dependencies retain the full command with a reason. | [Layout tests](internal/affected/layouts_test.go), [fallback regressions](internal/affected/selective_test.go), and [opt-in live checks](internal/affected/live_test.go). Earlier Jest/Vitest/npm/pnpm [measurements](docs/bench/2026-10-06-affected-real-projects.md#monorepos-aliases-plugnplay-and-projectdir-follow-up-6-oct) are supplemented by [Windows Yarn 4.6 and Gradle 8.14 checks](docs/bench/2026-10-07-affected-layouts.md): plain PnP and Groovy/Kotlin project folders catch the same failure full and narrowed; virtual PnP workspaces and computed Gradle dependencies fall back. Published monorepos and automatic Nx/Turbo narrowing remain open. |
| CI templates | [Azure and Bitbucket jobs](docs/ci-azure-bitbucket.md) with installation checks, budgets and reports | Local shell/YAML tests pass on scripted tools. Hosted runs need account access. |
| Updater verification | Signed provenance checked before installation, workflow and tag-commit bound | Unit failure/tamper checks pass; real v0.4.0 verification accepts original bytes and rejects modified binary/checksum bytes. No executable was installed by that test. |
| Reusable workflows | [Bug fix, dependency upgrade, review and release prep](docs/workflows.md), saved checks and budgets; issue tasks, scheduled batches and team queues through `--pr` | Workflow lifecycle, policy and CLI integration tests; [issue/workflow tests](cmd/rw/issue_workflow_test.go) cover draft PRs, checks, approvals and batch cleanup with fake agents/APIs. Live issue/workflow use remains unverified. |
| Recovery UX | [Recovery view and CLI](docs/recovery.md), interrupted progress, branches, conflicts, resume/retry/undo | Headless Chromium inspection and browser undo of a real resumed Codex task pass; kept user files are identified separately. Undone tasks cannot resume until redone. |
| Inspectable memory | [Memory view and CLI](docs/memory.md), inclusion reasons, preview, edit/add/remove | Headless Chromium add/edit/reload/remove and stale-edit rejection pass, with light/dark screenshots. The drawer fits 390px; the surrounding dashboard still has a desktop minimum width. |
| Task-shape shortcuts | `orchestrator.auto_single`, `light_planning`, `review_skip_max_lines`, `fit_budget` (all on by default), `rw run --plan`, bench mode `routed-classic`; a one-step task probes only its own provider | Unit and orchestrator tests pass (Windows, full suite). A [real run](docs/bench/2026-10-06-phase3-shortcuts.md) on 5 starter and 2 realistic tasks found and fixed two bugs. Afterwards routed was faster than single Codex (high) in both groups and used fewer tokens on the realistic tasks, with equal correctness. Against a medium-effort single agent it showed no correctness benefit; see the Phase 3 conclusions. |

See [the implementation verification record](docs/bench/2026-10-06-expansion-verification.md)
and [the follow-up review and reliability checks](docs/bench/2026-10-06-reliability-benefits.md)
for executed checks and boundaries.

Review fixes (7 October 2026): workflow resumes retain their approval gates,
budget fitting preserves multi-repo assignments and dependencies, and test
citations remain evidence unless individual execution can be confirmed.
The three regressions failed before the fixes and passed afterwards; the
CLI test also confirms that a gated resume asks without `--approve`.
Windows build and Windows/Linux/macOS vet passed. The subsequent full Windows
suite was blocked at compilation by concurrent `internal/affected` edits
(`vitestRelatedWorks`, `vitestConfigs` and `vitestGraph` return-value mismatches);
it does not establish a full-suite pass for these fixes.

**Built:**
- All of the original plan (M0–M8).
- Big-repo and Git LFS speed-ups, with a worktree pool.
- A multi-line prompt that handles pastes.
- Reliable cancel.
- CI on Windows, Linux and macOS.
- **Phase 1 batch (done):**
  - load limits
  - disk guard
  - debug and crash logs with `rw bugreport`
  - `rw health`: health log, hang watchdog and the clean streak toward the exit criterion
  - `rw undo`
- **Phase 2 (built; real-use coverage varies by item):**
  - plan approval
  - change review before applying
  - agents run your tests (`verify`)
  - follow-up messages
  - task history and resume
  - task queue
  - notifications
  - release pipeline with `rw update`
  - a guided first run (`rw setup`)
  - only the affected tests in fix rounds
  - Homebrew, `.deb`/`.rpm`/`.apk` and AUR packages
- **Phase 1 test items (done):**
  - parser fuzzing in CI (found and fixed 5 bugs)
  - a 30-minute stress test in CI on Linux and Windows
  - a second adversarial review of the worktree pool (17 findings, all fixed with regression tests)
- **Phase 3 (built; first measurements in):**
  - `rw bench` with a starter set and a `routed-nohandoff` mode
  - switching provider before the limit
  - `rw tune` (rule tuning, judge cost vs gain, models from your config)
  - context hand-off
  - cost per task and per day
  - cost-aware model tiers (off by default; measured on the starter set)
  - best of N for hard steps (off by default; not yet measured)
- **Phase 4 (built; real-use gaps remain, including 4.3 providers):**
  - `rw web` and `rw app`
  - per-repo `.relayweft.yaml` with `rw trust`
  - hooks
  - hunk-level review
  - talking to a running agent
  - follow-ups that survive restarts
  - scheduled runs and budgets
  - `rw report`
  - MCP servers
  - multi-repo tasks
  - `rw pr`, and GitHub issues as tasks
  - `rw watch` and `rw review`
  - learned routes, plan cost estimates
  - team budgets and stats export
  - the repo's own conventions as context
  - a VS Code extension
  - GitLab and Gitea/Forgejo for `rw pr`, issues, `rw watch` and `rw review`
  - issue tasks in GitHub Actions and GitLab CI, without the forge token in agents
  - team mode: several machines share one issue label
  - a dashboard in `rw web`
  - a JetBrains plugin
  - a container sandbox for agents (docker or podman)
- **Release v0.1.0** (3 Oct 2026, as Switchyard/`sy`): six binaries plus checksums, built by the release workflow, MIT license, Scoop manifest filled in.
- **Release v0.2.0** (4 Oct 2026, as Switchyard/`sy`): more providers, `sy health`, model tiers, CI and team mode, self-hosted forges, webhooks, `sy selftest` and the fixes from real use. `sy update` from v0.1.0 to v0.2.0 was run for real (checksum verified). Scoop manifest updated; winget manifests rendered.
  - The repository is public: release downloads, `rw update` (then `sy update`) and the Scoop install need no login.
  - winget needs the rendered manifests submitted to microsoft/winget-pkgs.
- **Release v0.3.0** (5 Oct 2026), built with Go 1.26.8. v0.3.0 brings:
  - the **rename to Relayweft (`rw`)**. On its first start, rw copies the Switchyard user folder once. `sy update` cannot install v0.3.0, so v0.2.0 users reinstall once (README, "Upgrading from Switchyard").
  - `rw setup`, best of N, affected tests in fix rounds, the container sandbox, the dashboard, the JetBrains plugin, Homebrew/`.deb`/`.rpm`/`.apk`/AUR packaging, Codex follow-ups in pool worktrees, saved half-done edits, macOS orphan cleanup, and `rw tune` thresholds that account for sample size. Every change had an independent adversarial review before merge, and the 30-minute stress test passes on Ubuntu and Windows.
- **Release v0.4.0** (6 Oct 2026): Bitbucket Cloud and Azure DevOps, conflict resolution, `rw mcp`, shell completion, a generated config reference, the documentation site, signed Linux packages and package repositories, release provenance and SBOMs, and fixes to process cleanup, affected tests, concurrent git and resume. See [CHANGELOG.md](CHANGELOG.md) and [the release notes](packaging/release-notes/v0.4.0.md). The live forge and daily-use gaps below still apply.

**Release verification (6 Oct 2026):** the exact v0.4.0 commit passed CI, lint, vulnerability checks, CodeQL, fuzzing, Pages and 30-minute Linux/Windows stress tests. The [publishing workflow](https://github.com/Sparkz400/Relayweft/actions/runs/37416738374) passed. All 22 published assets were downloaded and checked: 20 SHA-256 entries, the Sigstore signature, 21 build provenance attestations, six SBOM attestations and six package signatures. A real Windows v0.3.0 binary updated to the exact v0.4.0 asset and removed its old executable. Winget manifests pass validation.

**Automated tests and recorded checks:**
- Unit and integration tests (real git repos, fake CLIs, recorded output from the real CLIs).
- Real end-to-end runs with Claude Code (before Phase 2).
- `.cmd` shim launching on a Windows CI runner.
- **Phase 2 and 3, with fake agents and real git:**
  - plan edits and cancel
  - partial apply, reject and feedback in change review
  - a failing check feeding the fix round
  - resume skipping finished steps
  - follow-ups resuming a session, and the fresh-agent fallback
  - the TUI plan overlay, driven in the demo
- **The starter bench checks:** all five fail on the untouched project and pass with reference solutions.
- **A real benchmark with Claude Code:** 20 runs, all passed ([docs/bench](docs/bench/2026-10-03-starter-claude.md)).
- **Model tiers, for real:** 19 Claude runs and 3 Codex runs, all passed ([docs/bench](docs/bench/2026-10-04-tiers-claude.md)).
- **`rw web` in headless Chromium:** plan editing, hunk review, reload restore and the panels.
- **A 3-minute stress run:** goroutines, file handles, heap and processes stayed flat over 1,750 tasks.

**Verified for real on 4 Oct 2026 (Windows 11):**
- **Follow-ups:** `claude --resume` (Claude Code 2.1.288) and `codex exec resume` (codex-cli 0.160.0) through `rw web`, with the same session and the agent remembering; the pool-worktree fallback to a fresh agent. Recorded as fixtures, with a real Codex edit run.
- **Mid-step resume:** a Claude Code agent killed after the first of three edits in a git worktree, then resumed there with rw's prompt: it checked the first file and wrote the other two. Recorded as fixtures.
- **Claude verify through `allowedTools`:** a failing check fed the fix round. Found and fixed: Claude's PowerShell tool on Windows was refused (only `Bash(...)` was allowed), and refused tool calls were invisible.
- **`sy update`** against the real v0.1.0 release: asset, checksum, swap while running, `.old` cleanup. Fixed: a second update while the first update's `.old` still runs.
- **Windows toasts** (delivered, read back from the notification history) and **ntfy.sh** (every event, escaping, non-ASCII). Fixed: a non-ASCII click URL in a header.
- **`rw app` in Edge:** fixed a serious bug (closing rw killed the user's other Edge windows: the browser was in rw's kill-on-exit job), and rw now exits ~5s after its window closes instead of 30s; a reload keeps it.
- **The VS Code extension** in real VS Code 1.140 and 1.90 (`npm run test:integration`). Fixed: CRLF checkouts showed only hunks, and answered reviews stayed open.
- **Gitea 28 and GitLab CE 19.4** for `rw pr`, issues, `rw watch` and `rw review --post`. Fixed: a self-hosted forge's port and http were dropped; on Windows a file with a capital letter counted as "unreported", which blocked every unattended push; `--api` with an issue URL on another host sent the token there.
- **Forgejo 16.0 and GitLab CE 19.4 with GitLab Runner 19.4** (Docker executor), for `rw pr`, issues, `rw watch` and `rw review --post`, with a scripted agent CLI (no quota). Forgejo: failed and errored commit statuses, a review requesting changes with an inline comment, a stranger's review ignored, merged and closed pull requests dropped. GitLab: failed jobs of branch and merge request pipelines, with their logs, start a follow-up; a passing pipeline, a job allowed to fail and a still-running pipeline do not. Fixed: `rw watch` without `--every` asked to approve the follow-up's plan (and ran nothing when nobody answered); GitLab job logs came with Runner 19's timestamps and section markers on every line, which took about half of the log tail the agents get.
- **The Windows stress limits:** four CI runs rose at most 12 handles above warm-up; the slack is now 40 (was 100). Fixed: on Windows the test counted other `go test` runs as leaked processes.
- **Security fix:** a `./relayweft.yaml` that came with a cloned repo could run programs (e.g. from `rw doctor`) without `rw trust`. Its command settings now need trust like a repo's `.relayweft.yaml`.

**Verified for real on 4 Oct 2026 (macOS and Linux, on GitHub Actions runners):** with `.github/workflows/macos-real.yml` (manual; no secrets, fake agents only), on macOS 26.6.2 and 14.8.9 (Apple silicon) and 15.7.9 (Apple silicon and Intel) with Chrome 151 and 152, and Ubuntu 24.04 under Xvfb with fluxbox, dunst 1.9.2 and Chrome 154.
- **macOS notifications:** all seven test notifications (quotes, XML, non-ASCII, newlines, 2,000 characters, control characters) are in the Notification Center database with the right text, from Script Editor (`osascript`); on macOS 26 and 15 usernoted also logs each delivery. macOS stores a shortened body for very long ones.
- **notify-send:** seven Notify calls on the session bus, all held by dunst, shown on screen with the right text. Fixed: a NUL byte made the notification fail (`exec` rejects it), and the body was read as markup, so `<toast/>` vanished and `&amp;` showed as `&`.
- **`rw app` in Chrome:** opens a window titled Switchyard (now Relayweft), exits 5-6s after that window is closed, and leaves the other Chrome windows open; on macOS `open -n` hands the window to the running Chrome. Fixed on Linux: Ctrl+C or closing the terminal killed Chrome with all its windows when `rw app` or `rw web` had started it (it was in rw's process group).
- **Keep-awake on macOS:** a waiting `rw run --in` holds a `caffeinate -i -w <rw pid>` sleep assertion (seen in `pmset -g assertions`). It ends after Ctrl+C, and on its own when rw is killed. `--allow-sleep` takes none.
- **`sy update` on macOS:** a 0.0.1 build updated itself to release 0.2.0, which starts.

**Verified for real on 5 Oct 2026 (Windows 11, and Linux in Docker):**
- **First run:** from the v0.2.0 download (`sy`) to a finished first task (Claude haiku explaining a small repo) in a fresh profile: 13 seconds of machine time and three answers.
- **Linux packages and Homebrew:** the `.deb` (Ubuntu), `.rpm` (Fedora), `.apk` (Alpine) and the AUR PKGBUILD (Arch, makepkg and pacman) installed in Docker, and `sy update` named the package manager and changed nothing; the formula (then `switchyard`) installed with Homebrew on Linux, and passed `brew test`, `style` and `audit --strict`.
- **Affected tests on this repo:** 11 recent commits; with 5 fixes undone, the narrowed runs failed the same tests as the full suite.
- **Affected tests on real projects (6 Oct):** the recorded Jest, Vitest, pytest, Cargo, dotnet, Maven and Gradle runs covered 36 fix commits with their fixes undone; every full-run failure also failed the narrowed run. The findings fixed Jest index coverage, old Vitest versions, multiline Gradle includes and npx flag handling. See [the measurements and limits](docs/bench/2026-10-06-affected-real-projects.md).

**Verified on 6 Oct 2026 (real logs and live package feeds):**
- **Dashboard with real logs (4.30):** a copy of the Switchyard folder (11 sessions, 7 tasks) passed through first-start migration and was checked in headless Edge, across time ranges, project filters and the narrow layout. Its figures matched `rw stats`, `rw tune` and `rw health`. A logged-out CLI being counted as a limit hit was fixed. The merged [PR #60](https://github.com/Sparkz400/Relayweft/pull/60) also adds `TestDashboardMatchesCLI`, which compares generated logs against the CLI in normal CI.
- **Live signed Linux feeds (2.8):** [PR #62](https://github.com/Sparkz400/Relayweft/pull/62) records successful v0.3.0 package installation and v0.4.0 upgrades through apt (Debian stable and Ubuntu 24.04), dnf (Fedora 44) and apk (Alpine 3.22), with signature checks enabled. Completion, package-manager ownership, version reporting and removal were checked. A fresh Alpine arm64 install also passed under emulation. The [corrected Pages deployment](https://github.com/Sparkz400/Relayweft/actions/runs/37419578270) and [packaging CI](https://github.com/Sparkz400/Relayweft/actions/runs/37418242165), including Homebrew on macOS/Linux, passed. External winget/AUR publication and Windows/macOS binary signing remain open.

**Automated large-repository load coverage (6 Oct 2026):** [PR #60](https://github.com/Sparkz400/Relayweft/pull/60) adds `TestLoad` and [load.yml](.github/workflows/load.yml). The [2h30m run](https://github.com/Sparkz400/Relayweft/actions/runs/37394982495) passed on Ubuntu and Windows with three orchestrators sharing a golang/go checkout (16,061 tracked files plus Git LFS), scripted subprocess agents, cancels, resumes, undos and redos. Both reported zero leftover processes at quiet points and at most nine pool slots (about 1.3 GB). Resource assertions passed; heap and refs grew within the test's limits, so this is not evidence of perfectly flat usage. This closes the automated large-repository load gap, while sustained real-agent use on the user's hardware remains open.

**Not verified yet:**
- Codex: a real usage-limit hit. A logged-out CLI was recorded on 6 October using an isolated profile; its 401 stream now has a replay regression test.
- Real daily use on your Windows PC.
- Sustained heavy use with real agent CLIs on the user's Windows PC. Automated large-repository load coverage is recorded above; it does not close the daily-use or hardware-freeze exit criteria.
- A ~10-task comparison against a single agent on real, multi-file tasks ([docs/plan.md §9](docs/plan.md#9-measuring-success)). The starter set is too small to show it: on its one-file tasks a single agent is faster.
- A macOS notification banner on screen: the runners' screenshots never show one (Notification Center logs it "as banner", but the runner's screen is shared, which may hide banners). Delivery itself is verified.
- `rw app` in Edge on macOS and Linux (Chrome only), and on a real Linux desktop (GNOME/KDE) rather than Xvfb with fluxbox and dunst.
- Webhooks to real Slack and Discord (payloads checked against their current docs only).
- The container sandbox with Podman, with a signed-in Claude Code or Codex doing real work, and on Linux and macOS hosts outside CI.
- Best of N and affected-test selection with real agents; the JetBrains plugin driven on Windows or macOS.

**Open risk:** a full Windows freeze happened on 3 Oct while using `rw`.
- The logs show the same unexplained hard resets since August, before Relayweft existed, with no blue screen and no disk or memory exhaustion.
- So the most likely cause is the hardware or drivers failing under load, with Relayweft's load as the trigger.
- Phase 1 load limits are implemented and automatically tested; the no-freeze check on the same hardware remains an exit criterion.

---

## Phase 1 — Trustworthy (stabilize)

*Make it impossible for `rw` to hurt your machine, your repo or your quota, and make every failure diagnosable.*

**Status:** 1.2–1.5 and 1.7–1.10 are implemented, with automated coverage. The verification work in 1.1 and 1.6 is partial. The stress test, fuzzing and worktree-pool review are complete; the phase's real-use exit criteria remain open.

| # | Item | Implemented | Automatically tested | Verified in real use |
|---|---|---|---|---|
| 1.1 | Codex end-to-end recordings | Partial — limit/logout recordings missing | Covered — [recording/parser tests](internal/runner/runner_test.go) | Partial — real edit and resume; limit/logout pending |
| 1.2 | Load limits | Yes | Covered — [dispatch/checkout limits](internal/orchestrator/phase1_test.go) | Pending — sustained load on the user's PC |
| 1.3 | Disk guard | Yes | Covered — [disk/pruning tests](internal/orchestrator/phase1_test.go) | Pending — real disk-pressure use |
| 1.4 | Crash safety | Yes | Covered — [diagnostics](internal/diag/diag_test.go), [panic recovery](internal/orchestrator/phase1_test.go) | Partial — real debug logs; bugreport-only diagnosis gate open |
| 1.5 | Undo a task | Yes | Covered — [undo/redo tests](internal/orchestrator/phase1_test.go), large-repo load run above | Pending — real-agent undo/redo workflow |
| 1.6 | Windows real-use test pass | Partial — sleep/resume remains manual | Covered — [`TestSelftest`](cmd/rw/selftest_test.go), [`TestSelftestClose`](cmd/rw/selftest_test.go) (window close, `RW_TEST_CLOSE=1`) | Partial — recorded Windows checks; window close verified in conhost and Windows Terminal (6 October 2026); sleep/resume pending |
| 1.7 | Long-run stress test | Yes | Covered — [30-minute stress](.github/workflows/stress.yml) and 2h30m large-repo load run above | N/A — automated test deliverable; real-agent load gate remains |
| 1.8 | Parser fuzzing | Yes | Covered — [nightly fuzz workflow](.github/workflows/fuzz.yml) | N/A — automated test deliverable |
| 1.9 | Worktree-pool review | Yes — review and fixes complete | Covered — [review regressions](internal/orchestrator/pool_review_test.go) | N/A — review deliverable; daily-use gate remains |
| 1.10 | Health log and `rw health` | Yes | Covered — [health tests](internal/health/health_test.go), [`TestDashboardMatchesCLI`](cmd/rw/dashgen_test.go) | Recorded — real health logs checked in PR #60; two-week gate remains |

**Details and rationale:**

| # | Item | Why |
|---|---|---|
| 1.1 | **Real Codex end-to-end run** (partly done): real `codex exec --json`, edit/tool-call and `codex exec resume` runs are recorded as test fixtures (`internal/runner/testdata/codex_real_*.jsonl`, including `codex_real_edit.jsonl`). They match the parser and resume works. They also showed that Codex sends **no `rate_limits`**, so Codex can only fall back after a limit, not switch before it. Still to record: a limit hit and a logged-out CLI. | Recorded real output keeps parser and resume coverage grounded in the CLI. |
| 1.2 | **Load limits**: agents and git at below-normal priority, parallel checkout capped at min(4, cores/2), a busy gate that holds new agents above `max_cpu_percent` or below `min_free_memory_mb` (at most `busy_max_wait`). Was planned as: cap git's parallel checkout (`checkout.workers` = half the cores, at most 4), lower process priority for agents and git (`BELOW_NORMAL_PRIORITY_CLASS`), prewarm at most one pool slot at a time, and a `max_cpu_load` setting that pauses dispatch while the machine is pegged. | A freeze under load must never be triggered by `rw`. |
| 1.3 | **Disk guard**: `min_free_disk_gb` floor, pruning after `pool_max_idle`, `pool_warn_gb` warning, pools listed in `rw doctor`, one shared pool per repo. Was planned as: show the pool size in `rw doctor`, warn at more than X GB, prune slots unused for 14 days, and refuse to create a slot when free space drops under 10 GB. | Each pool slot is a full checkout of your repo. |
| 1.4 | **Crash safety**: `rw-debug.log`, `crash-*.log`, task-level panic recovery, `rw bugreport`. Was planned as: a panic handler that restores the terminal and writes `crash-<time>.log`; a persistent debug log (`%LocalAppData%\relayweft\logs\rw.log`) with every spawned command line, exit code and timing; `rw bugreport` zips the last session log, debug log, config and `doctor` output. | When something goes wrong, you can send one file and the reason is visible. |
| 1.5 | **Undo a task**: `rw undo` / `/undo` with preview, redo, later edits kept by 3-way merge, last 30 tasks. Was planned as: `rw undo` / `u` restores the working tree to the snapshot taken when the task started (the snapshot commit already exists), with a preview first. | It's the single biggest trust feature: trying a task becomes risk-free. |
| 1.6 | **Real-use test pass on Windows** (partly automated): `rw selftest` checks a user profile and project path with spaces and non-ASCII letters, the agent CLI behind a `.cmd` shim, many files plus Git LFS, OneDrive (detection, and `--onedrive` runs a task inside it), Defender on (status, exclusions, fresh-exe start time), and a `rw run` killed hard mid-task followed by `rw history`, `rw resume`, `rw undo` and redo. CI runs it on all three OSes (`TestSelftest`). It already found one bug: a normal end of `rw run` / `rw resume` printed the Ctrl+C notice. **Closing the window mid-task is automated too** (on Windows by default; `--close off` skips it, `--close only` runs just that; `TestSelftestClose` with `RW_TEST_CLOSE=1`, not in CI because it opens windows): `rw run` and the TUI each run the task in a window of their own, in the old console (`conhost.exe` hosting the command itself, so the default-terminal setting cannot hand it to Windows Terminal) and in Windows Terminal (`wt -w new` with a unique tab title; skipped with the reason when `wt.exe` is missing or the window cannot be identified). While the last step's agent works, the test posts `WM_CLOSE` to that window, as its X button does, so Windows sends `CTRL_CLOSE_EVENT`. It then checks that every process of the window (rw, the agent's `.cmd` shim, the agent, its child and their hidden consoles) has ended, that `rw history` lists the task as interrupted, that `rw resume` continues the agent's session where it ran, and `rw undo` and redo. Verified on this Windows 11 machine on 6 October 2026: all four cases pass (`rw run` ends within 50–100 ms with `0xC000013A`; the TUI exits cleanly; nothing left running within 100 ms; full `rw selftest` 70 ok, 0 failed). It found a bug, now fixed with a regression test (`TestTUISignals`): bubbletea turned the close (SIGTERM) into a normal quit, the TUI then cancelled the task, so `rw history` showed it as cancelled and `rw resume` refused it; now SIGTERM ends the TUI without cancelling and the task stays interrupted. Still by hand: sleep/resume during a task. Was planned as: Windows Terminal and the old console, a user name with a space, paths with spaces, OneDrive folders, a big repo with LFS, Defender on, sleep/resume during a task, closing the window mid-task. | These are where Windows tools usually break. |
| 1.7 | **Long-run stress test** (implemented: `internal/orchestrator/stress_test.go`, `.github/workflows/stress.yml`, demo and real-git modes with cancels). CI runs the orchestrator in a loop for 30 minutes and checks that memory, goroutines, open handles and leftover processes stay flat. | Catches leaks before you find them as freezes. |
| 1.8 | **Parser fuzzing** (implemented: 15 fuzz targets, nightly in `.github/workflows/fuzz.yml`; fixed a Codex error line counted as success, plan dependencies on reserved ids, reset-time overflow, `@` parsing and a change-list panic). Go fuzz tests for the Codex and Claude output parsers and for plan and verdict parsing. | Odd model output must never crash `rw` or leave it stuck. |
| 1.9 | **Review the worktree pool** (completed review: 17 findings fixed, including a crash leaving a slot that disabled worktrees, agent commits being lost, non-atomic apply, the Windows command-line limit, submodules, symlinks, git hooks in slots and orphan agents). It got the same adversarial pass that found the earlier bugs. | It is the newest and most complex code and has had only one review pass. |
| 1.10 | **Health log and `rw health`**: every `rw` writes start, end, load peaks, hangs, panics, agent timeouts and leftovers to a small `rw-health.log`; a watchdog dumps all stacks to `hang-*.log` when the TUI stops responding for a minute, and fatal runtime errors go to `fatal-*.log`. `rw health` (and **Health** in `rw web`) reads them and says whether the exit criterion below is met. | The exit criterion needs a record, not a memory. |

**Exit criteria:**
- 2 weeks of daily use with no crash, no hang and no lost work. `rw health` (or **Health** in `rw web`) shows the clean streak and the days of use; lost work is not in the logs and still needs your word.
- Every failure can be traced from `rw bugreport` alone.
- No machine freezes during heavy runs on the same hardware.

---

## Phase 2 — Useful every day

*Remove the reasons you'd fall back to plain `codex` or `claude`.*

| # | Item | Implemented | Automatically tested | Verified in real use |
|---|---|---|---|---|
| 2.1 | Plan approval | Yes | Covered — [orchestrator](internal/orchestrator/phase2_test.go), [TUI](internal/tui/phase2_test.go) | Pending — interactive real-agent workflow |
| 2.2 | Change review | Yes | Covered — [partial apply/feedback](internal/orchestrator/phase2_test.go), [TUI](internal/tui/phase2_test.go) | Pending — interactive real-agent workflow |
| 2.3 | Verification and affected tests | Yes — CLI restrictions below | Covered — [fix rounds](internal/orchestrator/verify_test.go), [affected tests](internal/affected/affected_test.go) | Partial — Claude fix round and real-project measurements; broader real-agent selection pending |
| 2.4 | Follow-up messages | Yes | Covered — [session resume/fallback](internal/orchestrator/phase2_test.go) | Recorded — Claude and Codex sessions on Windows |
| 2.5 | History and resume | Yes | Covered — [mid-step resume](internal/orchestrator/midstep_test.go), [resume regressions](internal/orchestrator/resume_review_test.go) | Partial — killed Claude resumed; real-CLI orphan cleanup remains open |
| 2.6 | Task queue | Yes | Covered — [queue tests](internal/tui/phase2_test.go) | Pending — unattended real-agent batch |
| 2.7 | Notifications | Yes | Covered — [notification tests](internal/notify/notify_test.go) | Partial — Windows, macOS delivery and Linux/dunst; macOS banner pending |
| 2.8 | Distribution | Yes — external publication/signing gaps below | Covered — [updater tests](cmd/rw/update_test.go), [packaging CI](.github/workflows/packaging.yml) | Partial — live feeds, self-update and Homebrew; external publication/binary signing pending |

**Details and rationale:**

| # | Item | Why |
|---|---|---|
| 2.1 | **Plan approval step**: the plan opens in the TUI (or on the terminal with `rw run --approve`). You can delete, reorder or edit subtasks, pin a role, or cancel. Turn it off with `/approve off`. | You stay in control of what runs, before any quota is spent. |
| 2.2 | **Review the diff before it lands** (`review_changes`): a per-file diff view where you can accept, accept only some files, reject, or send it back with feedback (the agent continues in its worktree). Rejected work is kept on a branch. | Bad edits are rejected before they reach your tree, not undone after. |
| 2.3 | **Agents can run tests safely**: `verify.commands`, detected by `rw init` (Go, npm/pnpm/yarn/bun, pytest, cargo, dotnet, Maven, Gradle). They become Claude `allowedTools`, are run by `rw` before the final review, and failures feed the fix round. Codex workers already run commands inside their workspace-write sandbox. After a fix round that is not the last (`max_fix_rounds` 2 or more), only the tests the changes affect run first (Go packages and their importers, jest/vitest related tests, npm/pnpm workspace packages, pytest by imports, cargo crates, dotnet test projects, Maven and Gradle modules, or a `verify.affected_commands` template); the full checks always run before the final review, and anything rw cannot tell runs in full. Measured on this repo: the same failures caught, wall time 0-16% lower (the orchestrator tests dominate). | Today Claude workers can edit but cannot verify their own work. |
| 2.4 | **Follow-up messages**: `@agent message` resumes that agent's CLI session (`codex exec resume`, `claude --resume`) in the pool worktree the agent ran in, falling back to a fresh agent with context. Finished agents only; a running agent is not interrupted. | Real work is iterative; today every follow-up starts a new task. |
| 2.5 | **Task history and resume**: the state of every task is saved after each step. `rw history` / `/history` list tasks; `rw resume` / `/resume` continue an interrupted one, skipping finished steps. Diffs per task come from `rw undo --list`. | Closing the window or a reboot no longer loses progress. |
| 2.6 | **Task queue**: submitting while a task runs queues it in the TUI (`/queue`). `rw run --file tasks.txt` runs a list overnight. Queued tasks run unattended. | Uses quota while you're away. |
| 2.7 | **Notifications**: a desktop notification when a task finishes or fails, a limit is hit, or `rw` waits for you (Windows toast, macOS, notify-send). | You don't have to watch the terminal. |
| 2.8 | **Distribution** (release pipeline used for v0.1.0 through v0.4.0): a tag builds release binaries for Windows, Linux and macOS with checksums. There are Scoop and winget manifests, a Homebrew tap in this repo (`brew install relayweft`), `.deb`, `.rpm` and `.apk` packages on each release (from v0.3.0), an AUR PKGBUILD (`relayweft-bin`), and `rw update` (checksum-verified, swaps the running .exe safely on Windows, and points to the package manager that installed `rw` instead of replacing its binary). Code signing needs a certificate: see `packaging/README.md`. Releases carry build provenance, SBOMs and a Sigstore-signed `checksums.txt` (from v0.4.0). | Installing no longer needs Go or a build. |

**Gaps closed since:**
- Dependencies can be edited in the plan view (`x`).
- Review works per hunk.
- Review warns when no worktree is possible.
- Feedback in the last round rejects instead of applying.
- Follow-ups survive restarts and reach running agents.
- A resumed step is told that it was interrupted.
- A step that was running when `rw` died continues its agent's own session, in the folder it ran in, with its half-done edits. The session id is saved as soon as the CLI reports it. Without a usable session, a fresh agent takes over.
- A follow-up to an agent that ran in a pool worktree (Claude or Codex) resumes it in that worktree. A stopped follow-up's work is kept on a branch.
- On Linux and macOS, killing `rw` now stops its agents and their children in the same process group through a pipe-watching shell wrapper, as the Windows job already does. Resume still continues the agent's session with its half-done edits. Verified with scripted agents on Linux (WSL and CI), macOS (CI) and Windows; real Claude Code and Codex have not been checked. A process that moves out of its process group (`setsid`) can escape cleanup. ([#53](https://github.com/Sparkz400/Relayweft/pull/53))
- The half-done edits of an interrupted step are saved on a branch (`rw/<task>/<step>-unfinished`) before its worktree is freed after 7 days or removed by `rw clean`. `rw history` and `rw resume` say where they are, with commands that work in Windows PowerShell 5.1 too.
- The worktree pool keeps its size: a step cancelled before its agent changed anything holds no worktree, and a full pool gives up the oldest held worktree after saving its edits on a branch. A resume racing that waits for it, and edits a step left behind elsewhere are saved before reuse. The 30-minute stress test passes again (it had failed since mid-step resume).
- A guided first run (`rw setup`): it finds the agent CLIs, checks versions and logins without using quota, says how to install or log in, writes the config with the ready ones and offers a read-only first task. `rw`, `rw run` and `rw web` start it when there is no config (not in CI, or with `RW_NO_SETUP=1`).
- Codex resumes get an explicit sandbox.
- The repo has an MIT license.
- **2.8 Linux packages:** signed live-feed installation and upgrades are verified ([PR #62](https://github.com/Sparkz400/Relayweft/pull/62); see the 6 Oct record above). Published package signatures and the downloader regression fix were also checked.

**Remaining gaps:**
- **2.3 Tests:** Codex runs commands in its own sandbox, not from an allowlist, so `verify.commands` can only restrict Claude. Codex offers no allowlist, so this is a limit of the CLI.
- **2.3 Affected tests:** [real-project coverage](docs/bench/2026-10-06-affected-real-projects.md) now includes [Yarn PnP and Gradle layout checks](docs/bench/2026-10-07-affected-layouts.md). Plain PnP and Groovy/Kotlin `projectDir` layouts narrow; PnP workspaces with peers, changed runtime metadata, and unread Gradle dependencies run in full. Still open: published monorepos and native task-graph validation before automatic Nx/Turbo narrowing. Yarn `workspaces foreach`, turbo/nx, vitest plugins other than react, react-swc, vue and tsconfig-paths, and jest custom resolvers still run in full.
- **2.8 Distribution:**
  - Code signing of the Windows and macOS binaries needs a certificate (Authenticode, Apple notarization). Release integrity does not: from v0.4.0 every asset has GitHub build provenance, each binary a CycloneDX SBOM (attested), and `checksums.txt` a keyless Sigstore signature (`packaging/README.md`, "Verifying a release"). Verified on the published v0.4.0 release and in two signed dry runs.
  - Released v0.4.0 checks SHA-256 and prints the provenance verification command. The local post-v0.4.0 batch verifies attestations itself through `gh`, binding the binary and checksum provenance to the release workflow and tag commit. The live verification test accepts the published assets and rejects tampered bytes; this local change is not released yet.
  - The Scoop, winget, Homebrew and AUR manifests are rendered for v0.4.0; Homebrew and AUR now always install completion. Rendering remains necessary after each release. Winget and AUR publication still need their external submissions.

**Exit criteria:**
- You reach for `rw` before plain `codex` or `claude` for multi-step work.
- ✅ A new user goes from install to first task in under 5 minutes. `rw setup` checks the CLIs and logins without quota, writes the config and runs a read-only first task. Measured on Windows: download 1.8s, setup 1.6s of rw's own time, first task with Claude haiku 8.8s (13s in all, plus three answers). `TestOnboarding` times it in CI on all three OSes with scripted agents.

---

## Phase 3 — Smarter (measure, then tune)

*Prove that Relayweft beats a single agent, then make it better on data.*

| # | Item | Implemented | Automatically tested | Verified in real use |
|---|---|---|---|---|
| 3.1 | Benchmark command | Yes — includes [ten-task suite](docs/bench/realistic.md) | Covered — [bench isolation](internal/orchestrator/bench_test.go), [history tasks](cmd/rw/benchhistory_test.go), [fair matrix](cmd/rw/bench_fair_test.go) | Partial — starter measurements and validated multi-file corpus; real-model multi-file comparison pending |
| 3.2 | Quota-aware scheduling | Partial — proactive Codex switching awaits CLI data | Covered — [preemption/retry](internal/orchestrator/phase1_test.go) | Partial — Claude quota recorded; real switching/limit scenarios remain |
| 3.3 | Rule tuning | Yes | Covered — [tune tests](internal/sessionlog/tune_test.go) | Partial — real-log reconciliation; failure-rich tuning evidence pending |
| 3.4 | Judge cost/gain measurement | Yes | Covered — [tune comparisons](internal/sessionlog/tune_test.go) | Pending — real judge cost/gain comparison |
| 3.5 | Context hand-off | Yes | Covered — [handoff tests](internal/orchestrator/handoff_test.go) | Partial — starter benchmark; larger-task benefit pending |
| 3.6 | Cost visibility | Yes | Covered — [task cost](internal/orchestrator/phase1_test.go), [stats](internal/sessionlog/stats_test.go) | Recorded — benchmark tokens/costs and real-log dashboard reconciliation |
| 3.7 | Model tiers | Yes | Covered — [tier rules](internal/router/tiers_test.go) | Partial — [Claude/Codex starter runs](docs/bench/2026-10-04-tiers-claude.md); multi-file value pending |
| 3.8 | Best of N | Yes | Covered — [selection](internal/orchestrator/bestof_test.go), [review regressions](internal/orchestrator/bestof_review_test.go) | Pending — real-agent pass-rate measurement |
| 3.9 | Merge-conflict resolution | Yes | Covered — [real git with scripted agents](internal/orchestrator/resolve_test.go) | Pending — real agent CLIs |

**Details and rationale:**

| # | Item | Why |
|---|---|---|
| 3.1 | **Benchmark command**: `rw bench` with `bench.yaml`, check commands, routed vs single, saved results, `rw bench --starter` (five Python tasks with check scripts), and `rw bench --from-history` (real tasks from past multi-file commits, checked by the repo's tests with the commit's test files in place, each validated to fail before and pass after). Every run starts in a fresh repository with only the starting commit's files, so agents cannot find a solution in the history. `--own-tests` (or `{tests}`/`{test_dirs}` in the check) runs only the commit's tests. | This is the success measure from [docs/plan.md §9](docs/plan.md#9-measuring-success), automated. |
| 3.2 | **Quota-aware scheduling** (done early, Claude; Codex as soon as its CLI reports `rate_limits`): `quota-preempt` at `switch_at_utilization`, and the planner and reviewer retry on the other provider. Was planned as: use Claude's live 5-hour and 7-day utilization (already received) and Codex limits to move work to the other provider *before* hitting the limit, not after. | Avoids stalls entirely. |
| 3.3 | **Rule tuning from stats**: `rw tune` flags failing routes, frequent escalations, rejected reviews, quota pressure and over-sized read-only models, and prints the `/route` / `/prefer` command for each. | Routing improves from your own data. |
| 3.4 | **Judge model** (measurement): decisions record whether the judge ran, and `rw tune` compares judged with rule-routed steps to suggest `/judge on` or `/judge off`. | Spend quota only where it pays. |
| 3.5 | **Context hand-off**: a repo map and notes from earlier tasks in the same repo go into planner and step prompts, and every writer gets what this task's read-only steps found. | Fewer tokens, faster workers. |
| 3.6 | **Cost visibility**: fresh tokens per provider, Claude API-equivalent $, and limit before and after, in the TUI, `rw run` and `rw stats`, plus a per-day table in `rw stats`. | You can see what each task cost. |
| 3.7 | **Cost-aware model tiers** (`routing.tiers: auto`, off by default): the rules still pick the role; a work step's model then comes from its estimated difficulty (role, files, prompt size, routine or hard words) and the quota left (the provider's reported limit, and the task, day and team budgets). The tiers reuse the explorer, worker and worker_high routes. Planner, reviewer, judge and explicitly set roles never move; risky steps keep their floor; a local model on standby keeps its route. Measured on the starter set: the tiers kept Sonnet for 6 of 7 steps, and the cost stayed the same within noise. Routine words now count only in a step's title. | Easy steps stop paying for strong models, and a nearly spent quota stretches further. |
| 3.8 | **Best of N for hard steps** (`routing.best_of`, off by default; `b` in the plan view): a writing step runs on two to four routes at once (default: its own route and the same role on the next provider), each in its own pool worktree from the same commit. `verify.commands` run in full in each worktree, one at a time. Passing checks win (not a candidate that changed nothing over one that changed something); otherwise the reviewer compares the diffs (untrusted, named A and B in shuffled order, not by provider); otherwise a fixed order (fewer failing checks, a change, the smaller diff, the cheaper run). The winner lands like any step and change review sees only it; every candidate's work is kept on a branch until the winner has landed. `when: hard` reuses the router's risk rules and the tiers' difficulty score. Providers at or near their limit are left out; candidates run at once only within `max_threads` and when the machine is not busy; the estimate counts every candidate. A best-of step that rw stopped before the pick runs again as a whole on resume; from the pick on, the winner resumes like any step. An untrusted repo file or `./relayweft.yaml` may lower `best_of`, not raise it. `best_of` records feed `rw tune` and the learned routes (a loss on checks or by the reviewer counts against the route); `rw bench` has a `routed-bestof` mode. Not yet measured. | A second opinion where it matters: on hard steps the checks, not one agent, decide which change lands. |
| 3.9 | **Resolve merge conflicts** (`orchestrator.conflicts`, default `auto`; [docs/conflicts.md](docs/conflicts.md)): a resolve agent merges conflicting writers in a pool worktree, with both sides' intents and checks. Conflicts with your own edits ask first; unattended and MCP tasks keep them on branches when approval is needed. rw checks markers, retained work and verification results, supports change review, and preserves both sides and failed attempts for resume and manual recovery. Binary, LFS, symlink and submodule conflicts stay manual. Tested with real git and scripted agents; not yet with real agent CLIs. | Conflicting parallel writers can finish without losing either change. |

**Gaps closed since:**
- `rw tune` reads models, efforts and the fast tier from your config.
- It compares the judge's own tokens with the retries the judge saved.
- `routed-nohandoff` measures the hand-off; the first run showed a small saving.
- `rw bench --from-history` builds the multi-file benchmark from your own history instead of hand-written tasks.
- Notes are dropped when they are older than 60 days or all their files are gone.
- Bench runs no longer share the repo's history: `git log --all` or `git show <sha>` in a run found the solution of a history task. Each run now gets a standalone repository with one commit of the starting files.
- `--own-tests` narrows a history task's check to the commit's own tests, so one long-failing test no longer fails every candidate.
- `rw bench` no longer prints the Ctrl+C notice at a normal end.
- `rw tune` counts an escalated step once, however often it repeats, and needs limit switches in two episodes 12 hours apart.
- A task that does not look multi-file, multi-part, broad, hard or sensitive runs as one worker step without the planner (`orchestrator.auto_single`). Checks and the final review still run, and `rw run --plan` always plans.
- The planner and reviewer of a task that does not look hard use the worker route (`light_planning`). The final review is skipped for a small change whose checks pass (`review_skip_max_lines`).
- With a budget, the planner is told what is left, and a plan over it is shrunk without dropping work (`fit_budget`). A one-step task probes only its own provider before it runs.

**First measurements** ([docs/bench](docs/bench/2026-10-03-starter-claude.md), [tiers](docs/bench/2026-10-04-tiers-claude.md)):
- On five small one-file tasks, a single Claude agent was about 3x faster and used half the tokens.
- As a result, one-step plans now skip the plan review. That cut routed time by 32% and tokens by 23%.
- With `--tiers`, 6 of 7 small work steps kept Sonnet and one went to Haiku. Cost and correctness were the same; the planner and reviewer are most of a routed task's cost. A single agent was still about 2x faster on these tasks.
- Whether routing pays off on bigger tasks is still open (exit criterion below).
- **After the task-shape shortcuts** ([record](docs/bench/2026-10-06-phase3-shortcuts.md)):
  - Starter set: routed 5/5 in 32s average and 72k tokens; single Codex (high) 5/5 in 41s and 70k; the old pipeline (`routed-classic`) 5/5 in 1m12s and 127k.
  - Two realistic tasks: routed 0/2 in 4m50s average and 157k tokens; single 0/2 in 9m48s and 209k; classic 0/2 in 9m23s and 323k.
  - That is 2 of 3 measures on this run, but with one repetition and no realistic pass in any mode. Routed mostly ran one Codex worker at medium effort against a high-effort single agent.
- **Evening follow-up** (17 more runs with a medium-effort single agent, then with the repo's tests as checks, a requirements checklist and fix-session continuation; same record):
  - Correctness was the same: both modes passed `resume-undo-coverage` and failed the other tasks.
  - The planner never ran, the checks always passed on the first try, and no fix round started.
  - On the realistic tasks, 9 of 10 final reviews approved, 7 of them approving work that fails the hidden tests. Each review cost 11k-62k Claude tokens.
- **Conclusions:**
  - rw is not more correct than one agent: with the current models the worker's first attempt decides the result.
  - rw is cheaper and faster than its old pipeline and than a high-effort agent, thanks to its defaults (medium effort, no planner for one-step tasks, checks in the loop), not to coordinating agents.
  - Tests in the prompt look like the real correctness lever, for a single agent as much as for rw.
  - The final review is the clearest waste of tokens.

**Remaining gaps:**
- `rw tune`'s rates now need a clear majority from few runs (90% confidence bound, on both sides when routed is compared with single): 3 failures in 5 runs, 4 in 10, 7 in 20. The recommendation thresholds still need validation against failure-rich real workloads.
- Whether tiers pay off on multi-file tasks: run `rw bench --from-history` with and without `--tiers`.
- Whether best of N raises the pass rate: run `rw bench` with `routed-bestof` against `routed` and `single`.
- Tests replace the final review (7 October): `independent_tests` is on and `review_when: untested` is the default, so the review runs only on a task without checks. Not measured live as a default yet: compare `routed` with `routed-review` on tasks some mode can pass.
- The independent test writer is built two ways: `orchestrator.independent_tests` (hidden tests written next to the work, now on by default in place of the final review; `rw bench` mode `routed-tests` and `--replay-tests`) and `rw run --tests-first` (tests written before any code, which the work is written against; mode `routed-tests-first`). Measure both on tasks that fail today. Tests first, measured live on 6 October ([docs/bench/2026-10-06-tests-first.md](docs/bench/2026-10-06-tests-first.md)): no failing task turned into a pass, at 1.1x to 1.5x routed rw's tokens, so `tests_first` stays off. The writer may now run the configured checks' test runner with any arguments (it was refused in 3 of 4 runs and wrote blind); next: measure again.
  - Softer gate (`orchestrator.independent_tests_gate: soft`, the default): writer tests start one fix round, then are advisory; on the replay that keeps a fix round on all 7 catches and fails 0 of 8 correct results instead of 3, but whether the fix round turns a catch into a pass is unmeasured ([evaluation](docs/bench/2026-10-06-independent-tests.md#softer-gate)).
- Whether rw reaches the usage limits later (the third measure) is untested: every bench run is far below any limit. Measure a day of queued tasks on one provider against rw spreading them over both.
- `dashboard-auth-failures` cannot be scored on this machine: its solution commit fails its own check.

**Exit criteria:** on the benchmark, Relayweft beats a single agent on at least 2 of the 3 measures: correctness, wall time, and how quickly the limits are reached.

> **Proposed rewording (October 2026, not yet decided):** the data above does not support "beats a single agent" on correctness. A criterion that matches what rw is: no worse than a single agent on correctness, cheaper or faster on the benchmark, and reaching the usage limits later in daily use.

---

## Phase 4 — Nicer and broader

| # | Item | Implemented | Automatically tested | Verified in real use |
|---|---|---|---|---|
| 4.1 | Browser UI | Yes | Covered — [web tests](internal/web/web_test.go), headless-browser checks above | Partial — real follow-ups and logs; remaining interactive workflows pending |
| 4.2 | Desktop window | Yes | Covered — [Windows](internal/web/open_windows_test.go), [Unix](internal/web/open_unix_test.go) | Partial — Windows Edge and runner Chrome; Linux desktop/other browsers pending |
| 4.3 | More providers | Yes — beta | Covered — [provider recordings/protocols](internal/runner/providers_test.go), [generic CLI](internal/runner/generic_test.go) | Partial — Qwen/Ollama; signed-in Gemini and DeepSeek pending |
| 4.4 | Per-repo profiles | Yes | Covered — [profiles](internal/config/repo_test.go), [trust](cmd/rw/localtrust_test.go) | Pending — real-use profile/trust workflow |
| 4.5 | Hooks | Yes | Covered — [hooks and token isolation](internal/orchestrator/phase2_test.go) | Pending — real-use hooks |
| 4.6 | Hunk-level review | Yes | Covered — [orchestrator](internal/orchestrator/phase2_test.go), [TUI](internal/tui/phase4_test.go) | Partial — real IDE integration with scripted agents; real-agent review pending |
| 4.7 | Talk to a running agent | Yes | Covered — [message delivery](internal/orchestrator/phase2_test.go) | Pending — running real-agent workflow |
| 4.8 | Persistent follow-ups | Yes | Covered — [restart persistence](internal/orchestrator/phase2_test.go) | Partial — real session resumes; cross-restart workflow pending |
| 4.9 | Scheduled runs | Yes | Covered — [scheduling](cmd/rw/schedule_test.go), [keep-awake](internal/proc/awake_test.go) | Partial — macOS sleep assertion; unattended real-agent scheduling pending |
| 4.10 | Task reports | Yes | Covered — [report tests](internal/report/report_test.go) | Pending — real-task report review |
| 4.11 | MCP servers | Yes | Covered — [CLI MCP configuration](internal/runner/mcp_test.go) | Pending — real-agent MCP server use |
| 4.12 | Multi-repo tasks | Yes | Covered — [workspace tests](internal/orchestrator/workspace_test.go) | Pending — real-agent multi-repo workflow |
| 4.13 | Pull requests | Yes | Covered — [PR tests](cmd/rw/pr_test.go) | Partial — live self-hosted forges with scripted agents; real-agent workflow pending |
| 4.14 | Issues as tasks | Yes | Covered — [issue tests](cmd/rw/issue_test.go) | Partial — live self-hosted forges with scripted agents; real-agent workflow pending |
| 4.15 | Budgets | Yes | Covered — [budget tests](internal/orchestrator/budget_test.go) | Pending — real-agent budget enforcement |
| 4.16 | Watch PRs | Yes | Covered — [watch tests](cmd/rw/watch_test.go) | Partial — live forge/Runner checks with scripted agents; real-agent follow-ups pending |
| 4.17 | Review PRs | Yes | Covered — [review tests](cmd/rw/review_test.go) | Partial — live self-hosted forges with scripted agents; real-agent review pending |
| 4.18 | Learned routing | Yes | Covered — [learning](internal/sessionlog/learn_test.go), [routing](internal/router/learned_test.go) | Pending — learned routes from sustained real work |
| 4.19 | Team budgets and stats export | Yes | Covered — [team budgets](internal/orchestrator/team_test.go), [export](cmd/rw/statsexport_test.go) | Pending — shared-folder team use |
| 4.20 | Repo conventions as context | Yes | Covered — [context](internal/orchestrator/repodocs_test.go), [PR templates](cmd/rw/prtemplate_test.go) | Pending — real-agent convention-following |
| 4.21 | Cost estimates | Yes | Covered — [estimates](internal/orchestrator/estimate_test.go) | Pending — estimated versus actual costs on real work |
| 4.22 | VS Code extension | Yes | Covered — [unit/integration workflow](.github/workflows/vscode.yml) | Recorded — real VS Code 1.140/1.90 integration suite |
| 4.23 | GitLab and Gitea/Forgejo | Yes | Covered — [GitLab](internal/forge/gitlab_test.go), [Gitea/Forgejo](internal/forge/gitea_test.go) | Partial — local live servers/Runner; hosted-service use remains unrecorded |
| 4.24 | Webhooks | Yes | Covered — [local-server tests](internal/notify/webhook_test.go) | Partial — real ntfy.sh; Slack/Discord pending |
| 4.25 | Bench-fed learned routes | Yes | Covered — [bench learning](cmd/rw/benchlearn_test.go) | Pending — real benchmark updates to learned routes |
| 4.26 | CI as an agent target | Yes | Covered — [token isolation](internal/proc/env_test.go); recorded CI-script checks below | Partial — real Forgejo runner with scripted agent; other runners/real agent pending |
| 4.27 | Shared issue queue | Yes | Covered — [fake-forge team queue](cmd/rw/teamqueue_test.go) | Pending — two real machines |
| 4.28 | Container sandbox | Yes | Covered — [sandbox selftest](cmd/rw/selftest_sandbox_test.go) | Partial — Windows Docker and signed-out Claude; Podman/signed-in work pending |
| 4.29 | JetBrains plugin | Yes | Covered — [unit, IDE and UI workflow](.github/workflows/jetbrains.yml) | Partial — real IntelliJ on Xvfb with scripted agent; Windows/macOS pending |
| 4.30 | Dashboard | Yes | Covered — [`TestDashboardMatchesCLI`](cmd/rw/dashgen_test.go), [aggregation](internal/sessionlog/dashboard_test.go) | Recorded — real logs in headless Edge, PR #60 |
| 4.31 | Azure DevOps | Yes — including [Pipelines template](ci/azure-pipelines.yml) | Covered — [fake API](internal/forge/azure_test.go), [CLI flow](cmd/rw/azure_test.go), [job tests](ci/templates_test.go) | Pending — hosted pipeline, organization, Entra login and Server deployment |

**Details:**

| # | Item |
|---|---|
| 4.1 | **`rw web`**: a local browser UI on the same engine. It has the agent tree, the activity log, the plan editor, hunk review, models and routes, settings, history, the queue and stats/tune. It listens on 127.0.0.1, with a per-run token and strict Host/Origin checks. |
| 4.2 | **Desktop window**: `rw app` opens `rw web` in Edge or Chrome app mode instead of using Wails. That means no cgo and nothing to install, and it closes when the window does. |
| 4.3 | **More providers** (built, beta): a provider is a name plus the CLI protocol it speaks (`kind: codex \| claude \| gemini \| qwen \| generic`), set up in config with its own `command` and `env`. `kind: generic` describes any other CLI in config alone: its arguments, text or JSON-lines output (rules for the answer, session, tools and tokens), resume and limit detection; descriptions of Qwen Code and Gemini CLI in that format read their recordings exactly like the built-in kinds. Presets (disabled by default): Gemini CLI, Qwen Code on local Ollama, DeepSeek, any Ollama model through Claude Code, and a plain `ollama run` model (generic). `standby: [roles]` lets a free local model take cheap read-only work once Codex and Claude are at or near their limits (Qwen Code: explorer and researcher; plain Ollama: the judge). Routing walks `routing.provider_order` instead of "the other provider"; `only_preferred` keeps slow local models out of fallbacks. Qwen Code and Claude-on-Ollama are recorded and were run end to end; Gemini has only a signed-out recording (the stream format comes from its source); DeepSeek is unrun (needs a key). See [docs/providers.md](docs/providers.md). |
| 4.4 | **Per-repo profiles**: a `.relayweft.yaml` in the repo is layered over your config. The parts that run commands need `rw trust`. Create one with `rw init --repo` or `/save repo`. |
| 4.5 | **Hooks**: `before_task` (a failure stops the task), `after_merge` and `after_task`, with `RW_*` environment variables. |
| 4.6 | **Hunk-level review**: in the TUI and in `rw web`. |
| 4.7 | **Talk to a running agent**: `@agent message` is delivered when the agent's turn ends, before its work is merged. |
| 4.8 | **Persistent follow-ups**: sessions are kept per project folder. |
| 4.9 | **Scheduled runs**: `rw run --at 02:30 / --in 3h / --when-reset claude` (task file or one task), `/schedule` in the TUI and the queue panel in `rw web`. The PC is kept awake while a scheduled run waits and runs; `rw schedule` prints a Task Scheduler / cron line. Plus **budgets** per task and per day (tokens and API-equivalent $). |
| 4.10 | **Task reports**: `rw report` writes one self-contained HTML (or `--md` Markdown) page per task: plan and results, routing decisions with rule and reason, reviews, checks, the diff and the cost. Everything is escaped, and a CSP blocks scripts. |
| 4.11 | **MCP servers**: an `mcp:` config section passes MCP servers to both CLIs per role (Claude: a temporary `--mcp-config` file; Codex: `-c mcp_servers.*`). `${VAR}` comes from your environment, repo files need `rw trust`, and `rw doctor` checks the commands. |
| 4.12 | **Multi-repo tasks**: `--repo name=path` or `workspace: repos:` in `.relayweft.yaml`. The planner assigns each subtask a repo; writers run in that repo's worktree pool or main tree; each repo's own (trusted) checks run there; the final review sees every repo's diff; one `rw undo <key>` reverts every repo; history and resume keep the repos. |
| 4.13 | **`rw pr`**: a finished task becomes a branch, a commit and a GitHub pull request, with the plan, checks and cost in the description. Your index, working tree and branches are never touched. A multi-repo task gets one PR per repo (`--repo`). |
| 4.14 | **Issues → tasks**: `rw run --issue 42` works on a GitHub issue. `--issues label:rw --pr` works through labelled issues one after another, opens a PR for each with "Closes #N", and comments on the issue. Combined with scheduled runs, this works overnight. |
| 4.15 | **Budgets**: token and $ limits per task and per day (`budget:`). `rw` asks before going over; unattended runs stop instead. |
| 4.16 | **Watch PRs**: `rw watch [--every 15m]` turns failed checks and review comments on PRs rw opened into a follow-up task on the PR branch (separate checkout, never forced, `watch.max_rounds`). |
| 4.17 | **`rw review <PR>`**: a read-only second-opinion review by the other provider; `--post` posts it as one comment review, inline where possible. |
| 4.18 | **Learned routing**: `rw tune --apply` (or `routing.learn: auto`) stores per-repo routes from your logs and bench, on clear evidence only; explicit settings always win, and decisions say when a learned route was used. |
| 4.19 | **Team budgets and stats export**: `rw stats --json` / `--merge`, and `budget.team` over a shared folder. |
| 4.20 | **Repo conventions as context**: CONTRIBUTING, the PR template, CODEOWNERS, CI commands and AGENTS.md go to the planner and reviewer as untrusted text; `rw pr` fills the PR template. |
| 4.21 | **Cost estimate before approval**: per step and total, against the remaining budget; `rw run --estimate`. |
| 4.22 | **VS Code extension** (`editors/vscode`): a thin client for `rw web --client` with the agent tree, plan approval and hunk review in the diff editor. Built and unit-tested, and tried in real VS Code 1.140 and 1.90 with an integration suite (`npm run test:integration`). |
| 4.23 | **GitLab and Gitea/Forgejo** (`internal/forge`): `rw pr`, issues as tasks, `rw watch` and `rw review` work on gitlab.com and self-managed GitLab (merge requests, pipeline jobs and their logs, unresolved diff comments by Developers and above, one thread per inline finding) and on Gitea and Forgejo, Codeberg included (commit statuses, reviews requesting changes, inline reviews). The origin remote's host picks the forge; self-hosted ones are named in `GH_HOST` / `GITLAB_HOST` / `GITEA_HOST`, and each forge's token goes only to its own hosts. GitLab quick actions are defused like mentions, and `rw watch` never pushes CI config of any forge. Tested against fake APIs and, for real, against Gitea 28, Forgejo 16 and GitLab CE 19.4 with a GitLab Runner. |
| 4.24 | **Webhook notifications** (`notify.webhooks`): Slack, Discord, ntfy or plain JSON, so overnight runs and `rw watch` reach your phone. Events `done`, `failed`, `limit`, `waiting` and `watch` (round results, merged or closed PRs), filterable per webhook; a task-file batch ends with a summary. `${VAR}` keeps the secret URL out of the file; a repo file's webhooks need `rw trust`; text is escaped against mentions and hidden links; errors and `rw bugreport` never show the URL path. `rw notify --test` checks each webhook. Tested against local servers and real ntfy.sh; not yet against real Slack or Discord. |
| 4.25 | **Bench results feed learned routes**: a bench mode `routed:<role>=<provider:model[:effort]>` runs the routed pipeline with one role on another route, so the learner gets an alternative to compare with (single-agent runs never counted). `learn: true` in a bench file, or `--learn`, updates the repo's learned routes when the bench ends, with the same clear-evidence rules as `rw tune --apply`. `rw bench --from-history` writes `learn: true` and a worker variant on the other provider, so one bench of your own history can change the routing with no manual tuning. `--no-learn` and `routing.learn: off` keep the routes as they are; a cancelled bench learns nothing. |
| 4.26 | **CI as an agent target** (`action.yml`, `ci/`, [docs/ci.md](docs/ci.md)): a GitHub Action, a GitLab job and a Forgejo/Gitea Actions workflow (`ci/forgejo-workflow.yml`) run `rw run --issue N --pr` or `--issues label:rw --pr` in CI (on a label, nightly or by hand), so tasks run without your PC. The action installs the release named by its ref (checksum-verified) or builds from source. Agents, verify commands, hooks and bench checks start without the forge and CI tokens; git pushes through a credential helper, not `.git/config`. Reports go to the job summary and an artifact; `rw history --json` lists a run's tasks. Tested locally (install against the real v0.1.0 release, the step scripts with a stub `rw`, token scrubbing); not yet run on real GitHub or GitLab runners. The Forgejo workflow ran on a real Forgejo 16 with forgejo-runner v12 in Docker, with rw (built from this change and from the v0.2.0 tag) and a scripted `claude` stand-in installed from a release on that Forgejo (checksum-verified): a labelled issue, a scheduled batch and a manual run each opened a pull request (`Closes #N`) and commented on the issue; the stand-in saw only its model key. rw finds the forge from the job's server, and the job's token never goes to GitHub. On a private repository the job's own token cannot open the pull request (Forgejo 16); a `RELAYWEFT_TOKEN` secret can. Not yet tried on Gitea or Codeberg. |
| 4.27 | **Team mode: a shared issue queue**: `rw run --issues label:rw --pr --team [--every 10m]` on several machines pulls from the same label. Each issue is claimed with one comment right before it runs; the earliest live claim by a trusted author wins, the lease is renewed while the task runs (`--lease`), and the comment ends as done (PR link), failed or released. Failed issues wait for `--retry-failed`. Works on GitHub, GitLab and Gitea. Tested against fake forges, not yet with two real machines. |
| 4.28 | **Container sandbox** (`sandbox:`, off by default; [docs/sandbox.md](docs/sandbox.md)): agents, the verify commands, `after_merge`/`after_task` hooks and bench checks run in a docker or podman container with only the step's folder writable. The repository's git folder is mounted read-only (a generated `.git` file for pool worktrees, a git config without remotes or helpers), each provider gets its own `HOME` so sessions resume (command-running settings files are cleared before each run), and `/work/...` paths are turned back into host paths. Only named variables and read-only credential files go in; rw's forge and CI tokens never do (also not through MCP configs). Every host git command in a folder an agent wrote to runs with `core.fsmonitor=false` and without submodule recursion, and a changed submodule `.git` fails the run, so files an agent writes cannot make git on the host run code. Project settings files an earlier agent changed (`.claude/settings*.json`, `.mcp.json`, `.codex/config.toml`) are shown to the next CLI in their starting version. Per provider and per role; a repo file may make it stricter without `rw trust`, never weaker. Cancel and timeouts `docker kill` the container, a wrapper ends it when rw dies, and a sweep removes leftovers. A missing runtime, image or CLI fails the step; nothing runs on the host instead. `rw doctor` checks it, `rw selftest --sandbox` runs it with a scripted agent (required in CI on Linux), and `packaging/sandbox` has the reference image. Two security reviews; run for real on Windows with Docker Desktop (pool worktrees, verify, timeout, hard kill, a planted submodule `.git`, real Claude Code without sign-in). |
| 4.29 | **JetBrains plugin** (`editors/jetbrains`): a thin client for `rw web --client` in IntelliJ IDEA, PyCharm, GoLand, WebStorm, Rider and the other JetBrains IDEs (2025.2+), with the agent tree, the activity log and a prompt box in a tool window, plan approval (edit, add, delete, reorder), and hunk review in the IDE's diff viewer (gutter icons, strike-through, CRLF checkouts as whole files). Same login and security as the VS Code extension; settings are IDE-wide, never per project, and an untrusted project never starts `rw`. Unsaved edits are saved before `rw` applies changes. Tested against a fake server, a real rw with a scripted agent, and in a headless IDE; the Plugin Verifier passes on IDEA 2025.2, 2025.3 and 2026.2. A UI test drives it in a real IntelliJ IDEA on Xvfb (tool window, plan dialog, Review tab, diff, apply). Not on the Marketplace yet (needs an account; steps in its README). |
| 4.30 | **Dashboard in `rw web`**: a panel over 7, 30 or 90 days, for this project or all. It shows tasks per day (done, failed, cancelled) with the success rate; fresh tokens per provider and API-equivalent $ per day against the daily and team budget; success, average tokens, $ and time per role and route, with escalations, rejected final reviews, decision flags and what `rw tune` flags; learned-route changes with their evidence; Claude's 5-hour and 7-day use, limit hits and switches per provider; and the `rw health` streak. One endpoint (`/api/dashboard`) aggregates on the server with the `rw stats`, `rw tune`, learn and health code, cached for a minute; the charts are inline SVG with table views, in both themes, and work at 480px. ~0.3 s for 2,900 tasks over 90 days. Tested with fixture and synthetic logs and in headless Chrome, plus real migrated Switchyard logs in headless Edge ([PR #60](https://github.com/Sparkz400/Relayweft/pull/60)). `TestDashboardMatchesCLI` reconciles generated logs with the CLI in normal CI; the 6 Oct verification record above describes the real-log check. |
| 4.31 | **Azure DevOps** ([docs/azure-devops.md](docs/azure-devops.md)): `rw pr`, work items as tasks (`--issue`, `--issues label:`, `--team`), `rw watch` and `rw review --post` support dev.azure.com, legacy visualstudio.com hosts and Azure DevOps Server via `AZURE_DEVOPS_HOST`. Host-scoped PAT or Entra authentication, work item links, build validation checks and review threads are covered by fake API tests. The [Azure Pipelines template](ci/azure-pipelines.yml) has local scripted job tests. A hosted pipeline, real organization, Entra login and Server deployment remain unverified. |

---

## Standing quality rules (all phases)

The rules every change follows are in [CONTRIBUTING.md](CONTRIBUTING.md#rules): a test for every bug fix, Windows first, no silent failure, safe by default, pinned CLI versions and an adversarial review before merge.

---

## Suggested order for the next steps

1. **Use it for real on Windows and send a `rw bugreport` after any problem.** Items 1.6 (the Windows test pass) and the exit criterion (2 weeks of daily use) need you at the keyboard. Start with `rw selftest` (add `--onedrive` if you use OneDrive); it now also closes a task's window in the old console and in Windows Terminal by itself (automated and verified on 6 October 2026) and prints the one check left to do by hand: sleep and resume during a task. `rw health` shows how far the 2-week streak has got. Then try each new feature once:
   - approve and edit a plan;
   - `/review-changes on` for one task;
   - `@ follow-up` after a task;
   - with a real agent, close the window mid-task once, then `rw resume` (the selftest covers this with a scripted agent);
   - `rw app`.
2. **1.1 The rest of the Codex recordings** (an edit run is already recorded). In any git repo on your PC, with `codex exec --json --skip-git-repo-check -m gpt-6-luna "create hello.txt containing hi"`:
   - once you hit a Codex limit, run it and keep the output (`codex-limit.jsonl`).
   - after `codex logout`, run it once more (`codex-logout.jsonl`), then `codex login`.
   Send the files; they become test fixtures.
3. **Run `rw bench` on ~10 real, multi-file tasks from your own repos, with Codex.** This decides the Phase 3 exit criterion. In each repo, `rw bench --from-history` writes the tasks (it runs your tests on each candidate commit, which costs no quota). Read and reword the prompts, then run `rw bench --file bench-history.yaml`. After a week of use, run `rw tune`.
4. **Releases:**
   - Prepare the changelog and release notes in a PR, pass CI and a signed release dry run on the merged commit, then tag that exact commit. Keep published tags fixed. See `packaging/README.md`.
   - Submit the rendered winget manifests to microsoft/winget-pkgs (needs a fork of winget-pkgs on your account).
   - After each release, render the manifests (`packaging/render-manifests.sh X.Y.Z`) and commit `packaging/scoop/rw.json`, `Formula/relayweft.rb`, `packaging/aur/PKGBUILD` and `packaging/aur/.SRCINFO`.
   - Publish `relayweft-bin` to the AUR (needs an AUR account; steps in `packaging/README.md`), then push the rendered PKGBUILD and .SRCINFO after each release.
   - Repeat installation and upgrade checks for future releases; v0.4.0 signed feeds and macOS/Linux Homebrew CI already passed. Personal Mac daily use remains separate from runner coverage.
   - For signed binaries, buy a code-signing certificate (see `packaging/README.md`).
5. **Open Phase 4 items:**
   - 4.3: record a Gemini run with an API key (`GEMINI_API_KEY`; personal Google sign-in is refused; the commands are in docs/providers.md) and a DeepSeek run, to turn them from beta into tested.
   - 4.26: run the GitHub Action and the GitLab job once on real runners (a test repository and a labelled issue), and the Forgejo workflow with a real agent and on Gitea.
   - 4.27: run `--team` on two machines against one label.
