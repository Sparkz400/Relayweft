# Security policy

## Reporting a vulnerability

Please report security issues privately via GitHub's
**Security → Report a vulnerability** on this repository (private
vulnerability reporting), not in a public issue. Include steps to reproduce
and the `sy version` you used. You should get an answer within a week.

## Scope

Switchyard runs coding agents (the official `codex` and `claude` CLIs) on
your machine and applies their changes to your working tree. Especially
relevant:

- `sy web` / `sy app`: the local HTTP server on 127.0.0.1 (single-use links,
  per-tab sessions, Host/Origin checks).
- Per-repo `.switchyard.yaml`: settings that run commands (verify, hooks,
  providers) must not apply without `sy trust`.
- `sy update`: downloads are checked against the release's `checksums.txt`.
- Git operations on your repository and its worktree pool.

Switchyard never handles API keys; the CLIs use their own logins.
