---
title: Roles and routing
weight: 2
---

Every step has a **role** (planner, worker, explorer, reviewer, ...). Every role has a **route** (a provider, a model and an effort) on each provider. `rw` picks the route per step, falls back to another provider when one hits its usage limit, and can learn better routes from your own logs.

<!-- include README.md#choosing-models-any-model-for-any-job body -->

## The router

<!-- include README.md#how-it-works-and-the-decisions-made-for-v1 item="**Router**" -->

## Usage limits

<!-- include README.md#how-it-works-and-the-decisions-made-for-v1 item="**Usage-limit detection**" -->

[Tune](../guides/bench-and-tune.md) has learned routes and best of N, and [providers](../guides/providers.md) covers Gemini CLI, Qwen Code, DeepSeek, local models and your own CLIs.
