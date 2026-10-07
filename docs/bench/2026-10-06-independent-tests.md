# Independent test writer, replayed on saved runs — 6 October 2026

**Result: tests written from the task text alone catch failures the final
review approved. A test the known solution passes failed 4 of the 7 runs
whose last final review approved work that fails the hidden check. They
never failed a passing run (0 of 3), but they failed the known solution in 3
of 5 tasks.** As a hard gate, the feature would have flagged 5 of those 7
approved failures, and 3 of the 8 correct results. It stays off by default
(`orchestrator.independent_tests: false`). A live run followed: on
package-manager-paths the independent tests caught a change the checks
passed, and one fix round cut the hidden failures from 24 to 2. That run
still fails. See [What this shows](#what-this-shows-and-what-it-does-not) and
[Live run](#live-run-routed-against-routed-tests). When it is on, failing
writer tests now get one fix round and then only advise
([Softer gate](#softer-gate)).

## What was built

`orchestrator.independent_tests` (see [docs/plan.md](../plan.md)):

- While the worker works, a test writer runs next to it. It uses the worker
  route on the other provider (here Claude Sonnet at medium effort, since the
  worker is Codex). It works in a pool worktree at the task's start, so it
  cannot see the change.
- Its prompt has the task text, the repo docs and the checks. It asks for one
  test per requirement the task names, through names the task gives or the
  repo already has.
- rw keeps only new files with `rwreq` in the name. It runs the writer's
  `COMMAND:` only if that command is a configured check, or a narrowed check
  prefix without shell syntax.
- After the checks, rw puts the files in the tree, runs them and removes
  them. A failure fails the round like a check. The fix agent gets the output
  and the writer's requirement list.

`rw bench --replay-tests` measures it without running any worker again. It
runs one writer per task at the task's base, then runs that writer's tests
against:

- every saved run's `solution.patch`,
- the known solution (`tests.from`),
- the base.

## Setup

- **Runs:** the 22 completed runs with a saved patch from today's Phase 3
  evidence (`D:\Entwicklung\rw-phase3-20261006`, all 12
  `bench-results-*.json`). They cover 5 tasks: 19 failing runs and 3 passing
  ones. Every passing run is from `resume-undo-coverage`.
- **Checks:** each task's public checks from `levers.yaml` were used as
  `verify.commands`. None of them is a hidden scoring test.
- **Writers:** one per task, `claude:sonnet@medium`, on the same build that
  has the feature.
- **Review verdicts:** taken from each run's `events.jsonl`.

## Results

Two ways to count a failure:

- **Raw:** the independent tests fail at all. This is what the feature acts
  on in a live task.
- **Beyond the solution:** the run fails a test that the known solution
  passes. Only these failures say the run is worse than the solution.

| Task | Failing runs | Raw | Beyond the solution | Approved by the last final review | Of those caught (beyond the solution) | Solution fails |
|---|---:|---:|---:|---:|---:|---|
| affected-module-index | 9 | 4 | 4 | 4 | 2 | 1 of 5 tests |
| package-manager-paths | 3 | 3 | 3 | 2 | 2 | none |
| resume-undo-coverage | 2 | 0 | 0 | 0 | - | none |
| azure-api-edge-cases | 4 | 4 | 0 | 1 | 0 (raw: 1) | 5 tests |
| bitbucket-api-edge-cases | 1 | 0 | 0 | 0 | - | 2 tests |
| **Total** | **19** | **11** | **7** | **7** | **4 (raw: 5)** | **3 of 5 tasks** |

- **Correct work:** the 3 passing runs pass every independent test. The known
  solution fails at least one in 3 of 5 tasks. The base fails them in all
  5 tasks, as it should.
- **Writer cost:** 364k fresh tokens in all, $2.81 API-equivalent. That is 29k
  to 115k per task, 47s to 3m per writer. The writer runs next to a worker
  that takes 5 to 15 minutes, so it adds little wall time.

What the catches were:

- **package-manager-paths.** None of the 3 runs recognizes Scoop in its real
  layouts. `C:\Users\me\scoop\apps\sy\0.2.0\sy.exe` is not detected as
  Scoop-managed. Two of these runs were approved by the final review, one of
  them by the full-strength Opus review (routed-classic). Their public checks
  passed.
- **affected-module-index.** The task says "cover custom Jest index settings".
  The writer turned that into "fall back to the full check when
  `modulePaths`/`moduleDirectories` point outside the roots". 4 runs still
  narrow to `--findRelatedTests` there; two of them were review-approved. The
  hidden test for this requirement is `TestSelectJestIndex`.

What the false alarms were:

- **affected-module-index (solution).** The writer's fake Jest config left out
  `moduleFileExtensions`. The solution treats that as uncertain and runs the
  full check, but the writer's fast-path test expected a narrowed run. The
  writer guessed a detail the task does not settle.
- **azure and bitbucket (solution).** Some failing tests ask for more than the
  solution does: no credentials on a redirect to another host, rejecting
  `%2e%2e` in work-item URLs. They may be real gaps in the solution, but they
  cannot separate runs from the solution. Every failing Azure run failed only
  tests the solution fails too.

## What this shows, and what it does not

- **It catches what reviews miss.** On these tasks, prose reviews approved 7
  failing runs. Tests written blind from the same task text caught 4 of
  those 7 with a test the solution passes. Reviews never caught one: the only
  rejection was later followed by an approval. This is the first rw
  mechanism that changed the verdict on a failing run.
- **It fails correct work too often for a hard gate.** 3 of 8 correct results
  (the solutions of 3 tasks) fail at least one writer test. In a live task,
  that means a fix round that chases a wrong test, or a task reported as
  failed. The two API-hardening tasks (azure, bitbucket) produced most of
  these: the writer asserted stricter security behavior than the reference.
- **Not measured here:** whether the fix round turns a caught failure into
  a pass. It was measured on 7 October with `rw bench --replay-fix`: 0 of 7
  caught failures became a pass, and the fix round broke 2 of 3 correct
  results ([2026-10-07-soft-gate-fix-round.md](2026-10-07-soft-gate-fix-round.md)).
- **Small sample.** 5 tasks, one writer each, 3 passing runs, all from one
  task. A second writer per task (`--writers 2`) would show how much the
  catches depend on one writer's reading of the task.
- **One run is scored by hand.** The raw failure of resume-undo-coverage
  run-001 (single high) came from a 300-second package timeout in an existing
  test. The full `cmd/rw` test suite was running at the same time. Rerun
  alone, the writer's tests pass on that run, so it counts as a miss.

## Live run: routed against routed-tests

The run used the two tasks where the replay caught failures, one repetition
per mode, on one binary. The tasks and checks came from `levers.yaml`
(`live-tests.yaml`). This build already has another session's
`review_when: failing`: the final review runs only when a check fails.

| Task | Mode | Hidden | Failing hidden assertions | Wall | Fresh tokens (Codex + Claude) | What happened |
|---|---|---|---:|---:|---:|---|
| affected-module-index | routed | Fail | 3 tests | 8m08s | 93k + 0 | Checks pass, no review |
| affected-module-index | routed-tests | Fail | 3 tests (the same) | 6m52s | 61k + 53k | Independent tests pass on the first try, no fix round |
| package-manager-paths | routed-tests | Fail | **2** | 6m10s | 113k + 101k | Independent tests fail (checks pass); light review names the cause; one fix round continues the worker's session; independent tests pass |
| package-manager-paths | routed | Fail | 24 | 3m50s | 43k + 0 | Checks pass, no review |

- **package-manager-paths: caught and mostly fixed.** The fix round fixed
  real Scoop layouts and stopped package queries for non-system paths. Both
  are in the hidden test, and every earlier run of this task failed them
  (24, 4 and 24 failing assertions). Two failures are left: winget detection
  (the task only says "other package-manager directory matching") and a
  Scoop `bin` subfolder that is now matched.
  - The routed-tests run is still a fail, but the closest any run of this
    task has come.
  - It is also the first live run of fix-session continuation: the fix
    continued the worker's Codex session.
- **affected-module-index: nothing to catch.** Both workers failed the same
  3 hidden tests. The replay writer's tests (which caught 4 runs this
  morning) pass on both of today's changes too: today's workers handled
  `modulePaths`/`moduleDirectories`. They missed a different detail,
  imports outside Jest's dependency index, which no writer test covered.
- **Cost:** routed-tests took 174k Codex + 154k Claude fresh tokens for the
  two tasks, against 136k + 0 for routed: about $0.95 more, API-equivalent.
  Wall time rose 32 seconds on average, since the writer runs next to the
  worker.

The fix round can turn a catch into most of a fix. It did not reach a pass
on these tasks: both still fail hidden assertions that no requirement test
covered. One repetition per mode.

## Softer gate

**Result: `orchestrator.independent_tests_gate: soft`, now the default.
Failing writer tests start one fix round, then they are advisory. On the
replay that keeps a fix round on all 7 catches and fails none of the 8
correct results (strict: 3). The cost is that a catch the fix round does not
fix no longer fails the task. The evidence is thin: the catches come from 2
tasks and the false alarms from 3.** `strict` keeps the old behavior.

What soft does in a task:

- The writer tests fail a round only once, and only when a fix round can
  follow (`max_fix_rounds`). The fix agent gets their output, as before.
- After that fix round, rw still runs the tests in every round. Failures
  are reported in the summary ("independent tests still fail, advisory"), in
  the final review's input, and as a choice record. They do not fail the
  round.
- The fix agent may answer `DISPUTE: <test>: <why>` for a test that asks
  for more than the task. rw logs each dispute as a choice record. It changes
  no outcome; it is the reason `rw explain` shows next to the advisory test:
  `independent tests advisory (round 2): TestRwReqX still fail after their
  fix round; …; the fix agent disputed TestRwReqX: …`.

### Scoring the candidates offline

The gate can only use what rw has at runtime: the base tree, the result, the
task text, the tests and the round. The known solution is used only to
score. Every row is scored on the first round, which is all a replay shows.

- **Catches:** the 7 failing runs that fail a writer test the known solution
  passes. The last final review approved 4 of them.
- **Correct results:** 5 known solutions and 3 runs that pass the hidden
  check.
- **Can fail the task:** what happens if the fix round does not change the
  result. A replay cannot show what a fix round does.

| Policy | Fix round on a catch (of 7 / of 4 approved) | Can fail the task on a catch (of 7 / of 4) | Fix round on a correct result (of 8) | Can fail a correct result (of 8) |
|---|---:|---:|---:|---:|
| strict (before) | 7 / 4 | 7 / 4 | 3 | 3 |
| (a) only tests that passed on the base gate | 3 / 2 | 3 / 2 | 1 | 1 |
| only tests that failed on the base gate | 4 / 2 | 4 / 2 | 2 | 2 |
| **(b) soft: one fix round, then advisory** | **7 / 4** | **0 / 0** | **3** | **0** |
| (a)+(b): soft, but base-passing tests stay hard | 7 / 4 | 3 / 2 | 3 | 1 |
| (c) disputes | unmeasured: needs the fix agent's answer | | | |
| (d) build failures handled apart | no data: 0 of 32 test runs had one | | | |

Why soft, and why not the others:

- **The base tree does not separate catches from false alarms here.** A
  test that passes on the base and fails on the result looks like the
  strongest signal: the change broke something that worked. It was true for
  package-manager-paths: `TestRwReqUpdateScoopRealLayouts` passed on the
  base and failed on all 3 runs, which broke real Scoop layouts. It was also
  true for the affected-module-index solution, a false alarm:
  `TestRwReqJestFastPathKept` passed on the base, and the solution
  deliberately changed that path. The affected-module-index catches fail on
  the base too, like the azure and bitbucket false alarms. By task, each
  base rule keeps 1 catch task and 1 or 2 false-alarm tasks. Choosing one
  would fit two tasks, not a principle.
- **Soft keeps what the feature is for.** The catch is worth something only
  if it reaches a fix agent. Soft sends every failure to one, as strict
  does. What it gives up is failing the task on a catch the fix round did not
  fix. In the one live catch (package-manager-paths, routed-tests, above),
  the independent tests passed after that fix round. Soft and strict would
  have ended that run the same way.
- **Disputes are recorded, not trusted.** A fix agent has every reason to
  dispute a test it cannot pass. The affected-module-index catch is a
  writer's reading of "cover custom Jest index settings", and an implementer
  could call that stricter than the task. So a dispute changes no outcome.
  Under soft, everything after the fix round is advisory anyway. Letting a
  dispute end the gate before the fix round would need another agent call.
- **Build failures are not treated apart.** None of the 32 test runs had
  one. All writer failures were named test failures. The one other failure
  was the hand-scored timeout in an existing test. Soft gives that one fix
  round at most.

What soft does not fix:

- A false alarm still costs a fix round: 3 of 8 correct results would get
  one. That fix round may also change correct code to meet a stricter test.
  The azure and bitbucket tests ask for stricter security, which is probably
  harmless. The affected-module-index test asks for a different Jest fast
  path, which is not. **Measured on 7 October:** the fix round broke the
  affected-module-index and azure solutions; bitbucket's survived. Every
  fix round made the writer tests pass, so soft never got to advise
  ([2026-10-07-soft-gate-fix-round.md](2026-10-07-soft-gate-fix-round.md)).
- The summary of a task with an uncaught catch now says "advisory", not
  "failed". A user who reads only the pass/fail state misses it.
- **Thin evidence.** 5 tasks, one writer each, 3 passing runs from one task.
  Runs of one task share one writer, so they are not independent. The
  numbers above amount to 2 tasks with catches and 3 with false alarms.
  Whether the fix round turns catches into passes needs live `routed-tests`
  runs under each gate.

Scored with `score_gates.py` in the evidence folder, which reads the same
`replay.json` and review verdicts as `analyze_replay.py`.

## Next

1. More live repetitions on tasks some mode can pass, so a pass rate can
   differ at all. No mode has passed either of these tasks in any run.
2. Reduce false alarms before turning it on by default. The gate is now soft
   ([Softer gate](#softer-gate)). Still open: tell the writer not to assert
   behavior stricter than the task states (needs a new replay with writers).
3. If more live runs confirm it, run the writer instead of the final review. The
   review cost 11k-62k tokens per run and caught nothing. The writer cost
   29k-115k and caught 4 of 7 approved failures.
   **Done on 7 October, ahead of those live runs:** `independent_tests` is on
   and `review_when: untested` is the default, so the final review runs only
   on a task without checks. Mode `routed-review` keeps the old default for
   the comparison.

## Reproduction

- **Binary:** `rw-reqtests.exe` in the evidence folder, SHA-256
  `3a1ba761681bc7e3e1bc6ac670c01e8a769c22db25416fa793eb3b589ff693fc`. It was
  built from this checkout, uncommitted, on top of `48e3e1d`. That binary's
  replay leaves the final review column empty (`event.Kind` does not read
  back from JSON). This is fixed in the checkout; the table above takes the
  verdicts from `analyze_replay.py`.
- **Command,** from `D:\Entwicklung\rw-phase3-20261006\relayweft`:

  ```sh
  ../rw-reqtests.exe bench --file ../levers.yaml --replay-tests <the 12 bench-results-*.json, comma-separated> --writers 1 --yes
  ```

- **Evidence:** `relayweft/bench-replay-20261006-202735` has `replay.json`,
  `replay.md`, `analysis.md` and the written tests and requirement lists per
  task. The log is `replay-run.log`; the analysis is `analyze_replay.py`;
  the gate scoring is `score_gates.py` (`python score_gates.py` from the
  evidence folder; no agents run).
- **Live run:** `rw-live-tests.exe`, SHA-256
  `323b301cf5f4d48f1f209b1bbb4e7e32afd7cfaac170cb047facb51a65669cb1`, from the
  same checkout at 21:17. The command was
  `../rw-live-tests.exe bench --file ../live-tests.yaml --yes`. Results are in
  `relayweft/bench-results-20261006-211813-1781088570`; the log is
  `live-tests-run.log`. The run took place 21:18–21:47. The live writers'
  tests were not kept: that build deleted them with the pool worktree. rw now
  saves a copy under its cache (`reqtests/`) and logs the path.
- **Run:** 20:27–21:01 Europe/Berlin. Claude Code 2.1.288 (Max), Windows 11,
  Go 1.27.0.
