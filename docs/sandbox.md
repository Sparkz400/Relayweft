# A container sandbox for the agents

By default sy starts Codex, Claude Code and the other agent CLIs on your machine, with your permissions. An agent can read your files, run any command its CLI allows, and reach anything you can reach. That is fine while you watch. For unattended runs (the queue, `sy run --at`, `--issues`, CI, team mode) you can run the agents in a container instead. Only the step's folder is writable there.

The sandbox is **off by default**. It works with Docker and with Podman (same command line).

## Turning it on

1. Install Docker (Docker Desktop on Windows and macOS) or Podman. On Windows, Docker Desktop must run Linux containers (the default).
2. Build the image once:

   ```sh
   docker build -t switchyard-sandbox packaging/sandbox
   # without a checkout of this repository:
   docker build -t switchyard-sandbox https://github.com/Sparkz400/switchyard.git#main:packaging/sandbox
   ```

   It has git, Node.js, Claude Code and the Codex CLI. Pin the CLIs with `--build-arg CLAUDE_CODE_VERSION=2.1.288 --build-arg CODEX_VERSION=0.160.0`.
3. In your `switchyard.yaml`:

   ```yaml
   sandbox:
     mode: docker        # or podman
   ```

4. Give the agents a way to sign in (next section), then run `sy doctor`. It checks the runtime, Linux containers, the image, each CLI in the image and the sign-in variables.

Your verify commands also run in the container, so they need your toolchain there. Build your own image on top and point `sandbox.image` at it:

```dockerfile
FROM switchyard-sandbox
USER root
RUN apt-get update && apt-get install -y --no-install-recommends golang && rm -rf /var/lib/apt/lists/*
USER node
```

## Signing in

Nothing of your environment goes into the container except what the config names. Your home folder is never mounted.

- **API keys and tokens by name.** Each provider has a `sandbox.env` list. The defaults:
  - `claude`: `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` (create a long-lived token for your subscription with `claude setup-token`);
  - `codex`: `OPENAI_API_KEY`, `CODEX_API_KEY`;
  - `gemini`: `GEMINI_API_KEY`.

  Only the variables that are set go in. A provider's `env` (for example DeepSeek's endpoint and key) goes in too. The values reach the container through the runtime's environment (`-e NAME`), never its command line.
- **Credential files, read-only.** `credentials: [~/.codex/auth.json]` mounts that file at the same place under the container's home. It must be under your home folder. A CLI cannot refresh a read-only file: Codex with a ChatGPT sign-in may need a fresh `codex login` on your PC now and then. Prefer API keys or `claude setup-token` for unattended runs.

sy's forge and CI tokens (`GITHUB_TOKEN`, `GITLAB_TOKEN`, `CI_JOB_TOKEN`, ...) never go in, not even when named: the config refuses them, as in [CI](ci.md#security).

## The settings

```yaml
sandbox:
  mode: docker                 # off (default) | docker | podman
  image: switchyard-sandbox    # the default
  network: on                  # on (default) | off: no network at all
  env: [MY_TEST_DB_URL]        # more variable names, for every sandboxed agent and verify command
  credentials: [~/.codex/auth.json]
  mounts:                      # more folders or files, read-only unless writable
    - {path: ~/.cache/go-build, target: /cache/go-build, writable: true}
  roles: [worker, worker_high] # only these roles (empty = every role)

providers:
  claude:
    sandbox: {env: [ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN]}   # the default
  ollama:
    sandbox: {mode: off}       # this provider runs on your machine
```

- A provider's own `sandbox` section adds to the top-level one. Its `mode`, `image`, `network` and `command` replace the top-level ones; its `env`, `credentials` and `mounts` are added; its `roles` replace the top-level `roles`. A list you write replaces the preset's list, like every list in the file.
- `command` is the CLI's name in the image when it differs from the provider's command (default: the command without folder and `.cmd`/`.exe`).
- `network: off` cuts every connection, the model API too. It suits verify commands and CLIs that need no network. An allow-list is not supported.
- A local model on this machine (Ollama) is `host.docker.internal` from inside the container, not `127.0.0.1`. Point the provider's `ANTHROPIC_BASE_URL` there, or give it `sandbox: {mode: off}`.
- sy's own verify commands follow the top-level section (they run in the sandbox whenever `sandbox.mode` is on). Hooks and bench checks are your own commands and run on your machine.

### In a repository's `.switchyard.yaml`

A repo file may make the sandbox **stricter** without `sy trust`: turn it on (with your image), set `network: off`, or widen it to more roles. Anything that makes it **weaker** needs `sy trust`: turning it off, picking the runtime or the image, passing more variables, files or folders in, or a provider's section (repo files cannot touch `providers` untrusted anyway). Until then sy uses your own settings and says what it ignored. The same holds for a `./switchyard.yaml` that came with a clone.

