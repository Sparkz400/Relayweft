# sy bench: starter set, Claude only (3 Oct 2026)

`sy bench --starter`, run with `--provider claude` because no Codex CLI was
available. The modes were routed, routed without the context hand-off, and a
single Claude Opus agent at high effort. Switchyard was at branch
claude/phase2-3 (before one-step plans skipped the plan review).

## What it shows

- **All 15 runs passed.** These starter tasks are small, one-file changes.
- **On small tasks the single agent is about 3x faster and uses about half
  the tokens.** Every routed task ran 4 model calls:
  - an Opus planner that made a one-step plan;
  - an Opus plan review (approved 10 of 10);
  - a Sonnet worker;
  - an Opus final review (approved 10 of 10).

  The single agent made one call.
- **The context hand-off helped a little:** 152k vs 157k fresh tokens and
  1m15s vs 1m24s average wall time over 5 tasks. That is too few runs to
  rely on.
- **Change made from this:** one-step plans now skip the plan review by
  default (`orchestrator.review_single_step_plan: false`). This saves one
  strong-model call per small task. The final review still checks the work.
- **Still open:** this does not show whether routing pays off on bigger,
  multi-file tasks. That needs `sy bench` on about 10 real tasks from your
  own repos, with Codex available.

Starter repo commit 523b9ac8422b5f45ab9e3571caabea36031f3e30

```
TASK               MODE                     CHECK  WALL   CODEX TOK  CLAUDE TOK  NOTE
trailing-fields    routed                   pass   1m2s   0          29k         
trailing-fields    routed-nohandoff         pass   1m37s  0          30k         
trailing-fields    single:claude:opus:high  pass   21s    0          14k         
negative-stock     routed                   pass   1m12s  0          31k         
negative-stock     routed-nohandoff         pass   1m21s  0          30k         
negative-stock     single:claude:opus:high  pass   26s    0          13k         
quoted-fields      routed                   pass   1m20s  0          27k         
quoted-fields      routed-nohandoff         pass   1m43s  0          35k         
quoted-fields      single:claude:opus:high  pass   31s    0          15k         
json-report        routed                   pass   1m55s  0          42k         
json-report        routed-nohandoff         pass   1m28s  0          37k         
json-report        single:claude:opus:high  pass   37s    0          19k         
explain-low-stock  routed                   pass   47s    0          22k         
explain-low-stock  routed-nohandoff         pass   50s    0          24k         
explain-low-stock  single:claude:opus:high  pass   15s    0          10k         

MODE                     PASSED  AVG WALL  CODEX TOK  CLAUDE TOK  ≈$ API-EQUIV
routed                   5/5     1m15s     0          152k        1.49
routed-nohandoff         5/5     1m24s     0          157k        1.56
single:claude:opus:high  5/5     26s       0          71k         0.92
```
