---
title: Roles and routing
weight: 2
---

Every step has a role (planner, worker, explorer, reviewer, ...), and the router picks the route for it: a provider, a model and an effort. When a provider hits its usage limit, the work moves to another one.

<!-- include README.md#choosing-models-any-model-for-any-job body -->

## The router

<!-- include README.md#how-it-works-and-the-decisions-made-for-v1 item="**Router**" -->

## Usage limits

<!-- include README.md#how-it-works-and-the-decisions-made-for-v1 item="**Usage-limit detection**" -->

[Tune](../guides/bench-and-tune.md) has learned routes and best of N, and [providers](../guides/providers.md) covers Gemini CLI, Qwen Code, DeepSeek, local models and your own CLIs.
