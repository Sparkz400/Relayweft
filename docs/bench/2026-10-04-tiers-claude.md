# rw bench: cost-aware model tiers, Claude (4 Oct 2026)

`rw bench --starter` with `--provider claude`, run once without and once
with `--tiers` (`routing.tiers: auto`), plus a single Claude Opus agent at
high effort as the baseline. Relayweft was at main 9f39356 (v0.2.0 plus
the resume work), Claude Code 2.1.288, on Windows 11. A small extra run
used Codex (codex-cli 0.160.0) with `--tiers`.

The plan was 25 Claude runs. The bench stopped itself at 19, when Claude's
5-hour limit reached 67% (the quota guard was 66%). Each tiers run and
each plain routed run went to its own `rw bench --only <task>` call, so
the two modes alternate in time.

## What it shows

- **All 22 runs passed.** These starter tasks are small, one-file changes.
- **On these tasks the tiers changed almost nothing.** They picked the
  model of 7 Claude work steps:
  - 6 stayed `standard` (Sonnet, the worker route);
  - 1 went `fast` (Haiku) and passed.

  The Codex work steps all stayed `standard` (gpt-6.1-sol at medium).
- **The cost is the same within noise.** Without the two outliers in the
  plain routed runs (below), plain routed used 27k fresh tokens and
  ≈$0.31 per task, tiers 29k and ≈$0.32. The planner and the reviewer run
  on Opus either way, and they are most of a routed task's cost. The one
  Haiku step was a little cheaper (≈$0.29) but slower (1m6s vs 43s-50s
  for Sonnet on the same task).
- **The difficulty score is noisy.** The same task (trailing-fields) got
  0.35 in one run and 0.60 in the next, because the planner wrote a
  different step prompt each time. Planner prompts are long (270-330
  words), so "long" often added 0.1. Routine words that only appeared in
  passing ("rename", "docstring", "comments", "readme") took 0.15 off in 4
  of 7 steps. "rename" is not in the task or the plan summary; it came
  from the planner's step text, and it sent a real logic fix to Haiku.
- **Quota mattered a little.** Claude's limit was at 59-65% during the
  tiers runs, so 35-41% was left, below `tiers_save_below` (50%). Every
  score moved down by 0.05-0.09. That is what put the "rename" step under
  0.35. Codex reports no limit, so its scores did not move.
- **A single agent is still faster and cheaper on tasks this small:**
  25s and 16k fresh tokens (≈$0.21) per task, against about 50s and 27k
  routed.
- **Changes made from this:**
  - Routine words now count only in the step's title (in the prompt
    only for a step without a title). A prompt that mentions the readme
    or "keep the comments" in passing no longer moves a step down. Hard
    words still count anywhere: moving up is the safe mistake.
  - `rw bench` printed "cancelling the bench... (Ctrl+C again to force
    quit)" at the end of every normal run. Fixed.
- **Still open:** tiers can only pay off where steps differ: real
  multi-file tasks with routine and hard steps mixed. The starter set
  cannot show that. The next measurement is `rw bench --from-history` on
  this repo or yours, with and without `--tiers`.

Starter repo commit d8c9ea7 (the `rw bench --starter` files of main
9f39356).

## Runs

Claude, in run order. WORKER is the work step's model, with its tier in
the tiers runs.

