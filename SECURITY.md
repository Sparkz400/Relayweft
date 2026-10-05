# Security policy

## Reporting a vulnerability

Please report security issues privately via GitHub's
**Security → Report a vulnerability** on this repository (private
vulnerability reporting), not in a public issue. Include steps to reproduce
and the `rw version` you used. You should get an answer within a week.

## Scope

Relayweft runs coding agents (the official `codex` and `claude` CLIs) on
your machine and applies their changes to your working tree. Especially
relevant:

- `rw web` / `rw app`: the local HTTP server on 127.0.0.1 (single-use links,
  per-tab sessions, Host/Origin checks).
- Per-repo `.relayweft.yaml`: settings that run commands (verify, hooks,
  providers) must not apply without `rw trust`.
- `rw update`: downloads are checked against the release's `checksums.txt`.
- Git operations on your repository and its worktree pool.

Relayweft never handles API keys; the CLIs use their own logins.
