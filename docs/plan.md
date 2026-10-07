# Relayweft — Project Plan

> A Windows-first, animated terminal app that routes coding work between ChatGPT (Codex CLI) and Claude (Claude Code) subscriptions — picking the right model for each step, running agents in parallel, and only calling the expensive model when it matters.

**Name:** Relayweft (until v0.2.0: Switchyard, a rail yard where trains get routed onto the right track; renamed because that name was taken on the VS Code Marketplace)
**Command:** `rw` (until v0.2.0: `sy`)
**Language:** Go
**Auth:** subscriptions by default through the providers' CLIs; optional providers manage their own credentials. No token extraction.

---

## 1. Goals

1. **Stretch subscription usage:** send cheap work (reading, searching, docs) to fast models and save the strongest models for planning and review.
2. **Better output:** plan, then execute, then review, with the strong model checking at fixed checkpoints.
3. **Faster results:** run independent agents in parallel.
4. **Look great:** a live, animated agent tree in the terminal.
5. **Survive limits:** when one subscription hits its usage limit, shift work to the other provider automatically.

### Current scope

- Relayweft drives agent CLIs rather than implementing a model client. Codex and Claude are the defaults; [extra providers](providers.md) are opt-in and may use provider-specific API keys.
- Windows remains the primary desktop target. Windows, Linux and macOS have automated CI coverage; each local batch records which systems were actually run. Native desktop and sustained-use evidence remain separate gates in [the roadmap](../ROADMAP.md).

---

## 2. Architecture

```
            ┌──────────────────────────────────────────┐
            │                 TUI (Bubble Tea)         │
            │  agent tree · live status · session log  │
            └───────────────▲──────────────────────────┘
                            │ events
┌───────────┐   ┌───────────┴───────────┐   ┌──────────────────┐
│  Router   │◀──│     Orchestrator      │──▶│   Session Log    │
│ rules +   │   │ plan → fan-out → join │   │ JSONL decisions, │
│ optional  │──▶│ → review checkpoints  │   │ tokens, timings  │
│ LLM judge │   └───────┬───────┬───────┘   └──────────────────┘
└───────────┘           │       │
                ┌───────▼──┐ ┌──▼────────┐
                │  Codex   │ │  Claude   │   Runner adapters
                │  runner  │ │  runner   │   (subprocesses)
                └───────┬──┘ └──┬────────┘
                        │       │
                 codex exec   claude -p
                  --json     --output-format stream-json
```

### Components

| Component | Responsibility |
|---|---|
| **Runner adapters** | Spawn the CLI, stream stdout line by line, normalize events into a common type, detect errors and usage limits, kill cleanly. |
| **Orchestrator** | Owns the task lifecycle: plan, split into subtasks, dispatch to agents, join results, trigger review. |
| **Router** | Picks provider + model + effort for each step. Rules first; an LLM judge only for unclear cases. |
| **Reviewer** | Strong model, called only at checkpoints. Reads plan/diff/errors and gives advice. Never writes code itself. |
| **TUI** | Renders the tree, animates state changes, shows the log, accepts commands. |
| **Session log** | Append-only JSONL of every decision, cost estimate and outcome. Basis for tuning the router. |

---

## 3. Roles

| Role | Default route | Does |
|---|---|---|
| **Main / planner** | Codex `gpt-6.1-sol` high, or Claude `opus` | Plans, splits work, applies review feedback |
| **Worker** | Codex `gpt-6.1-sol` medium, or Claude `sonnet` | Edits code, runs tests |
| **Explorer** | Codex `gpt-6-luna`, or Claude `haiku` | Reads the codebase, answers "where is X?" |
| **Researcher** | Codex `gpt-6-luna`, or Claude `haiku` | Reads docs, summarizes APIs |
| **Reviewer** | The strongest model on the *other* provider | Checkpoint reviews only |

Using the other provider for review is deliberate: a different model catches different mistakes, and it spreads usage across both subscriptions.

All model names live in config, so new releases are a one-line change.

---

## 4. Routing

### 4.1 Rules (v1, free and instant)

Evaluated top to bottom; first match wins.

