# Relayweft Roadmap

**Goal:** a tool you trust with real work every day. It must be **reliable first, useful second, smart third, pretty fourth.**

The phases are ordered. A phase starts only when the previous phase's **exit criteria** are met. Features never jump ahead of reliability.

> **Note (October 2026):** Phases 2 to 4 were built ahead of Phase 1's exit criteria, at your request. Their code is tested but has not been used for real. Phase 1's open items (1.1, 1.6) and its exit criteria still come first. Until they are met, treat Phase 2 to 4 features as a beta.

> **Renamed (October 2026):** up to v0.2.0 the project was called Switchyard and its command was `sy`. From v0.3.0 it is Relayweft and the command is `rw`, because "Switchyard" was taken on the VS Code Marketplace. The history below says `sy` wherever `sy` was what ran.

---

## Where we are (October 2026)

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
- **Phase 2 (done, needs real-world use):**
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
- **Phase 4 (done; 4.3 more providers is in beta):**
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
- **Next: v0.4.0** (prepared): `rw completion` for bash, zsh, fish and PowerShell; a generated config reference (`docs/config.md`); release provenance, SBOMs and a signed `checksums.txt`; gofmt, golangci-lint and govulncheck in CI; CHANGELOG, CONTRIBUTING, a PR template and a Code of Conduct.

**Verified:**
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

