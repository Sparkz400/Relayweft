# Tiers and best-of on multi-file tasks — 7 October 2026

**Result: neither paid off.** In 3 runs, tiers never picked a different
model from plain routed. Best-of cost about 2.3× routed's tokens and
changed no verdict. The run was stopped early at the user's request. It
covers 3 of the 6 planned tasks, one repetition each, and only 2 of those
3 tasks can be scored.

## Setup

- Modes: `routed`, `routed-tiers` (`routing.tiers: auto`) and
  `routed-bestof` (`routing.best_of.when: always`, n 2, default routes,
  meaning the step's route plus the same role on the next provider).
- Fair mode, one repetition, 20-minute limit per run.
- Product defaults otherwise: `auto_single`, `light_planning`,
  `review_when: failing`.
- Budget: reservations off and a 600k-token cap. The 6 October pilot
  measured the budget instead of best-of, because admission and
  `fit_budget` trimmed the candidates. Here neither triggered.
- Tasks: six validated history tasks from
  [realistic.yaml](realistic.yaml), with the agent checks of the levers
  run. Every known solution passed and every base failed (`--validate`).
  Planned order: resume-undo-coverage, package-manager-paths,
  process-ancestry, pool-lock-probes, setup-safe-backups,
  routing-evidence-thresholds.
- Stopped at 07:10, during the first run of pool-lock-probes. That row is
  not counted.

## Results

Fresh tokens: uncached input plus output.

| Task | routed | routed-tiers | routed-bestof |
|---|---|---|---|
| resume-undo-coverage | Pass · 14m · 77k | Pass\* · 16m · 67k | Pass · 15m · 86k + 112k Claude |
| package-manager-paths | Fail · 7m · 62k | Fail · 5m · 46k | Fail · 6m · 59k + 65k Claude |
| process-ancestry | unscoreable · 8m · 65k | unscoreable · 7m · 67k | unscoreable · 5m · 59k + 79k Claude |
| **Total tokens** | **204k** | **180k** | **460k** (≈$2.23 API-equivalent, Claude only) |

\* The bench recorded this run as a fail. The hidden check never ran a
test: the Go test binary crashed with `runtime: SetWaitableTimer failed;
errno= 6 / fatal error: runtime: netpoll failed`, which is a Windows
runtime fault. The same patch rescored as a pass three times
(`score_candidates.py --sanity`). The other eight landed patches rescored
to the bench's own verdicts.

process-ancestry cannot be scored. Its hidden test calls `Ancestors`,
`caseFold` and `ProgramName`, and the prompt names none of them. All three
modes, and the losing best-of candidate, fail to build against it. It
belongs with `azure-api-edge-cases` and `dashboard-auth-failures`: fix
the prompt or the test before it is used again.

## Tiers: it never fired

All three tiers runs scored the task at difficulty **0.50, tier standard**.
That is Codex gpt-6.1-sol at medium effort, the same route plain routed
uses. This follows from how the routing works:

- With `auto_single`, these tasks run as one worker step without a
  planner. That step has no file list. Its prompt is the 50-70-word task
  text, so neither "short" nor "long" applies, and none of the routine or
  hard words appear in it.
- A step starts at its role's centre, 0.5, and nothing moves it. Strong
  starts at 0.65.
- Claude's limit was 13-15% used, so the quota shift (`tiers_save_below`)
  did not apply either.

Tiers therefore cannot change anything on single-step tasks. Every
difference between the tiers and routed rows is run-to-run noise from an
identical setup. On the 4 October starter tasks, tiers moved 1 of 7 steps.
Tiers can only matter on planned tasks with several steps, where step
titles and file lists differ. On the current defaults, few tasks are
planned.

## Best-of: both candidates always reached the reviewer

In all three runs:

- Both candidates finished: Codex medium and Claude Sonnet medium.
- Both passed the public checks, so the checks never decided.
- An Opus reviewer picked the winner: Codex once, Claude twice. Each pick
  cost 16k-60k Claude tokens.

The losing candidates are saved as patches and were scored against the
hidden tests (`score_candidates.py`, no agents run):

| Task | Landed (picked) | Loser | What it means |
|---|---|---|---|
| resume-undo-coverage | Codex · pass | Claude · fail | The loser declared its own `TestResumeKeepsEarlierAgentFiles`, which clashes with the hidden test's name. That is a naming collision, not a wrong fix. Plain routed's Codex passed too. |
| package-manager-paths | Claude · fail | Codex · fail (`TestPackageManagerByOwner`, `ByPath`) | Both candidates were wrong, so no pick could have passed. |
| process-ancestry | Claude · unscoreable | Codex · unscoreable | — |

The pick never had a passing candidate to choose over a failing one. The
second candidate added 65k-112k Claude tokens per task and changed nothing.
For best-of to pay off, the two providers would have to fail on
*different* tasks. On the 6 October tasks and on these, they fail on the
same ones.

## Conclusions

1. **Tiers: no effect on the default pipeline.** It needs planned,
   multi-step tasks to do anything. It is not worth turning on by default.
   Measuring it again only makes sense with `rw run --plan` or
   `routed-classic` tasks.
2. **Best-of: about 2.3× tokens, no verdict changed.** This matches the
   earlier finding: the worker's first attempt decides the outcome, and a
   second model with the same prompt rarely disagrees in a way the checks
   can see. Keep it off by default.
3. **A bench gap found on the way:** a check that crashes the Go runtime
   is recorded as a task failure. `rw bench` should report a check that
   produced no test results as a check error, or retry it once. It should
   not score it as a fail.
4. Tasks some mode passes remain scarce. Only resume-undo-coverage passes
   on this machine; it passed in all three modes here.

## Reproduction

- Binary built from the main working tree (uncommitted batch on top of
  `48e3e1d`), SHA-256
  `f43550b804d5a8c778561f500e60ce48907fe24032c99b89bb29b94dfa352ffb`.
  The source diff is saved beside it.
- Codex CLI 0.160.0; Claude Code 2.1.288 (Max); Windows 11; Go 1.27.0.
  `rw doctor` passed.
- Run: 05:33-07:10 Europe/Berlin, 7 October 2026. Other Claude Code
  sessions were active on the machine during the run.
- Evidence: `D:\Entwicklung\rw-tiers-bestof-20261007`. It contains:
  - `tiers-bestof.yaml`, `config.yaml`, `run.sh` and `driver.log`;
  - per-task logs in `logs/`;
  - `bench-results-*.json` and run directories under `relayweft/`, with
    `candidate-NNN.patch` for each loser;
  - `analyze.py`, `score_candidates.py` and `candidate-scores.json`.
