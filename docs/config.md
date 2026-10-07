# Configuration reference

<!-- Generated from internal/config (the Config struct, default.yaml and
their comments) by: go test ./internal/config -run TestConfigDoc -update-docs
Do not edit by hand: CI fails when it is out of date. -->

Every key of `relayweft.yaml` and of a repository's `.relayweft.yaml`.
`rw init --print` prints the commented default file.

**Where rw reads it.** `--config <file>`, else `./relayweft.yaml`, else
`<user config dir>/relayweft/relayweft.yaml` (`%APPDATA%\relayweft` on
Windows, `~/.config/relayweft` on Linux, `~/Library/Application Support/relayweft`
on macOS), else the built-in defaults. A file only needs the keys it
changes; the rest keep their defaults. Then a repository's
`.relayweft.yaml` (in the repo root or the project folder) is layered on
top, and command-line flags last:

    built-in defaults < your config < learned routes < .relayweft.yaml < flags

Maps (`roles`, `providers`, `mcp.servers`) merge per key, so a repo file with
`roles: {worker: {prefer: claude}}` keeps the worker's routes.

**The "In a repo file" column.** A `.relayweft.yaml` comes from whoever
pushed to the repository, and a `./relayweft.yaml` may have come with a
clone, so they may not set everything. Your own config and `--config`
always apply in full.

- **yes**: applies.
- **needs trust**: runs commands, reaches other folders or sends data
  somewhere; ignored until you review the file with `rw trust` (any
  change to the file needs a new `rw trust`; `rw trust --revoke` withdraws it).
- **stricter only, until trusted**: an untrusted file may make it
  stricter (turn the sandbox on, cut its network), not weaker.
- **lower only, until trusted**: an untrusted file may lower it, not
  raise it above your own setting.
- **stricter only**: a repo file may tighten your limit, never loosen
  it, trusted or not.

Durations are written like `90s`, `30m`, `1h30m`. In key paths, `<role>` is
one of planner, worker, worker_high, explorer, researcher, reviewer, judge;
`<provider>` a provider name (codex, claude, gemini, ...); `[]` an item of a list.

## Contents