**Not verified yet:**
- Codex: a usage-limit hit and a logged-out CLI.
- Real daily use on your Windows PC.
- Behaviour under heavy load: a big repo, three agents in parallel, hours of use.
- A ~10-task comparison against a single agent on real, multi-file tasks ([docs/plan.md §9](docs/plan.md#9-measuring-success)). The starter set is too small to show it: on its one-file tasks a single agent is faster.
- A macOS notification banner on screen: the runners' screenshots never show one (Notification Center logs it "as banner", but the runner's screen is shared, which may hide banners). Delivery itself is verified.
- `rw app` in Edge on macOS and Linux (Chrome only), and on a real Linux desktop (GNOME/KDE) rather than Xvfb with fluxbox and dunst.
- Webhooks to real Slack and Discord (payloads checked against their current docs only).
- The container sandbox with Podman, with a signed-in Claude Code or Codex doing real work, and on Linux and macOS hosts outside CI.
- Best of N and affected-test selection with real agents; the JetBrains plugin driven on Windows or macOS.

**Open risk:** a full Windows freeze happened on 3 Oct while using `rw`.
- The logs show the same unexplained hard resets since August, before Relayweft existed, with no blue screen and no disk or memory exhaustion.
- So the most likely cause is the hardware or drivers failing under load, with Relayweft's load as the trigger.
- Phase 1 still adds load limits, so `rw` can never be the thing that pushes a machine over.

---

## Phase 1 — Trustworthy (stabilize)

*Make it impossible for `rw` to hurt your machine, your repo or your quota, and make every failure diagnosable.*

| # | Item | Why |
|---|---|---|
| | *Status: 1.2, 1.3, 1.4 and 1.5 are **done**. 1.1, 1.6, 1.7, 1.8 and 1.9 are open.* | |
| 1.1 | 🟡 **Real Codex end-to-end run** (partly done): a real `codex exec --json` run and a real `codex exec resume` are recorded as test fixtures (`internal/runner/testdata/codex_real_*.jsonl`). They match the parser and resume works. They also showed that Codex sends **no `rate_limits`**, so Codex can only fall back after a limit, not switch before it. Still to record: a run with tool calls and edits, a limit hit, and a logged-out CLI. | Codex was tested only against hand-built output until now. |
| 1.2 | ✅ **Load limits** (done): agents and git at below-normal priority, parallel checkout capped at min(4, cores/2), a busy gate that holds new agents above `max_cpu_percent` or below `min_free_memory_mb` (at most `busy_max_wait`). Was planned as: cap git's parallel checkout (`checkout.workers` = half the cores, at most 4), lower process priority for agents and git (`BELOW_NORMAL_PRIORITY_CLASS`), prewarm at most one pool slot at a time, and a `max_cpu_load` setting that pauses dispatch while the machine is pegged. | A freeze under load must never be triggered by `rw`. |
| 1.3 | ✅ **Disk guard** (done): `min_free_disk_gb` floor, pruning after `pool_max_idle`, `pool_warn_gb` warning, pools listed in `rw doctor`, one shared pool per repo. Was planned as: show the pool size in `rw doctor`, warn at more than X GB, prune slots unused for 14 days, and refuse to create a slot when free space drops under 10 GB. | Each pool slot is a full checkout of your repo. |
| 1.4 | ✅ **Crash safety** (done): `rw-debug.log`, `crash-*.log`, task-level panic recovery, `rw bugreport`. Was planned as: a panic handler that restores the terminal and writes `crash-<time>.log`; a persistent debug log (`%LocalAppData%\relayweft\logs\rw.log`) with every spawned command line, exit code and timing; `rw bugreport` zips the last session log, debug log, config and `doctor` output. | When something goes wrong, you can send one file and the reason is visible. |
| 1.5 | ✅ **Undo a task** (done): `rw undo` / `/undo` with preview, redo, later edits kept by 3-way merge, last 30 tasks. Was planned as: `rw undo` / `u` restores the working tree to the snapshot taken when the task started (the snapshot commit already exists), with a preview first. | It's the single biggest trust feature: trying a task becomes risk-free. |
| 1.6 | 🟡 **Real-use test pass on Windows** (partly automated): `rw selftest` checks a user profile and project path with spaces and non-ASCII letters, the agent CLI behind a `.cmd` shim, many files plus Git LFS, OneDrive (detection, and `--onedrive` runs a task inside it), Defender on (status, exclusions, fresh-exe start time), and a `rw run` killed hard mid-task followed by `rw history`, `rw resume`, `rw undo` and redo. CI runs it on all three OSes (`TestSelftest`). It already found one bug: a normal end of `rw run` / `rw resume` printed the Ctrl+C notice. Still by hand: sleep/resume during a task, and closing the window mid-task in Windows Terminal and in the old console. Was planned as: Windows Terminal and the old console, a user name with a space, paths with spaces, OneDrive folders, a big repo with LFS, Defender on, sleep/resume during a task, closing the window mid-task. | These are where Windows tools usually break. |
| 1.7 | ✅ **Long-run stress test** (done: `internal/orchestrator/stress_test.go`, `.github/workflows/stress.yml`, demo and real-git modes with cancels). CI runs the orchestrator in a loop for 30 minutes and checks that memory, goroutines, open handles and leftover processes stay flat. | Catches leaks before you find them as freezes. |
| 1.8 | ✅ **Parser fuzzing** (done: 15 fuzz targets, nightly in `.github/workflows/fuzz.yml`; fixed a Codex error line counted as success, plan dependencies on reserved ids, reset-time overflow, `@` parsing and a change-list panic). Go fuzz tests for the Codex and Claude output parsers and for plan and verdict parsing. | Odd model output must never crash `rw` or leave it stuck. |
| 1.9 | ✅ **Review the worktree pool** (done: 17 findings fixed, including a crash leaving a slot that disabled worktrees, agent commits being lost, non-atomic apply, the Windows command-line limit, submodules, symlinks, git hooks in slots and orphan agents). It got the same adversarial pass that found the earlier bugs. | It is the newest and most complex code and has had only one review pass. |
| 1.10 | ✅ **Health log and `rw health`** (done): every `rw` writes start, end, load peaks, hangs, panics, agent timeouts and leftovers to a small `rw-health.log`; a watchdog dumps all stacks to `hang-*.log` when the TUI stops responding for a minute, and fatal runtime errors go to `fatal-*.log`. `rw health` (and **Health** in `rw web`) reads them and says whether the exit criterion below is met. | The exit criterion needs a record, not a memory. |

**Exit criteria:**
- 2 weeks of daily use with no crash, no hang and no lost work. `rw health` (or **Health** in `rw web`) shows the clean streak and the days of use; lost work is not in the logs and still needs your word.
- Every failure can be traced from `rw bugreport` alone.
- No machine freezes during heavy runs on the same hardware.

---

## Phase 2 — Useful every day

*Remove the reasons you'd fall back to plain `codex` or `claude`.*

| # | Item | Why |
|---|---|---|
| 2.1 | ✅ **Plan approval step**: the plan opens in the TUI (or on the terminal with `rw run --approve`). You can delete, reorder or edit subtasks, pin a role, or cancel. Turn it off with `/approve off`. | You stay in control of what runs, before any quota is spent. |
| 2.2 | ✅ **Review the diff before it lands** (`review_changes`): a per-file diff view where you can accept, accept only some files, reject, or send it back with feedback (the agent continues in its worktree). Rejected work is kept on a branch. | Bad edits are rejected before they reach your tree, not undone after. |
| 2.3 | ✅ **Agents can run tests safely**: `verify.commands`, detected by `rw init` (Go, npm/pnpm/yarn/bun, pytest, cargo, dotnet, Maven, Gradle). They become Claude `allowedTools`, are run by `rw` before the final review, and failures feed the fix round. Codex workers already run commands inside their workspace-write sandbox. After a fix round that is not the last (`max_fix_rounds` 2 or more), only the tests the changes affect run first (Go packages and their importers, jest/vitest related tests, pytest by imports, cargo crates, dotnet test projects, Maven and Gradle modules, or a `verify.affected_commands` template); the full checks always run before the final review, and anything rw cannot tell runs in full. Measured on this repo: the same failures caught, wall time 0-16% lower (the orchestrator tests dominate). | Today Claude workers can edit but cannot verify their own work. |
| 2.4 | ✅ **Follow-up messages**: `@agent message` resumes that agent's CLI session (`codex exec resume`, `claude --resume`) in the pool worktree the agent ran in, falling back to a fresh agent with context. Finished agents only; a running agent is not interrupted. | Real work is iterative; today every follow-up starts a new task. |
| 2.5 | ✅ **Task history and resume**: the state of every task is saved after each step. `rw history` / `/history` list tasks; `rw resume` / `/resume` continue an interrupted one, skipping finished steps. Diffs per task come from `rw undo --list`. | Closing the window or a reboot no longer loses progress. |
| 2.6 | ✅ **Task queue**: submitting while a task runs queues it in the TUI (`/queue`). `rw run --file tasks.txt` runs a list overnight. Queued tasks run unattended. | Uses quota while you're away. |
| 2.7 | ✅ **Notifications**: a desktop notification when a task finishes or fails, a limit is hit, or `rw` waits for you (Windows toast, macOS, notify-send). | You don't have to watch the terminal. |
| 2.8 | ✅ **Distribution** (pipeline built, not yet run): a tag builds release binaries for Windows, Linux and macOS with checksums. There are Scoop and winget manifests, a Homebrew tap in this repo (`brew install relayweft`), `.deb`, `.rpm` and `.apk` packages on each release (from v0.3.0), an AUR PKGBUILD (`relayweft-bin`), and `rw update` (checksum-verified, swaps the running .exe safely on Windows, and points to the package manager that installed `rw` instead of replacing its binary). Code signing needs a certificate: see `packaging/README.md`. Releases carry build provenance, SBOMs and a Sigstore-signed `checksums.txt` (from v0.4.0). | Installing no longer needs Go or a build. |

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

**Remaining gaps:**
- **2.3 Tests:** Codex runs commands in its own sandbox, not from an allowlist, so `verify.commands` can only restrict Claude. Codex offers no allowlist, so this is a limit of the CLI.
- **2.3 Affected tests:** only the Go selection was run for real (on this repo). The jest/vitest, pytest, cargo, dotnet, Maven and Gradle selections are tested on file trees, not yet on real projects.
- **2.8 Distribution:**
  - Code signing of the Windows and macOS binaries needs a certificate (Authenticode, Apple notarization). Release integrity does not: from v0.4.0 every asset has GitHub build provenance, each binary a CycloneDX SBOM (attested), and `checksums.txt` a keyless Sigstore signature (`packaging/README.md`, "Verifying a release"). Not yet seen on a real release until v0.4.0 is cut; a dry run passed.
  - `rw update` checks only the SHA-256; it prints the `gh attestation verify` command instead of verifying the signature itself (that would need sigstore-go and a fresh trust root).
  - The Scoop, winget, Homebrew and AUR manifests must be rendered (and the AUR one pushed) after each release. Once they name v0.4.0 or later, drop the `version >= 0.4.0` completion guards in the Homebrew and AUR templates.
  - The Linux packages have no GPG/apk signature (provenance and the signed checksums cover them), and there is no apt or dnf repository.

**Exit criteria:**
- You reach for `rw` before plain `codex` or `claude` for multi-step work.
- ✅ A new user goes from install to first task in under 5 minutes. `rw setup` checks the CLIs and logins without quota, writes the config and runs a read-only first task. Measured on Windows: download 1.8s, setup 1.6s of rw's own time, first task with Claude haiku 8.8s (13s in all, plus three answers). `TestOnboarding` times it in CI on all three OSes with scripted agents.

---

## Phase 3 — Smarter (measure, then tune)

*Prove that Relayweft beats a single agent, then make it better on data.*

| # | Item | Why |
|---|---|---|
| 3.1 | ✅ **Benchmark command**: `rw bench` with `bench.yaml`, check commands, routed vs single, saved results, `rw bench --starter` (five Python tasks with check scripts), and `rw bench --from-history` (real tasks from past multi-file commits, checked by the repo's tests with the commit's test files in place, each validated to fail before and pass after). Every run starts in a fresh repository with only the starting commit's files, so agents cannot find a solution in the history. `--own-tests` (or `{tests}`/`{test_dirs}` in the check) runs only the commit's tests. | This is the success measure from [docs/plan.md §9](docs/plan.md#9-measuring-success), automated. |
| 3.2 | ✅ **Quota-aware scheduling** (done early, Claude; Codex as soon as its CLI reports `rate_limits`): `quota-preempt` at `switch_at_utilization`, and the planner and reviewer retry on the other provider. Was planned as: use Claude's live 5-hour and 7-day utilization (already received) and Codex limits to move work to the other provider *before* hitting the limit, not after. | Avoids stalls entirely. |
| 3.3 | ✅ **Rule tuning from stats**: `rw tune` flags failing routes, frequent escalations, rejected reviews, quota pressure and over-sized read-only models, and prints the `/route` / `/prefer` command for each. | Routing improves from your own data. |
| 3.4 | ✅ **Judge model** (measurement): decisions record whether the judge ran, and `rw tune` compares judged with rule-routed steps to suggest `/judge on` or `/judge off`. | Spend quota only where it pays. |
| 3.5 | ✅ **Context hand-off**: a repo map and notes from earlier tasks in the same repo go into planner and step prompts, and every writer gets what this task's read-only steps found. | Fewer tokens, faster workers. |
| 3.6 | ✅ **Cost visibility**: fresh tokens per provider, Claude API-equivalent $, and limit before and after, in the TUI, `rw run` and `rw stats`, plus a per-day table in `rw stats`. | You can see what each task cost. |
| 3.7 | ✅ **Cost-aware model tiers** (`routing.tiers: auto`, off by default): the rules still pick the role; a work step's model then comes from its estimated difficulty (role, files, prompt size, routine or hard words) and the quota left (the provider's reported limit, and the task, day and team budgets). The tiers reuse the explorer, worker and worker_high routes. Planner, reviewer, judge and explicitly set roles never move; risky steps keep their floor; a local model on standby keeps its route. Measured on the starter set: the tiers kept Sonnet for 6 of 7 steps, and the cost stayed the same within noise. Routine words now count only in a step's title. | Easy steps stop paying for strong models, and a nearly spent quota stretches further. |
| 3.8 | ✅ **Best of N for hard steps** (`routing.best_of`, off by default; `b` in the plan view): a writing step runs on two to four routes at once (default: its own route and the same role on the next provider), each in its own pool worktree from the same commit. `verify.commands` run in full in each worktree, one at a time. Passing checks win (not a candidate that changed nothing over one that changed something); otherwise the reviewer compares the diffs (untrusted, named A and B in shuffled order, not by provider); otherwise a fixed order (fewer failing checks, a change, the smaller diff, the cheaper run). The winner lands like any step and change review sees only it; every candidate's work is kept on a branch until the winner has landed. `when: hard` reuses the router's risk rules and the tiers' difficulty score. Providers at or near their limit are left out; candidates run at once only within `max_threads` and when the machine is not busy; the estimate counts every candidate. A best-of step that rw stopped before the pick runs again as a whole on resume; from the pick on, the winner resumes like any step. An untrusted repo file or `./relayweft.yaml` may lower `best_of`, not raise it. `best_of` records feed `rw tune` and the learned routes (a loss on checks or by the reviewer counts against the route); `rw bench` has a `routed-bestof` mode. Not yet measured. | A second opinion where it matters: on hard steps the checks, not one agent, decide which change lands. |

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

**First measurements** ([docs/bench](docs/bench/2026-10-03-starter-claude.md), [tiers](docs/bench/2026-10-04-tiers-claude.md)):
- On five small one-file tasks, a single Claude agent was about 3x faster and used half the tokens.
- As a result, one-step plans now skip the plan review. That cut routed time by 32% and tokens by 23%.
- With `--tiers`, 6 of 7 small work steps kept Sonnet and one went to Haiku. Cost and correctness were the same; the planner and reviewer are most of a routed task's cost. A single agent was still about 2x faster on these tasks.
- Whether routing pays off on bigger tasks is still open (exit criterion below).

**Remaining gaps:**
- `rw tune`'s rates now need a clear majority from few runs (90% confidence bound, on both sides when routed is compared with single): 3 failures in 5 runs, 4 in 10, 7 in 20. The rates themselves are still guesses: no real log has failures yet.
- Whether tiers pay off on multi-file tasks: run `rw bench --from-history` with and without `--tiers`.
- Whether best of N raises the pass rate: run `rw bench` with `routed-bestof` against `routed` and `single`.

**Exit criteria:** on the benchmark, Relayweft beats a single agent on at least 2 of the 3 measures: correctness, wall time, and how quickly the limits are reached.

---

## Phase 4 — Nicer and broader

| # | Item |
|---|---|
| 4.1 | ✅ **`rw web`**: a local browser UI on the same engine. It has the agent tree, the activity log, the plan editor, hunk review, models and routes, settings, history, the queue and stats/tune. It listens on 127.0.0.1, with a per-run token and strict Host/Origin checks. |
| 4.2 | ✅ **Desktop window**: `rw app` opens `rw web` in Edge or Chrome app mode instead of using Wails. That means no cgo and nothing to install, and it closes when the window does. |
| 4.3 | 🟡 **More providers** (built, beta): a provider is a name plus the CLI protocol it speaks (`kind: codex | claude | gemini | qwen | generic`), set up in config with its own `command` and `env`. `kind: generic` describes any other CLI in config alone: its arguments, text or JSON-lines output (rules for the answer, session, tools and tokens), resume and limit detection; descriptions of Qwen Code and Gemini CLI in that format read their recordings exactly like the built-in kinds. Presets (disabled by default): Gemini CLI, Qwen Code on local Ollama, DeepSeek, any Ollama model through Claude Code, and a plain `ollama run` model (generic). `standby: [roles]` lets a free local model take cheap read-only work once Codex and Claude are at or near their limits (Qwen Code: explorer and researcher; plain Ollama: the judge). Routing walks `routing.provider_order` instead of "the other provider"; `only_preferred` keeps slow local models out of fallbacks. Qwen Code and Claude-on-Ollama are recorded and were run end to end; Gemini has only a signed-out recording (the stream format comes from its source); DeepSeek is unrun (needs a key). See [docs/providers.md](docs/providers.md). |
| 4.4 | ✅ **Per-repo profiles**: a `.relayweft.yaml` in the repo is layered over your config. The parts that run commands need `rw trust`. Create one with `rw init --repo` or `/save repo`. |
| 4.5 | ✅ **Hooks**: `before_task` (a failure stops the task), `after_merge` and `after_task`, with `RW_*` environment variables. |
| 4.6 | ✅ **Hunk-level review**: in the TUI and in `rw web`. |
| 4.7 | ✅ **Talk to a running agent**: `@agent message` is delivered when the agent's turn ends, before its work is merged. |
| 4.8 | ✅ **Persistent follow-ups**: sessions are kept per project folder. |
| 4.9 | ✅ **Scheduled runs**: `rw run --at 02:30 / --in 3h / --when-reset claude` (task file or one task), `/schedule` in the TUI and the queue panel in `rw web`. The PC is kept awake while a scheduled run waits and runs; `rw schedule` prints a Task Scheduler / cron line. Plus **budgets** per task and per day (tokens and API-equivalent $). |
| 4.10 | ✅ **Task reports**: `rw report` writes one self-contained HTML (or `--md` Markdown) page per task: plan and results, routing decisions with rule and reason, reviews, checks, the diff and the cost. Everything is escaped, and a CSP blocks scripts. |
| 4.11 | ✅ **MCP servers**: an `mcp:` config section passes MCP servers to both CLIs per role (Claude: a temporary `--mcp-config` file; Codex: `-c mcp_servers.*`). `${VAR}` comes from your environment, repo files need `rw trust`, and `rw doctor` checks the commands. |
| 4.12 | ✅ **Multi-repo tasks**: `--repo name=path` or `workspace: repos:` in `.relayweft.yaml`. The planner assigns each subtask a repo; writers run in that repo's worktree pool or main tree; each repo's own (trusted) checks run there; the final review sees every repo's diff; one `rw undo <key>` reverts every repo; history and resume keep the repos. |
| 4.13 | ✅ **`rw pr`**: a finished task becomes a branch, a commit and a GitHub pull request, with the plan, checks and cost in the description. Your index, working tree and branches are never touched. A multi-repo task gets one PR per repo (`--repo`). |
| 4.14 | ✅ **Issues → tasks**: `rw run --issue 42` works on a GitHub issue. `--issues label:rw --pr` works through labelled issues one after another, opens a PR for each with "Closes #N", and comments on the issue. Combined with scheduled runs, this works overnight. |
| 4.15 | ✅ **Budgets**: token and $ limits per task and per day (`budget:`). `rw` asks before going over; unattended runs stop instead. |
| 4.16 | ✅ **Watch PRs**: `rw watch [--every 15m]` turns failed checks and review comments on PRs rw opened into a follow-up task on the PR branch (separate checkout, never forced, `watch.max_rounds`). |
| 4.17 | ✅ **`rw review <PR>`**: a read-only second-opinion review by the other provider; `--post` posts it as one comment review, inline where possible. |
| 4.18 | ✅ **Learned routing**: `rw tune --apply` (or `routing.learn: auto`) stores per-repo routes from your logs and bench, on clear evidence only; explicit settings always win, and decisions say when a learned route was used. |
| 4.19 | ✅ **Team budgets and stats export**: `rw stats --json` / `--merge`, and `budget.team` over a shared folder. |
| 4.20 | ✅ **Repo conventions as context**: CONTRIBUTING, the PR template, CODEOWNERS, CI commands and AGENTS.md go to the planner and reviewer as untrusted text; `rw pr` fills the PR template. |
| 4.21 | ✅ **Cost estimate before approval**: per step and total, against the remaining budget; `rw run --estimate`. |
| 4.22 | ✅ **VS Code extension** (`editors/vscode`): a thin client for `rw web --client` with the agent tree, plan approval and hunk review in the diff editor. Built and unit-tested, and tried in real VS Code 1.140 and 1.90 with an integration suite (`npm run test:integration`). |
| 4.23 | ✅ **GitLab and Gitea/Forgejo** (`internal/forge`): `rw pr`, issues as tasks, `rw watch` and `rw review` work on gitlab.com and self-managed GitLab (merge requests, pipeline jobs and their logs, unresolved diff comments by Developers and above, one thread per inline finding) and on Gitea and Forgejo, Codeberg included (commit statuses, reviews requesting changes, inline reviews). The origin remote's host picks the forge; self-hosted ones are named in `GH_HOST` / `GITLAB_HOST` / `GITEA_HOST`, and each forge's token goes only to its own hosts. GitLab quick actions are defused like mentions, and `rw watch` never pushes CI config of any forge. Tested against fake APIs and, for real, against Gitea 28, Forgejo 16 and GitLab CE 19.4 with a GitLab Runner. |
| 4.24 | ✅ **Webhook notifications** (`notify.webhooks`): Slack, Discord, ntfy or plain JSON, so overnight runs and `rw watch` reach your phone. Events `done`, `failed`, `limit`, `waiting` and `watch` (round results, merged or closed PRs), filterable per webhook; a task-file batch ends with a summary. `${VAR}` keeps the secret URL out of the file; a repo file's webhooks need `rw trust`; text is escaped against mentions and hidden links; errors and `rw bugreport` never show the URL path. `rw notify --test` checks each webhook. Tested against local servers and real ntfy.sh; not yet against real Slack or Discord. |
| 4.25 | ✅ **Bench results feed learned routes**: a bench mode `routed:<role>=<provider:model[:effort]>` runs the routed pipeline with one role on another route, so the learner gets an alternative to compare with (single-agent runs never counted). `learn: true` in a bench file, or `--learn`, updates the repo's learned routes when the bench ends, with the same clear-evidence rules as `rw tune --apply`. `rw bench --from-history` writes `learn: true` and a worker variant on the other provider, so one bench of your own history can change the routing with no manual tuning. `--no-learn` and `routing.learn: off` keep the routes as they are; a cancelled bench learns nothing. |
| 4.26 | ✅ **CI as an agent target** (`action.yml`, `ci/`, [docs/ci.md](docs/ci.md)): a GitHub Action, a GitLab job and a Forgejo/Gitea Actions workflow (`ci/forgejo-workflow.yml`) run `rw run --issue N --pr` or `--issues label:rw --pr` in CI (on a label, nightly or by hand), so tasks run without your PC. The action installs the release named by its ref (checksum-verified) or builds from source. Agents, verify commands, hooks and bench checks start without the forge and CI tokens; git pushes through a credential helper, not `.git/config`. Reports go to the job summary and an artifact; `rw history --json` lists a run's tasks. Tested locally (install against the real v0.1.0 release, the step scripts with a stub `rw`, token scrubbing); not yet run on real GitHub or GitLab runners. The Forgejo workflow ran on a real Forgejo 16 with forgejo-runner v12 in Docker, with rw (built from this change and from the v0.2.0 tag) and a scripted `claude` stand-in installed from a release on that Forgejo (checksum-verified): a labelled issue, a scheduled batch and a manual run each opened a pull request (`Closes #N`) and commented on the issue; the stand-in saw only its model key. rw finds the forge from the job's server, and the job's token never goes to GitHub. On a private repository the job's own token cannot open the pull request (Forgejo 16); a `RELAYWEFT_TOKEN` secret can. Not yet tried on Gitea or Codeberg. |
| 4.27 | ✅ **Team mode: a shared issue queue**: `rw run --issues label:rw --pr --team [--every 10m]` on several machines pulls from the same label. Each issue is claimed with one comment right before it runs; the earliest live claim by a trusted author wins, the lease is renewed while the task runs (`--lease`), and the comment ends as done (PR link), failed or released. Failed issues wait for `--retry-failed`. Works on GitHub, GitLab and Gitea. Tested against fake forges, not yet with two real machines. |
| 4.28 | ✅ **Container sandbox** (`sandbox:`, off by default; [docs/sandbox.md](docs/sandbox.md)): agents, the verify commands, `after_merge`/`after_task` hooks and bench checks run in a docker or podman container with only the step's folder writable. The repository's git folder is mounted read-only (a generated `.git` file for pool worktrees, a git config without remotes or helpers), each provider gets its own `HOME` so sessions resume (command-running settings files are cleared before each run), and `/work/...` paths are turned back into host paths. Only named variables and read-only credential files go in; rw's forge and CI tokens never do (also not through MCP configs). Every host git command in a folder an agent wrote to runs with `core.fsmonitor=false` and without submodule recursion, and a changed submodule `.git` fails the run, so files an agent writes cannot make git on the host run code. Project settings files an earlier agent changed (`.claude/settings*.json`, `.mcp.json`, `.codex/config.toml`) are shown to the next CLI in their starting version. Per provider and per role; a repo file may make it stricter without `rw trust`, never weaker. Cancel and timeouts `docker kill` the container, a wrapper ends it when rw dies, and a sweep removes leftovers. A missing runtime, image or CLI fails the step; nothing runs on the host instead. `rw doctor` checks it, `rw selftest --sandbox` runs it with a scripted agent (required in CI on Linux), and `packaging/sandbox` has the reference image. Two security reviews; run for real on Windows with Docker Desktop (pool worktrees, verify, timeout, hard kill, a planted submodule `.git`, real Claude Code without sign-in). |
| 4.29 | ✅ **JetBrains plugin** (`editors/jetbrains`): a thin client for `rw web --client` in IntelliJ IDEA, PyCharm, GoLand, WebStorm, Rider and the other JetBrains IDEs (2025.2+), with the agent tree, the activity log and a prompt box in a tool window, plan approval (edit, add, delete, reorder), and hunk review in the IDE's diff viewer (gutter icons, strike-through, CRLF checkouts as whole files). Same login and security as the VS Code extension; settings are IDE-wide, never per project, and an untrusted project never starts `rw`. Unsaved edits are saved before `rw` applies changes. Tested against a fake server, a real rw with a scripted agent, and in a headless IDE; the Plugin Verifier passes on IDEA 2025.2, 2025.3 and 2026.2. A UI test drives it in a real IntelliJ IDEA on Xvfb (tool window, plan dialog, Review tab, diff, apply). Not on the Marketplace yet (needs an account; steps in its README). |
| 4.30 | ✅ **Dashboard in `rw web`**: a panel over 7, 30 or 90 days, for this project or all. It shows tasks per day (done, failed, cancelled) with the success rate; fresh tokens per provider and API-equivalent $ per day against the daily and team budget; success, average tokens, $ and time per role and route, with escalations, rejected final reviews, decision flags and what `rw tune` flags; learned-route changes with their evidence; Claude's 5-hour and 7-day use, limit hits and switches per provider; and the `rw health` streak. One endpoint (`/api/dashboard`) aggregates on the server with the `rw stats`, `rw tune`, learn and health code, cached for a minute; the charts are inline SVG with table views, in both themes, and work at 480px. ~0.3 s for 2,900 tasks over 90 days. Tested with fixture and synthetic logs and in headless Chrome; not yet with real logs. |

---

## Standing quality rules (all phases)

The rules every change follows are in [CONTRIBUTING.md](CONTRIBUTING.md#rules): a test for every bug fix, Windows first, no silent failure, safe by default, pinned CLI versions and an adversarial review before merge.

---

## Suggested order for the next steps

1. **Use it for real on Windows and send a `rw bugreport` after any problem.** Items 1.6 (the Windows test pass) and the exit criterion (2 weeks of daily use) need you at the keyboard. Start with `rw selftest` (add `--onedrive` if you use OneDrive); it prints the two checks left to do by hand. `rw health` shows how far the 2-week streak has got. Then try each new feature once:
   - approve and edit a plan;
   - `/review-changes on` for one task;
   - `@ follow-up` after a task;
   - close the window mid-task, then `rw resume`;
   - `rw app`.
2. **1.1 The rest of the Codex recordings** (an edit run is already recorded). In any git repo on your PC, with `codex exec --json --skip-git-repo-check -m gpt-6-luna "create hello.txt containing hi"`:
   - once you hit a Codex limit, run it and keep the output (`codex-limit.jsonl`).
   - after `codex logout`, run it once more (`codex-logout.jsonl`), then `codex login`.
   Send the files; they become test fixtures.
3. **Run `rw bench` on ~10 real, multi-file tasks from your own repos, with Codex.** This decides the Phase 3 exit criterion. In each repo, `rw bench --from-history` writes the tasks (it runs your tests on each candidate commit, which costs no quota). Read and reword the prompts, then run `rw bench --file bench-history.yaml`. After a week of use, run `rw tune`.
4. **Releases:**
   - Before you release v0.3.0:
     1. Delete the old `v0.3.0` tag on GitHub: `git push origin :refs/tags/v0.3.0`. It points at e8ea295, from before the rename.
     2. Tag the merged rename.
     3. After the release, render the manifests for 0.3.0. The `packaging` checks skip until you do.
   - Submit the rendered winget manifests to microsoft/winget-pkgs (needs a fork of winget-pkgs on your account).
   - After each release, render the manifests (`packaging/render-manifests.sh X.Y.Z`) and commit `packaging/scoop/rw.json`, `Formula/relayweft.rb`, `packaging/aur/PKGBUILD` and `packaging/aur/.SRCINFO`.
  - Publish `relayweft-bin` to the AUR (needs an AUR account; steps in `packaging/README.md`), then push the rendered PKGBUILD and .SRCINFO after each release.
  - Install a `.deb`/`.rpm` from a release once, and `brew install relayweft` on a real Mac.
   - For signed binaries, buy a code-signing certificate (see `packaging/README.md`).
5. **Open Phase 4 items:**
   - 4.3: record a Gemini run with an API key (`GEMINI_API_KEY`; personal Google sign-in is refused; the commands are in docs/providers.md) and a DeepSeek run, to turn them from beta into tested.
   - 4.26: run the GitHub Action and the GitLab job once on real runners (a test repository and a labelled issue), and the Forgejo workflow with a real agent and on Gitea.
   - 4.27: run `--team` on two machines against one label.
