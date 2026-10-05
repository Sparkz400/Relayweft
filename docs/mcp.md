# rw mcp: hand tasks to rw from Claude Code or Codex

`rw mcp` runs rw as an [MCP](https://modelcontextprotocol.io) server on stdin and stdout. A coding agent you work with (Claude Code, Codex) can then give a multi-step task to rw from inside its own session: rw plans it, runs its worker and reviewer agents, and lands the changes in your working tree. The agent follows the task and reports back to you.

This is the other direction from the `mcp:` config section, which gives MCP servers *to* rw's agents (README, "MCP servers").

> **Status:** tested with a scripted MCP client over real stdio, with scripted agents and real git, on Windows, Linux and macOS (CI). Tried for real on Windows 11 with Claude Code 2.1.288 (Claude Haiku calling rw, rw's agents on Claude Haiku: a one-step task, and a task with plan approval) and codex-cli 0.160.0.

## Set-up

`rw` must be on your PATH (or give its full path as the command). rw works in the folder the client starts it in: for Claude Code and Codex that is the folder you started them in.

### Claude Code

For every project (user scope):

```
claude mcp add --scope user rw -- rw mcp
```

Or for one repository, shared with your team, in the repository's `.mcp.json`:

```json
{
  "mcpServers": {
    "rw": { "command": "rw", "args": ["mcp"] }
  }
}
```

Claude Code asks before it calls a tool. To allow rw's tools in a project, add `"mcp__rw"` to `permissions.allow` in `.claude/settings.json`, or answer "always" once. For `claude -p`, pass `--allowedTools mcp__rw`.

### Codex

```
codex mcp add rw -- rw mcp
```

Or in `~/.codex/config.toml`:

```toml
[mcp_servers.rw]
command = "rw"
args = ["mcp"]
tool_timeout_sec = 120   # task_status may wait up to 50 seconds
```

Codex starts MCP servers with only a few environment variables (`PATH`, your home and temp folders, and on Windows `APPDATA` and the like). rw's agents inherit what rw gets. If your agent CLIs need more, such as an API key or a proxy, name them: `env_vars = ["ANTHROPIC_API_KEY", "HTTPS_PROXY"]`.

### Options

`rw mcp` takes the flags of `rw run`: `--dir`, `--repo name=path` for a multi-repo workspace, `--provider`, `--route`, `--prefer`, `--no-review`, `--budget-task-usd` and so on. Put them after `mcp` in the args, for example `["mcp", "--prefer", "all=codex"]`.

`rw doctor` shows where `rw mcp` is set up (the project's `.mcp.json`, `~/.claude.json`, `~/.codex/config.toml`) and whether its `rw` is found.

## The tools

| Tool | What it does |
|---|---|
| `run_task` | Starts a task and returns its `task_id` at once. Options: `approve_plan`, `review_changes`, `read_only` (one read-only agent answers a question), `budget_usd`, `budget_tokens`. |
| `task_status` | Phase, the plan with each step's progress, running agents, cost so far, providers at their limit, recent activity, and what the task waits for. `wait_seconds` (up to 50) returns early when the task ends or needs an answer. |
| `task_result` | Summary (or the read-only answer), steps, diff stat, checks, cost, undo key, a Markdown report path, and agents that take a follow-up. |
| `list_tasks` | This session's tasks and this repository's task history. |
| `cancel_task` | Stops the running task. Finished steps stay. |
| `resume_task` | Continues an interrupted, failed or cancelled task. Finished steps are skipped. |
| `follow_up` | A message to one of rw's agents: a running one gets it when its turn ends; a finished one continues its session as a new task. |
| `approve_plan`, `edit_plan` | Answer a plan that waits: approve it, reject it, or run an edited plan. |
| `apply`, `reject` | Answer a change review: land all or some files, send the changes back with feedback, or reject them. |
| `undo` | Reverts a finished task's changes (`redo` puts them back). |

One task runs at a time. The plan approval and change review follow your config (`orchestrator.approve_plan`, on by default, and `orchestrator.review_changes`, off by default) unless `run_task` sets them. A one-step task skips the planner, so it has no plan to approve.

## An example session

You, in Claude Code:

> Use rw to add input validation to the signup form, with tests. Show me the plan first.

Claude Code calls the tools:

```
run_task      {"prompt": "Add input validation to the signup form ... with tests", "approve_plan": true}
              -> {"task_id": "20261005-233145-fd9e-task-1", "message": "started: ..."}
task_status   {"task_id": "...", "wait_seconds": 40}
              -> {"status": "waiting", "phase": "approve-plan",
                  "waiting": [{"request_id": "a1", "type": "plan", "plan": {"steps": [...]}, "estimate": "~120k tok ..."}]}
```

It shows you the plan. You say "go ahead, but leave the CSS alone", so it calls `edit_plan` with that step removed, then:

```
task_status   {"task_id": "...", "wait_seconds": 40}
              -> {"status": "running", "phase": "execute", "agents": [{"id": "validate", "state": "running", ...}], ...}
task_status   {"task_id": "...", "wait_seconds": 40}
              -> {"status": "done", "next": "finished: task_result has the summary, ..."}
task_result   {"task_id": "..."}
              -> {"summary": "2/2 subtasks ok; reviewer approved", "diff": {"files": [...], "added": 84, "deleted": 3},
                  "checks": [{"command": "npm test", "ok": true}], "undo_key": "...", "report": "C:\\Users\\...\\reports\\....md"}
```

In the real runs, Claude Haiku needed one `task_status` call for the plan and one for the end.

## Safety

- **Only this folder.** rw works in the folder it was started in and the repos given with `--repo` at start-up. No tool takes a path. Task ids are checked, and tasks of other folders are not found.
- **The calling agent is untrusted input,** like a task you type. The repository's `.relayweft.yaml` still needs `rw trust` before its commands run. A task's budget can be lowered, never raised. When a budget is reached the task stops; the calling agent cannot let it go on. rw's settings cannot be changed through the tools.
- **No secrets in results.** Results carry task text, agent answers, file names, diff stats and costs; never environment values. Claude Code gives the servers it starts its own session's variables, among them a messaging token; rw removes them before it starts any agent, so they reach no agent or hook.
- **No recursion.** rw sets `RW_AGENT` for every agent it starts. An `rw mcp` that finds it, or finds an rw above it in the process tree (for clients such as Codex that pass MCP servers only a few variables), offers no tools and refuses every call: "rw mcp refuses here: ... A task's agent must not start rw tasks of its own". So putting `rw mcp` in your user-wide Claude Code config is safe: rw's own Claude agents get a server that does nothing. `rw doctor` warns if rw's own `mcp:` section gives its agents `rw mcp`.

## Limits

- **Your quota is shared.** If rw's agents use the same Claude account as the Claude Code session that calls rw, both draw from the same 5-hour and weekly limits. The calling session uses little while it waits (one short result per `task_status` call), but it should not do much else meanwhile. `task_status` lists providers at their limit (`limits`), and rw switches providers before a limit as usual. To keep your Claude quota for the session, route rw's work to Codex: `args = ["mcp", "--prefer", "all=codex"]`.
- **The client's timeout.** Tool calls return at once, except `task_status` with `wait_seconds`, which waits up to 50 seconds. Codex times tool calls out after 60 seconds by default.
- **Reconnects.** When the client disconnects (it restarts the server, or the session ends), rw cancels the running task and stops its agents. Every task saves its state after each step: the next `rw mcp` lists it, and `resume_task` continues it. Read-only questions and follow-ups keep no state, so their results are gone with the process.
- **One task at a time** per `rw mcp`. Two Claude Code sessions in the same repository start two `rw mcp`, which work like two `rw run` in the same repository.
- **Approvals wait** until the calling agent answers. A task waiting for `approve_plan` or `apply` does nothing meanwhile.
