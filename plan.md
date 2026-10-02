# Switchyard — Project Plan

> A Windows-first, animated terminal app that routes coding work between ChatGPT (Codex CLI) and Claude (Claude Code) subscriptions — picking the right model for each step, running agents in parallel, and only calling the expensive model when it matters.

**Name:** Switchyard (a rail yard where trains get routed onto the right track)
**Command:** `sy`
**Language:** Go
**Auth:** subscriptions only — no API keys, no token extraction

---

## 1. Goals

1. **Stretch subscription usage:** send cheap work (reading, searching, docs) to fast models and save the strongest models for planning and review.
2. **Better output:** plan, then execute, then review, with the strong model checking at fixed checkpoints.
3. **Faster results:** run independent agents in parallel.
4. **Look great:** a live, animated agent tree in the terminal.
5. **Survive limits:** when one subscription hits its usage limit, shift work to the other provider automatically.

### Non-goals (for now)

- No direct API calls or API keys.
- No custom model client. Switchyard only drives the official `codex` and `claude` CLIs.
- No cross-platform polish beyond Windows (macOS/Linux should mostly work, but aren't tested).

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
switchyard/
├── cmd/sy/main.go
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
├── switchyard.yaml    # default config
├── PLAN.md
└── README.md
```

---

## 8. Milestones

| # | Milestone | Done when |
|---|---|---|
| **M0** | **Demo mode** | `sy --demo` shows the full animated tree driven by fake agents. The look is nailed down. |
| **M1** | Single runner | One real Codex agent runs a task; its events animate the tree. |
| **M2** | Both providers | Claude runner added; manual provider choice via flag. |
| **M3** | Rule router | Routing rules pick provider/model per step; decisions logged. |
| **M4** | Parallel agents | Worker/explorer/researcher run concurrently in worktrees; results merged. |
| **M5** | Review checkpoints | Reviewer runs before plan, on repeated errors, before done. |
| **M6** | Limit fallback | Usage-limit detection and automatic provider switch. |
| **M7** | Stats + tuning | `sy stats` shows usage per model and route; compare against single-agent baseline. |
| **M8** | LLM judge (optional) | Judge added for the cases where rules underperform. |

---

## 9. Measuring success

Run the same set of ~10 real tasks two ways: plain single-agent CLI vs Switchyard. Compare:

- tasks completed correctly,
- wall-clock time,
- how quickly each subscription's limit is reached.

If Switchyard isn't clearly better on at least two of three, tune the router before adding features.

---

## 10. Risks

| Risk | Mitigation |
|---|---|
| CLI output formats change | Pin versions; adapter tests against recorded JSON fixtures |
| Parallel agents burn quota faster | Cap `max_threads`; parallelism off for small tasks |
| Merge conflicts between worktrees | Split subtasks by file ownership; reviewer checks merged diff |
| Limit messages hard to detect | Collect real limit-hit outputs into fixtures; fall back on exit codes |
| Terms of service | Only drive the official CLIs with their normal login; never extract or reuse tokens |
