---
title: Relayweft
layout: hextra-home
---

{{< hextra/hero-badge link="docs/project/roadmap/" >}}
  <span>Beta: what is verified and what is not</span>
  {{< icon name="arrow-circle-right" attributes="height=14" >}}
{{< /hextra/hero-badge >}}

<div class="hx:mt-6 hx:mb-6">
{{< hextra/hero-headline >}}
  Your coding agents, working together.
{{< /hextra/hero-headline >}}
</div>

<div class="hx:mb-12">
{{< hextra/hero-subtitle >}}
  A terminal app that routes coding work between Codex, Claude Code and other agent CLIs, on the subscriptions you already have.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:mb-6">
{{< hextra/hero-button text="Get started" link="docs/start/quickstart/" >}}
{{< hextra/hero-button text="GitHub" link="https://github.com/Sparkz400/Relayweft" style="secondary" >}}
</div>

<div class="content">

## What it is

You give `rw` a task in your repo. A planner splits it into steps. You approve or edit the plan. Agents work on the steps in parallel, each writing agent in its own git worktree. You can review each agent's changes, file by file and hunk by hunk, before they land. Your tests run, a reviewer checks the result (on the other provider, when you use two), and you see what it cost. `rw undo` takes it all back.

It drives the official `codex` and `claude` CLIs with their normal login. It never touches model API keys. Gemini CLI, Qwen Code, DeepSeek and local models are optional.

## The demo

One task with two parallel agents: plan approval with an edited step, change review that leaves one hunk out, the checks, the cost, then `rw undo`. The agents are scripted, so it uses no quota; everything else is the real `rw`. ([how it is recorded](/docs/demo/record.sh), [as WebM](/docs/demo/demo.webm))

![rw: plan approval, parallel agents, change review, undo](/docs/demo/demo.gif)

You can run the same pipeline yourself with fake agents: `rw --demo` (the TUI) or `rw web --demo` (the browser UI).

## Install

{{< tabs >}}
{{< tab name="Windows" >}}
With [Scoop](https://scoop.sh):

```sh
scoop install https://raw.githubusercontent.com/sparkz400/relayweft/main/packaging/scoop/rw.json
```

Or download `rw-windows-amd64.exe` (or `-arm64`) from the [latest release](https://github.com/Sparkz400/Relayweft/releases/latest), rename it to `rw.exe` and put it on your PATH. `rw update` keeps it current.

winget: the manifest is ready, but the package is not in winget-pkgs yet.
{{< /tab >}}
{{< tab name="macOS" >}}
With Homebrew:

```sh
brew tap sparkz400/relayweft https://github.com/Sparkz400/Relayweft
brew install relayweft
```
{{< /tab >}}
{{< tab name="Linux" >}}
Each release (from v0.3.0) has `.deb`, `.rpm` and `.apk` packages for amd64 and arm64:

```sh
curl -fsSLO https://github.com/Sparkz400/Relayweft/releases/latest/download/relayweft-linux-amd64.deb
sudo apt install ./relayweft-linux-amd64.deb     # or: sudo dnf install ./relayweft-linux-amd64.rpm
```

Homebrew works on Linux too. An AUR package (`relayweft-bin`) is prepared in `packaging/aur` but not published yet.
{{< /tab >}}
{{< tab name="Binary" >}}
Every release has plain binaries and a `checksums.txt`. On Linux (use `rw-darwin-arm64` on a Mac):

```sh
curl -fsSLO https://github.com/Sparkz400/Relayweft/releases/latest/download/rw-linux-amd64
curl -fsSLO https://github.com/Sparkz400/Relayweft/releases/latest/download/checksums.txt
sha256sum --ignore-missing -c checksums.txt
install -m 755 rw-linux-amd64 ~/.local/bin/rw
```
{{< /tab >}}
{{< tab name="Go" >}}
With Go 1.26 or newer:

```sh
go install github.com/sparkz400/relayweft/cmd/rw@latest
```

`rw version` then says `dev`. Run the same command again to update.
{{< /tab >}}
{{< /tabs >}}

Then install and log in to at least one agent CLI, and run `rw setup` in your repo. The [quick start](docs/start/quickstart.md) has the details, and [Install](docs/start/install.md) covers upgrading from Switchyard (`sy`).

## Where it stands

Relayweft is young, and the [roadmap](docs/project/roadmap.md) says plainly what is verified.

- **Phase 1, the core** (routing, worktrees, merges, undo, load and crash safety) is tested on Windows, Linux and macOS. Its exit criterion, two weeks of daily use without a crash or a hang, is not met yet.
- **Everything else is beta until then:** plan approval, change review, the browser UI, CI runs, team mode, the sandbox and more. These are built and tested with fake agents and real git, and some were tried with the real CLIs, but they have not had real daily use.
- **It is not shown that routing beats a single agent.** On the small starter bench a single agent was faster and cheaper. A comparison on real multi-file tasks is still open. See the [benchmarks](docs/project/benchmarks/_index.md).

</div>
