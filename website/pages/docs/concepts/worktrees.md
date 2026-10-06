---
title: The worktree pool
weight: 5
---

Writing agents that run at the same time each work in their own git worktree, outside your repo. Their results are merged back into your working tree as they finish. Your index, `HEAD` and branch are never touched.

<!-- include README.md#how-it-works-and-the-decisions-made-for-v1 item="**Parallel worktrees**" -->

The pool also has a disk guard: see [machine load and disk](../guides/machine.md).
