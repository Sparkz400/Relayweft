# Switchyard

A Windows-first, animated terminal app that routes coding work between your **ChatGPT (Codex CLI)** and **Claude (Claude Code)** subscriptions. It picks a model for each step, runs agents in parallel and only calls the expensive models when it matters.

```
sy            # start the TUI in your project
sy --demo     # see the whole thing animate with fake agents (no CLIs, no quota)
```

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
2. **Install Go 1.24+** from <https://go.dev/dl/> and **Git 2.38+** from <https://git-scm.com/>.
3. **Build Switchyard:**
   ```powershell
   git clone https://github.com/sparkz400/switchyard
   cd switchyard
   git checkout claude/build-switchyard
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

**Keys.** Focus starts in the prompt: type a task and press `enter`. `tab`/`esc` move focus to the agents, where:

| Key | Action |
|---|---|
| `tab` / `shift+tab` | select the next / previous agent (its own log is shown); cycling past the last one returns to "all" |
| `k` `k` | kill the selected agent (press twice) |
| `p` | pause / resume dispatching (running agents finish, no new agent starts) |
| `x` | cancel the whole task |
| `l` | toggle the full-screen log (`pgup`/`pgdn` scroll) |
| `m` | model picker |
| `enter`, `i`, `/` | back to the prompt |
| `q` | quit (press twice while agents run, which stops them) |

**Commands** (type at the prompt; `/help` lists them):
- `/models`, `/route <role> <provider>:<model>[:effort]`, `/prefer <role|all> <codex|claude|other|auto>`, `/save`
- `/single <provider>:<model>[:effort] <task>` runs a single-agent baseline
- `/limit <codex|claude> [reset|set]` to correct the limit state by hand
- `/threads <n>`, `/parallel on|off`, `/review on|off`, `/judge on|off`
- `/pause`, `/resume`, `/kill <agent>`, `/cancel`, `/clear`, `/usage`

## Other commands

```
sy run "task"                         headless: same pipeline, events printed as lines
sy run --single claude:opus "task"    single-agent baseline (for comparison in stats)
sy stats [--here] [--since 7d]        usage per model, rules fired, routed vs baseline
sy models [--refresh] [--all]         routes + catalogs; refresh Codex catalog
sy doctor                             check CLIs, versions, codex login, git, terminal
sy init [--global] [--force] [--print]
```

`sy run` exits 1 when the task fails, so it is scriptable. For the success measurement in the plan, run the same ~10 tasks with `sy run "..."` and with `sy run --single <provider:model> "..."`, then compare with `sy stats`.

## How it works (and the decisions made for v1)

- **Runners** (`internal/runner`) spawn the CLIs, pass the prompt on **stdin** (multi-line prompts as arguments get mangled by `cmd.exe` when the CLI is an npm `.cmd` shim), stream JSON lines and normalize them into one `Event` type. The TUI and the log only ever see `Event`s.
  - Codex: `codex exec --json --color never --skip-git-repo-check -m <model> -c model_reasoning_effort=<effort> --sandbox <workspace-write|read-only> -C <dir> -`
  - Claude: `claude -p --output-format stream-json --verbose --model <model> [--effort <e>] --permission-mode acceptEdits`. Read-only roles use `--permission-mode dontAsk --tools Read,Grep,Glob,WebSearch,WebFetch`.
  - **Workers on Claude cannot run shell commands by default** (`acceptEdits` only allows edits). To let them run tests, set e.g. `write_allowed_tools: ["Bash(go test *)", "Bash(npm test)"]`, or `write_permission_mode: auto`, under `providers.claude`.
- **Read-only roles** (planner, explorer, researcher, reviewer, judge) can never write: Codex runs them with `--sandbox read-only`, Claude without write tools.
- **Parallel worktrees** (`internal/orchestrator/git.go`):
  - At the start, your working tree (tracked + untracked, honouring `.gitignore`) is captured as a snapshot commit with a temporary index. **Your index, HEAD and branch are never touched.**
  - Each writing agent gets `git worktree add --detach` at the current integration commit, outside your repo (in the user cache dir).
  - When it finishes, its work is merged with `git merge-tree --write-tree` (an object-only merge, hence git 2.38+).
  - The changed files are written into your working tree with `git restore --source`, so CRLF/`autocrlf` on Windows is handled by git itself.
  - If you edited one of those files meanwhile, a `git merge-file` 3-way merge is used; if that conflicts, nothing is overwritten.
  - On a conflict, that agent's work is kept on branch `sy/<session>/<step>` and the reviewer is told.
  - Dependent subtasks start from the merged state.
  - Without git, or with one writing step, agents work directly in your directory, one writer at a time.
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

## Configuration

`sy` looks for `./switchyard.yaml`, then `<user config dir>/switchyard/switchyard.yaml`, then falls back to the built-in default (the file in this repo). Partial files work: anything you leave out keeps its default. See [`switchyard.yaml`](switchyard.yaml) for every option with comments.

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
internal/limits/        limit detection, reset parsing, provider state
internal/sessionlog/    JSONL writer + stats
internal/proc/          process-tree kill (Unix process groups, Windows taskkill + job object)
internal/tui/           Bubble Tea model, views, model picker, commands
```

See [plan.md](plan.md) for the original design.