- [`roles`](#roles)
- [`providers`](#providers)
- [`routing`](#routing)
- [`orchestrator`](#orchestrator)
- [`verify`](#verify)
- [`notify`](#notify)
- [`hooks`](#hooks)
- [`mcp`](#mcp)
- [`workspace`](#workspace)
- [`sandbox`](#sandbox)
- [`budget`](#budget)
- [`context`](#context)
- [`watch`](#watch)
- [`limit_patterns`](#limit_patterns)
- [`theme`](#theme)
- [`log_dir`](#log_dir)

## roles

Each role has a route on every provider (see providers: below). `prefer` picks which one is used: codex | claude | &lt;any provider&gt; - always that provider (unless it is at its usage limit) other - another provider than the planner's (used for review) auto - whichever provider has used fewer tokens this session If the preferred provider is at its usage limit, the role's route on the next provider (routing.provider_order) is used automatically. Leave `effort` empty to use the CLI default. A provider without a model for a role never runs it.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `roles` | map of sections | `explorer`, `judge`, `planner`, `researcher`, `reviewer`, `worker`, `worker_high` | yes | `roles` are the jobs in a task (planner, worker, worker_high, explorer, researcher, reviewer, judge), each with a route on every provider. |
| `roles.<role>.prefer` | string | - | yes | `prefer` picks the provider: codex, claude or any provider name (always that one, unless it is at its usage limit), other (another provider than the planner's, for review) or auto (whichever has used fewer tokens this session). |
| `roles.<role>.<provider>` | section | - | yes | The role's route on each provider: one entry per provider name (codex:, claude:, gemini:, ...), each with model and effort. |
| `roles.<role>.<provider>.model` | string | - | yes | `model` is the model id given to the CLI ("" = this provider never runs the role). |
| `roles.<role>.<provider>.effort` | string | - | yes | `effort` is the reasoning effort, one of the provider's efforts ("" = the CLI's default). |

## providers

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `providers` | map of sections | `claude`, `codex`, `deepseek`, `gemini`, `ollama`, `ollama-run`, `qwen` | needs trust | `providers` are the agent CLIs rw drives: codex and claude, and gemini, deepseek, qwen, ollama and ollama-run (all off until you turn them on), or any CLI you describe (docs/providers.md). |
| `providers.<provider>.disabled` | bool | - | needs trust | true hides the provider from routing |
| `providers.<provider>.only_preferred` | bool | - | needs trust | `only_preferred`: used only by roles that prefer it by name, never as a fallback, for prefer: other or for prefer: auto (for example a slow local model that should not silently take a strong model's job). |
| `providers.<provider>.kind` | string | - | needs trust | `kind` is the CLI protocol: codex, claude, gemini, qwen or generic. Empty means the provider's name, so only extra providers need it (for example `ollama: {kind: claude, ...}` runs Claude Code against local models). |
| `providers.<provider>.label` | string | - | needs trust | display name (default: the provider name) |
| `providers.<provider>.env` | map of strings | - | needs trust | `env` is added to the CLI's environment; ${VAR} is read from yours. Values never go on the command line. |
| `providers.<provider>.allow_repo_settings` | bool | - | needs trust | `allow_repo_settings` lets the CLI load a repo's own settings (.gemini, .qwen, a .env for Gemini), which can run commands or change where it connects. Off by default; only your own config can turn it on. |
| `providers.<provider>.standby` | list of strings | - | needs trust | `standby` lists roles this provider takes when every provider that would otherwise run them is at or near its usage limit (a free local model for cheap read-only work), even with only_preferred. |
| `providers.<provider>.generic` | section | - | needs trust | `generic` describes the CLI for kind: generic. |
| `providers.<provider>.generic.args` | list of strings | - | needs trust | on every run, first |
| `providers.<provider>.generic.model_args` | list of strings | - | needs trust | when the route has a model, e.g. ["-m", "{model}"] |
| `providers.<provider>.generic.effort_args` | list of strings | - | needs trust | when the route has an effort |
| `providers.<provider>.generic.read_only_args` | list of strings | - | needs trust | `read_only_args` are added for read-only agents and must keep the CLI from editing files or running commands (a plan mode). Without them a CLI with tools takes no read-only work: rw cannot check what it does. |
| `providers.<provider>.generic.write_args` | list of strings | - | needs trust | `write_args` are added for writing agents. Without them the provider takes read-only work only. |
| `providers.<provider>.generic.resume_args` | list of strings | - | needs trust | `resume_args` continue an earlier session (a follow-up), e.g. ["--resume", "{session}"]. Without them every follow-up starts a fresh agent with the earlier one's context. |
| `providers.<provider>.generic.no_tools` | bool | - | needs trust | `no_tools`: the CLI only answers the prompt (a plain model such as `ollama run`); it cannot read files or run commands, so it is read-only by nature and needs no read_only_args. |
| `providers.<provider>.generic.output` | string | - | needs trust | `output` is how stdout is read: text (the whole output is the answer) or jsonl (one JSON object per line, read with JSON rules). |
| `providers.<provider>.generic.json` | list of sections | - | needs trust | `json` are the rules for output: jsonl. |
| `providers.<provider>.generic.json[].match` | map of strings | - | needs trust | `match`: the rule applies to a line where every path has this value ("*" = present). |
| `providers.<provider>.generic.json[].each` | string | - | needs trust | `each` applies rules to every element of the array at this path (Claude-style content blocks). |
| `providers.<provider>.generic.json[].rules` | list of sections | - | needs trust | the rules for each element. Same keys as `providers.<provider>.generic.json[]`. |
| `providers.<provider>.generic.json[].session` | string | - | needs trust | the session id |
| `providers.<provider>.generic.json[].text` | string | - | needs trust | assistant text |
| `providers.<provider>.generic.json[].delta` | bool | - | needs trust | text is a chunk of a streamed message |
| `providers.<provider>.generic.json[].final` | string | - | needs trust | the run's answer |
| `providers.<provider>.generic.json[].thinking` | string | - | needs trust | progress shown, not kept |
| `providers.<provider>.generic.json[].tool` | string | - | needs trust | a tool call's name |
| `providers.<provider>.generic.json[].tool_input` | string | - | needs trust | `tool_input` is the tool call's input object: its file_path, path, command... is shown, and is the edited file for edit_tools. |
| `providers.<provider>.generic.json[].error` | string | - | needs trust | `error` is an error message. It fails the run when the run ends without an answer, or always with fatal. |
| `providers.<provider>.generic.json[].fatal` | bool | - | needs trust | the error fails the run |
| `providers.<provider>.generic.json[].limit` | bool | - | needs trust | `limit`: a matching line means the usage limit was hit (the error text, if any, may say when it resets). |
| `providers.<provider>.generic.json[].input_tokens` | string | - | needs trust | the path of the input token count |
| `providers.<provider>.generic.json[].cached_tokens` | string | - | needs trust | the path of the cached input token count |
| `providers.<provider>.generic.json[].output_tokens` | string | - | needs trust | the path of the output token count |
| `providers.<provider>.generic.session` | string | - | needs trust | `session` (text output) is a regular expression over stdout and stderr whose first group is the session id. |
| `providers.<provider>.generic.usage` | section | - | needs trust | `usage` (text output) are regular expressions over stdout and stderr whose first group is a token count. |
| `providers.<provider>.generic.usage.input` | string | - | needs trust | input tokens |
| `providers.<provider>.generic.usage.cached` | string | - | needs trust | cached input tokens |
| `providers.<provider>.generic.usage.output` | string | - | needs trust | output tokens |
| `providers.<provider>.generic.input_excludes_cached` | bool | - | needs trust | `input_excludes_cached`: the input count does not include the cached part (rw counts input with the cache included). |
| `providers.<provider>.generic.edit_tools` | list of strings | - | needs trust | `edit_tools` are the tool names that change files (jsonl `tool`). |
| `providers.<provider>.generic.limit_patterns` | list of strings | - | needs trust | `limit_patterns` are added to the global limit_patterns for this CLI. |
| `providers.<provider>.generic.limit_exit_codes` | list of integers | - | needs trust | `limit_exit_codes` are exit codes that mean "at the usage limit". |
| `providers.<provider>.sandbox` | section | - | needs trust | `sandbox` is added to the top-level sandbox section for this provider's agents. Same keys as `sandbox`. |
| `providers.<provider>.install_hint` | string | - | needs trust | `install_hint` is what `rw doctor` suggests when the command is missing. |
| `providers.<provider>.command` | string | - | needs trust | `command` is the CLI to run, found on PATH (with PATHEXT on Windows, so codex.cmd works) or a full path. |
| `providers.<provider>.tested_version` | string | - | needs trust | `tested_version` is the CLI version rw was tested with; `rw doctor` warns when yours differs. |
| `providers.<provider>.write_sandbox` | string | - | needs trust | `write_sandbox` is Codex's sandbox for writing agents (read-only agents always get read-only). |
| `providers.<provider>.write_permission_mode` | string | - | needs trust | `write_permission_mode` is the permission mode of writing agents (Claude Code: acceptEdits, auto, bypassPermissions or dontAsk; Gemini: auto_edit; Qwen: auto-edit). |
| `providers.<provider>.write_allowed_tools` | list of strings | - | needs trust | `write_allowed_tools` are tools Claude Code's writing agents may use without asking, e.g. ["Bash(go test *)"] so workers can run tests. |
| `providers.<provider>.extra_args` | list of strings | - | needs trust | `extra_args` are appended to every run of the CLI. |
| `providers.<provider>.limit_cooldown` | duration | - | needs trust | `limit_cooldown` is how long a provider stays "at limit" when the CLI says no reset time. |
| `providers.<provider>.efforts` | list of strings | - | needs trust | `efforts` are the reasoning efforts the CLI accepts (empty: it has no effort setting). |
| `providers.<provider>.models` | list of sections | - | needs trust | `models` is the catalog shown in the model picker (any other id can be typed too). |
| `providers.<provider>.models[].id` | string | - | needs trust | what the CLI is given |
| `providers.<provider>.models[].label` | string | - | needs trust | shown in the model picker |
| `providers.<provider>.models[].tier` | string | - | needs trust | fast \| standard \| strong |

## routing

`routing` tunes how each step's role and provider are picked.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `routing.max_files_before_high` | integer | `5` | yes | rule 5: more files than this -&gt; worker_high |
| `routing.sensitive_paths` | list of strings | 10 entries (`rw init --print`) | yes | `sensitive_paths` are path words that send a step to worker_high. |
| `routing.judge` | bool | `false` | yes | rule-less cases ask the judge model (costs a little quota) |
| `routing.judge_below_confidence` | number | `0.65` | yes | `judge_below_confidence`: with judge on, a rule decision less certain than this asks the judge model. |
| `routing.switch_at_utilization` | number | `0.9` | yes | move work to another provider once one reports this share of its limit used (0 = off) |
| `routing.provider_order` | list of strings | `[]` | yes | `provider_order` is the order providers are tried in when one is at its limit (and what prefer: other picks first). Empty = codex, claude, then the rest by name. |
| `routing.learn` | string | `suggest` | yes | `learn` controls the learned routes per repo: auto (refreshed daily at task start), suggest (only `rw tune --apply`) or off ("" = suggest). |
| `routing.learn_min_samples` | integer | `8` | yes | runs (recent ones count more) a route needs before it is learned |
| `routing.tiers` | string | `off` | yes | `tiers` picks a work step's model from its difficulty and the quota left: auto \| off ("" = off). |
| `routing.tiers_save_below` | number | `0.5` | yes | `tiers_save_below` is the quota left (0..1) below which tiers step down to save it; 0 = 0.5. |
| `routing.best_of` | section | - | lower only, until trusted | `best_of` runs writing steps on several routes and keeps the best result. |
| `routing.best_of.when` | string | `off` | lower only, until trusted | off \| hard (large, sensitive or difficult steps) \| always |
| `routing.best_of.n` | integer | `2` | lower only, until trusted | candidates per step (2-4) |
| `routing.best_of.routes` | list of strings | `[]` | lower only, until trusted | `routes` are the candidates as provider[:model[:effort]] (a provider alone means the step's role there). Empty: the step's own route plus the same role on the next providers in provider_order. |

## orchestrator

`orchestrator` tunes a task's lifecycle: parallel agents, reviews, retries, and what keeps your machine responsive.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `orchestrator.max_threads` | integer | `3` | yes | parallel agents at once |
| `orchestrator.parallel` | bool | `true` | yes | `parallel` runs independent subtasks at the same time (false: one at a time, like --no-parallel). |
| `orchestrator.worktrees` | bool | `true` | yes | parallel writing agents get their own git worktree (pooled, reused between tasks; `rw clean` removes them) |
| `orchestrator.worktree_max_files` | integer | `0` | yes | &gt;0: repos with more tracked files skip worktrees (writers take turns in your tree) |
| `orchestrator.review_before_plan` | bool | `true` | yes | `review_before_plan` has the reviewer check the plan before it runs. |
| `orchestrator.review_single_step_plan` | bool | `false` | yes | also review one-step plans (rw bench: 10/10 approved, +1 strong-model call each) |
| `orchestrator.review_on_repeat_error` | bool | `true` | yes | `review_on_repeat_error` asks the reviewer when a subtask fails the same way twice. |
| `orchestrator.review_before_done` | bool | `true` | yes | `review_before_done` has the reviewer check the result before the task ends; changes it asks for start a fix round. |
| `orchestrator.max_plan_revisions` | integer | `1` | yes | `max_plan_revisions` is how often the planner may revise a plan the reviewer sent back. |
| `orchestrator.max_fix_rounds` | integer | `1` | yes | `max_fix_rounds` is how many fix rounds failed checks or the final review may start. |
| `orchestrator.max_attempts` | integer | `3` | yes | per subtask, not counting limit fallbacks |
| `orchestrator.agent_timeout` | duration | `30m` | yes | `agent_timeout` stops an agent that runs longer than this. |
| `orchestrator.small_task_words` | integer | `12` | yes | tasks shorter than this skip planning and run as one worker step |
| `orchestrator.auto_single` | bool | `true` | yes | `auto_single` runs a task as one worker step, without the planner, when its text does not look multi-file, multi-part, broad or hard (on the bench a single agent was 2-3x faster on such tasks). Checks and the final review still run. |
| `orchestrator.light_planning` | bool | `true` | yes | `light_planning` has the planner and reviewer use the worker route on their provider for a task that does not look hard or sensitive (the planner and reviewer were most of a routed task's cost). |
| `orchestrator.review_when` | string | `untested` | yes | `review_when` says when the final review runs. untested: only on a task without checks; with checks, the checks and the independent tests decide, and their failing output advises the fix round. failing: with checks, only after they fail and a fix round follows (the reviewer advises it); without checks, after every round. large: unless the checks pass on a change of at most review_skip_max_lines lines. always: after every round. On the bench the final review approved 7 of 10 failing results and never changed a verdict, while independent tests caught 4 of the 7 it approved, so by default tests take its place. |
| `orchestrator.review_skip_max_lines` | integer | `80` | yes | `review_skip_max_lines` is review_when: large's limit: the final review is skipped when the checks pass and the change is at most this many added plus removed lines, no file is sensitive and no earlier review asked for changes (0 = always review). |
| `orchestrator.fit_budget` | bool | `true` | yes | `fit_budget` plans within a task, day or team budget: the planner is told what is left, and a plan estimated over it drops the plan review and best-of candidates, then merges single-repo work into one step. Multi-repo plans keep their repository assignments and dependencies. Without budget limits it does nothing. |
| `orchestrator.independent_tests` | bool | `true` | yes | `independent_tests` has an agent on another provider write tests for the task's requirements while the worker works, in a worktree at the task's start, so it never sees the change. rw runs them after the work like the checks; a failure starts a fix round. Needs a git repo and verify.commands. With review_when: untested (the default) they stand in for the final review. |
| `orchestrator.independent_tests_gate` | string | `soft` | yes | `independent_tests_gate` says what failing independent tests do. soft: they fail a round only once, and only when a fix round can follow; after that fix round, tests that still fail are reported with the result (and any reason the fix agent gave against them), not held against the task. strict: they fail every round like a check, so tests that still fail after the last fix round fail the task. In a replay the known solution failed a writer test in 3 of 5 tasks, so by default they get one fix round and then advise. |
| `orchestrator.tests_first` | bool | `false` | yes | `tests_first` has an agent write acceptance tests for the task's requirements before any code is written, on another provider than the implementer's (rw run --tests-first sets it for one task). |
| `orchestrator.approve_plan` | bool | `true` | yes | TUI / rw run --approve: show the plan and let you edit it before anything runs |
| `orchestrator.review_changes` | bool | `false` | yes | show each agent's changes (per file) before they land in your tree |
| `orchestrator.handoff` | bool | `true` | yes | give agents a repo map, earlier tasks' notes and read-only findings |
| `orchestrator.low_priority` | bool | `true` | yes | agents and git run below normal CPU priority |
| `orchestrator.max_cpu_percent` | integer | `90` | yes | while the machine is busier than this, no new agent starts (0 = off) |
| `orchestrator.min_free_memory_mb` | integer | `1024` | yes | ... or has less free RAM than this (0 = off) |
| `orchestrator.busy_max_wait` | duration | `2m` | yes | start anyway after waiting this long |
| `orchestrator.min_free_disk_gb` | number | `10` | yes | never create a pool worktree below this much free disk (falls back to your tree) |
| `orchestrator.pool_warn_gb` | number | `20` | yes | warn when this repo's worktree pool grows beyond this |
| `orchestrator.pool_max_idle` | duration | `336h` | yes | pool worktrees unused this long are removed (14 days; 0 = keep) |
| `orchestrator.snapshot_max_file_mb` | integer | `100` | yes | untracked files bigger than this are left out of snapshots and worktrees (0 = no limit) |
| `orchestrator.conflicts` | string | `auto` | stricter only, until trusted | `conflicts` is what happens when a step's change conflicts with another step's or with your own uncommitted edits: auto (an agent resolves conflicts between steps; rw asks before one resolves a conflict with your edits), resolve, ask, or fail (keep the change on a branch). |
| `orchestrator.max_resolve_rounds` | integer | `2` | stricter only, until trusted | `max_resolve_rounds` caps the resolve agent's attempts per conflict (0 = 2). |
| `orchestrator.resolve_role` | string | "" | yes | `resolve_role` is the resolve agent's role (worker or worker_high); "" = the route that wrote the later change. |

## verify

Your repo's own checks. Agents may run them without asking; Relayweft runs them after the agents finish and before the final review, and failures go into the fix round. `rw init` detects them (go test, npm test, pytest, ...).

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `verify.preflight` | list of strings | `[]` | needs trust | `preflight` lists short checks that must succeed through each enabled Claude writing provider before planning or implementation. Use commands that pass on the starting tree (e.g. compile-only tests and formatter probes). Empty disables the probe. It uses provider quota, counted in the task budget. Explicit tool results are required, not agent claims. A task that runs as one step without the planner probes only the provider of that step (none when it is not Claude). |
| `verify.commands` | list of strings | `[]` | needs trust | `commands` are the checks, run through the system shell in the project folder, e.g. ["go test ./...", "go vet ./..."]. |
| `verify.auto` | bool | `true` | needs trust | `auto` detects the checks of a repo with no commands from its build files at the start of each task: go build and go test, cargo test, the package.json test script, pytest, dotnet test, mvn or gradle test. They run like configured commands (in the sandbox when agents use one). |
| `verify.timeout` | duration | `10m` | needs trust | `timeout` is the time limit for each command. |
| `verify.affected` | string | `auto` | needs trust | `affected`: "auto" (or "") runs only the tests the changes affect after a fix round, and the full checks before the final review; "off" always runs the full checks. |
| `verify.affected_commands` | map of strings | `{}` | needs trust | `affected_commands` maps a command to its narrowed form, with {files}, {packages} and {test_files}; "off" never narrows that command. |

## notify

Desktop notifications (Windows toast, macOS, notify-send): a task finished or failed, a provider hit its limit, or rw waits for your approval.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `notify.enabled` | bool | `true` | yes | desktop notifications (Windows toast, macOS, notify-send) |
| `notify.min_task` | duration | `1m` | yes | skip "done" for tasks shorter than this |
| `notify.webhooks` | list of sections | `[]` | needs trust | `webhooks` (Slack, Discord, ntfy or plain JSON) get done, failed, limit, waiting, watch and summary messages: overnight runs, scheduled tasks, the morning summary and rw watch post here (rw notify --test sends a test message). |
| `notify.webhooks[].url` | string | - | needs trust | the webhook; ${VAR} is read from your environment |
| `notify.webhooks[].kind` | string | - | needs trust | "" = from the URL's host |
| `notify.webhooks[].token` | string | - | needs trust | ntfy access token, or json's bearer token |
| `notify.webhooks[].events` | list of strings | - | needs trust | done, failed, limit, waiting, watch, summary (default: all) |
| `notify.morning` | string | "" | yes | `morning` is a time of day ("07:30"): the summary of the unattended tasks since the same time the day before (queued, scheduled, task files) goes to the webhooks (event summary) and as a desktop notification, from an rw web, TUI or long rw run that is running then. "" = off; rw morning --schedule sets up a system task instead. |

## hooks

Your own commands around every task (run through the system shell in the project folder). Environment: RW_TASK, RW_TASK_ID, RW_DIR, RW_STATUS (after_task: done|failed|cancelled), RW_SUMMARY, RW_STEP and RW_FILES (after_merge: the step and the files it changed, one per line). A failing before_task hook stops the task.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `hooks.before_task` | list of strings | `[]` | needs trust | e.g. ["git fetch --quiet"] |
| `hooks.after_merge` | list of strings | `[]` | needs trust | e.g. ["gofmt -w ."] |
| `hooks.after_task` | list of strings | `[]` | needs trust | e.g. ["npm run lint"] |
| `hooks.timeout` | duration | `5m` | needs trust | time limit for each hook |

## mcp

`mcp` gives the agents MCP servers, passed to both CLIs on every run of the listed roles. ${VAR} in any value is read from your environment when the agent starts, so secrets stay out of the file. `rw doctor` checks them.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `mcp.servers` | map of sections | `{}` | needs trust | `servers` are the MCP servers by name (letters, digits, - and _). |
| `mcp.servers.<server>.command` | string | - | needs trust | the server program (a stdio server) |
| `mcp.servers.<server>.args` | list of strings | - | needs trust | its arguments |
| `mcp.servers.<server>.env` | map of strings | - | needs trust | added to its environment; use ${VAR} for secrets |
| `mcp.servers.<server>.url` | string | - | needs trust | instead of command: the server's URL |
| `mcp.servers.<server>.type` | string | - | needs trust | `type` is the transport of a url server: http (default) or sse. Codex only speaks streamable HTTP, so sse servers go to Claude only. |
| `mcp.servers.<server>.headers` | map of strings | - | needs trust | HTTP headers for a url server; use ${VAR} for secrets |
| `mcp.servers.<server>.providers` | list of strings | - | needs trust | `providers` limits the server to codex or claude (empty = both). |
| `mcp.roles` | list of strings | `[]` | needs trust | `roles` get the servers; empty means worker, worker_high, explorer and researcher. |
| `mcp.allow_tools` | bool | - | needs trust | `allow_tools` pre-approves the servers' tools for Claude (mcp__&lt;server&gt;), also for read-only roles: without it a headless Claude cannot call them. Default true. Note that MCP tools may change things outside the repo even for a read-only role. |
| `mcp.strict` | bool | `false` | needs trust | `strict` passes --strict-mcp-config to Claude: only these servers, none of the user's own Claude Code MCP config. |

## workspace

`workspace` makes every task of the project a multi-repo task.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `workspace.repos` | map of strings | `{}` | needs trust | `repos` maps a short name to another git repository: a path relative to the project folder (to the repo file's folder in a .relayweft.yaml). |

## sandbox

Run the agents in a container (docker or podman) with only the step's folder writable, and rw's verify commands too (docs/sandbox.md). Off by default. Build the image once: docker build -t relayweft-sandbox packaging/sandbox Only what you name goes in: the env variables below and each provider's sandbox.env (API keys), and credential files, read-only. rw's forge and CI tokens never do. A repo's .relayweft.yaml may turn the sandbox on or its network off; turning it off or passing more in needs `rw trust`.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `sandbox.mode` | string | `off` | stricter only, until trusted | off \| docker \| podman |
| `sandbox.image` | string | "" | stricter only, until trusted | default relayweft-sandbox |
| `sandbox.network` | string | "" | stricter only, until trusted | on (default) or off |
| `sandbox.env` | list of strings | `[]` | stricter only, until trusted | `env` names variables passed from your environment into the container (only those that are set). Nothing else of your environment goes in. |
| `sandbox.credentials` | list of strings | `[]` | stricter only, until trusted | `credentials` are files mounted read-only at the same place under the container's home, e.g. ~/.codex/auth.json. They must be under your home folder. |
| `sandbox.mounts` | list of sections | `[]` | stricter only, until trusted | `mounts` are more folders or files (read-only unless writable). |
| `sandbox.mounts[].path` | string | - | stricter only, until trusted | on this machine; ~ is your home |
| `sandbox.mounts[].target` | string | - | stricter only, until trusted | in the container (default: the same place under its home, for paths under yours) |
| `sandbox.mounts[].writable` | bool | - | stricter only, until trusted | mounted read-write |
| `sandbox.roles` | list of strings | `[]` | stricter only, until trusted | `roles` limits the sandbox to these roles (empty = every role). |
| `sandbox.command` | string | "" | stricter only, until trusted | `command` is the CLI's name in the image (provider sections; default: the provider's command without folder and .cmd/.exe). |

## budget

Spending caps (0 = off). tokens = fresh tokens on both providers; usd = Claude's API-equivalent price (not billed on a subscription). Day totals count today's finished tasks (local time, from the session logs) plus the running one. At a limit, a task you watch asks whether to go on; queued, scheduled and `rw run` tasks without --approve stop instead. Flags: --budget-task-tokens, --budget-task-usd, --budget-day-usd.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `budget.reserve` | bool | `false` | stricter only | `reserve` enables admission estimates: reserve each running agent's median token/cost estimate and keep 20% of each limit for review/fixes in routed tasks with final review enabled. Waiting candidates cannot all spend the same remaining budget. Estimates are not hard CLI caps; incomplete usage keeps its unused estimate held for this task. |
| `budget.task_tokens` | integer | `0` | stricter only | fresh tokens one task may use (0 = no limit) |
| `budget.task_usd` | number | `0` | stricter only | API-equivalent $ one task may cost (0 = no limit) |
| `budget.day_tokens` | integer | `0` | stricter only | fresh tokens today's tasks may use (0 = no limit) |
| `budget.day_usd` | number | `0` | stricter only | API-equivalent $ today's tasks may cost (0 = no limit) |
| `budget.warn_at` | number | `0.8` | stricter only | warn once when a limit is this full |
| `budget.team` | section | - | stricter only | `team` is a day budget several machines share through a folder. |
| `budget.team.dir` | string | "" | needs trust | "" = off |
| `budget.team.day_tokens` | integer | `0` | stricter only | fresh tokens all machines together may use today (0 = no limit) |
| `budget.team.day_usd` | number | `0` | stricter only | API-equivalent $ all machines together may spend today (0 = no limit) |

## context

What the planner and the final reviewer learn from the repo's own docs: CONTRIBUTING, the PR template, CODEOWNERS, CI workflow commands (as hints, never run), AGENTS.md / CLAUDE.md. Summarized without a model call and marked as untrusted repo data in the prompt.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `context.repo_docs` | bool | `true` | yes | give the planner and the final reviewer the summary |
| `context.repo_docs_max_kb` | integer | `8` | yes | total size of the summary |

## watch

rw watch: after rw pr opened a pull request, failed checks and review comments (changes requested) get a follow-up task on the PR's branch, pushed to it (never forced). At most this many per pull request.

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `watch.max_rounds` | integer | `3` | stricter only | `max_rounds` caps the follow-up tasks per pull request (0 = none: rw watch only reports). |

## limit_patterns

Text that means "usage limit reached" (case-insensitive regular expressions).

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `limit_patterns` | list of strings | 11 entries (`rw init --print`) | yes | `limit_patterns` are case-insensitive regular expressions: CLI output that matches one means "usage limit reached". |

## theme

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `theme` | string | `auto` | yes | `theme` is the TUI's look: auto, unicode or ascii (ascii is picked automatically on the legacy console). |

## log_dir

| Key | Type | Default | In a repo file | Description |
|---|---|---|---|---|
| `log_dir` | string | "" | needs trust | `log_dir` is where the session logs go ("" = &lt;user config dir&gt;/relayweft/sessions). |
