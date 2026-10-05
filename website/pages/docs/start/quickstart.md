---
title: Quick start
weight: 2
---

You need `rw`, Git 2.38+ and one agent CLI with a login. [Install](install.md) has all three.

## 1. Run the guided setup

In your repo:

```sh
cd path/to/your/repo
rw setup
```

It takes about a minute, and Enter takes the default at every question. `rw setup`:

- finds Claude Code, Codex, Gemini CLI, Qwen Code and Ollama, and checks their versions and logins without using quota;
- says how to install or log in to the ones that are missing;
- writes your config with the ready ones turned on;
- saves your repo's test commands to `.relayweft.yaml`;
- offers a first read-only task ("explain this repo", one short Haiku call), so you see a whole run.

`rw`, `rw run` and `rw web` start the setup by themselves when there is no config yet (not in CI; `RW_NO_SETUP=1` turns that off). In scripts, `rw setup --yes` asks nothing and also runs the first task.

## 2. Look around without quota

```sh
rw --demo        # the whole pipeline in the TUI, with fake agents
rw web --demo    # the same in your browser
```

## 3. Run a real task

```sh
rw               # the TUI in this repo
```

Type a task and press Enter. Then:

1. A planner splits the task into steps. Tasks under 12 words skip this and run as one step.
2. **You approve the plan.** You can edit, delete or reorder steps, give a step another role, or cancel.
3. Agents work on the steps, in parallel where they can. Each writing agent gets its own git worktree.
4. Their work is merged into your working tree. With `/review-changes on`, you check each agent's changes first.
5. Your checks run (the test commands `rw setup` found), then a reviewer looks at the result.
6. You see the cost, and the command that undoes it all.

[How a task runs](../concepts/pipeline.md) explains each step. The [TUI guide](../guides/tui.md) has the keys and commands, and [rw web](../guides/web.md) is the same in a browser.

## 4. Undo, if you want

```sh
rw undo          # preview, then revert the last task's changes
rw undo --redo   # put them back
```

Your branches and index are never touched, and edits you made after the task are kept. See [undo and resume](../concepts/undo.md).

## When something is off

`rw doctor` checks the CLIs, logins, git, the terminal, machine load, free disk and the worktree pools. `rw bugreport` zips everything needed to report a problem. See [troubleshooting](../reference/troubleshooting.md).
