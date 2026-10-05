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

sy's forge and CI tokens (`GITHUB_TOKEN`, `GITLAB_TOKEN`, `CI_JOB_TOKEN`, ...) never go in, not even when named: the config refuses them in `env`, as in [CI](ci.md#security). An MCP server runs in the container with its values, so a sandboxed run with an MCP server that uses one of them (`${GITHUB_TOKEN}`) fails with what to do: leave that server out for the provider (`mcp.servers.<name>.providers`) or run the provider outside the sandbox. Other `${VAR}` values of your MCP servers do go in (in Claude's MCP config file or Codex's environment).

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
- **Commands sy runs on the agents' code** run in the sandbox whenever writing agents do: your verify commands, the `after_merge` and `after_task` hooks (`npm run lint` runs scripts an agent may have written), and `sy bench` checks. They use the top-level section when it is on; otherwise the sandbox of the first provider whose writers (`worker`, `worker_high`) are sandboxed, without that provider's sign-in variables and files. They get the top-level `env`, and the hooks their `SY_*` variables (`SY_DIR` is `/work`). `before_task` hooks run on your machine: no agent has run yet. When only read-only roles are sandboxed, all of these run on your machine; `sy doctor` warns.

### In a repository's `.switchyard.yaml`

A repo file may make the sandbox **stricter** without `sy trust`: turn it on (with your image; when yours is off it may also pick docker or podman, which only decides which installed runtime runs it), set `network: off`, or widen it to more roles. Anything that makes it **weaker** needs `sy trust`: turning it off, switching the runtime of a sandbox you turned on, picking the image, passing more variables, files or folders in, or a provider's section (repo files cannot touch `providers` untrusted anyway). Until then sy uses your own settings and says what it ignored. The same holds for a `./switchyard.yaml` that came with a clone.

## What the container sees

| In the container | From | |
|---|---|---|
| `/work` | the step's folder: a pool worktree, or your project folder | read-write; read-only for read-only agents (explorer, reviewer, ...) |
| `/work/.git` | the project's `.git`, or for a pool worktree a generated `.git` file pointing into `/sy/git` | read-only |
| `/sy/git` | the repository's shared git folder (for a pool worktree) | read-only |
| `.../config` | copies of the repository's git configs (its own, each submodule's under `modules/`, `config.worktree`) with only their format and extensions | read-only |
| `<submodule>/.git` | a checked-out submodule's `.git` file | read-only |
| `/sy/home` | `HOME`: a folder sy keeps per project **and provider** (verify commands and hooks have their own), so the CLIs' sessions survive between runs | read-write |
| `/sy/run` | this run's prompt | read-only |
| `/sy/mcp` | Claude's MCP config file, when MCP servers are configured | read-only |

Why this layout:

- **Paths.** The agent always works in `/work`. A pool worktree's own `.git` file names a host path (`D:\...` on Windows), which means nothing in a Linux container, so sy mounts the shared git folder at a fixed place and hands the container a `.git` file that points there. When the agent prints `/work/src/a.go`, sy turns it back into the host path. The change review, the merge and `sy undo` see the same paths as without a sandbox, on Windows, Linux and macOS.
- **git works, read-only.** `git status`, `git diff` and `git log` work in the container. The agent cannot write the git folder: not its hooks, not its config, not the index (sy commits the agent's files itself, as without a sandbox). A writable `.git` would let code in the container run commands on your machine the next time git runs there.
- **No remotes.** The git configs in the container have the format and extensions only (and a submodule's `core.worktree`). Remote URLs (which may hold a token, also a submodule's), credential helpers, `http.extraheader`, `core.fsmonitor` and `core.hooksPath` stay out.
- **Submodules.** A submodule's `.git` is in the work tree, outside the read-only git folder, and in a pool worktree, where submodules are not checked out, it does not exist yet. An agent could write one that points git at a folder it made, with a config that runs commands; git on your machine would follow it the next time it looks into the submodule. So:
  - every git command sy runs where agents write overrides `core.fsmonitor` and turns off submodule recursion (`-c core.fsmonitor=false -c submodule.recurse=false`, also passed on to the git processes git starts), and sy's work-tree diffs and status ignore submodules;
  - a checked-out submodule's `.git` is mounted read-only;
  - after every sandboxed run (agents, verify commands, hooks) sy compares each submodule's `.git` with what it was before. One that appeared or changed is removed (a file is put back), a symlink in its path is removed, and the run fails with a message that says so.
- **One home per provider.** The CLIs read settings from their home that can run commands (Claude Code's `settings.json` hooks, Codex's `config.toml`, shell rc files, `.gitconfig`). Each provider gets its own home, so an agent of a weaker provider (a local model reading issue text) cannot leave such a file for the CLI of a provider that holds an API key. sy also removes those files from a home before each run (unless a mount of yours goes there), without following symlinks.
- **Sessions.** Claude Code and Codex keep their sessions in the home folder, and the agent always runs in `/work`. So `claude --resume` and `codex exec resume` work across pool worktrees and sy runs, for follow-ups and for a step that continues after sy was stopped. A session that started outside the sandbox (or inside, when you turn it off) is not found: a fresh agent takes over then.

The container runs with `--cap-drop ALL`, `--security-opt no-new-privileges`, `--pids-limit 4096` and `--pull never` (the image is built here; a mistyped name never fetches someone else's image). A mount whose folder holds the docker or podman socket (`/var/run`, `~/.docker/run`, `$XDG_RUNTIME_DIR/podman`, ...) is refused: the agent would get the runtime, and with it your machine. On Linux and macOS it runs as your user id, so the files it writes are yours. On Windows it runs as root (without capabilities): Docker Desktop's file sharing cannot open files in a folder 17 or more levels deep for any other user (seen with Docker Desktop on engine 29.8).

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

## What the sandbox protects, and what it does not

It protects, while the agent runs:
- your files outside the step's folder (only `/work` is writable, plus the provider's home);
- the repository's git folder and config, and with them git's hooks and settings on your machine;
- your environment and credentials other than those you name; sy's forge and CI tokens;
- the container runtime (no socket, no capabilities).

It does **not** protect:
- **Code the agent wrote, once it runs outside.** sy runs it in the sandbox only for verify commands, `after_merge`/`after_task` hooks and bench checks. Your editor, your own `npm install`, `make` or `git commit` hooks in the project folder run it on your machine. Read the diff before you run anything.
- **Files in your project folder that your tools act on.** A step that runs in the project folder (not a pool worktree) writes there directly. Your tools may act on such files: an `.envrc`, an editor's task file, a `Makefile`. The submodule `.git` case above is checked; others are not.
- **Symlinks.** A symlink the agent creates is a real symlink on your disk (on Windows a reparse point). sy does not follow symlinks where it writes itself (homes, submodule checks), and git stores a symlink as a link. Other programs of yours may follow one.
- **The network.** It is open by default (the agents need their model API), so an agent can send what it sees, and its credentials, to the internet. `network: off` cuts it, the model API too.
- **The credentials you pass in.** The agent can use and read them.
- **Providers you keep outside.** A provider with `sandbox: {mode: off}`, or a role outside `roles`, runs on your machine, also when a repo file's `prefer` routes work to it.

## Limits

- The container is only as strong as the runtime: a kernel or runtime bug can break out of any container.
- Cancelling within the first moment of a container start may leave that container running until this sy ends and the next one sweeps it (its input is closed, so the wrapper should end it first).
- Not tried yet: Podman, a signed-in Claude Code or Codex doing real work in the container (a real Claude Code ran without a sign-in), rootless Docker on Linux (it maps your user id differently), and Docker Desktop on macOS.

## Tests

- `sy selftest` runs the sandbox scenario when Docker or Podman with Linux containers is there (`--sandbox only` runs just that; `--sandbox off` skips it). It builds a small test image with git and a scripted stand-in for Claude Code (no quota), then: a task with two writers in pool worktrees and a third step that combines their files; sy's verify command in the container; an agent that writes a submodule's `.git` (sy must remove it and fail the run); a step stopped by its timeout; and an sy killed hard while its agent works. The stand-in checks from the inside that it is in `/work`, that git works, that the git folder is read-only, that no forge token and no remote URL got in, and that a read-only agent cannot write.
- CI runs it on Linux (`TestSandboxSelftest`, required there).