1. Provider at usage limit → use the other provider.
2. Step is a review checkpoint → reviewer route.
3. Step is read-only (search, explain, summarize) → explorer/researcher route.
4. Same error seen twice → escalate one tier and trigger reviewer ("error repeats").
5. Diff touches more than N files or sensitive paths (auth, payments, migrations) → worker at high effort.
6. Default → worker route.

### 4.2 LLM judge (v2, optional)

For steps no rule matches confidently, ask a fast model via the subscription CLI a closed question:

> "Pick one: A) explorer B) worker C) worker-high D) planner. Reply with the letter only."

Use it sparingly, since it costs a little quota per call. The session log shows where the rules guess wrong, which is where the judge is worth adding.

### 4.3 Review checkpoints

Matching the Astra pattern, the reviewer is spawned only:

- **before a plan** is executed,
- when an **error repeats**,
- **before done**.

It stays silent on every routine turn.

---

## 5. Runner details

### Codex
```
codex exec --json -m <model> -c model_reasoning_effort=<effort> --sandbox workspace-write "<prompt>"
```

### Claude
```
claude -p --output-format stream-json --verbose --model <model> --permission-mode acceptEdits "<prompt>"
```

> Verify the exact flags against your installed CLI versions (`codex exec --help`, `claude --help`) and pin those versions in the README. Output formats can change between releases.

### Normalized event type

```go
type Event struct {
    AgentID   string
    Provider  string    // "codex" | "claude"
    Kind      EventKind // Started, Thinking, ToolCall, FileEdit, Message, Usage, Error, LimitHit, Done
    Text      string
    Tokens    TokenUsage
    Timestamp time.Time
}
```

Each adapter parses its own CLI's JSON lines into `Event`s and sends them on a channel. The TUI and log only ever see `Event`.

### Parallel safety

Parallel agents editing one working tree will collide. Each writing agent gets its own **git worktree** (`git worktree add`), and the orchestrator merges results back. Read-only agents share the main tree.

---

## 6. TUI

**Stack:** Bubble Tea (app loop), Lip Gloss (styling), Bubbles (spinners, viewport, progress), Harmonica (spring animations).

### Layout (mirrors the reference screenshot)

```
┌ header: project · active models · provider status / limits ────────┐
├ legend: colored provider/role keys ────────────────────────────────┤
│ ┌ reviewer panel ┐   ┌ main agent box ┐                            │
│ │ checkpoints,   │   └───────┬────────┘                            │
│ │ last advice,   │   ┌ router panel: decisions + confidence bars ┐ │
│ │ calls, tokens  │   └───────┬───────────────────────────────────┘ │
│ │                │   ┌ worker ┐ ┌ explorer ┐ ┌ researcher ┐         │
│ │                │   └────────┘ └──────────┘ └────────────┘        │
│ └────────────────┘   ┌ back to main · review + verify ┐            │
├ session log (scrolling) ───────────────────────────────────────────┤
└ prompt line ───────────────────────────────────────────────────────┘
```

### Animations

- Spinners on running agents, a checkmark/cross on finish.
- Pulses traveling along the connector lines when work is dispatched or returned.
- Confidence/usage bars that ease to their new value.
- Brief highlight flash when the reviewer posts advice.

### Keys

`enter` submit task · `tab` focus panel · `l` toggle full log · `p` pause/resume · `k` kill agent · `q` quit

### Windows notes

- Target **Windows Terminal** (true color, good Unicode). Legacy conhost gets a fallback ASCII theme.
- npm-installed CLIs are `.cmd` shims; resolve with `exec.LookPath` and handle `.cmd`/`.exe`.
- Kill child process trees properly (Windows job objects), or orphaned agents keep running.

---

## 7. Project structure

```
relayweft/
├── cmd/rw/main.go
├── internal/
│   ├── config/        # YAML config: models, routes, limits, theme
│   ├── event/         # Event types
│   ├── runner/
│   │   ├── codex.go
│   │   ├── claude.go
│   │   └── fake.go    # demo mode
│   ├── router/        # rules + optional judge
│   ├── orchestrator/  # lifecycle, worktrees, checkpoints
│   ├── sessionlog/    # JSONL writer + stats
│   └── tui/           # Bubble Tea models, views, styles
├── relayweft.yaml    # default config
├── PLAN.md
└── README.md
```

