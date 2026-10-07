# Phase 3 check after the task-shape shortcuts — 6 October 2026

**Result: rw is now cheaper and faster than its old pipeline and than a
high-effort single agent, with the same correctness. It is not more correct
than one agent.** The afternoon run met the letter of the Phase 3 exit
criterion (2 of 3 measures), but routed mostly ran one Codex worker at medium
effort. The evening follow-up against a medium-effort single agent showed no
correctness benefit from planning, reviewing or mixing providers. See
[Conclusions](#conclusions).

## What changed before this run

Four defaults, all in `orchestrator` (see [docs/plan.md](../plan.md) and
[docs/config.md](../config.md)):

- `auto_single`: a task whose text names at most 2 files, has at most 2 list
  items and at most 150 words, and has no step words (then, in parallel, ...),
  no broad words (refactor, migrate, every file, ...), no sensitive path and a
  difficulty below the strong tier runs as one worker step without the
  planner. Checks and the final review still run. `rw run --plan` always plans.
- `light_planning`: the planner and reviewer of a task that does not look hard
  or sensitive use the worker route on their provider.
- `review_skip_max_lines: 80`: the final review is skipped when
  `verify.commands` pass, every step succeeded, no review asked for changes and
  the diff has at most 80 changed lines, no binary file and no sensitive path.
- `fit_budget`: the planner is told what the budget has left, and a plan
  estimated over it drops the plan review, then best-of candidates, then merges
  into one step that keeps every step's work. A task whose budget cannot fund
  planning runs as one step.

`rw bench` mode `routed-classic` turns all four off, so the old pipeline can be
measured in the same run.

This run found two bugs in the first version, both fixed and covered by tests
before the final numbers below:

- A question that also asked for a file ("Where is X? Write the answer to
  ANSWER.md") became a read-only step and could not write the file
  (`TestShortcutQuestionThatWrites`).
- A one-step task still ran the Claude permission preflight when its only
  writer was Codex: 51k fresh tokens, almost a third of that run
  (`TestAutoSinglePreflightsOnlyItsProvider`). A planned task still probes
  every Claude writing provider before the planner.

## Starter set (5 small Python tasks)

`rw bench --starter`, modes `single:codex:gpt-6.1-sol:high`, `routed`,
`routed-classic`; `verify.commands: ["python -m unittest discover -s tests"]`;
fair mode, one repetition.

| Mode | Passed | Avg wall | Fresh tokens (Codex + Claude) | ≈$ API-equivalent (Claude only) |
|---|---|---:|---:|---:|
| Single Codex, high | 5/5 | 41s | 70k + 0 | 0.00 |
| Routed, as run | 4/5 | 34s | 54k + 48k | 0.15 |
| Routed, after the write fix (explain-low-stock rerun) | 5/5 | 32s | 72k + 0 | 0.00 |
| Routed-classic | 5/5 | 1m12s | 100k + 27k | 0.36 |

Every routed task ran as one Codex worker at medium effort. On the four edit
tasks the unit tests passed and the diff was 6-8 lines, so the final review
was skipped. The as-run failure was the read-only bug above: a Claude Haiku
explorer answered but could not write `ANSWER.md`. Its rerun with the fix wrote
the file through a Codex worker in 28s and 18k tokens.

Compared with routed-classic (the old pipeline), routed took 55% less wall time
and 43% fewer fresh tokens. Compared with the single agent, it was 22% faster
with about the same tokens.

## Realistic tasks (2 multi-file tasks)

Tasks `affected-module-index` and `package-manager-paths` from
[realistic.yaml](realistic.yaml). Both were validated first: the known
solutions pass and the bases fail with the protected tests. The configuration
was [realistic-config.yaml](realistic-config.yaml): 200k-token / $10 task
budget, reservations on, fair mode, one repetition.

| Task | Mode | Hidden check | Wall | Fresh tokens (Codex + Claude) | Note |
|---|---|---|---:|---:|---|
| affected-module-index | Single Codex, high | Fail | 9m49s | 102k + 0 | |
| affected-module-index | Routed, as run | Fail | 6m17s | 62k + 113k | 51k Claude preflight, then a Codex worker; light review approved |
| affected-module-index | Routed, preflight fix | Fail | 3m43s | 43k + 14k | No preflight; light review approved |
| affected-module-index | Routed-classic | Fail | 7m48s | 133k + 14k | Budget admission stopped the Jest worker |
| package-manager-paths | Single Codex, high | Fail | 9m46s | 107k + 0 | |
| package-manager-paths | Routed | Fail | 5m57s | 86k + 14k | One Codex worker; light review approved |
| package-manager-paths | Routed-classic | Fail | 10m58s | 143k + 32k | The planner made a one-step plan; Opus review approved |

Totals over the two tasks, with the preflight fix counted for routed:

| Mode | Passed | Avg wall | Fresh tokens |
|---|---|---:|---:|
| Single Codex, high | 0/2 | 9m48s | 209k |
| Routed | 0/2 | 4m50s | 157k |
| Routed-classic | 0/2 | 9m23s | 323k |

## What this shows, and what it does not

- **Small tasks:** the old routed pipeline was slower and more expensive than
  a single agent (as on 3 October). Default routing now is not.
- **The exit criterion:** routed beat the single agent on wall time in both
  groups and on tokens in the realistic group. It tied on correctness: 5/5 and
  0/2 in both. That is 2 of 3 measures on this run.
- **Why it is not proof:**
  - One repetition per mode, and seven tasks in total.
  - Routed mostly *was* a single agent: the worker route is Codex at medium
    effort, the baseline is Codex at high effort. Most of the gain is "medium
    effort with checks" against "high effort without", not planning or
    parallel agents.
  - Both realistic tasks failed everywhere, so correctness gives no signal
    there.
- **Reviews did not catch failures:** in all four realistic routed and
  routed-classic runs that reached the final review, the reviewer approved
  work that fails the hidden tests. That includes the full-strength Opus
  review. The public checks also passed. On these tasks the review adds cost,
  not correctness.
- **Planning was overhead here:** in `package-manager-paths` the full planner
  spent 1 minute and 23k tokens to produce a one-step plan.

## Evening follow-up: medium baseline, screening and levers

Planned next was the matrix: all ten realistic tasks, three repetitions, with
`single:codex:gpt-6.1-sol:medium` added as a baseline to separate effort from
rw's pipeline. It was cut down twice to save quota, and stopped after 17
completed runs because the conclusion below no longer depended on it.

- `dashboard-auth-failures` was left out: its solution commit fails its own
  check on this machine (`TestDashboardMatchesCLI`: 0 Claude limit hits, want
  1), so no run of it could be scored.
- The matrix was stopped after three `affected-module-index` runs. No mode has
  passed that task in 14 runs across three sessions.
- **Screening** (one run per mode, the earlier binary) covered 2 tasks fully
  and a third partly. Both runs of `bitbucket-api-edge-cases` that hit the
  20-minute limit are counted as failures.
- **Levers** added three changes to the binary:
  - writing agents check every requirement the task names, and the reviewer
    rejects one without evidence;
  - fix rounds continue the writer's session;
  - the review gets shortened repo conventions.

  It also gave every task the repo tests of the packages it touches as
  `verify.commands`. Each check was confirmed to pass at the task's base
  commit; none is a hidden scoring test. Only `affected-module-index` had
  checks before. Single high was dropped from this run: it never passed and
  was always the slowest and costliest.

| Run | Task | Single high | Single medium | Routed |
|---|---|---|---|---|
| Matrix | affected-module-index | Fail · 13.1m · 113k | Fail · 6.8m · 79k | Fail · 9.3m · 184k |
| Screening | resume-undo-coverage | Fail · 15.3m · 148k | Fail · 14.0m · 114k | **Pass** · 7.0m · 90k |
| Screening | azure-api-edge-cases | Fail · 18.1m · 189k | Fail · 15.7m · 140k | Fail · 12.3m · 163k |
| Screening | bitbucket-api-edge-cases | Timeout · 20m · 185k | Fail · 8.3m · 94k | Timeout · 20m |
| Levers | affected-module-index | - | Fail · 4.8m · 78k | Fail · 5.8m · 127k |
| Levers | resume-undo-coverage | - | **Pass** · 6.8m · 82k | **Pass** · 7.6m · 74k |
| Levers | azure-api-edge-cases | - | Fail · 8.9m · 130k | (stopped) |

Tokens are fresh tokens (uncached input plus output), Codex and Claude
together.

What happened inside the routed runs:

- Every routed task ran as one Codex worker at medium effort (`auto_single`).
  The planner never ran.
- Where checks existed, they passed on the first try. No fix round started in
  the levers run, so fix-session continuation was never exercised live.
- Reviews rarely objected. On the realistic tasks today, 9 of 10 final
  reviews approved, the full-strength Opus review and the requirements
  checklist included. 7 of those 9 approved work that fails the hidden
  tests. The one review that asked for changes did not lead to a pass. Reviews cost 11k-62k Claude fresh tokens each. The 60k-token
  levers review took 13 seconds, so most of its cost was Claude Code's own
  starting context, not the review.
- The one screening pass of routed was not unique to rw: in the levers run,
  single medium with the checks in its prompt passed the same task.

## Conclusions

1. **rw is not more correct than one agent.** With the current models, the
   outcome is decided by the worker's first attempt. Planning, reviewing and
   mixing providers did not change it on any task today.
2. **rw is cheaper and faster than it was, and than a high-effort agent.**
   The gain comes from its defaults (medium effort, no planner for one-step
   tasks, checks in the loop), not from coordinating agents.
3. **Tests in the prompt look like the real correctness lever.** They help a
   single agent as much as rw. That is one data point, not proof.
4. **The final review is the clearest waste.** It cost tokens in every run and
   changed no outcome. Skipping it whenever the checks pass, not only for
   small diffs, is the obvious next default.
5. **Phase 3's premise needs rewording.** "Beats a single agent" on
   correctness is not supported. What is supported: no worse on correctness,
   cheaper or faster. Whether rw reaches the usage limits later (the third
   measure) is untested: every run is far below any limit.

## If this is measured again

- An independent test writer: the other provider writes tests for each
  requirement without seeing the implementation, and rw runs them before
  the review. It is the one way to use two models together that the failure
  pattern (plausible code that misses a named requirement) suggests could
  help.
- Find or write tasks some mode passes. A task nobody passes cannot show a
  correctness difference.
- Measure the limits directly: a day of queued tasks on one provider, against
  rw spreading them over both.

## Reproduction

- Binaries were built from this checkout (uncommitted, on top of `48e3e1d`),
  three times:
  - The starter run used the first build, before both fixes (not hashed).
  - The explain-low-stock rerun and the realistic run used a build with the
    write fix and the step words, SHA-256
    `51c5d03391b681e4fa81e7033a329bc0c5a0f53a8ad58d561eaacb51bb4d0881`.
  - The routed rerun used a build that also had the preflight fix,
    `130c218bae1a26428abb710c607e057421f597483beb0c59dc3bf71e0f70191e`.
- Codex CLI 0.160.0; Claude Code 2.1.288 (Max); Windows 11, Go 1.27.0. Both
  CLIs were signed in (`rw doctor`).
- Evidence: `D:\Entwicklung\rw-phase3-20261006` (`starter-run.log`,
  `explain-rerun.log`, `realistic-run.log`, `routed-rerun.log`,
  `bench-results-*.json` with per-run directories, `phase3.yaml`,
  `starter-config.yaml`, `realistic-config.yaml`).
- Runs: starter 13:49–14:04, explain rerun 14:18, realistic 14:25–15:16,
  routed rerun 15:16–15:20 (Europe/Berlin). The full Go test suite ran during
  the first half of the starter run, so those wall times carry some CPU
  contention, equally across modes.
