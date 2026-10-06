---
title: Troubleshooting
weight: 4
---

## First steps

1. Run `rw doctor`. It checks the agent CLIs and their logins, git, the terminal, machine load, free disk and the worktree pools, and says what to fix.
2. If that does not explain it, run `rw bugreport` and attach the zip to an [issue](https://github.com/Sparkz400/Relayweft/issues). Check it first if your repo is private: it holds your config and the last session logs.
3. For a security problem, do not open an issue: see the [security model](security.md).

## Common questions

**`rw` says a CLI is missing or logged out.** Install it and log in as the CLI's own docs say (`claude auth login`, `codex login`), then run `rw doctor` again. One provider is enough.

**`rw doctor` warns that my CLI version differs.** `rw` reads the CLIs' JSON output, which can change between releases. A different version usually works. If a run fails to parse, `rw bugreport` has what is needed to fix it.

**A provider hit its usage limit.** Until the reset time (read from the CLI's message, else an hour), `rw` uses each role's route on the next provider in `routing.provider_order`. `/limit` corrects the state by hand. See [roles and routing](../concepts/routing.md#usage-limits).

**Agents in worktrees cannot build my project.** Pool worktrees do not contain ignored files such as `node_modules`. Set `orchestrator.worktrees: false` (writers then take turns in your tree) or `max_threads: 1`. See [the worktree pool](../concepts/worktrees.md).

**Claude workers cannot run my tests.** By default Claude workers may only edit files. `rw` lets them run exactly your `verify.commands`; for anything else, add `write_allowed_tools` under `providers.claude`. See [checks](../concepts/checks.md).

**The TUI looks broken on Windows.** Use Windows Terminal. The old console gets an ASCII theme by itself; `--ascii` and `--unicode` force either.

**`rw` was closed in the middle of a task.** Run `rw resume`. Finished steps are skipped. See [undo and resume](../concepts/undo.md).

**I do not want what the task did.** Run `rw undo`. It shows what it will revert first.

**Upgrading from Switchyard (`sy`).** See [install](../start/install.md#upgrading-from-switchyard-sy).

<!-- include README.md#when-something-goes-wrong level=2 -->
