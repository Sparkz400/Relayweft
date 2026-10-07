# Tests first, live — 6 October 2026

**Result: tests written first did not turn any failing task into a pass.
Routed rw with `tests_first` failed the same three tasks routed rw fails,
and passed the one it passes. `orchestrator.tests_first` stays off by
default.** In every run the work passed the tests written first and the
configured checks, then failed the hidden scoring test. The tests written
first missed exactly the requirement the hidden test checks.

## Decision rule (set before the run)

Tests first becomes the default only if all three hold:

1. It passes at least one task that routed rw always fails today
   (`affected-module-index`, `package-manager-paths`,
   `azure-api-edge-cases`).
2. It does not break `resume-undo-coverage`, which routed rw passes.
3. It uses at most about 1.5x routed rw's fresh tokens.

Condition 1 failed, so the default stays off.

## Setup

- **Tasks and checks:** the 4 tasks above from `levers.yaml`, with the same
  public checks as `verify.commands`. Hidden scoring tests were never given
  to agents.
- **Mode:** `routed-tests-first`, repeat 1. The writer used the worker route
  on the other provider (`claude:sonnet@medium`); Codex implemented.
- **Build:** this checkout, uncommitted, on top of `48e3e1d`. It has the new
  defaults `review_when: failing` and `verify.auto`, so every run skipped
  the final review once its checks passed.
- **Budget:** `task_tokens` raised from 200k (the levers run) to 300k, to
  leave room for the test writer. No run came near it.
- **Baselines:**
  - today's earlier routed runs (Phase 3 evidence);
  - the live-tests bench that ran just before this one (`routed` and
    `routed-tests`, same build family, same tasks).

## Results

| Task | Routed today | Tests first | Tests written | Fresh tokens: routed → tests first | Wall |
|---|---|---|---|---|---|
| affected-module-index | 0/5 | fail | 5, fail before, pass after | ~135k → 176k (1.3x) | 7m |
| package-manager-paths | 0/2 (43k, 100k) | fail | 3, fail before, pass after | 43k–100k → 70k | 4.5m |
| resume-undo-coverage | 2/2 (~81k) | **pass** | 3, fail before, pass after | 81k → 119k (1.5x) | 12m |
| azure-api-edge-cases | 0/1 (163k) | fail | 7, fail before, pass after | 163k → 186k (1.1x) | 7m |

What the hidden tests caught:

- **affected-module-index.** It failed `TestSelectJestIndex`,
  `TestSelectJestRoots` and `TestSelectGradleIncludeLines`. The writer
  tested commented Gradle includes and Jest ignore patterns. It did not test
  "a file outside Jest's roots runs the full check" or block-style Gradle
  includes. The work passed the writer's 5 tests.
- **package-manager-paths.** It failed `TestPackageManagerByOwner`, on a
  multi-owner query. The writer tested owner-query parsing, but not several
  owners.
- **azure-api-edge-cases.** The hidden test does not build:
  `undefined: maxDiffFile`. It expects an internal name the task never
  states. This says nothing about the work's behavior. Every earlier azure
  run, in every mode, fails the same way (4 of 4), so this task cannot
  score behavior.

## What this shows

- **Tests first did not change a verdict.** Tests written from the task
  text covered the requirements the writer read into it, and the
  implementer met those. The hidden tests check readings the writer did not
  pick. Replay gave the same picture for the independent writer
  (`2026-10-06-independent-tests.md`): about half the catches depend on one
  writer's reading.
- **It costs 1.1x to 1.5x routed rw's tokens.** The writer takes 34k to
  111k tokens, and the worker gets no cheaper.
- **The writer often works blind.** (Correction, 7 October: the event logs
  show refusals in all 4 runs, and in 3 of 4 the writer never ran a test;
  azure's got through because its narrowed command happened to start with
  the exact check.) In 2 of 4 runs the writer's `go test`
  and `gofmt` calls were refused for approval
  (`providers.claude.write_allowed_tools` allows only `gofmt`). It could
  not see its own tests fail. rw's red run caught that the tests compile and
  fail, but a writer that can run the check could check its tests against
  the task.
- **The checks-decide defaults held up.** `review_when: failing` skipped
  the review in all 4 runs because the checks passed. On these tasks the
  review never changed a verdict (Phase 3), so this saved its tokens without
  losing a catch. `verify.auto` did not apply: every task had checks.

## Next

- Let the test writer run the configured checks (the check commands in its
  allowed tools), then measure again. Done on 7 October: the writer may run
  the test runner each check starts with (`go test`) with any arguments.
  The refused calls were the checks narrowed to the new tests
  (`go test -count=1 -run "A|B" ./pkg`), which no exact-command rule
  matches; Claude Code 2.1.288 allows them under `go test *`, also piped to
  `tail` or `Select-Object`. A `cd pkg && go test` in PowerShell is still
  refused, so the prompt says to run from the project folder. Not yet
  measured live.
- Measure tests first on tasks whose hidden tests check behavior the task
  states explicitly. Today's corpus partly scores internal names
  (azure) and readings the task leaves open (affected).
- Small sample: 4 tasks, 1 run each. A pass on one more task would not have
  been significant either.

## Reproduction

- **Binary:** `rw-checks.exe` in `D:\Entwicklung\rw-phase3-20261006`,
  SHA-256 `c235c053f21a6d2f33b9fb22ffee30d9000f3b5d6bd5409ce73b809d8f6ff007`.
- **Files:** `testsfirst.yaml` (levers.yaml with the mode
  `routed-tests-first`), `testsfirst-config.yaml`, `run-testsfirst.sh` and
  `testsfirst-tasks.txt`. The clone is `relayweft-tf`, kept separate from
  the live-tests bench's clone.
- **Logs:** `testsfirst-logs/` and `testsfirst-driver.log`. Results are in
  `relayweft-tf/bench-results-20261006-21*` and `…-22*` (`check.txt` holds
  the hidden test output).
- **Run:** 21:43–22:14 Europe/Berlin, after the live-tests bench. Codex
  implemented, Claude Sonnet wrote the tests.
