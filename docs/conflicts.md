# Merge conflicts

Writing agents work in parallel, each in its own pool worktree. When one finishes, rw merges its change into your tree. Two things can make that merge conflict:

- **Another step's change** landed first on the same lines.
- **Your own edits.** You changed the same lines in your tree while the agents worked.

Until now the step then failed and its change was kept on a branch. Now rw can run a **resolve step** instead.

## The resolve step

1. rw puts the step's change on a branch (`rw/<session>/<task>/<step>`) first, whatever happens next.
2. In the step's own pool worktree, rw starts `git merge` of the step's change into the tree it conflicts with. The conflict markers are left in the files, as git leaves them.
3. An agent (`<step>--resolve`) resolves the conflict there. It gets:
   - both sides' intents: the step titles, prompts and summaries;
   - the conflicted files and what each side did to them (both changed, deleted, renamed);
   - the conflict hunks;
   - the repo's checks (`verify.commands`).
4. rw checks the result:
   - no conflict markers are left in the conflicted files: lines starting with `<<<<<<< `, `||||||| ` or `>>>>>>> ` (a file that had such lines on either side may keep as many; a bare `=======` is a Markdown heading underline too, so it is not counted);
   - the step's change is not dropped as a whole (as `git merge --abort` would do);
   - `verify.commands` pass. Checks that already failed on the tree before the merge do not count against it.
5. A failed check is another attempt, with the reason, up to `max_resolve_rounds` (default 2).
6. With `review_changes` on, you review the resolution. It is marked as a conflict resolution and shows what lands: the step's change with the conflict resolved. You can accept it, accept some files or hunks, reject it, or send it back with feedback (twice at most).
7. The resolution lands like any step's change. The final review sees a note about it.
8. Both versions stay on branches, because the resolution may change lines of either side: `<step>` has the step's change, `<step>-other-side` (or `<step>-your-edits`) the tree it conflicted with.

The resolve agent runs on the route that wrote the step's change. `orchestrator.resolve_role` (worker or worker_high) picks a role instead. If that provider is at its limit, the same role runs elsewhere.

In the TUI and `rw web` the resolve agent shows up next to its step, with the routing reason "resolves b's conflict with step a in shared.txt".

## When no agent resolves it

rw keeps the old behaviour (the step fails and its change is not applied) when:

- `orchestrator.conflicts` is `fail`, or you say no when asked;
- a conflicted file is binary, a Git LFS file, a symlink or a submodule, or bigger than 4 MB;
- every attempt failed, or you rejected the resolution.

Then rw keeps three branches:

- `<step>`: the step's change;
- `<step>-other-side` (or `<step>-your-edits`): the tree it conflicted with;
- `<step>-resolve-attempt`: the agent's last attempt, if there was one.

The message says why, and how to apply the change by hand:

```sh
git diff --binary --no-ext-diff --no-color <base> <branch> --output=rw-<step>.patch
git apply --reject rw-<step>.patch
```

What does not fit goes to `.rej` files next to the files. The patch goes through a file, not a pipe, so it also works in Windows PowerShell 5.1.

## Conflicts with your own edits

No agent ever works in your folder. rw resolves a conflict with your edits like this:

1. It snapshots your tree (with a temporary index; your index is not touched).
2. The agent resolves the conflict in the pool worktree, against that snapshot.
3. The result lands through the usual 3-way path: a file you changed again in the meantime is merged once more, and if that conflicts, nothing is written.
4. Your version from before the resolution is kept on `<step>-your-edits`.

No other step starts or lands in that repo until this is done.

Because the agent edits lines you are working on, `auto` asks you first. A task that runs unattended (queued, scheduled, `--file`) cannot ask, so the change is kept on a branch, as before.

`rw mcp` also keeps the work on a branch when approval is needed: the calling agent has no tool to answer a conflict question. Automatically allowed resolutions still run, and MCP change reviews identify them with `changes.conflict`.

## Settings

```yaml
orchestrator:
  conflicts: auto          # auto | resolve | ask | fail
  max_resolve_rounds: 2    # attempts per conflict (1-5)
  resolve_role: ""         # worker or worker_high; empty = the route that wrote the change
```

| `conflicts` | Between steps | With your edits |
|---|---|---|
| `auto` (default) | an agent resolves it | rw asks first |
| `resolve` | an agent resolves it | an agent resolves it |
| `ask` | rw asks first | rw asks first |
| `fail` | kept on a branch | kept on a branch |

In the TUI: `/conflicts auto|resolve|ask|fail`. The question takes `y` or `n` (Enter does nothing there, so an Enter meant for the prompt cannot say yes). Unattended tasks never ask: there, `ask` means `fail`.

A repo's `.relayweft.yaml` (or a `./relayweft.yaml` that came with a clone) may make `conflicts` stricter or lower `max_resolve_rounds` without `rw trust`, never the other way: a resolve agent costs quota and may write into files you edit.

## Resume and undo

- From the moment rw starts the merge, the task state records the step's change as kept. If rw stops (a cancel, a crash, a closed window, also while it asks you), `rw resume` lands that change again and resolves the conflict anew. The writer does not run again.
- After a resume, the tree the kept change meets may hold edits you made while rw was stopped, so in `auto` rw asks before an agent resolves that conflict.
- `rw undo` reverts a task with a resolved conflict like any task.
