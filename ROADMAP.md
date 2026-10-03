# Switchyard Roadmap

**Goal:** a tool you trust with real work every day. It must be **reliable first, useful second, smart third, pretty fourth.**

The phases are ordered. A phase starts only when the previous phase's **exit criteria** are met. Features never jump ahead of reliability.

> **Note (October 2026):** Phases 2 and 3 were built ahead of Phase 1's exit criteria, at your request. Their code is tested but has not been used for real. Phase 1's open items (1.1, 1.6–1.9) and its exit criteria still come first. Until they are met, treat Phase 2 and 3 features as a beta.

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
- **Phase 3 (built; tuning waits for data):**
  - `sy bench` with a starter set
  - switching provider before the limit
  - `sy tune` (rule tuning and judge advice)
  - context hand-off
  - cost per task and per day

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

**Not verified yet:**
- A real Codex run against a model.
- Real daily use on your Windows PC.
- Behaviour under heavy load: a big repo, three agents in parallel, hours of use.
- A ~10-task comparison against a single agent (`plan.md` §9).
- **Phase 2 and 3 against the real CLIs:**
  - `codex exec resume` and `claude --resume` (the flags come from the CLIs' docs and help output, not from a recorded run)
  - Claude running verify commands through `allowedTools`
- **The release pipeline:** no tag has been pushed yet. `sy update` has not run against a real release.
- **Notifications on a real desktop:** Windows toast, macOS and notify-send.

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
| 1.1 | **Real Codex end-to-end run**: record real `codex exec --json` output (success, tool calls, edits, limit hit, logged out) as test fixtures. | Codex has only been tested against output we built by hand from its docs and binary. |
| 1.2 | ✅ **Load limits** (done): agents and git at below-normal priority, parallel checkout capped at min(4, cores/2), a busy gate that holds new agents above `max_cpu_percent` or below `min_free_memory_mb` (at most `busy_max_wait`). Was planned as: cap git's parallel checkout (`checkout.workers` = half the cores, at most 4), lower process priority for agents and git (`BELOW_NORMAL_PRIORITY_CLASS`), prewarm at most one pool slot at a time, and a `max_cpu_load` setting that pauses dispatch while the machine is pegged. | A freeze under load must never be triggered by `sy`. |
| 1.3 | ✅ **Disk guard** (done): `min_free_disk_gb` floor, pruning after `pool_max_idle`, `pool_warn_gb` warning, pools listed in `sy doctor`, one shared pool per repo. Was planned as: show the pool size in `sy doctor`, warn at more than X GB, prune slots unused for 14 days, and refuse to create a slot when free space drops under 10 GB. | Each pool slot is a full checkout of your repo. |
| 1.4 | ✅ **Crash safety** (done): `sy-debug.log`, `crash-*.log`, task-level panic recovery, `sy bugreport`. Was planned as: a panic handler that restores the terminal and writes `crash-<time>.log`; a persistent debug log (`%LocalAppData%\switchyard\logs\sy.log`) with every spawned command line, exit code and timing; `sy bugreport` zips the last session log, debug log, config and `doctor` output. | When something goes wrong, you can send one file and the reason is visible. |
| 1.5 | ✅ **Undo a task** (done): `sy undo` / `/undo` with preview, redo, later edits kept by 3-way merge, last 30 tasks. Was planned as: `sy undo` / `u` restores the working tree to the snapshot taken when the task started (the snapshot commit already exists), with a preview first. | It's the single biggest trust feature: trying a task becomes risk-free. |
| 1.6 | **Real-use test pass on Windows**: Windows Terminal and the old console, a user name with a space, paths with spaces, OneDrive folders, a big repo with LFS, Defender on, sleep/resume during a task, closing the window mid-task. | These are where Windows tools usually break. |
| 1.7 | **Long-run stress test**: CI runs `sy run` in demo mode in a loop for 30 minutes and checks that memory, goroutines, open handles and leftover processes stay flat. | Catches leaks before you find them as freezes. |
| 1.8 | **Parser fuzzing**: Go fuzz tests for the Codex and Claude output parsers and for plan and verdict parsing. | Odd model output must never crash `sy` or leave it stuck. |
| 1.9 | **Review PR #2 (worktree pool)** with the same adversarial pass that found the earlier bugs. | It is the newest and most complex code and has had only one review pass. |

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

**Known gaps (follow-ups to Phase 2):**
- **2.1 Plan approval:** dependencies can't be edited in the overlay. Only deleting a step removes it from the dependencies.
- **2.2 Change review:**
  - Review is per file, not per hunk.
  - Changes made in the main tree (one writer with review off) are not reviewable. With review on, every writer gets a worktree.
  - Feedback reruns are capped at 2 per step.
- **2.3 Tests:** Codex runs commands through its sandbox, not an allowlist, so `verify.commands` only restricts Claude.
- **2.4 Follow-ups:**
  - Only agents that finished in the current `sy` session can take a follow-up; sessions are not kept across restarts.
  - A Claude agent that ran in a pool worktree can't be resumed from the main tree, so it gets a fresh agent with context.
- **2.5 Resume:** a step that was running when `sy` died starts again from scratch. Its partial edits stay in the tree, or in its worktree.
- **2.8 Distribution:**
  - Code signing needs a certificate.
  - The Scoop and winget manifests must be rendered after each release.
  - The repo has no LICENSE file, so the manifests say "Proprietary".

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

**Known gaps (follow-ups to Phase 3):**
- **`sy tune` model names:** the cheap and strong model names it suggests are fixed in code (haiku / gpt-6-luna), not read from your model catalog.
- **`sy tune` confidence:** its thresholds are first guesses. Adjust them once real logs exist.
- **The judge's cost:** it is measured only indirectly, as the success rate of judged vs rule-routed steps. The judge's own tokens are not compared to what it saves.
- **Context hand-off:** the gain is not measured yet. Compare `sy bench` with `orchestrator.handoff` on and off.
- **Repo notes:** they record only successful tasks. They can go stale after big refactors; delete the notes file to reset.

**Exit criteria:** on the benchmark, Switchyard beats a single agent on at least 2 of the 3 measures: correctness, wall time, and how quickly the limits are reached.

---

## Phase 4 — Nicer and broader

| # | Item |
|---|---|
| 4.1 | **`sy web`**: a local browser UI with the same engine (agent tree, diff review, model picker), clickable. |
| 4.2 | **Desktop window**: the same web UI in a native window via Wails, as `Switchyard.exe`. |
| 4.3 | **More providers**: a generic runner interface for other official CLIs (for example Gemini CLI) via config. |
| 4.4 | **Per-repo profiles**: routes, test commands and limits in `.switchyard.yaml` committed with the repo. |
| 4.5 | **Hooks**: run your own scripts before and after a task or merge (format, lint, notify). |
| 4.6 | **Hunk-level review**: accept or reject single hunks in the change-review overlay, not just whole files. |
| 4.7 | **Talk to a running agent**: queue a message that is delivered when the agent's current turn ends, instead of only finished agents. |
| 4.8 | **Persistent follow-ups**: keep agent sessions across `sy` restarts, so `@agent` works on yesterday's task. |
| 4.9 | **Scheduled runs**: start a task file at a set time, for example when the Claude 5-hour window resets. |

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

1. **Use it for real on Windows and send a `sy bugreport` after any problem.** Items 1.6 (the Windows test pass) and the exit criterion (2 weeks of daily use) need you at the keyboard. Try each Phase 2 feature once:
   - approve and edit a plan;
   - `/review-changes on` for one task;
   - `@ follow-up` after a task;
   - close the window mid-task, then `sy resume`.
2. **1.1 Real Codex run.** Run `codex exec --json -m gpt-6-luna "say hi" > codex.jsonl` on your logged-in PC and send the file; it becomes a test fixture. Also check whether the output contains `rate_limits`; if it does, the quota switch then works for Codex too. Also run `codex exec resume --last "and now say bye"` once, to confirm that follow-ups work with your Codex version.
3. **1.9 Review the PR #2 pool code and 1.8 parser fuzzing.** No user input needed.
4. **1.7 Long-run stress test in CI.**
5. **Cut the first release** (`git tag v0.1.0 && git push --tags`), then render the Scoop and winget manifests with `packaging/render-manifests.sh`. Signing needs a certificate.
6. **Run `sy bench --starter`, then `sy bench` on ~10 real tasks** as soon as Codex works. After a week of use, run `sy tune`. Its suggestions and the bench results decide the routing defaults.
