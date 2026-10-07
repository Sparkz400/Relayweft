# Relayweft adoption decision

The product promise is controlled agent execution: checks, recoverable changes,
provider switching and resumable work. Extra agents have not demonstrated a
reliable correctness advantage. New feature work is frozen while this is tested.

## Shipped baseline

New single-repository tasks use one worker, a snapshot, configured or detected
project checks, at most one repair and final full checks. Multi-repository work
and `--plan` retain planning. Resumed work retains its saved plan. Final model
review and generated independent tests are opt-in; a requested review that cannot
answer is a failed gate, never approval. Learned routes suggest changes, and
tiers, best-of and the routing judge remain off. Existing user overrides still
apply. Queue filling, workflows and phone controls are optional tools.

These settings do not prove the task's requirements were met. Results retain
separate agent, check and requirements status. Dollar estimates are API-equivalent
costs, not subscription bills or savings. A zero budget is unlimited.

## Ten real tasks, then a decision

Use ten actual backlog tasks across the next working week or two. Include a
small fix, a multi-file change, a test failure, a docs change, interrupted work,
concurrent user edits and queued work. Do not invent unnecessary work to fill
the table. Compare Relayweft with the native agent using the same model, effort,
starting revision, instructions and available check commands. Alternate which
runs first, keep changes isolated, and review both results against requirements
written before either run. Record incomplete or failed runs too.

| Task | Accepted: native / rw | Human intervention and cleanup: native / rw | Elapsed: native / rw | Reported usage / actual quota | Useful rw control |
|---|---|---|---|---|---|
| 1 | Pending | Pending | Pending | Pending | Pending |
| 2 | Pending | Pending | Pending | Pending | Pending |
| 3 | Pending | Pending | Pending | Pending | Pending |
| 4 | Pending | Pending | Pending | Pending | Pending |
| 5 | Pending | Pending | Pending | Pending | Pending |
| 6 | Pending | Pending | Pending | Pending | Pending |
| 7 | Pending | Pending | Pending | Pending | Pending |
| 8 | Pending | Pending | Pending | Pending | Pending |
| 9 | Pending | Pending | Pending | Pending | Pending |
| 10 | Pending | Pending | Pending | Pending | Pending |

Keep Relayweft if its controls repeatedly save human effort without additional
correctness regressions or lost work. Narrow it to the controls that help if
most optional features go unused. Archive it if native tools remain preferable
and the extra controls do not earn their maintenance cost. This is an adoption
decision for the user's work, not a universal benchmark claim. Do not extend
the trial by adding features to explain away an unfavorable result.

## Evidence boundaries

On 7 October, all six eligible historical cases were revalidated on Windows:
the known solution passed and the base failed with protected tests. This was a
no-model check of the corpus, not a fresh comparison of agent output.

Automated tests and historical replays cannot supply human acceptance or
intervention time. The table is deliberately pending. The reliability suite and
hosted CI establish software checks only, not completion of this trial.

The historical ten-task corpus needs careful interpretation:

- `affected-module-index` includes an exact fallback-reason wording assertion;
  a mismatch alone is not evidence of incorrect selection.
- `azure-api-edge-cases` and `process-ancestry` have hidden tests that reference
  internal symbols absent from the original request. Those original runs cannot
  establish comparative correctness from a compile failure.
- `dashboard-auth-failures` has a historical fixture failure on a known solution
  in a later environment. Validate it afresh before using its score.
- Infrastructure crashes, unavailable tools and incomplete runs must be labeled
  separately from incorrect output. Lower effort against a higher-effort baseline
  does not isolate Relayweft's benefit.

The four suspect entries are now disabled for model runs. The runner prints
their exclusion and refuses an explicit request before invoking an agent.
`--validate` remains available for diagnosis. Do not reuse them as a quality gate until their contracts and
checks are repaired and both the solution and base have been validated. Preserve
the dated reports as historical evidence, including their limitations; do not
rewrite their scores as if they measured today's defaults.
