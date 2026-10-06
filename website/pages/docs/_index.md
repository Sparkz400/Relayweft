---
title: Documentation
cascade:
  type: docs
---

Relayweft (`rw`) routes coding work between agent CLIs: Codex, Claude Code and, if you like, Gemini CLI, Qwen Code, DeepSeek or a local model. It plans a task, runs the steps in parallel, merges the results into your tree and lets you check every step.

{{< cards >}}
  {{< card link="start/quickstart/" title="Quick start" icon="play" subtitle="Install, log in, run rw setup, run a first task." >}}
  {{< card link="concepts/" title="Concepts" icon="light-bulb" subtitle="Roles, routing, the worktree pool, plan approval, change review and undo." >}}
  {{< card link="guides/" title="Guides" icon="book-open" subtitle="The TUI and browser UI, providers, forges, CI, team mode, the sandbox, MCP, budgets, bench and editors." >}}
  {{< card link="reference/" title="Reference" icon="document-text" subtitle="Commands, configuration, the security model and troubleshooting." >}}
{{< /cards >}}

These pages are built from the Markdown in the [repository](https://github.com/Sparkz400/Relayweft): the README, `docs/`, the roadmap and the editor READMEs. When the site and the repository differ, the repository is right.

> [!NOTE]
> Most of Relayweft is **beta**. The core (Phase 1) is tested on Windows, Linux and macOS but has not finished its two weeks of daily use. The features of Phases 2 to 4 are tested with fake agents and real git but have not been used for real. The [roadmap](project/roadmap.md) lists what is verified.
