# Day planner

A Claude subscription and a ChatGPT (Codex) subscription each give you a
usage window that lasts 5 hours from its first use. Windows you don't use
are lost. Overnight, an ordinary `rw run --file tasks.txt` uses whatever
room the windows have. Once both providers are at their limit, every step
fails with "every provider is at its usage limit", and so does the rest of
the queue.

The day planner keeps the queue going until morning:

```
rw dayplan --file tasks.txt --fresh-at 09:00          # print the plan, run nothing
rw run --file tasks.txt --fill --fresh-at 09:00       # run it
```

## What `--fill` does

Before each task, rw plans the rest of the queue again from the newest
readings. Then it does one of three things:

- **Runs the next task now, leaning on one provider.** That provider is
  the one whose running window resets first and still has room: that quota
  is lost soonest. A fresh window counts as resetting 5 hours from now, so
  a new window opens only when the running ones are full. The task's
  planner, workers and explorers go to that provider. The review still goes
  to the other one, unless the other one has no room ("only claude has
  room"). Then the whole task stays on the leaned provider.
- **Waits for the next reset** when every window is full or at its limit.
  It prints a countdown every minute and keeps the PC awake (`--allow-sleep`
  turns that off). Then it plans again.
- **Stops** when nothing can start before `--until` or `--fresh-at`. It
  lists the tasks it did not run and why.

A task that a usage limit stopped part-way is resumed once a window has
room. Its finished steps are kept. A task that failed for another reason is
not retried.

The lean is only a preference. A provider at its limit, or at
`routing.switch_at_utilization`, still hands work to the other one, as in
any run. `--provider` and roles you set explicitly (`--prefer`, `--route`,
the repo file, session edits) are never moved.

## Options

| Flag | Meaning |
|---|---|
| `--until 07:00` | Start no task at or after this local time. The default is to run until the queue is done. |
| `--fresh-at 09:00` | Open no window that would still run at 09:00, so both subscriptions start the working day with a full window. Implies `--until 09:00`. Windows already running may still be used. |
| `--at`, `--in`, `--when-reset` | Start the whole run later, as for any scheduled run. |

Times are local: a clock time is the next one after now, or use
`"2026-10-07 07:00"` or RFC3339.

`budget.day_tokens` and `budget.day_usd` count too. A task that would go
over today's budget waits for midnight, when the budget starts over. A
task whose estimate is larger than the whole day budget is skipped. While
a task runs, the budget is enforced as always: an unattended task that
hits a limit stops.

## Where the numbers come from

`rw dayplan` shows its inputs at the top:

```
Day plan · 60 queued task(s) · now 20:24 · full windows at Wed 09:00
  codex   no window reading yet: assumed unused · window size unknown: a task counts as 10%
  claude  5h window 3% used, resets 21:30 (read 19:17) · a window holds ~899k tokens (1 reading(s))
  a task: ~48k tokens, 4m50s (the median of 47 task(s))
```

- **Window state:** In a running rw, this comes from the live quota
  reports of Claude Code and Codex. Otherwise it is the newest quota or
  limit record in the session logs. A reading whose window has passed counts
  as reset. When the weekly window is the fullest and over the ceiling, the
  provider counts as limited until the weekly reset.
- **Window size:** This is learned from the logs. It is the fresh tokens
  rw's agents spent on a provider between two readings of the same 5-hour
  window, divided by how far the window's use moved. The median over the
  last 30 days is used. Your own interactive use of the same subscription
  counts against the window but not against rw's tokens, so the estimate
  errs on the small side. With no reading, each task counts as 10% of a
  window ("guess").
- **A task:** This is the median fresh tokens and duration of the finished
  tasks in this repo (with at least 3), else of all repos, else 150k tokens
  and 8 minutes. Every queued task gets the same estimate. The plan is a
  forecast, and `--fill` corrects it before every task from what really
  happened.
- **The ceiling:** A provider above `routing.switch_at_utilization`
  (default 0.9) takes no new task.

## Limits

- One task runs at a time, as in every `rw run --file`. The planner decides
  which window each task uses and when to wait. It does not run two tasks
  in parallel.
- Only enabled Claude and Codex providers are planned for. A provider with
  `only_preferred` (a local model) is left out.
- `--fill` works on a task file or one task. It does not work with
  `--issues`, `--team` or `--single`.
