# Recover interrupted work

Open **Recovery** in `rw web` or `rw app`, or run:

```sh
rw recovery
rw recovery --json
rw recovery --resume TASK_ID
rw recovery --retry TASK_ID
rw recovery --undo TASK_ID
```

The view lists this project's 100 most recent tasks, progress, saved work,
preserved branches and step errors. A task still marked running whose process
lock is free is shown as interrupted. Live tasks do not offer recovery actions.

**Resume** continues the stored plan and skips successful steps. **Retry
unfinished** permits resuming failed or cancelled work; it also skips successful
steps. Work held in a pool or saved on a branch uses the existing resume path.
If no plan was saved, planning starts again. CLI recovery asks for plan approval.

**Preview undo** shows the recorded file changes before applying them. Recovery
undo affects agent-reported files only and keeps unreported files. Later edits
are merged; a conflict stops the undo without a partial restore. Undo snapshots
are retained for the most recent 30 tasks per working tree. Use `rw undo --redo
KEY` to redo an undo. A missing snapshot cannot be reconstructed by the UI.

While its undo snapshot is retained, an undone task is marked **undone** and
cannot resume, even with `--force`: its previously successful steps have been
reverted. Redo it before resuming, or start a new task.

Reported file edits are saved as they arrive, including edits before a hard
interruption. After resume, agent-only undo includes those earlier reports.
Shell commands do not always report which files they wrote. Such files remain
unreported and are kept by Recovery undo; a path mentioned only in the agent's
final answer is not file-change provenance. Review the terminal `rw undo`
preview if you need to include those files explicitly.

The web view refuses a task from a different project. Open that project in a
separate `rw web --dir PATH` instance to recover it. Saved branches remain
ordinary Git branches; inspect them with `git show BRANCH`. Recovery does not
delete branches or reset your branch or index.