```
TASK               MODE                     CHECK  WALL   CLAUDE TOK  ≈$    WORKER          NOTE
trailing-fields    routed                   pass   50s    76k         0.72  sonnet          first run: cold prompt cache
trailing-fields    single:claude:opus:high  pass   23s    17k         0.22  opus
trailing-fields    routed                   pass   43s    25k         0.30  sonnet
trailing-fields    single:claude:opus:high  pass   20s    15k         0.21  opus
negative-stock     routed                   pass   56s    28k         0.35  sonnet
negative-stock     single:claude:opus:high  pass   21s    15k         0.20  opus
quoted-fields      routed                   pass   52s    30k         0.35  sonnet
quoted-fields      single:claude:opus:high  pass   34s    17k         0.24  opus
json-report        routed                   pass   1m51s  104k        1.02  sonnet x2       final review asked for a fix round
json-report        single:claude:opus:high  pass   34s    20k         0.27  opus
explain-low-stock  routed                   pass   44s    23k         0.24  sonnet
explain-low-stock  single:claude:opus:high  pass   18s    13k         0.14  opus
trailing-fields    routed --tiers           pass   1m6s   32k         0.29  haiku (fast)    0.35 "rename" -> 0.30
negative-stock     routed --tiers           pass   43s    27k         0.31  sonnet (std)    0.45
quoted-fields      routed --tiers           pass   59s    29k         0.34  sonnet (std)    0.45
json-report        routed --tiers           pass   58s    35k         0.39  sonnet (std)    0.55
explain-low-stock  routed --tiers           pass   27s    21k         0.21  sonnet (std)    0.45
trailing-fields    routed --tiers           pass   46s    28k         0.33  sonnet (std)    0.60
negative-stock     routed --tiers           pass   45s    29k         0.37  sonnet (std)    0.60

MODE                     PASSED  AVG WALL  CLAUDE TOK  ≈$ API-EQUIV
routed                   6/6     59s       286k        2.98
routed --tiers           7/7     49s       202k        2.24
single:claude:opus:high  6/6     25s       97k         1.27

Without the cold first run and the fix round (4 routed runs):
routed                   4/4     49s       106k        1.24
```

The NOTE column of the tiers runs is the difficulty score before the
quota shift.

Codex with `--tiers`, without `--provider`. Codex comes first in
`provider_order`; `--prefer reviewer=codex` (and the same for explorer,
researcher and judge) kept the Claude quota out of it:

```
TASK             MODE            CHECK  WALL   CODEX TOK  WORKER
trailing-fields  routed --tiers  pass   1m2s   32k        gpt-6.1-sol@medium (std, 0.50)
json-report      routed --tiers  pass   2m46s  50k        gpt-6.1-sol@medium (std, 0.40)
negative-stock   routed --tiers  pass   1m27s  17k        gpt-6.1-sol@medium (std, 0.50)
```

## Limits

Claude's 5-hour limit went from 46% to 67% (7-day: 27% to 30%) over the
19 Claude runs, in 15 minutes, for ≈$6.50 API-equivalent. Other Claude
sessions on the same account ran at the same time, so this cannot be split
per mode, and the "how quickly the limits are reached" measure stays open.

## What it means for `rw tune`

Across this bench, the earlier one and the logs on this PC, about 120
agent runs failed 0 times, and 1 of about 30 final reviews asked for
changes.
No step escalated, no provider switched at its limit and the judge never
ran. So `rw tune`'s rates rest on almost no failures. Its thresholds now
depend on the number of runs: a rate counts only when the lower end of
its 90% confidence interval is above the threshold. 2 failures in 5 runs
no longer flag a route; 3 do. A provider must run out twice, at least
12 hours apart, not in one afternoon or one evening past midnight.
Escalations keep their plain rate: their minimum of 3 already guards
small samples.

## Caveats

- One-file starter tasks are easy. Every run passed in every mode, so the
  tiers could only save cost, and there was little to save: the worker
  step is about a third of a routed task's cost.
- 19 Claude runs is small. The two plain routed outliers (a cold prompt
  cache on the first run, and a fix round) move its averages more than
  the tiers do.
- The quota shift was on during all tiers runs because the account was
  already past 50% used. A run with more quota left would score 0.05-0.09
  higher, and the "rename" step would have stayed on Sonnet.
- The workers could not run the tests: Python is not in the starter's
  `verify.commands`, so Claude Code asked for approval and nobody could
  give it. The checks ran afterwards, outside the agents.