## What the container sees

| In the container | From | |
|---|---|---|
| `/work` | the step's folder: a pool worktree, or your project folder | read-write; read-only for read-only agents (explorer, reviewer, ...) |
| `/work/.git` | the project's `.git`, or for a pool worktree a generated `.git` file pointing into `/sy/git` | read-only |
| `/sy/git` | the repository's shared git folder (for a pool worktree) | read-only |
| `.../config` | a copy of the repository's git config with only its format and extensions | read-only |
| `/sy/home` | `HOME`: a folder sy keeps per project, so the CLIs' sessions survive between runs | read-write |
| `/sy/run` | this run's prompt | read-only |
| `/sy/mcp` | Claude's MCP config file, when MCP servers are configured | read-only |

Why this layout:

- **Paths.** The agent always works in `/work`. A pool worktree's own `.git` file names a host path (`D:\...` on Windows), which means nothing in a Linux container, so sy mounts the shared git folder at a fixed place and hands the container a `.git` file that points there. When the agent prints `/work/src/a.go`, sy turns it back into the host path. The change review, the merge and `sy undo` see the same paths as without a sandbox, on Windows, Linux and macOS.
- **git works, read-only.** `git status`, `git diff` and `git log` work in the container. The agent cannot write the git folder: not its hooks, not its config, not the index (sy commits the agent's files itself, as without a sandbox). A writable `.git` would let code in the container run commands on your machine the next time git runs there.
- **No remotes.** The git config in the container has the format and extensions only. Remote URLs (which may hold a token), credential helpers, `http.extraheader`, `core.fsmonitor` and `core.hooksPath` stay out.
- **Sessions.** Claude Code and Codex keep their sessions in the home folder, and the agent always runs in `/work`. So `claude --resume` and `codex exec resume` work across pool worktrees and sy runs, for follow-ups and for a step that continues after sy was stopped. A session that started outside the sandbox (or inside, when you turn it off) is not found: a fresh agent takes over then.

The container runs with `--cap-drop ALL`, `--security-opt no-new-privileges` and `--pids-limit 4096`. On Linux and macOS it runs as your user id, so the files it writes are yours. On Windows it runs as root (without capabilities): Docker Desktop's file sharing cannot open files in a folder 17 or more levels deep for any other user (seen with Docker Desktop on engine 29.8).

## Stopping

- Each container is named `sy-<sy's pid>-<its start time>-<random>` and labelled `switchyard.sandbox`.
- **Cancel and timeouts** run `docker kill` on the container, then stop the client.
- **When sy dies** (closing the window, a crash, `taskkill`), the container's input closes with the docker client, and a small wrapper in the container ends it. In the real test on Windows the container was gone 0.6 s after sy was killed hard.
- **Leftovers.** A pool worktree's pid file records its container, and the next sy that takes the worktree removes it first. The first sandboxed run of every sy also removes the containers of an sy that has ended (`sy doctor` lists them).

## Fail closed

If the sandbox is on but Docker is missing or not running, the image is missing, or the CLI is not in the image, the step fails with what to do, for example:

```
sandbox: image "switchyard-sandbox" not found: build it with `docker build -t switchyard-sandbox packaging/sandbox` (docs/sandbox.md) or set sandbox.image
```

It never runs on your machine instead. A verify command fails the same way.

## Codex in the sandbox

Codex's own sandbox needs kernel features that containers block. In sy's sandbox Codex runs with `--sandbox danger-full-access`: the container is the sandbox, and a read-only agent's folder is mounted read-only.

## Limits

- What the agent writes into `/work` reaches your tree after the change review, as always. Read the diff before you run the code on your machine.
- The network is open by default (the agents need their model API). An agent can still send what it sees to the internet.
- The per-project home folder is shared by that project's agents. One agent can leave files there (a CLI setting, for example) that the next one reads. It stays inside the sandbox.
- A provider with `sandbox: {mode: off}` runs on your machine for every role routed to it, including through a repo file's `prefer`.
- Not tried yet: Podman, a real Claude Code or Codex run in the container (only the CLIs' `--version`), rootless Docker on Linux (it maps your user id differently), and Docker Desktop on macOS.

## Tests

- `sy selftest` runs the sandbox scenario when Docker or Podman with Linux containers is there (`--sandbox only` runs just that; `--sandbox off` skips it). It builds a small test image with git and a scripted stand-in for Claude Code (no quota), then: a task with two writers in pool worktrees and a third step that combines their files; sy's verify command in the container; a step stopped by its timeout; and an sy killed hard while its agent works. The stand-in checks from the inside that it is in `/work`, that git works, that the git folder is read-only, that no forge token and no remote URL got in, and that a read-only agent cannot write.
- CI runs it on Linux (`TestSandboxSelftest`, required there).
