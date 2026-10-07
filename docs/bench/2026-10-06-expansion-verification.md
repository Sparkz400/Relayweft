# Roadmap expansion verification — 6 October 2026

The later [batch review and reliability record](2026-10-06-reliability-benefits.md)
adds regression fixes, full Linux race execution, live provider recovery and
headless browser checks. Read it for the current verification state; the
results below describe the original implementation pass.

This record covers the local changes after `48e3e1d` for the eight requested
areas. It records executed checks, not release approval. Existing working-tree
changes were snapshotted before implementation. The preexisting process guard
and load-test files were retained byte for byte.

## Implemented

| Area | Change | Evidence |
|---|---|---|
| Benchmark | Ten historical multi-file tasks; fixed starting commits; protected correctness checks; isolated fair runs; rotated mode order; repetitions; single, routed, tiers and best-of-N; Markdown and JSON results | All ten known solutions passed and all ten starting versions failed their protected checks on Windows. An eight-run scripted matrix exercised all four modes and two repetitions. No real-model comparison was run. |
| Reliability | Single-agent cancellation cannot finish as success; unavailable providers return an error; single-agent undo retains file provenance; automatic notes share a lock with manual edits; recovery stays within its project; preserved branches no longer count as failed merges | Regression tests cover the confirmed failures. Windows and Linux cancellation, resume, conflict and process-cleanup checks passed. The preexisting Unix guard changes were exercised, not rewritten. |
| Affected tests | Jest aliases and multiple project roots; Vitest monorepos, imported configs and aliases; Gradle custom directories and applied build scripts | Focused layout tests and the full Windows affected-test package passed. Unsupported or uncertain layouts run the full check. |
| CI templates | Azure Pipelines and Bitbucket Pipelines, with a shared installer/runner, exact binary digests, pinned agent versions, scoped credentials, budgets and reports | YAML and shell checks passed. Scripted runs for both forges tested successful and failed jobs, arguments, credentials, cleanup and reports. No hosted pipeline was executed. |
| Updater | Signed SLSA attestations for both binary and checksums, bound to the official release workflow, issuer, hosted runner and release-tag commit, before installation | Failure and tampering tests passed. A real GitHub CLI verification accepted v0.4.0 and rejected modified binary and checksum bytes. The live test never installs the download. |
| Workflows | User-owned bugfix, dependency-upgrade, review and release-prep starters; saved prompts/checks; stricter budget preservation; approvals | Parse, save/load, replacement, budget/check policy and scripted CLI integration tests passed. |
| Recovery | Project task list, interrupted progress, saved work and branches, errors/conflicts, resume/retry, preview and undo | API tests exercised a real temporary Git repository. Undo kept unreported user edits, rejected invalid/cross-project actions and refused a repeated undo. UI controller tests exercised preview/confirmation and busy-state controls. |
| Memory | Stored note inspection, inclusion reasons and prompt preview; revision-checked add/edit/remove; concurrent automatic writes | Twelve concurrent writes retained all twelve notes. Selection, stale-editor conflicts, input validation and API/controller lifecycle tests passed. |

## Executed checks

Environment: Windows amd64, PowerShell, Go 1.27.0, Git 2.55.0.windows.3 and
Git Bash. Linux binaries were cross-compiled on Windows and executed in WSL
Ubuntu. Tests used scripted providers; none called a real model.

```sh
go test -p 4 ./... -count=1 -timeout 20m
go test ./cmd/rw -count=1 -timeout 20m
go vet ./...
GOOS=linux go vet ./...
GOOS=darwin go vet ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...
```

The first full Windows run passed every package except `cmd/rw`, where the
completion registry did not classify the new value flags. The registry was
fixed and its regression tests passed. The complete CLI package rerun passed
in 311.438 seconds. Every Windows package therefore has a passing full-package
run after the relevant fix; the orchestrator package took 584.572 seconds.
The final benchmark example/help change also passed its focused CLI checks.
Vet passed for all three operating systems. Golangci-lint reported **0 issues**.
The final Windows CLI executable built successfully, and the new commands'
help pages were smoke-tested.

Linux runtime checks passed for the complete process, web, workflow and
session-log packages and focused orchestrator tests matching
`Single|ProjectMemory|RecoveryState|FairBench|Cancel|Resume|Conflict`.
The Jest/Vitest/Gradle layout tests also passed there. The full Linux
affected-test package could not complete because tests invoking `go` had no
Linux Go executable; that package passed in full on Windows. Node was also
absent in WSL, so UI controller tests ran on Windows instead.

Corpus and live updater checks:

```sh
rw bench --file docs/bench/realistic.yaml --validate --check-timeout 3m
RW_VERIFY_LIVE=1 go test ./cmd/rw -run '^TestUpdateLive$' -count=1 -v
```

Corpus validation: **10/10** solution/base pairs passed. The live updater
test passed with real GitHub/Sigstore verification and tampering rejection.
The live test requires `gh` with attestation support and network access.
`git diff --check` also passed.

Local execution logs and the preserved baseline are under
`D:\Entwicklung\Switchyard-work-20261006`. They are local evidence, not
published CI artifacts. The checked-in tests and commands above are the
reproducible verification path.

## Remaining verification

- Run the real-model benchmark matrix with provider credentials and quota.
  The supplied configuration is 120 runs with a $10 API-equivalent cap per run;
  pilot a single task first. The corpus is one project's public Go history,
  not a claim of general or uncontaminated model performance.
- Execute both CI templates on the relevant hosted accounts with secrets and
  the target project's build dependencies.
- Review Recovery and Memory visually in an interactive browser. API and
  controller tests passed; no browser was connected for visual inspection.
- Run the complete Linux/macOS CI and race suites. This Windows toolchain has
  CGO disabled; neither Windows nor the available WSL environment has GCC.
  macOS received cross-platform vet only, not runtime testing.
- Obtain the independent adversarial review required by CONTRIBUTING before
  merging orchestrator/security changes. No merge, release or publication was
  performed. Sustained real-agent use remains a separate roadmap gate.