---

## 8. Milestones

| # | Milestone | Done when |
|---|---|---|
| **M0** | **Demo mode** | `rw --demo` shows the full animated tree driven by fake agents. The look is nailed down. |
| **M1** | Single runner | One real Codex agent runs a task; its events animate the tree. |
| **M2** | Both providers | Claude runner added; manual provider choice via flag. |
| **M3** | Rule router | Routing rules pick provider/model per step; decisions logged. |
| **M4** | Parallel agents | Worker/explorer/researcher run concurrently in worktrees; results merged. |
| **M5** | Review checkpoints | Reviewer runs before plan, on repeated errors, before done. |
| **M6** | Limit fallback | Usage-limit detection and automatic provider switch. |
| **M7** | Stats + tuning | `rw stats` shows usage per model and route; compare against single-agent baseline. |
| **M8** | LLM judge (optional) | Judge added for the cases where rules underperform. |

---

## 9. Measuring success

Run the same set of ~10 real tasks two ways: plain single-agent CLI vs Relayweft. Compare:

- tasks completed correctly,
- wall-clock time,
- how quickly each subscription's limit is reached.

If Relayweft isn't clearly better on at least two of three, tune the router before adding features.

---

## 10. Risks

| Risk | Mitigation |
|---|---|
| CLI output formats change | Pin versions; adapter tests against recorded JSON fixtures |
| Parallel agents burn quota faster | Cap `max_threads`; parallelism off for small tasks |
| Merge conflicts between worktrees | Split subtasks by file ownership; reviewer checks merged diff |
| Limit messages hard to detect | Collect real limit-hit outputs into fixtures; fall back on exit codes |
| Terms of service | Only drive the official CLIs with their normal login; never extract or reuse tokens |

---

## 11. Status and v1 decisions

All milestones M0–M8 are implemented. The README covers usage. The open questions were decided as follows:

