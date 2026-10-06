---
title: Security model
weight: 3
---

<!-- include SECURITY.md -->

## Where each part is described

- **Repo files need your trust.** A repo's `.relayweft.yaml` (or a `./relayweft.yaml` that came with a clone) cannot run commands, start MCP servers, reach other folders or send your results anywhere until you run `rw trust`. See [per-repo settings](config.md#per-repo-settings-relayweftyaml).
- **The browser UI** listens on 127.0.0.1 only, with single-use links and per-tab sessions. See [rw web and rw app](../guides/web.md).
- **Agents in a container.** For unattended runs, agents can run in Docker or Podman with only the step's folder writable. See [the sandbox](../guides/sandbox.md).
- **CI and forge tokens.** Agents, checks and hooks never get the forge token; only `rw`'s own push and API calls use it. See [CI](../guides/ci.md#security).
- **Text from issues, comments and CI logs** reaches the agents only as fenced, untrusted text, and unattended pushes refuse changes no agent reported. See [forges](../guides/forges.md).
- **Updates** are checked against the release's `checksums.txt` before they replace `rw`.
- **Your machine.** Agents and git run at low priority, and `rw` holds new agents while the machine is busy. See [machine load and disk](../guides/machine.md).
