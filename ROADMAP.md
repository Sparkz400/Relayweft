# Switchyard Roadmap

**Goal:** a tool you trust with real work every day. It must be **reliable first, useful second, smart third, pretty fourth.**

The phases are ordered. A phase starts only when the previous phase's **exit criteria** are met. Features never jump ahead of reliability.

> **Note (October 2026):** Phases 2 to 4 were built ahead of Phase 1's exit criteria, at your request. Their code is tested but has not been used for real. Phase 1's open items (1.1, 1.6) and its exit criteria still come first. Until they are met, treat Phase 2 to 4 features as a beta.

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
  - debug and crash logs with `sy bugreport`
  - `sy undo`
- **Phase 2 (done, needs real-world use):**
  - plan approval
  - change review before applying
  - agents run your tests (`verify`)
  - follow-up messages
  - task history and resume
  - task queue
  - notifications
  - release pipeline with `sy update`
- **Phase 1 test items (done):**
  - parser fuzzing in CI (found and fixed 5 bugs)
  - a 30-minute stress test in CI on Linux and Windows
  - a second adversarial review of the worktree pool (17 findings, all fixed with regression tests)
- **Phase 3 (built; first measurements in):**
  - `sy bench` with a starter set and a `routed-nohandoff` mode
  - switching provider before the limit
  - `sy tune` (rule tuning, judge cost vs gain, models from your config)
  - context hand-off
  - cost per task and per day
- **Phase 4 (mostly done):**
  - `sy web` and `sy app`
  - per-repo `.switchyard.yaml` with `sy trust`
  - hooks
  - hunk-level review
  - talking to a running agent
  - follow-ups that survive restarts
- **Release v0.1.0** (3 Oct 2026): six binaries plus checksums, built by the release workflow, MIT license, Scoop manifest filled in.
  - The repository is public: release downloads, `sy update` and the Scoop install need no login.
  - winget needs the rendered manifests submitted to microsoft/winget-pkgs.

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
- **`sy web` in headless Chromium:** plan editing, hunk review, reload restore and the panels.
- **A 3-minute stress run:** goroutines, file handles, heap and processes stayed flat over 1,750 tasks.