| Question | Decision |
|---|---|
| Model choice | Every role has a route on **both** providers plus `prefer: codex/claude/other/auto`. It can be changed live in the TUI (model picker `m`, `/route`, `/prefer`), with flags (`--route`, `--prefer`, `--provider`), or in `relayweft.yaml`. The Codex catalog is read from `codex debug models` (`rw models --refresh`). |
| Prompt delivery | Prompts go to both CLIs via stdin, which avoids `cmd.exe` quoting of multi-line prompts through npm `.cmd` shims. |
| Worktree merge strategy | Snapshot the working tree as a commit using a temporary index (the user's index, HEAD and branch are untouched). Run a detached worktree per writing agent at the current integration commit. Merge with `git merge-tree --write-tree` (git ≥ 2.38). Write changed files back with `git restore --source` (CRLF-safe); files the user edited meanwhile are 3-way merged with `git merge-file`. On a conflict, keep the agent's commit on `rw/<session>/<step>` and tell the reviewer. |
| Usage-limit detection | Claude's `rate_limit_event` (status `rejected`), regex `limit_patterns` on error text, stderr and exit codes. The reset time is parsed from the message, else `limit_cooldown`. The header shows Claude's live quota utilization. |
| Legacy conhost | Detected when no `WT_SESSION`, `TERM_PROGRAM`, `ConEmuANSI`, ... is set on Windows; the ASCII theme is used. `--ascii`/`--unicode` override it. |
| Killing agent trees | Unix: process groups. Windows: `taskkill /T /F` per agent, plus a kill-on-close job object around `rw` itself so nothing outlives it. |
| Pause | Holds dispatching of new agents. Running agents finish (suspending a CLI mid-request risks API timeouts). |
| Small tasks | Fewer than `small_task_words` (12) words skips the planner and parallelism. With `auto_single` (default), so does a longer task that names at most 2 files, has at most 2 list items, at most 150 words, no broad word (refactor, migrate, across, every file, ...), no sensitive path and a difficulty below the strong tier; `rw run --plan` always plans. |
| Default execution | `single_worker: true` runs new single-repository tasks with one worker, snapshots, project checks and bounded repairs. `--plan` explicitly requests planning; multi-repository and resumed tasks keep their planning boundaries. |
| Light planning | With `light_planning` (default), a task that does not look hard or sensitive is planned and reviewed on the worker route of the planner's and reviewer's providers. |
| Final review | Off by default (`review_before_done: false`). When enabled, `review_when` decides when it runs. `untested` (the default): only on a task without checks; with checks, the checks and the independent tests decide, a failure's output (not a reviewer's advice) goes into the fix round, and a step that failed fails the task without a review. `failing`: with checks, only after they fail and a fix round follows, so its advice goes into that round; when the checks pass the task ends without one; without checks, after every round. `large`: skipped when `verify.commands` ran and pass, every step succeeded, no review asked for changes and the diff has at most `review_skip_max_lines` (80) added plus removed lines, no binary file, no sensitive path and no listed criteria. `always`: after every round. |
| Detected checks | With no `verify.commands` and `verify.auto` on (the default), rw detects the repo's checks from its build files when a task starts, the way `rw init` does (go build and test, cargo, the package.json test script, pytest, dotnet, Maven, Gradle), and runs them like configured ones. |
| Budget-fitted plans | With `fit_budget` (default) and a budget limit, the planner gets the remaining budget and a suggested step count. A plan whose estimate plus the final review (or the independent test writer in its place) and one fix round exceeds what is left first drops the plan review, then best-of candidates, then merges single-repo work into one step carrying every step's prompt. Multi-repo plans retain their repository assignments and dependencies; budget admission still applies. A single-repo task whose budget cannot fund the planner, one writer and the finish runs as one step. Estimates come from the reservation history (fixed defaults in `rw bench`). |
| Tests first | With `rw run --tests-first` (or `tests_first`), the worker route on the next provider other than the implementer's writes acceptance tests before the plan, from the task text alone. It may run the test runner each configured check starts with (`go test`, `pytest`, `npm test`, …) with any arguments, so it can run just its new tests and see them fail. rw undoes its changes outside test files, runs its command once (it must fail; a command that is not a plain test runner or a configured check is not run), adds it in front of `verify.commands` for this task, advances the worktree snapshot so every writer sees the tests, and puts the tests back before each verify run. With no tests written, a terminal asks the person to write them; otherwise the task stops before any code. With `--approve` the tests are shown for approval first. Unlike `independent_tests`, the writers see these tests and they stay in your tree; a task with both runs only these. |
| Independent tests | With `independent_tests` (off by default; generated expectations can be wrong), a task that changes files in a git repo with `verify.commands` gets a test writer next to its worker: the worker route on the next provider that can write (the worker's own provider if none can), in a pool worktree at the task's start, so it never sees the change. Its prompt has the task text, the repo docs and the checks, and asks for one test per requirement the task names, through names the task gives or the repo already has. rw keeps only new files with `rwreq` in their name (at most 20 files, 1 MB) and the writer's `COMMAND:` line if it is a check or starts with a narrowed check prefix and has no shell syntax (else the checks run the files). After the checks, rw puts the files in your tree, runs them and removes them. A failure fails the round like a check: the fix agent gets the output and the writer's requirement list, and the files sit in its tree while it works (unless it works in a pool worktree). It may answer `DISPUTE: <test>: <why>` for a test that asks for more than the task; rw records that as a choice. With `independent_tests_gate: soft` (the default) the tests fail a round only once, and only when a fix round can follow; tests that still fail after it are reported in the summary and in `rw explain` (with the dispute, if any) instead of failing the task. `strict` fails every round, as before. The tests never land, a file whose path the change already has is skipped, and a resumed task gets none. `rw bench --replay-tests` measures it on saved runs, `rw bench --replay-fix` what the fix round it starts does to them; mode `routed-review` runs the final review in its place, to compare live. |
| Read-only safety | Codex `--sandbox read-only`. Claude `--permission-mode dontAsk --tools Read,Grep,Glob,WebSearch,WebFetch`. |
| Token accounting | "Tokens" in the UI and stats are fresh tokens (uncached input + output). Cached reads are shown separately in `rw stats`. |
