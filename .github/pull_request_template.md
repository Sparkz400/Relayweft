## Summary

What changed and why, in a few short sentences. Name the ROADMAP item or issue it closes.

## Design

How it works. The choices you made, and why not the alternatives.

## Tests

Each new test, what it covers, and that it fails without the fix. Say which OS you ran the full suite on.

## Not verified

What you could not run for real (another OS, a real CLI, a real forge). Write "nothing" if so.

## ROADMAP update

The rows and lines of ROADMAP.md this changes, or "none".

## CHANGELOG entry

The line you added under `[Unreleased]` in CHANGELOG.md, or "none" (no user-visible change).

## Checklist

- [ ] Every bug fix has a regression test that fails without the fix
- [ ] `go vet ./...` and `go test -p 4 ./...` pass
- [ ] Tested on Windows, or CI on Windows is green (processes, paths and the console need a Windows-specific test)
- [ ] Errors reach the TUI and session log with a next step
- [ ] Orchestrator, git, process or runner change: had an adversarial review, and its fixes are listed
- [ ] CHANGELOG.md has an entry under `[Unreleased]` (or no user-visible change)
- [ ] ROADMAP.md is updated, or the text is under "ROADMAP update"
