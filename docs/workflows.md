# Saved workflows

Workflows combine a task prompt, check commands, approvals and a budget. They
live in your user configuration directory, under `relayweft/workflows`. A
repository cannot install one implicitly.

```sh
rw workflow --init
rw workflow
rw workflow --show bugfix
rw run --workflow bugfix "The CSV parser drops the final empty field"
rw run --workflow dependency-upgrade "Upgrade the HTTP client to the next major version"
rw run --workflow review "Review the changes against main"
rw run --workflow release-prep "Prepare the next patch release"
rw run --workflow bugfix --file bugs.txt          # a queue: one task per line
rw run --workflow review --at 02:30 "Review the changes against main"
rw run --workflow bugfix --issue 123 --with-comments --pr --draft
```

`--init` installs four starters without overwriting your changes. The starters
ask for plan approval and change review. Writing workflows require configured
checks: run `rw init` in the project, or add `checks` to the workflow. They use
the existing runner, verification, recovery and budget enforcement.

To customize one, save the output of `--show` to a file, edit it, then run
`rw workflow --save bugfix.yaml --replace`. Review check commands before
installing a workflow: they execute as shell commands in the task's project.

```yaml
name: bugfix
description: Reproduce, repair and verify a bug
prompt: |
  Fix {{task}}. Reproduce the bug with a failing test, make the fix,
  run the relevant checks and explain the cause.
checks:
  - go test ./...
task_tokens: 200000
task_usd: 10
approve_plan: true
review_changes: true
require_checks: true
```

The prompt substitutes `{{task}}` literally; it is not an executable template.
Checks add to existing project checks. Positive budget values tighten an
existing budget and never raise it; zero preserves the existing limit. Dollar
budgets are API-equivalent estimates, not subscription billing. Day budgets
still apply. The configuration change lasts for this run only.

## Everywhere tasks run

| Where | How |
| --- | --- |
| Terminal | `rw run --workflow NAME "task"`, with `--file` for a queue, `--issue` or `--issues` for issue tasks, and `--at`, `--in` or `--when-reset` to start later |
| `rw web` / `rw app` | the workflow picker left of the task box; the Queue panel's schedule form has one too. *Install starter workflows…* in an empty picker does what `rw workflow --init` does |
| TUI (`rw`) | `/workflow` lists them, `/workflow NAME task` runs one, `/schedule 02:30 /workflow NAME task` schedules one |
| MCP (`rw mcp`) | `run_task` with `"workflow": "NAME"` |

A queued or scheduled task reads its workflow when you queue it. If you edit
the file afterwards, the task still runs the version you queued. The
workflow's checks, budget caps and approvals apply to that task only: other
tasks in the same `rw web` or TUI session keep your normal settings. A resumed
task keeps the workflow it started with. `rw resume` automatically restores
its terminal approval prompts; `--approve` is not required. A gated workflow
refuses to start through an interface that supplies no approver.

`--workflow` does not combine with `--single` (one agent, no plan to approve).

## Issues through verification to a PR

`--issue N` or `--issue URL` uses the issue's title, body and labels as the
workflow's `{{task}}`. Add `--with-comments` to include comments. Checks,
budget caps and approval gates apply just as they do to a typed task.
`--pr` pushes a branch and opens a PR only after the task succeeds; `--draft`
makes that PR a draft. Without `--pr`, a single issue leaves its changes
locally for review or a later `rw pr`.

For a fully unattended flow, save a copy of `bugfix` named `bugfix-auto` with
`approve_plan: false`, `review_changes: false` and `require_checks: true`.
Configure the project's checks or add `checks` to that workflow, then run:

```sh
rw run --workflow bugfix-auto --issue 123 --pr --draft
rw run --workflow bugfix-auto --issues label:bug --limit 5 --pr --draft --at 02:30
rw run --workflow bugfix-auto --issues label:bug --pr --draft --team --every 10m
```

Label batches still require `--pr` and a clean working tree. Each successful
issue gets its own PR with `Closes #N`, then its changes are removed locally
before the next issue starts. Failed checks prevent a PR; leftover changes
stop the batch. The usual issue comments and team claims still apply.

Scheduled runs load the workflow when scheduled and read the issues when
the run starts. Every issue in a batch or team queue uses that saved workflow
version, even if you edit the workflow file while it runs. The task history
keeps the expanded prompt and workflow for resume.

## Approvals in queues and scheduled runs

A queued or scheduled task normally runs unattended: it never waits for an
approval, and a budget limit stops it. A workflow's own approvals are the
exception. If the workflow sets `approve_plan` or `review_changes`, its task
waits for you at that point, even when it was queued, scheduled or started from
a task file. Nothing it plans runs and nothing it changes lands without you.
In everything else the task stays unattended:

- a budget limit stops it, and it does not ask to continue;
- a merge conflict that would ask you is kept on a branch instead;
- your session's `approve_plan` and `review_changes` settings do not apply to it.

While such a task waits, the tasks queued after it wait too. Relayweft tells
you as it does for any task that needs you: a notification, and a post to
your `notify.webhooks` (event `waiting`). The queue in `rw web` marks these tasks
*waits for approval*.

In the terminal, the question appears where `rw run` runs. If stdin is closed,
for example in a script or CI, the answer is no and the task stops before
anything runs. To run a workflow fully unattended, save a copy of it with
`approve_plan: false` and `review_changes: false`. Keep `require_checks` or
`checks` on that copy, because the checks are then what guards the result.
Over MCP, `approve_plan: false` or `review_changes: false` in `run_task` cannot
turn off a workflow's approvals.

The review and release starters describe the intended scope in their prompts;
those prompts are not a filesystem or publication sandbox.
