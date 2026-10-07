# Batch review, reliability and benefit checks — 6 October 2026

This follow-up reviews the local batch after `48e3e1d`. No project commit,
pull request, push, release or installation was made. The original working
tree was copied with SHA-256 hashes before this pass; validation repositories,
provider profiles and browser data are isolated outside the source checkout.

Local evidence root: `D:\Entwicklung\Switchyard-validation-20261006-094055`.
The `baseline/` directory and `baseline-manifest.json` preserve the starting
files; `logs/`, `artifacts/`, `browser/` and `live-*-fixed/` hold the checks.
This record supersedes the pending checks in the earlier
[implementation record](2026-10-06-expansion-verification.md) where stated.

## Defects fixed during this review

1. An undone interrupted task still offered Resume. It could skip successful
   steps whose files had already been reverted and report success with work
   missing. Recovery now marks retained undone tasks as undone, and the engine
   refuses resume, including forced resume, until redo restores the changes.
2. Recovery's undo preview listed unreported user files among the changes even
   though the action kept them. It now separates kept files and shows later-edit,
   submodule and missing-repository warnings.
3. A hard kill lost file attribution reported before an agent returned. The
   resumed task finished, but agent-only undo retained the first half of its
   work. Streamed edit reports are now saved with task state and restored before
   resuming. Only paths inside the task's repositories are attributed; pooled
   work continues to use its merge diff.

Each fix has a regression test observed failing before the fix and passing
afterwards. The first two were found by source/controller review; the third
was reproduced with **both real Codex and Claude Code** before being fixed.

The real benchmark also exposed a misleading merge message: an agent using
shell writes supplied no structured file reports, so successful Git merges
could print "merged 0 files". The count now uses the merged Git diff. This
changes the message only; the files were already merged and attributed.
After this message correction, the existing worktree merge flow passed again
on Windows (4.534 seconds), orchestrator vet passed, and a fresh Windows
executable built successfully. The timed pilot used the earlier executable;
its merge-count wording has no effect on its check results.

## Real provider recovery

Codex CLI **0.160.0** and Claude Code **2.1.288** each ran a disposable task
with one writing step. The test killed `rw` after the first file was written,
continued the saved provider session, and finished the second file. Both then
passed agent-only undo and redo:

| Check | Codex | Claude |
|---|---|---|
| Same interrupted session resumed | Pass | Pass |
| Both agent files removed by undo | Pass | Pass |
| Both files restored by redo | Pass | Pass |
| Concurrent user edit preserved | Pass | Pass |
| Original branch HEAD and index unchanged | Pass | Pass |
| No surviving owned child process after hard kill | Pass | Pass |

The first Codex probe used PowerShell to write the files. Its command events
carried no file-change reports, so agent-only undo correctly refused to touch
any of them. The final tests used structured edit tools. This is a real
limitation: a prose list of paths or an arbitrary shell command cannot safely
establish ownership of concurrent filesystem changes.

A signed-out Codex stream was also recorded using an empty `CODEX_HOME` and
no API-key environment variables in that subprocess. The user's regular login
was untouched. Its WebSocket and HTTPS 401 failures now have a checked-in
fixture and process-level replay test, verifying failure, no invented edits or
usage, and authentication-unavailable classification. A real quota-limit hit
was not forced.

The machine's ordinary `rw health --days 14 --json` report was also checked:
three days with use, approximately 1.7 clean days of available records, zero
recorded crashes or hangs, nine unclean exits and no detected leftovers. An
unclean exit does not establish its cause. The required 14-day/10-use-day
criterion is **not met**; today's isolated tests do not turn that into a pass.

## Browser checks

The connected Chrome extension blocked loopback with `ERR_BLOCKED_BY_CLIENT`.
The user selected headless testing. Playwright 1.55.0 and its installed Chromium
ran the actual locally built server, without mocked API responses.

- Recovery renders interrupted progress, preserved branches and Resume.
- A real resumed Codex task previews its two agent files separately from the
  kept user file. Browser undo removes both agent files; the view then shows
  undone and hides resume/undo actions. CLI redo restores the files.
- Memory add/edit survives reload. A stale browser edit is rejected after a
  concurrent CLI note, preserving the other client's change. Remove deletes
  only the selected disposable note.
- Screenshots were inspected at 1440×1000 in light and dark themes. The Memory
  drawer also fits 390×844. The surrounding dashboard still has its existing
  880px desktop minimum width, so this is not full mobile-layout acceptance.
- Both browser flows finished with zero JavaScript page errors.

Screenshots and executable test scripts are in the evidence root's
`artifacts/` and `browser/` directories. This is headless browser evidence,
not human acceptance of every screen or a native `rw app` check.

## Automated and live verification

- Full Windows `go test -p 4 ./... -count=1 -timeout 20m`: pass.
- Full Linux `go test -race -p 2 ./... -count=1 -timeout 30m`: pass in a
  non-root Go 1.26 container with Node and Git identity configured. The initial
  incomplete image failed on missing Node/Git identity; that setup failure was
  corrected before the full rerun.
- After the streamed-provenance fix, the complete Linux orchestrator, runner
  and web packages passed again with the race detector. All three packages
  also passed again on Windows (orchestrator: 710.407 seconds).
- Final `go vet ./...` on Windows and Linux, plus Darwin/arm64 cross-vet: pass.
- Final golangci-lint v2.14.0: **0 issues**.
- Historical benchmark validation: **10/10** known solutions pass and their
  starting versions fail the protected checks.
- Live v0.4.0 updater provenance verification: pass, including rejection of
  tampered bytes. The check did not replace any installed executable.

