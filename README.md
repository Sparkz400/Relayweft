# Switchyard

A Windows-first, animated terminal app that routes coding work between your **ChatGPT (Codex CLI)** and **Claude (Claude Code)** subscriptions. It picks a model for each step, runs agents in parallel and only calls the expensive models when it matters.

```
sy            # start the TUI in your project
sy web        # the same engine in your browser (sy app: in its own window)
sy --demo     # see the whole thing animate with fake agents (no CLIs, no quota)
```

![sy web: agent tree and live activity](docs/web/running.png)

Switchyard uses **subscriptions only**. It never touches API keys or tokens; it drives the official `codex` and `claude` CLIs exactly as you would, with their normal login.

---

## Quick start (Windows)

1. **Install the CLIs and log in once** (PowerShell):
   ```powershell
   npm install -g @openai/codex
   codex login
   npm install -g @anthropic-ai/claude-code   # or the native installer
   claude            # log in once, then exit
   ```
2. **Install Git 2.38+** from <https://git-scm.com/>.
3. **Install Switchyard**, one of:
   - Download `sy-windows-amd64.exe` from the [latest release](https://github.com/sparkz400/switchyard/releases/latest), rename it to `sy.exe` and put it on your PATH. Later, `sy update` replaces it with the newest release (checksum-verified).
   - Scoop: `scoop install https://raw.githubusercontent.com/sparkz400/switchyard/main/packaging/scoop/sy.json`. winget follows once the package is accepted into winget-pkgs; see `packaging/README.md`.
   - From source with **Go 1.24+**:
     ```powershell
     git clone https://github.com/sparkz400/switchyard
     cd switchyard
     go build -o sy.exe ./cmd/sy
     ```
     Or `go install ./cmd/sy`, which puts `sy.exe` in `%USERPROFILE%\go\bin` (that folder must be on your PATH). CI also builds `sy.exe` as a downloadable artifact on every push.
4. **Check the setup:**
   ```powershell
   .\sy.exe doctor
   ```
5. **Try it:**
   ```powershell
   .\sy.exe --demo                      # the full animated pipeline, fake agents
   cd C:\path\to\your\repo
   C:\path\to\switchyard\sy.exe         # the real thing
   ```

Use **Windows Terminal** for the full look. Legacy `conhost` is detected and gets an ASCII theme (force either with `--ascii` / `--unicode`).

**Tested CLI versions:** `codex-cli 0.160.0` and `Claude Code 2.1.287`. Output formats can change between releases. `sy doctor` warns when your versions differ, and the parsers are covered by recorded JSON fixtures in `internal/runner/testdata`.

---

## What happens when you submit a task

```
plan (planner, read-only)
  -> review the plan (reviewer, other provider)   <- checkpoint 1
  -> fan out: explorer / researcher / worker agents in parallel
       writing agents each get their own git worktree, merged back as they finish
       same error twice -> escalate one tier + reviewer diagnoses   <- checkpoint 2
       provider hits its usage limit -> same role on the other provider
  -> review before done (reviewer)                 <- checkpoint 3
       changes requested -> one fix round -> review again
```

Tasks shorter than 12 words skip the planner and run as one worker step (or one explorer step for questions).

With the defaults, two more steps involve you or your repo's checks:
- **You approve the plan** before any agent runs (`approve_plan`).
- **Your checks run** (`verify.commands`, such as `go test ./...`) after the agents finish and before the final review. Failures go into the fix round together with the reviewer's advice.

## Working with it every day

- **Approve the plan.** The plan opens for you before anything runs. You can delete, reorder or reword subtasks, pin a subtask to a role, or cancel. Turn it off with `/approve off` or `orchestrator.approve_plan: false`. Small tasks (one step) skip it.
- **Review changes before they land** (opt-in: `/review-changes on` or `orchestrator.review_changes: true`). Each writing agent's result is shown file by file with its diff. You can accept everything, accept only some files or only some hunks of a file, reject it, or send it back with feedback. With feedback, the agent continues in its own worktree and you see the new result. Rejected or partly-accepted work is kept on a `sy/...` branch.
- **Edit the plan's order.** In the plan view, `x` edits a step's dependencies. Cycles are refused.
- **Agents run your tests.** `sy init` detects your checks (`go test`, `npm test`/`pnpm`/`yarn`, `pytest`, `cargo test`, `dotnet test`, Maven, Gradle) and writes them to `verify.commands`. Claude workers may run exactly these commands without asking, Codex workers already can in their sandbox, and Switchyard runs them itself before the final review. Change them with `/verify`.
- **Follow up.** `@worker-id also handle the empty case` (or `@ message` for the last agent) continues that agent's own CLI conversation (`codex exec resume` / `claude --resume`), so it remembers what it did.
  - If the conversation can't be resumed, a fresh agent on the same route gets the earlier task and answer as context.
  - Agents are remembered per project across restarts.
  - Sending `@agent` to an agent that is **still running** delivers the message when its current turn ends, before its work is merged.
- **Queue tasks.** Submitting while a task runs queues the new one (`/queue` to list, `/queue rm <n>`, `/queue clear`). Queued tasks run one after another, unattended: no approvals. Headless, use `sy run --file tasks.txt`, one task per line or blocks separated by `---`.
- **History and resume.** Every task's plan and per-step results are saved as it runs. If `sy`, the terminal or the PC dies mid-task, `sy resume` (or `/resume`) continues it: finished steps are skipped and the rest runs, then verify and review. `sy history` (or `/history`) lists recent tasks with status and cost.
- **Notifications.** A desktop notification (a Windows toast, macOS Notification Center or `notify-send`) when a task that ran at least `notify.min_task` (1 minute) finishes or fails, when a provider hits its limit, and when `sy` waits for your approval.
- **Context hand-off** (`orchestrator.handoff`). Planner and workers get a compact map of the repo, short notes from earlier successful tasks in the same repo, and what this task's read-only steps found. They spend fewer tokens finding their way around.

## Choosing models: any model for any job

Each **role** has a route on **both** providers, and `prefer` decides which one is used:

| Role | Default prefer | Codex default | Claude default |
|---|---|---|---|
| planner | codex | gpt-6.1-sol @high | opus @high |
| worker | codex | gpt-6.1-sol @medium | sonnet @medium |
| worker_high | codex | gpt-6.1-sol @xhigh | opus @high |
| explorer | claude | gpt-6-luna @low | haiku |
| researcher | claude | gpt-6-luna @low | haiku |
| reviewer | **other** | gpt-6.1-sol @xhigh | opus @high |
| judge | claude | gpt-6-luna @low | haiku @low |

`prefer` can be `codex`, `claude`, `other` (the opposite of the planner's provider, which is what the reviewer uses), or `auto` (whichever provider has used fewer tokens this session). If the preferred provider is at its usage limit, the role's route on the other provider is used automatically.

There are four ways to change any of this, at any time:

- **In the TUI:** press `m` (or `ctrl+o`) for the **model picker**. Arrow keys pick a role and column, `enter` lists the catalog (or `custom…` to type any model id), and `s` saves to `switchyard.yaml`. The "NOW USES" column shows the live result, including limit fallbacks. Changes apply to the next agent that starts.
- **At the prompt:** `/route worker claude:sonnet:high`, `/prefer all claude`, `/save`.
- **On the command line:** `sy --route reviewer=claude:fable:max --prefer explorer=codex`. Naming a route on the command line also sets that role's `prefer`. `sy --provider claude` forces every role onto one provider.
- **In the file:** `sy init` writes a commented `switchyard.yaml` you can edit.

**Model catalogs.** The Codex catalog comes from `codex debug models`; run `sy models --refresh` to update it after a Codex release (`--all` includes hidden models). Claude takes aliases (`fable`, `opus`, `sonnet`, `haiku`) or full ids (`claude-opus-5-5`, ...). Efforts: Codex `low|medium|high|xhigh|max|ultra`, Claude `low|medium|high|xhigh|max`, or empty for the CLI default.

## The TUI

```
┌ header: project · phase · provider usage / quota / limit ──────────────┐
├ legend ────────────────────────────────────────────────────────────────┤
│ ┌ reviewer ┐   ┌ main agent ┐                                          │
│ │checkpoints│  └─────┬──────┘                                          │
│ │advice     │  ┌ router: decisions + confidence bars ┐                 │
│ │           │  └─────┬───────────────────────────────┘                 │
│ │           │  ┌ worker ┐ ┌ explorer ┐ ┌ researcher ┐  ● pulses travel │
│ │           │  └────────┘ └──────────┘ └────────────┘                  │
│ └───────────┘  ┌ back to main · merge + review + verify ┐              │
├ session log (or the selected agent's log) ─────────────────────────────┤
└ prompt ────────────────────────────────────────────────────────────────┘
```

**Prompt.** Focus starts in the prompt: type a task and press `enter`. The prompt is multi-line:
- Pasting multi-line text keeps every line and **never submits**; you press `enter` yourself when ready.
- On Windows, pastes arrive as single keystrokes, so Switchyard detects them by their speed.
- `alt+enter` (or `ctrl+j`) adds a new line by hand.
- `ctrl+x` cancels the running task from anywhere, even while typing.

**Keys.** `tab`/`esc` move focus to the agents, where:

| Key | Action |
|---|---|
| `tab` / `shift+tab` | select the next / previous agent (its own log is shown); cycling past the last one returns to "all" |
| `k` `k` | kill the selected agent (press twice) |
| `p` | pause / resume dispatching (running agents finish, no new agent starts) |
| `x` / `ctrl+x` | cancel the whole task: every agent's process tree is stopped, nothing new starts, and agents that never ran are marked stopped |
| `l` | toggle the full-screen log (`pgup`/`pgdn` scroll) |
| `m` | model picker |
| `enter`, `i`, `/` | back to the prompt |
| `q` | quit (press twice while agents run, which stops them) |

**Commands** (type at the prompt; `/help` lists them):
- `/models`, `/route <role> <provider>:<model>[:effort]`, `/prefer <role|all> <codex|claude|other|auto>`, `/save`
- `/single <provider>:<model>[:effort] <task>` runs a single-agent baseline
- `/limit <codex|claude> [reset|set]` to correct the limit state by hand
- `/threads <n>`, `/parallel on|off`, `/review on|off`, `/judge on|off`
- `/approve on|off` (plan approval), `/review-changes on|off` (per-file change review), `/verify [<cmd>|clear]`
- `@<agent> message` (or `@ message` for the newest agent; `tab` completes ids) sends a follow-up, `/agents` lists who can take one
- `/queue`, `/queue rm <n>`, `/queue clear`; `/history`; `/resume [<id>]` continues an interrupted task
- `/pause`, `/unpause` (`/resume` also unpauses while paused), `/kill <agent>`, `/cancel`, `/clear`, `/usage`
- `/undo` previews reverting the last task, `/undo yes` applies it; `/redo` and `/redo yes` put it back

## Other commands

```
sy run "task"                         headless: same pipeline, events printed as lines
sy run --single claude:opus "task"    single-agent baseline (for comparison in stats)
sy run --file tasks.txt               run several tasks one after another, unattended
sy run --approve "task"               approve the plan (and changes, with review_changes) on the terminal
sy history [--all] [-n 20]            recent tasks: status, steps done, cost; marks interrupted ones
sy resume [task id] [--force]         continue an interrupted task (default: the last one in this directory)
sy tune [--here] [--since 7d]         routing suggestions from your own logs, as ready-to-paste commands
sy update [--check] [--yes]           update sy to the latest GitHub release (checksum-verified)
sy web / sy app [--port N] [--demo]   the browser UI / the same in its own window
sy init --repo                        write this repo's .switchyard.yaml (shared settings)
sy trust [--revoke]                   review and trust the commands in this repo's .switchyard.yaml
sy undo [--list] [--redo] [--yes] [task]   revert a task's changes (preview first), or put them back
sy bench [--init] [--file bench.yaml] [--only a,b]   routed vs single agents on your own tasks
sy bench --starter <dir>              a ready-made 5-task Python benchmark repo
sy stats [--here] [--since 7d]        usage per model, rules fired, routed vs baseline, per day, recent task costs
sy models [--refresh] [--all]         routes + catalogs; refresh Codex catalog
sy doctor                             CLIs, logins, git, terminal, machine load, free disk, worktree pools
sy bugreport [--out file.zip]         one zip with logs, crash logs, config and doctor output to send
sy init [--global] [--force] [--print]
sy clean [--dir <path>] [--idle 72h]  remove this repo's pooled worktrees (or every repo's idle ones)
```

`sy run` exits 1 when the task fails, so it is scriptable.

### Undo: try anything, risk-free

Every task in a git repo records the working tree before and after it ran. The snapshots are kept under `refs/switchyard/tasks`, so git never garbage-collects them and your branches stay untouched.

- `sy undo` (or `/undo` in the TUI) shows which files the last task changed, then reverts exactly those: changed files are restored, created files removed, deleted files recreated.
- Files you edited *after* the task keep your edits (3-way merge). If an edit overlaps the task's change, nothing at all is changed and you're told which file.
- `sy undo --redo` (or `/redo`) puts the task's changes back. `sy undo --list` shows the last 30 tasks.
- After every task, `sy run` prints the exact `sy undo <task>` command.

### Bench: does Switchyard beat a single agent on *your* work?

1. Run `sy bench --init` to create `bench.yaml`. Fill in a few real tasks, each with a check command (`go test ./...`, `npm test`, ...), then commit.
2. Run `sy bench`. It runs every task in every mode (`routed`, `single:codex:gpt-6.1-sol:high`, ...) from a clean checkout of `HEAD`, in a worktree outside your repo. A run passes when its check command exits 0.
3. Read the results: a table of pass rate, wall time, tokens per provider and Claude's API-equivalent cost. It is printed and saved as `bench-results-<time>.md`.

It uses real quota, so it asks first.

No tasks of your own yet? `sy bench --starter bench-starter` creates a small Python repo with five tasks (two bug fixes, a parser feature, a CLI flag and a read-only question), each with a check script. Then run `cd bench-starter && sy bench`. It needs Python 3. The `routed-nohandoff` mode runs the same routes without the context hand-off, to measure what it saves.

First results (Claude only): [docs/bench](docs/bench/2026-10-03-starter-claude.md). On tasks this small a single agent is faster and cheaper. Skipping the review of one-step plans, now the default, cut routed time by 32% and tokens by 23%.

### Tune: let your logs pick the routes

`sy tune` reads the session logs and suggests changes, each with the `/route`, `/prefer` or `/judge` command to apply it (and the `sy --route` flag form). It looks for:
- routes that fail often;
- roles whose steps keep escalating;
- final reviews that reject a lot of work;
- a provider that keeps running out of quota while cheap roles still use it;
- read-only roles on an expensive model that never fail;
- whether turning the judge on (or off) would pay;
- routed tasks doing worse than single-agent runs.

It needs about 10 logged tasks before its suggestions mean anything. `sy stats` also has a per-day table (tasks, success, fresh tokens per provider, $).

### Cost of every task

When a task finishes, the TUI log, `sy run` and `sy stats` show what it used:

> codex 12k · claude 40k fresh tokens · ≈$0.31 API-equivalent · claude limit 61%→64%

"Fresh" means uncached input plus output. The $ figure is what Claude Code reports a task *would* cost on the API; on a subscription you are not billed it, but it is a good relative measure. The limit share comes from the provider's own quota reports.

### Budgets

Cap what one task and one day may use (`budget:` in `switchyard.yaml`, 0 = off):

```yaml
budget: {task_tokens: 0, task_usd: 2, day_tokens: 0, day_usd: 10, warn_at: 0.8}
```

Tokens are fresh tokens on both providers; $ is Claude's API-equivalent price. The day total is today's finished tasks (local time, from the session logs of every `sy`) plus the running one. Before each agent starts and after each one finishes, `sy` checks the limits:

- at `warn_at` (80%) the log shows one warning per limit;
- at a limit, a task you are watching asks: **continue** (until this task ends) or **stop**. The TUI shows a small prompt (y/n), `sy web` a dialog, `sy run --approve` asks on the terminal;
- queued, scheduled and `--file` tasks, and `sy run` without `--approve`, never ask: they stop cleanly with "stopped by budget: …" and say how to raise the limit.

Flags for one run: `--budget-task-tokens`, `--budget-task-usd`, `--budget-day-usd` (on `sy`, `sy run`, `sy web`). The TUI and web headers show e.g. `$0.42/$10 today`, and `sy stats` adds the daily budget to its per-day table.

### Scheduled runs

Start work later, for example overnight or when a usage window resets:

```
sy run --file tasks.txt --at 02:30        # today, or tomorrow if 02:30 has passed; also "2026-10-04 02:30" or RFC3339
sy run --in 3h "update the dependencies"
sy run --file tasks.txt --when-reset claude   # claude | codex | any
```

`--when-reset` uses the newest known reset time: Claude's quota reports and limit hits are logged, so a later `sy run` finds them. When it is unknown or already past, the run starts now and says so. While waiting, `sy` prints a countdown every minute (Ctrl+C cancels) and keeps the PC from sleeping until the run ends (Windows: `SetThreadExecutionState`; macOS: `caffeinate`); `--allow-sleep` turns that off. Scheduled runs are unattended: no approvals, and a budget limit stops them.

In the TUI, `/schedule 02:30 <task>`, `/schedule in 2h <task>` or `/schedule reset claude <task>` puts the task in the queue with a start time; it runs when due, after any running task. `/schedule` lists them, `/schedule rm <n>` removes one. In `sy web`, the Queue panel has the same form, and scheduled items show their time and a remove button.

`sy schedule --file tasks.txt --at 02:30 [--daily]` prints a ready Windows Task Scheduler (`schtasks /create …`) or cron command, so the OS starts `sy` even when no terminal is open. It installs nothing.

### Protecting your machine

Switchyard should never be what tips a PC over.

- **Low priority.** Agents and every git command run below normal CPU priority (`low_priority`), and their child processes inherit it.
- **Bounded checkout.** Git's parallel checkout is capped at half the cores, at most 4.
- **Busy gate.** While the machine is above `max_cpu_percent` (90) or below `min_free_memory_mb` (1024), no *new* agent starts. The first agent of a task always runs, and after `busy_max_wait` (2m) the next one starts anyway, so a busy machine slows `sy` down but never stalls it. The log says when an agent is held.
- **Disk guard.**
  - No new pool worktree is created below `min_free_disk_gb` (10); writers then take turns in your tree instead.
  - Pool slots unused for `pool_max_idle` (14 days) are removed automatically.
  - You get a warning when a repo's pool passes `pool_warn_gb` (20).
  - `sy doctor` lists every pool with its size.
- **Switch before the limit.** When a provider reports it has used `switch_at_utilization` (90%) of its limit (Claude's 5-hour or 7-day window), work moves to the other provider *before* the limit hits. The router panel shows these decisions as `quota-preempt`.

### When something goes wrong

- **Debug log.** Every agent spawn and exit, git command, routing decision and error is written to `sy-debug.log`, which rotates at 10 MB. It lives in `%AppData%\switchyard\logs` on Windows and `~/.config/switchyard/logs` on Linux.
- **Crash logs.** A crash anywhere writes `crash-<time>.log` there, with the stack and the recent log. A crash inside a task ends only that task, not `sy`.
- **`sy bugreport`.** Zips the environment, PATH, `sy doctor` output, your config, the last 3 session logs, and the debug and crash logs into one file to send.

## How it works (and the decisions made for v1)

- **Runners** (`internal/runner`) spawn the CLIs, pass the prompt on **stdin** (multi-line prompts as arguments get mangled by `cmd.exe` when the CLI is an npm `.cmd` shim), stream JSON lines and normalize them into one `Event` type. The TUI and the log only ever see `Event`s.
  - Codex: `codex exec --json --color never --skip-git-repo-check -m <model> -c model_reasoning_effort=<effort> --sandbox <workspace-write|read-only> -C <dir> -`
  - Claude: `claude -p --output-format stream-json --verbose --model <model> [--effort <e>] --permission-mode acceptEdits`. Read-only roles use `--permission-mode dontAsk --tools Read,Grep,Glob,WebSearch,WebFetch`.
  - **Workers on Claude cannot run shell commands by default** (`acceptEdits` only allows edits). To let them run tests, set e.g. `write_allowed_tools: ["Bash(go test *)", "Bash(npm test)"]`, or `write_permission_mode: auto`, under `providers.claude`.
- **Read-only roles** (planner, explorer, researcher, reviewer, judge) can never write: Codex runs them with `--sandbox read-only`, Claude without write tools.
- **Parallel worktrees** (`internal/orchestrator/git.go`):
  - At the start, your working tree (tracked + untracked, honouring `.gitignore`) is captured as a snapshot commit with a temporary index. **Your index, HEAD and branch are never touched.**
  - Each writing agent gets a **pooled worktree** outside your repo (in the user cache dir), moved to the current integration commit with `git checkout --detach`. Slots persist between tasks, so git only rewrites the files that changed: about 0.2 s per slot in a 20,000-file repo, against about 6 s to create and delete a fresh worktree. Missing slots are created in the background while the planner runs. Ignored files (`node_modules`, build caches) survive in the slots. A lock file per slot keeps two `sy` instances apart. `sy clean` removes the pool.
  - When it finishes, its work is merged with `git merge-tree --write-tree` (an object-only merge, hence git 2.38+).
  - The changed files are written into your working tree with `git restore --source`, so CRLF/`autocrlf` on Windows is handled by git itself.
  - If you edited one of those files meanwhile, a `git merge-file` 3-way merge is used; if that conflicts, nothing is overwritten.
  - On a conflict, that agent's work is kept on branch `sy/<session>/<step>` and the reviewer is told.
  - Dependent subtasks start from the merged state.
  - Without git, or with one writing step, agents work directly in your directory, one writer at a time.
  - Big repos:
    - The snapshot starts from a copy of your index, so only changed files are re-hashed.
    - Worktrees keep Git LFS files as pointer files (`GIT_LFS_SKIP_SMUDGE=1`), and agents are told so.
    - Checkouts use all CPU cores (`checkout.workers=0`).
    - In repos with 5,000+ tracked files, `sy doctor` and the session log suggest `git config core.fsmonitor true` and `git config core.untrackedCache true`, which make snapshots nearly instant.
    - `orchestrator.worktree_max_files` can still turn worktrees off above a file count (off by default).
  - Limitation: worktrees do not contain ignored files such as `node_modules`, so agents in worktrees may not be able to run builds that need them. Set `orchestrator.worktrees: false` (writers then run one at a time in your tree) or `max_threads: 1` if that matters.
- **Usage-limit detection** (`internal/limits`):
  - Claude's `rate_limit_event` with status `rejected`, any error text matching `limit_patterns`, or a non-zero exit with such text.
  - The reset time is parsed from "try again at 3:05 PM", "try again in 2h 10m" or Claude's `...|<unix time>`; otherwise `limit_cooldown` (1h) applies.
  - The header shows Claude's live quota utilization from the CLI's own events. `/limit` corrects the state by hand.
- **Router** (`internal/router`). The rules are evaluated top to bottom:
  1. limit → other provider
  2. review checkpoint → reviewer
  3. read-only step → explorer/researcher
  4. same error twice → escalate one tier (worker → worker_high → planner) and ask the reviewer
  5. more than `max_files_before_high` files, or a sensitive path (auth, payments, migrations, ...) → worker_high
  6. default → worker

  With `routing.judge: true`, low-confidence default decisions ask the judge model a closed A/B/C/D question.
- **Killing.** Each agent runs in its own process group (Unix) or is killed with `taskkill /T` (Windows). On Windows, `sy` also puts itself in a kill-on-close job object, so no agent outlives `sy`, even after a crash.
- **Session log.** Append-only JSONL in `%AppData%\switchyard\sessions` (`~/.config/switchyard/sessions` on Linux; `~/Library/Application Support/switchyard/sessions` on macOS). It records every decision, agent run (tokens, time, outcome), review, merge and limit hit. `sy stats` reads it; demo mode never writes it.

## The browser UI: `sy web` and `sy app`

`sy web` runs the same engine as the TUI behind a page in your browser. `sy app` opens that page in its own window, using Edge or Chrome in app mode, so you need nothing extra. The page has:
- the agent tree;
- a filterable activity log;
- the plan editor (drag to reorder, edit prompts, kinds, roles and dependencies);
- change review with per-hunk checkboxes;
- routes and models, settings, history and resume, the queue, and stats with `sy tune` suggestions;
- dark and light themes.

The server listens on 127.0.0.1 only. Each link `sy` prints or opens works once, within 2 minutes; press Enter in `sy`'s terminal for a new one. The page trades the link for a session that lives only in that browser tab, and there are no cookies. Requests from other sites, other ports and other host names are refused.

| | |
|---|---|
| ![plan approval](docs/web/plan-approval.png) | ![change review](docs/web/change-review.png) |

## Configuration

`sy` looks for `./switchyard.yaml`, then `<user config dir>/switchyard/switchyard.yaml`, then falls back to the built-in default (the file in this repo). Partial files work: anything you leave out keeps its default. See [`switchyard.yaml`](switchyard.yaml) for every option with comments.

### Per-repo settings: `.switchyard.yaml`

A `.switchyard.yaml` in a repository holds the settings for that repo (in the repo root, or in the project folder). It is layered over your own config: built-in defaults < your config < the repo file < command-line flags. It only needs what the repo cares about; roles merge per key, so `roles: {worker: {prefer: claude}}` keeps the worker's routes.

- Create one with `sy init --repo`, which detects the test commands, or with `/save repo` from the TUI. Commit it to share.
- **Commands need your trust.** The parts that run commands on your machine are ignored until you have reviewed them with `sy trust`: `verify`, `hooks`, `providers` and `log_dir`. A repo file comes from whoever pushed to the repo, so this works like direnv: any change to the file needs a new `sy trust`. `sy trust --revoke` withdraws it. Routes, preferences and toggles always apply.

### Hooks

Your own commands run around every task (`hooks:` in the config):
- `before_task`: if it fails, the task does not start.
- `after_merge`: after each agent's changes land in your tree.
- `after_task`: after every end (done, failed or cancelled).

They run in the project folder through the system shell. They get `SY_TASK`, `SY_TASK_ID`, `SY_DIR`, `SY_STATUS`, `SY_SUMMARY`, `SY_STEP` and `SY_FILES`. Typical uses are a formatter after every merge or a linter after the task.

## Development

```
go test -race ./...          # unit + integration tests (real git repos, fake CLIs)
go vet ./...
GOOS=windows go build ./cmd/sy
```

```
cmd/sy/                 CLI entry point and subcommands
internal/config/        YAML config, live store, default.yaml (keep in sync with ./switchyard.yaml)
internal/event/         the normalized Event type
internal/runner/        codex.go, claude.go, fake.go (demo) + recorded fixtures
internal/router/        rules + judge
internal/orchestrator/  lifecycle, worktrees, merges, checkpoints, prompts
internal/limits/        limit detection, reset parsing, provider state and quota utilization
internal/diag/          debug log, crash logs
internal/sysload/       CPU, memory and disk readings for the load and disk guards
internal/sessionlog/    JSONL writer + stats
internal/proc/          process-tree kill (Unix process groups, Windows taskkill + job object)
internal/tui/           Bubble Tea model, views, model picker, commands
```

See [plan.md](plan.md) for the original design and [ROADMAP.md](ROADMAP.md) for what comes next.
