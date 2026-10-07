# Four-mode pilot rerun — 6 October 2026

**Finished: 0/4 protected-check passes.** The reliability changes retained all
four results and kept reported usage below 200,000 fresh tokens per mode. They
did not make this task succeed: every routed mode stopped with required work
still unfunded. No general routing, tiering or best-of benefit is established.

## Results

One `affected-module-index` task, four modes, one repetition. Times include
preflight, agents, evidence capture and scoring.

| Mode | Protected check | Wall time | Codex fresh tokens | Claude fresh tokens | Outcome |
|---|---|---:|---:|---:|---|
| Single Codex, high | Fail | 9m 58s | 108,672 | 0 | Agent completed; hidden checks found Jest and Gradle defects |
| Routed | Fail | 1m 31s | 36,404 | 105,884 | Budget admission stopped the first implementation worker |
| Routed tiers | Fail | 13m 00s | 140,501 | 16,048 | Gradle patch landed; Jest worker could not fit |
| Routed best-of-two | Fail | 6m 24s | 30,533 | 132,015 | Jest candidate landed; Gradle candidates and requested fix could not fit |

Total: **30m 53s and 570,057 fresh tokens**. Per-mode totals were 108,672,
142,288, 156,549 and 162,548. All final usage records were present and reconciled
with individual agent records. Claude reported $0.6953, $0.1965 and $1.2832
API-equivalent usage for the three routed modes ($2.1750 total). These are not
subscription charges. Codex supplies no dollar price; its zero dollar field
does not mean free usage. No provider quota-limit error was recorded.

The previous pilot also scored 0/4, taking 49m 22s with routed totals of
233,504, 214,481 and at least 254,051 tokens. This rerun stopped earlier and
spent fewer reported tokens, but incomplete work is not faster successful
delivery. The previous best-of usage was incomplete, and the experiments have
different preflight, command-permission and reservation behavior.

## What the live run verified

- Native JSON and Markdown checkpoints survived transitions between all four
  modes. Every row has full events, scoring output and a solution patch. The
  ordinary routed patch is empty because implementation never started.
- All three Claude preflights executed the two exact configured commands and
  passed the successful-tool-result requirement. Their fresh-token costs were
  **50,640**, **2,811** and **2,914**. Later probes reported substantially more
  cached input; this mode-order effect limits comparisons.
- Reservations held workers until active usage was known, then explicitly
  declined work that could not fit. All four final totals stayed below the
  configured threshold. This demonstrates this run's behavior, not a hard cap:
  the tiers worker used 103,877 tokens against a 60,000-token estimate.
- Best-of preserved and landed the completed Claude Jest candidate when the
  other three candidates were skipped. Its final review correctly requested
  the missing Gradle work; the 60,000-token fix estimate could not fit after
  162,548 tokens had been reported.
- The 38 evidence/accounting assertions passed. Reapplying each saved patch
  to a fresh historical tree and restoring the protected tests reproduced the
  original failure outcome and failing test names for all four modes.

No live interruption occurred in this rerun, so interrupted accounting and
hard-kill durability retain their earlier automated evidence rather than new
live coverage. There were no retained losing-candidate branches: only one
best-of candidate actually ran, and its landed work is in `solution.patch`.

## Remaining failures

The single agent still narrowed a case-sensitive Jest root incorrectly on
Windows and mishandled supported and uncertain Gradle layouts. Tiers preserved
its Gradle work but still failed Gradle edge cases, while Jest was never
implemented. Best-of's Jest patch fell back unnecessarily for an unrelated
unindexed file; Gradle remained at the starting implementation. All four also
failed the `TestSelectJestRoots` explanation-wording assertion, but their
failures were not limited to wording. `TestJestShowConfigCommand` passed in
each run.

Preflight proved the exact commands, not arbitrary shell variants. The Claude
Jest candidate encountered eight permission-denial events involving script or
compound commands, `go vet`, and alternative `go test -run` invocations. It
recovered using the configured package test, which also passed the host-side
verification before merge. `gofmt -l` exited successfully while listing existing
CRLF files; that probe proves command execution, not formatting cleanliness.

Ordinary routing spent 142,288 tokens on preflight, planning and plan review,
then could not admit a 60,000-token worker plus the 40,000-token finishing
reserve. Tiers and best-of got further, but neither could fund the whole task.
The admission gate constrains additional starts; it does not yet choose a plan
that fits all required implementation and finishing work.

## Next work before another paid comparison

1. Budget the complete remaining plan, including required workers and a real
   fix/review cycle. Reduce plan-review/probe overhead and optional candidates
   when that plan cannot fit; do not silently raise the cap or omit required work.
2. Steer agents toward the exact allowed public checks and standalone formatter
   commands. Add narrowly scoped variants only when needed. Keep execution
   permission and formatting correctness as separate checks.
3. Use deterministic regressions for these orchestration decisions, then repeat
   this pilot under a declared policy. Expand to paired repetitions across the
   other tasks only after successful completion is demonstrated. Do not teach
   benchmark agents the protected-test answers.

## Reproduction and evidence

Manifest: [pilot.yaml](pilot.yaml). Configuration:
[realistic-config.yaml](realistic-config.yaml). [Result rows and provenance](2026-10-06-pilot-rerun.json).

- Run: 12:47:55–13:18:48 Europe/Berlin, 6 October 2026; natural CLI exit 0
  means the matrix completed, not that its checks passed.
- Historical task base: `ed2104e2ebb7bf709876343c8c80da58a592efd7`.
  Protected `internal/affected/eco_test.go` came from
  `24ee2b4197df22d4ae05f233beca66ea1341b329` and remained hidden until scoring.
- Fair mode, learning off, one repetition, 20-minute per-mode limit,
  200,000 fresh-token/$10 API-equivalent thresholds, reservations enabled.
- Binary SHA-256:
  `4E2FEDC5C869F2EECF5621DFC4E763574CC8E6D6C5591AB7F6E8ADF1720F3826`.
  Its Go sources and build hash matched the previously tested follow-up build.
- Codex CLI 0.160.0; Claude Code 2.1.288; Windows Go 1.27.0. Both providers were
  signed in. The first doctor invocation incorrectly included unsupported
  `--dir`; the corrected invocation passed. Both records are retained.
- Evidence root: `D:\Entwicklung\Switchyard-pilot-20261006-1249`.
  Native report: `bench-results-20261006-124755-2060716218.json`;
  sibling `run-001`–`run-004` directories live under its filename stem.
  `pilot.log`, the session journal, `analysis.json`, `evidence-audit.json`,
  `replay-audit.json`, replay outputs, exact input copies and hashes are retained.
- This is one task and one mode order on a working desktop. Provider cache/load
  and compiler caches were not controlled. The remaining nine tasks and the
  repeated matrix were not run.
- The pilot left existing source files unchanged, HEAD at
  `48e3e1da6276ca68257ccd2fb1e546df9f8e7e14`, and staging empty. Only the result
  documentation was added/updated afterward. No user-repository commit or PR.
