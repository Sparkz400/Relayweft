# Benchmark follow-up — 6 October 2026

Implemented the three follow-ups from the four-mode pilot. The implementation
pass made no commit, PR, installation or additional model benchmark. The later
[authorized rerun](2026-10-06-pilot-rerun.md) also scored 0/4; neither pilot
establishes a performance or quality benefit.

## Durable evidence

- JSON and Markdown checkpoints are created before the first agent, refreshed
  before each run, before its scoring check, and after each completed/failed run.
  JSON uses a synced sibling-file replacement; write errors stop the matrix.
- Per-run artifacts retain full scoring output, setup output, event JSONL, binary
  solution patches and every kept candidate branch's patch/index. Patches are
  captured before protected tests are restored and before the workspace resets.
- Running/checking rows distinguish an interrupted process from a finished
  score. Timeout rows retain reported cost and unfinished work. Evidence stays
  outside the disposable workspace. Hard termination can still leave the
  current run unfinished; earlier checkpoints remain readable.

## Permission preflight

Optional `verify.preflight` runs short commands through each enabled Claude
provider before planning/implementation. The probe gets shell tools and explicit
command rules with `dontAsk`; success requires corresponding successful tool
results, not a model's final claim. Denials, command failures, missing results
and the two-minute timeout stop implementation. Probes consume quota, which is
included in task accounting. Normal tasks, single-agent runs and follow-ups use
the path. Single-agent writers now receive configured command permissions too.

The first pilot task declares public `agent_checks` (existing affected-package
tests and `gofmt -l`), separate from its hidden scoring command. They are also
the public verification commands. The pilot configuration grants scoped
formatter rules and enables budget reservations. Other benchmark tasks can add
their own public checks. Preflight and reservations remain opt-in globally.

The public commands passed locally on the historical pilot base without model
quota. Scripted CLI tests cover permission behavior. Live Claude execution of
the new preflight remains part of the next pilot. A passing probe establishes
those commands at that time; it does not authorize or prove every later command.

## Budget admission and incomplete usage

`budget.reserve: true` reserves each running agent's historical median estimate
(documented fallback estimates when history is insufficient). Benchmark runs
use fixed defaults so earlier modes cannot change later modes' estimates.
Preflight and incomplete records cannot train routing. Admission and conversion
to actual usage share a lock. Parallel agents wait for accounting
when combined estimates cannot fit. Routed tasks with final review retain 20%
of each enabled limit for reviews/fixes. An extra best-of candidate that cannot
fit is explicitly skipped without cancelling the candidate already completed.

Interrupted Claude streams retain per-message token usage, deduplicated by
message ID. Final aggregate usage overrides those partial readings. Missing
final accounting is marked `incomplete`; cost summaries say “at least” and
history estimates exclude incomplete samples. Unreported portions of an agent's
reservation stay held for that task, without being presented as actual spend.

These are soft estimates, not provider-enforced token caps. One active agent
can exceed its reservation, and 20% may not cover a whole fix/review cycle.
Reservations coordinate agents within one task, not simultaneous rw processes
or machines. This pass did not raise the pilot's 200k-token/$10 thresholds.

## Validation

Evidence directory: `D:\Entwicklung\Switchyard-validation-20261006-followup`.
The 716-file pre-change baseline preserves the earlier uncommitted batch.

- Reproduced missing intermediate reports and missing interrupted Claude usage
  with failing regressions before their fixes.
- Focused regressions pass for readable checkpoints between runs, timeout
  evidence, untracked/binary/candidate patches, unchanged staging/HEAD,
  permission proof versus claims, Windows shell rules, preflight cost, concurrent
  reservation accounting/cancellation, and landing a budget-limited best-of
  winner.
- Config/runner suites and focused trust/parser checks pass; affected-package
  tests and formatter probes pass on the pilot's historical base.
- The broader run caught a budget-status/approval lock wait introduced in this
  pass. A failing regression reproduced it; a nonblocking status snapshot fixed
  it, and the regression plus browser budget-approval flow pass.
- Windows vet and build pass, Darwin/arm64 cross-vet passes, and the Claude
  parser completed a 30-second fuzz run (659,077 executions) without a failure.
- Final full Windows suite passes: `go test -p 4 ./... -count=1 -timeout 20m`
  with Go 1.27.0 (orchestration package 606.876s; web package 7.862s).
- Final full Linux race suite passes in the non-root validation container:
  `go test -race -p 2 -count=1 -timeout 20m ./...` with Go 1.26.8
  (orchestration package 75.896s; web package 10.123s). No Go source changed
  after these final suites started.
- The built CLI validates `pilot.yaml` without agents: its known solution
  passes and its historical base fails with the protected tests.
- HEAD remains `48e3e1da6276ca68257ccd2fb1e546df9f8e7e14`; staging is empty.
  The earlier source files are preserved in the baseline. `source-audit.json`
  and `followup-only.diff` isolate this pass from the earlier uncommitted work.

The subsequent [four-mode rerun](2026-10-06-pilot-rerun.md) is now complete:
0/4 protected checks passed. Evidence, successful exact-command preflights and
budget admission were verified live, but the routed modes still could not fund
all required work. Resolve those findings before expanding. Native macOS and
sustained daily-use acceptance remain separate from these automated checks.

The checked-in [pilot.yaml](pilot.yaml) fixes that scope to exactly four runs:

```sh
rw bench --config docs/bench/realistic-config.yaml --file docs/bench/pilot.yaml
```

The rerun used this manifest with `--yes`, the tested binary, and isolated
history/config/cache paths. Without `--yes`, it prompts before consuming quota.
