# What the soft gate's fix round does, replayed — 7 October 2026

**Result: the fix round the independent tests start turned none of 7
caught failures into a pass, and it broke 2 of 3 correct results.** After
one fix round the writer's tests passed in all 10 cases, so the soft gate
never reached its advisory step. Soft and strict ended the same way. The
fix agents bent the code to the writer's tests, whether those tests were
right or not. None of them disputed a test.

The replay of the tests alone
([2026-10-06-independent-tests.md](2026-10-06-independent-tests.md#softer-gate))
scored the soft gate at 0 of 8 correct results failed. That count looked
only at the verdict. It did not look at the code the fix round leaves
behind.

## What was measured

`rw bench --replay-fix <bench-replay folder>` (new) takes the replay of
6 October. For each saved change whose writer tests failed there, it runs
a task from the change:

- The change is the task's one step, put in place after the start
  snapshot, so the review and the fix agent see it as the task's work.
- The same writer's saved tests are the task's independent tests. No
  writer runs again.
- The checks, the final review, the fix round and the re-verification run
  unchanged, under `independent_tests_gate: soft`, with that build's
  defaults: `max_fix_rounds: 1`, `review_when: failing`, fair bench
  settings.
- Then the hidden check scores the result.

Subjects:

- the 7 caught failures (4 affected-module-index, 3 package-manager-paths);
- the 3 known solutions the writer tests failed (affected-module-index,
  azure, bitbucket), each as its full commit, test changes included;
- left out: the 4 failing azure runs (their hidden test needs a
  `maxDiffFile` no run defines, so they cannot pass), and the
  resume-undo-coverage run (its failure was a timeout).

**Scoring.** "Hidden after" scores the code alone. The hidden test files are
set to the solution's, and the package's other test files to the base's.
A test file an agent added is removed. Without this, two results failed to
build: a fix agent had added its own `TestSelectJestIndex`, or test
helpers, next to the hidden file. That says nothing about the behavior
(`rescore_fixreplay.py`).

## Results

| Task | Subject | Hidden before → after | Failing hidden assertions before → after | Writer tests after | Fresh tokens (Codex + Claude) | Wall |
|---|---|---|---:|---|---:|---:|
| affected-module-index | routed run | fail → fail | 12 → 12 (the same) | pass | 36k + 15k | 1m52s |
| affected-module-index | routed-classic run | fail → fail | 13 → 13 (the same) | pass | 59k + 17k | 1m55s |
| affected-module-index | routed run | fail → fail | 12 → 12 (the same) | pass | 31k + 13k | 1m47s |
| affected-module-index | single Codex run | fail → fail | 12 → 12 (the same) | pass | 29k + 14k | 1m54s |
| package-manager-paths | routed run | fail → fail | 24 → 23 | pass | 26k + 15k | 2m10s |
| package-manager-paths | routed-classic run | fail → fail | 4 → 2 | pass | 49k + 19k | 5m26s |
| package-manager-paths | single Codex run | fail → fail | 24 → 22 | pass | 87k + 19k | 7m58s |
| affected-module-index | **known solution** | **pass → fail** | 0 → 1 | pass | 41k + 19k | 3m06s |
| azure-api-edge-cases | **known solution** | **pass → fail** | 0 → 2 | pass | 73k + 28k | 5m04s |
| bitbucket-api-edge-cases | known solution | pass → pass | 0 → 0 | pass | 126k + 78k | 11m42s |

In all: 789k fresh tokens. One light review (Claude Sonnet) and one fresh
fix agent (Codex) per subject.

**Catches: 0 of 7 became a pass.**

- **package-manager-paths.** In all 3 runs, the fix round fixed exactly
  what the writer caught: the two real Scoop layouts the hidden test
  checks now pass. One run also started treating a Scoop `bin` subfolder
  as Scoop, which the hidden test rejects (the live run of 6 October did
  the same). What is left is outside the writer's tests:
  - 20 package-owner queries for non-system paths,
  - winget detection.
- **affected-module-index.** The fix rounds made the Jest
  `modulePaths`/`moduleDirectories` fallbacks the writer asked for. The
  hidden check asserts none of that. All 4 runs fail the same 12–13 hidden
  assertions before and after: the Jest dependency index and multiline
  Gradle includes.

**Correct results: 2 of 3 broken.**

- **affected-module-index solution.** The checks passed. The writer's test
  expected a narrowed Jest run where the solution deliberately falls back.
  The light review turned the failing test into instructions:
  - change the fast path,
  - "restore" `eco_test.go` (the solution's own updated test file).

  The fix agent did both. It also put back the old wording of the
  roots-fallback message, which the hidden `TestSelectJestRoots` checks.
- **azure solution.** The writer's tests asked for more than the solution
  does:
  - the full long description posted somewhere,
  - credentials scoped to Azure hosts,
  - `%2e%2e` rejected.

  The fix agent did all of it. Posting the description as an extra PR
  comment fails the hidden `TestAzurePulls` (an unexpected POST).
- **bitbucket solution.** The fix agent added the stricter behavior the
  writer asked for (Codex at xhigh, routed as auth-sensitive). The hidden
  tests still pass. This was the most expensive subject: 204k tokens,
  11m42s.

## What this means for the gate

- **The harm happens before the gate softens.** Soft makes failing writer
  tests advisory only after their fix round. Every fix round made them
  pass, so there was nothing left to advise on. In practice soft and strict
  behave alike, and both let a wrong test rewrite correct code.
- **Disputes did not happen.** The fix agent may answer
  `DISPUTE: <test>: <why>`. In 10 fix rounds, including 3 against tests
  stricter than the task, none did. The agents conform.
- **The catch does reach the code, but rarely the pass.** Where the writer
  named a requirement the hidden test also checks (Scoop layouts), the fix
  round fixed it every time. The hidden tests check more than any writer
  wrote. So on these two tasks a catch cuts failures but never yields a
  pass, which matches the live run of 6 October (24 → 2).
- **Per subject,** a fix round cost 41k–204k fresh tokens. The 10 rounds
  gained 7 hidden assertions and lost 3 on correct work.

What would change it:

- **No fix round on writer tests alone.** Report failing writer tests
  (advisory from the first round) and start a fix round only when the
  checks fail. On these 10 subjects that keeps all 3 correct results
  correct. It gives up the Scoop fixes (2 assertions on each of 3 runs), and
  no pass was lost.
- **A writer that asserts only what the task states.** The three false
  alarms were stricter than the task. This needs a new replay with writers.
- **A fix prompt that treats the writer tests as claims to check, not
  instructions to meet.** It needs to be measured the same way
  (`--replay-fix`); the current `DISPUTE` wording was never used.

## Limits

- **One repetition per subject, two tasks with catches, three with false
  alarms.** Runs of one task share one writer.
- **The fix agent started fresh.** A live task continues the worker's
  session when it can (fix-session continuation). On 6 October the one
  live catch, with a continued session, went from 24 to 2 failing
  assertions on package-manager-paths. Here the fresh fix of the same task
  went from 24 to 23 and from 24 to 22.
- **The final review ran (`review_when: failing`).** Since 7 October the
  default is `review_when: untested` with `independent_tests` on. With
  checks, no review runs, and the fix agent gets the test output without a
  reviewer's advice. In the affected-module-index solution, the review's
  advice is what told the fix agent to restore the test file. Under the new
  default that round would differ. Not measured.
- **The hidden check is the only judge.** A fix that adds a stricter but
  sensible behavior (azure's credential scoping) counts as a break when the
  reference test pins the old behavior.

## Reproduction

- **Binaries:** in `D:\Entwicklung\rw-phase3-20261006`, built from a snapshot
  of this checkout (uncommitted, on top of `48e3e1d`):
  - `rw-fixreplay.exe` (SHA-256
    `f8925c78abc451dc7d2a44b35fb60d1002a623c0bad1821c2f74c48770e2eca8`): the
    affected-module-index runs;
  - `rw-fixreplay2.exe` (SHA-256
    `e0cff5cfa8e47eb6cc6f6462a3cd9cb81f7a950f5b2c610baabe0dc492a9a43e`): it
    replays solutions as their full commit, and ran the rest.
- **Commands,** from `relayweft/`:

  ```sh
  ../rw-fixreplay.exe bench --file ../levers.yaml --replay-fix bench-replay-20261006-202735 \
    --replay-subjects "affected-module-index/,package-manager-paths/,azure-api-edge-cases/solution,bitbucket-api-edge-cases/solution" --yes
  ../rw-fixreplay2.exe bench --file ../levers.yaml --replay-fix bench-replay-20261006-202735 \
    --replay-subjects "package-manager-paths/,affected-module-index/solution,azure-api-edge-cases/solution,bitbucket-api-edge-cases/solution" --yes
  ```

  The first run was stopped after its fifth subject. Its solution row
  replayed the solution without its own test changes, so the old tests
  failed the checks; that row is not counted.
- **Evidence:**
  - Results: `relayweft/bench-fixreplay-20261007-054151` (runs 002–005) and
    `relayweft/bench-fixreplay-20261007-055346`. Each run folder has:
    - `events.jsonl`;
    - `solution.patch` (the change plus the fix);
    - `check-before.txt`, `check.txt` and `check-rescored.txt`;
    - `req-after.txt`.
  - Logs: `fixreplay-run.log` and `fixreplay-run2.log`.
  - Rescore: `python rescore_fixreplay.py <folder>` writes `rescore.json`.
- **Run:** 05:41–06:30 Europe/Berlin, Windows 11, Go 1.27.0.