Both 30-minute stress runs passed. Linux covered 2,480 demo tasks and 2,047
Git-backed tasks, with 409 mid-task cancellations. Windows covered 3,225 demo
tasks and 270 Git-backed tasks, with 54 cancellations. The long runs were built
before the streamed-provenance fix; the final code additionally passed
three-minute stress runs on Windows and Linux. Those covered 21 and 320
Git-backed tasks, with four and 64 cancellations respectively. All runs passed
their goroutine, heap, handle, worktree and leftover-process assertions. The
Git phase kept at most three pool slots/registered worktrees. These are
scripted-agent tests, not 30 minutes of real-model use.

The Windows long-run command omitted `-count=1`, following the old local
instructions. After printing `PASS` and the package's `ok` result, Go 1.27
continued processing a 336,879,705-byte test-cache input log (6,745,027 stat
records). Only that post-test Go process was stopped, after its test child had
exited and more than 80 million filesystem operations had been observed.
Consequently the outer command's exit is nonzero; the test body and all its
assertions passed. The final three-minute Windows run used `-count=1` and exited
normally. Local instructions now match CI, which already disables test caching.
The raw output and this distinction are retained in `windows-stress-cache-stop.json`.

Windows race detection was not run (this Go environment has CGO disabled and
no C compiler); Linux supplies the executed race evidence. Native macOS tests
were not run.

## Benefit experiment

A comparison started with `realistic.yaml` and `repeat` changed to 1 after the
reliability checks. While its first case was running, the user selected a
**one-task, four-mode pilot** because the original 40-run screening matrix
would take several hours. The pilot uses `affected-module-index`, one attempt
per mode and the same 20-minute limit per attempt. The other nine tasks and
the supplied three-repetition, 120-run matrix remain unrun.

**The four-mode pilot finished. None of the four attempts passed the protected
check.** These are attempts to solve a historical task, separate from the
passing tests of the current checkout.

| Mode | Protected check | Pipeline | Wall time | Codex fresh tokens | Claude fresh tokens |
|---|---|---|---:|---:|---:|
| Single Codex, high | Fail | Reported completion | 19m 08s | 146,448 | 0 |
| Routed | Fail | Budget stopped fix round | 8m 25s | 167,196 | 66,308 |
| Routed tiers | Fail | Budget stopped final review | 13m 21s | 196,074 | 18,407 |
| Routed best-of-two | Fail | Budget stopped candidate selection | 8m 28s | 119,667 | **at least** 134,384 |

The four recorded wall times total **49m 22s**. The ordinary routed attempt
finished sooner than the single-agent attempt, but it finished unsuccessfully;
that is not evidence of faster successful delivery. Recorded total fresh
tokens were 146,448, 233,504, 214,481 and **at least** 254,051 respectively.
One cancelled Claude Jest candidate returned no usage, so the best-of total
understates its actual consumption. The queued Codex Jest candidate never
started before the budget stop. No provider quota-limit hit was recorded.

The configured 200,000-token budget is checked when agents report usage, so
in-flight agents can overshoot it. All three routed modes exhausted that
budget. Routed review identified flaws but could not start the requested fix
round; tiers stopped before final review; best-of stopped before either
subtask selected and landed a candidate. Its candidates remained on saved
branches at the time the task ended. Claude candidates also reported that
non-interactive command approval blocked Go checks. This pilot measures the
current end-to-end setup, including that constraint, rather than isolating
model quality.

The original benchmark stored only the last check-output line (`FAIL`).
Post-run diagnostics reran the identical protected check on the captured
`internal/affected` package for routed modes and on a reconstruction of the
single agent's eight recorded patches. All four failed again. The first three
had substantive Jest indexing/fallback and Gradle layout failures, including
supported-layout regressions and unresolved-layout cases that still narrowed.
`TestSelectJestRoots` also checks explanation wording; the failures were not
limited to that wording assertion. Best-of's primary tree still had the
starting implementation because no candidate landed. Full diagnostic output
is retained in `logs/pilot-diagnostic-*.txt`; these reruns do not replace the
original times or outcomes.

The [four recorded rows](2026-10-06-pilot.json) were reconstructed from the
completed `bench` and `task_end` session records. Stopping the original matrix
after the fourth check also ended its parent shell before native aggregate
export. A fifth preparation header printed, but the journal confirms **no
fifth task or provider agent started**. The original console log, journal,
stop record and export script remain in the evidence folder.

This is one task, one repetition and one mode order on a working desktop.
Compiler caches outside the workspace and provider load were not controlled.
A read-only disk inventory ran during the latter attempts at the user's
request; no cleanup occurred. A single task cannot establish a general
routing, tiering or best-of-N advantage. Fresh tokens and API-equivalent
estimates are not subscription bills or a direct measurement of time to a
provider's quota limit. Codex does not report dollar cost, so a zero cost field
must not be interpreted as a free run or compared against Claude spending.

**No general benefit is demonstrated by this pilot.** Before spending quota on
the larger matrix, retain complete failure diagnostics, resolve the configured
Claude check-command approval path, and decide an explicit budget policy for
planning, candidates, review and fixes. Any revised experiment should retain
this pilot and report its changed settings, then run paired repetitions across
the remaining tasks. The roadmap's benefit exit criterion remains open.

## Remaining gates

- Native macOS runtime/race execution for this local batch and hosted Azure/
  Bitbucket pipeline runs.
- Sustained real-agent daily use on the user's Windows hardware, including the
  roadmap's two-week crash/hang/lost-work and no-freeze criteria.
- A real Codex quota-limit recording, broader provider/sandbox/desktop checks
  already listed in the roadmap, and human visual acceptance.
- Independent review of these new fixes and all-platform CI before a future
  merge. This local pass does not claim that merge or release gate is closed.