**Not verified yet:**
- Real Codex runs with tool calls and file edits, a usage-limit hit and a logged-out CLI. A simple run and a resume are now recorded (see 1.1).
- Real daily use on your Windows PC.
- Behaviour under heavy load: a big repo, three agents in parallel, hours of use.
- A ~10-task comparison against a single agent on real, multi-file tasks (`plan.md` §9). The starter set is too small to show it: on its one-file tasks a single agent is faster.
- **Phase 2 and 3 against the real CLIs:**
  - `codex exec resume` and `claude --resume` (the flags come from the CLIs' docs and help output, not from a recorded run)
  - Claude running verify commands through `allowedTools`
- **The release pipeline:** `sy update` has not yet run against a real release.
- **Notifications on a real desktop:** Windows toast, macOS and notify-send.
- **`sy app` window detection** on real Windows and macOS machines.
- **The Windows stress run:** its handle-count limits are first guesses until the first CI run.

**Open risk:** a full Windows freeze happened on 3 Oct while using `sy`.
- The logs show the same unexplained hard resets since August, before Switchyard existed, with no blue screen and no disk or memory exhaustion.
- So the most likely cause is the hardware or drivers failing under load, with Switchyard's load as the trigger.
- Phase 1 still adds load limits, so `sy` can never be the thing that pushes a machine over.

---

## Phase 1 — Trustworthy (stabilize)

*Make it impossible for `sy` to hurt your machine, your repo or your quota, and make every failure diagnosable.*

| # | Item | Why |
|---|---|---|
| | *Status: 1.2, 1.3, 1.4 and 1.5 are **done**. 1.1, 1.6, 1.7, 1.8 and 1.9 are open.* | |
| 1.1 | 🟡 **Real Codex end-to-end run** (partly done): a real `codex exec --json` run and a real `codex exec resume` are recorded as test fixtures (`internal/runner/testdata/codex_real_*.jsonl`). They match the parser and resume works. They also showed that Codex sends **no `rate_limits`**, so Codex can only fall back after a limit, not switch before it. Still to record: a run with tool calls and edits, a limit hit, and a logged-out CLI. | Codex was tested only against hand-built output until now. |
| 1.2 | ✅ **Load limits** (done): agents and git at below-normal priority, parallel checkout capped at min(4, cores/2), a busy gate that holds new agents above `max_cpu_percent` or below `min_free_memory_mb` (at most `busy_max_wait`). Was planned as: cap git's parallel checkout (`checkout.workers` = half the cores, at most 4), lower process priority for agents and git (`BELOW_NORMAL_PRIORITY_CLASS`), prewarm at most one pool slot at a time, and a `max_cpu_load` setting that pauses dispatch while the machine is pegged. | A freeze under load must never be triggered by `sy`. |
| 1.3 | ✅ **Disk guard** (done): `min_free_disk_gb` floor, pruning after `pool_max_idle`, `pool_warn_gb` warning, pools listed in `sy doctor`, one shared pool per repo. Was planned as: show the pool size in `sy doctor`, warn at more than X GB, prune slots unused for 14 days, and refuse to create a slot when free space drops under 10 GB. | Each pool slot is a full checkout of your repo. |
| 1.4 | ✅ **Crash safety** (done): `sy-debug.log`, `crash-*.log`, task-level panic recovery, `sy bugreport`. Was planned as: a panic handler that restores the terminal and writes `crash-<time>.log`; a persistent debug log (`%LocalAppData%\switchyard\logs\sy.log`) with every spawned command line, exit code and timing; `sy bugreport` zips the last session log, debug log, config and `doctor` output. | When something goes wrong, you can send one file and the reason is visible. |
| 1.5 | ✅ **Undo a task** (done): `sy undo` / `/undo` with preview, redo, later edits kept by 3-way merge, last 30 tasks. Was planned as: `sy undo` / `u` restores the working tree to the snapshot taken when the task started (the snapshot commit already exists), with a preview first. | It's the single biggest trust feature: trying a task becomes risk-free. |
| 1.6 | **Real-use test pass on Windows**: Windows Terminal and the old console, a user name with a space, paths with spaces, OneDrive folders, a big repo with LFS, Defender on, sleep/resume during a task, closing the window mid-task. | These are where Windows tools usually break. |
| 1.7 | ✅ **Long-run stress test** (done: `internal/orchestrator/stress_test.go`, `.github/workflows/stress.yml`, demo and real-git modes with cancels). CI runs the orchestrator in a loop for 30 minutes and checks that memory, goroutines, open handles and leftover processes stay flat. | Catches leaks before you find them as freezes. |
| 1.8 | ✅ **Parser fuzzing** (done: 15 fuzz targets, nightly in `.github/workflows/fuzz.yml`; fixed a Codex error line counted as success, plan dependencies on reserved ids, reset-time overflow, `@` parsing and a change-list panic). Go fuzz tests for the Codex and Claude output parsers and for plan and verdict parsing. | Odd model output must never crash `sy` or leave it stuck. |
| 1.9 | ✅ **Review the worktree pool** (done: 17 findings fixed, including a crash leaving a slot that disabled worktrees, agent commits being lost, non-atomic apply, the Windows command-line limit, submodules, symlinks, git hooks in slots and orphan agents). It got the same adversarial pass that found the earlier bugs. | It is the newest and most complex code and has had only one review pass. |

**Exit criteria:**
- 2 weeks of daily use with no crash, no hang and no lost work.
- Every failure can be traced from `sy bugreport` alone.
- No machine freezes during heavy runs on the same hardware.

---

## Phase 2 — Useful every day

*Remove the reasons you'd fall back to plain `codex` or `claude`.*

| # | Item | Why |
|---|---|---|
| 2.1 | ✅ **Plan approval step**: the plan opens in the TUI (or on the terminal with `sy run --approve`). You can delete, reorder or edit subtasks, pin a role, or cancel. Turn it off with `/approve off`. | You stay in control of what runs, before any quota is spent. |
| 2.2 | ✅ **Review the diff before it lands** (`review_changes`): a per-file diff view where you can accept, accept only some files, reject, or send it back with feedback (the agent continues in its worktree). Rejected work is kept on a branch. | Bad edits are rejected before they reach your tree, not undone after. |
| 2.3 | ✅ **Agents can run tests safely**: `verify.commands`, detected by `sy init` (Go, npm/pnpm/yarn/bun, pytest, cargo, dotnet, Maven, Gradle). They become Claude `allowedTools`, are run by `sy` before the final review, and failures feed the fix round. Codex workers already run commands inside their workspace-write sandbox. | Today Claude workers can edit but cannot verify their own work. |
| 2.4 | ✅ **Follow-up messages**: `@agent message` resumes that agent's CLI session (`codex exec resume`, `claude --resume`), falling back to a fresh agent with context. Finished agents only; a running agent is not interrupted. | Real work is iterative; today every follow-up starts a new task. |
| 2.5 | ✅ **Task history and resume**: the state of every task is saved after each step. `sy history` / `/history` list tasks; `sy resume` / `/resume` continue an interrupted one, skipping finished steps. Diffs per task come from `sy undo --list`. | Closing the window or a reboot no longer loses progress. |
| 2.6 | ✅ **Task queue**: submitting while a task runs queues it in the TUI (`/queue`). `sy run --file tasks.txt` runs a list overnight. Queued tasks run unattended. | Uses quota while you're away. |
| 2.7 | ✅ **Notifications**: a desktop notification when a task finishes or fails, a limit is hit, or `sy` waits for you (Windows toast, macOS, notify-send). | You don't have to watch the terminal. |
| 2.8 | ✅ **Distribution** (pipeline built, not yet run): a tag builds release binaries for Windows, Linux and macOS with checksums. There are Scoop and winget manifests, and `sy update` (checksum-verified, swaps the running .exe safely on Windows). Code signing needs a certificate: see `packaging/README.md`. | Installing no longer needs Go or a build. |

**Gaps closed since:**
- Dependencies can be edited in the plan view (`x`).
- Review works per hunk.
- Review warns when no worktree is possible.
- Feedback in the last round rejects instead of applying.
- Follow-ups survive restarts and reach running agents.
- A resumed step is told that it was interrupted.
- Codex resumes get an explicit sandbox.
- The repo has an MIT license.

**Remaining gaps:**
- **2.3 Tests:** Codex runs commands in its own sandbox, not from an allowlist, so `verify.commands` can only restrict Claude. Codex offers no allowlist, so this is a limit of the CLI.
- **2.4 Follow-ups:** a Claude agent that ran in a pool worktree can't be resumed from the main tree, because Claude ties its sessions to the folder. It gets a fresh agent with context instead.
- **2.5 Resume:** a step that was running when `sy` died starts again. It is told about any half-done edits.
- **2.8 Distribution:**
  - Code signing needs a certificate.
  - The Scoop and winget manifests must be rendered after each release.

**Exit criteria:**
- You reach for `sy` before plain `codex` or `claude` for multi-step work.
- A new user goes from install to first task in under 5 minutes.

---

## Phase 3 — Smarter (measure, then tune)

*Prove that Switchyard beats a single agent, then make it better on data.*

| # | Item | Why |
|---|---|---|
| 3.1 | ✅ **Benchmark command**: `sy bench` with `bench.yaml`, check commands, routed vs single, saved results, and `sy bench --starter` (five Python tasks with check scripts). | This is the success measure from `plan.md` §9, automated. |
| 3.2 | ✅ **Quota-aware scheduling** (done early, Claude; Codex as soon as its CLI reports `rate_limits`): `quota-preempt` at `switch_at_utilization`, and the planner and reviewer retry on the other provider. Was planned as: use Claude's live 5-hour and 7-day utilization (already received) and Codex limits to move work to the other provider *before* hitting the limit, not after. | Avoids stalls entirely. |
| 3.3 | ✅ **Rule tuning from stats**: `sy tune` flags failing routes, frequent escalations, rejected reviews, quota pressure and over-sized read-only models, and prints the `/route` / `/prefer` command for each. | Routing improves from your own data. |
| 3.4 | ✅ **Judge model** (measurement): decisions record whether the judge ran, and `sy tune` compares judged with rule-routed steps to suggest `/judge on` or `/judge off`. | Spend quota only where it pays. |
| 3.5 | ✅ **Context hand-off**: a repo map and notes from earlier tasks in the same repo go into planner and step prompts, and every writer gets what this task's read-only steps found. | Fewer tokens, faster workers. |
| 3.6 | ✅ **Cost visibility**: fresh tokens per provider, Claude API-equivalent $, and limit before and after, in the TUI, `sy run` and `sy stats`, plus a per-day table in `sy stats`. | You can see what each task cost. |

**Gaps closed since:**
- `sy tune` reads models, efforts and the fast tier from your config.
- It compares the judge's own tokens with the retries the judge saved.
- `routed-nohandoff` measures the hand-off; the first run showed a small saving.
- Notes are dropped when they are older than 60 days or all their files are gone.

**First measurements** ([docs/bench](docs/bench/2026-10-03-starter-claude.md)):
- On five small one-file tasks, a single Claude agent was about 3x faster and used half the tokens.
- As a result, one-step plans now skip the plan review. That cut routed time by 32% and tokens by 23%.
- Whether routing pays off on bigger tasks is still open (exit criterion below).

**Remaining gaps:**
- `sy tune`'s thresholds are first guesses. Adjust them once real logs exist.

**Exit criteria:** on the benchmark, Switchyard beats a single agent on at least 2 of the 3 measures: correctness, wall time, and how quickly the limits are reached.

---

## Phase 4 — Nicer and broader

| # | Item |
|---|---|
| 4.1 | ✅ **`sy web`**: a local browser UI on the same engine. It has the agent tree, the activity log, the plan editor, hunk review, models and routes, settings, history, the queue and stats/tune. It listens on 127.0.0.1, with a per-run token and strict Host/Origin checks. |
| 4.2 | ✅ **Desktop window**: `sy app` opens `sy web` in Edge or Chrome app mode instead of using Wails. That means no cgo and nothing to install, and it closes when the window does. |
| 4.3 | **More providers**: a generic runner interface for other official CLIs (for example Gemini CLI) via config. |
| 4.4 | ✅ **Per-repo profiles**: a `.switchyard.yaml` in the repo is layered over your config. The parts that run commands need `sy trust`. Create one with `sy init --repo` or `/save repo`. |
| 4.5 | ✅ **Hooks**: `before_task` (a failure stops the task), `after_merge` and `after_task`, with `SY_*` environment variables. |
| 4.6 | ✅ **Hunk-level review**: in the TUI and in `sy web`. |
| 4.7 | ✅ **Talk to a running agent**: `@agent message` is delivered when the agent's turn ends, before its work is merged. |
| 4.8 | ✅ **Persistent follow-ups**: sessions are kept per project folder. |
| 4.9 | **Scheduled runs**: start a task file at a set time, for example when the Claude 5-hour window resets. |
| 4.10 | ✅ **Task reports**: `sy report` writes one self-contained HTML (or `--md` Markdown) page per task: plan and results, routing decisions with rule and reason, reviews, checks, the diff and the cost. Everything is escaped, and a CSP blocks scripts. |
| 4.11 | ✅ **MCP servers**: an `mcp:` config section passes MCP servers to both CLIs per role (Claude: a temporary `--mcp-config` file; Codex: `-c mcp_servers.*`). `${VAR}` comes from your environment, repo files need `sy trust`, and `sy doctor` checks the commands. |
| 4.12 | ✅ **Multi-repo tasks**: `--repo name=path` or `workspace: repos:` in `.switchyard.yaml`. The planner assigns each subtask a repo; writers run in that repo's worktree pool or main tree; each repo's own (trusted) checks run there; the final review sees every repo's diff; one `sy undo <key>` reverts every repo; history and resume keep the repos. |

---

## Standing quality rules (all phases)

- **A bug fix needs a test.** Every fixed bug gets a regression test that fails without the fix. This has been the practice so far.
- **Windows first.** Every PR passes CI on Windows, Linux and macOS. Anything touching processes, paths or the console also gets a Windows-specific test.
- **No silent failure.** Every error reaches the TUI log and the session log with a next step, such as "run `sy doctor`" or "/limit reset".
- **Safe by default.** Never commit to your branch, never touch your index, never overwrite your concurrent edits, and keep the option to undo.
- **Pin and record CLI versions.** When Codex or Claude Code updates, record new output fixtures before raising the tested version in `sy doctor`.
- **Adversarial review before merge.** Changes to the orchestrator, git, process or runner code get a second review pass focused on concurrency, Windows and failure paths.

---

## Suggested order for the next steps

1. **Use it for real on Windows and send a `sy bugreport` after any problem.** Items 1.6 (the Windows test pass) and the exit criterion (2 weeks of daily use) need you at the keyboard. Try each new feature once:
   - approve and edit a plan;
   - `/review-changes on` for one task;
   - `@ follow-up` after a task;
   - close the window mid-task, then `sy resume`;
   - `sy app`.
2. **1.1 The rest of the Codex recordings.** In any git repo on your PC:
   - an edit: `codex exec --json --skip-git-repo-check -m gpt-6-luna "create hello.txt containing hi" > codex-edit.jsonl`
   - once you hit a Codex limit, run the same command again and keep the file (`codex-limit.jsonl`).
   - after `codex logout`, run it once more (`codex-logout.jsonl`), then `codex login`.
   Send the files; they become test fixtures.
3. **Run `sy bench` on ~10 real, multi-file tasks from your own repos, with Codex.** This decides the Phase 3 exit criterion. After a week of use, run `sy tune`.
4. **Releases:**
   - Submit the rendered winget manifests to microsoft/winget-pkgs (needs a fork of winget-pkgs on your account).
   - After each release, render the manifests (`packaging/render-manifests.sh X.Y.Z`).
   - For signed binaries, buy a code-signing certificate (see `packaging/README.md`).
5. **Open Phase 4 items:**
   - 4.3: more providers. Started, then paused at your request.
   - 4.9: scheduled runs.
