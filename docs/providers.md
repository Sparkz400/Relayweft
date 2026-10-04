# More providers: Gemini, Qwen, DeepSeek and local models

Out of the box Switchyard routes between Codex and Claude Code. Five more providers ship in the default config, all **disabled**:

| Provider | CLI it runs | Models | Sign-in | Notes |
|---|---|---|---|---|
| `gemini` | [Gemini CLI](https://geminicli.com) (`kind: gemini`) | `pro`, `flash`, `flash-lite` | `GEMINI_API_KEY` (paid); personal Google accounts no longer work | read-only roles run in Gemini's `plan` mode |
| `deepseek` | Claude Code against DeepSeek's Anthropic-compatible API (`kind: claude`) | `deepseek-v4-pro`, `deepseek-flash` | `DEEPSEEK_API_KEY` in your environment | pay-per-use API |
| `qwen` | [Qwen Code](https://github.com/QwenLM/qwen-code) (`kind: qwen`) | `qwen3.6:35b-a3b-coding` on local Ollama | none (local) | `only_preferred` |
| `ollama` | Claude Code against local Ollama (`kind: claude`) | any Ollama model with tool support | none (local) | `only_preferred` |
| `ollama-run` | `ollama run`, a plain local model (`kind: generic`) | any Ollama model | none (local) | no tools: read-only work only; stands by for the judge |

A provider is a name plus the **kind** of CLI it speaks: `codex`, `claude`, `gemini`, `qwen`, or `generic` for a CLI described entirely in your config ([below](#any-other-cli-kind-generic)). A `kind: claude` provider runs Claude Code against any Anthropic-compatible endpoint. That is how DeepSeek and local models work, and it keeps Claude Code's tested read-only tool restriction.

## Turning one on

1. Install the CLI (`sy doctor` prints the command if it is missing):
   - Gemini: `npm install -g @google/gemini-cli`. Since 18 June 2026, Gemini CLI no longer serves personal Google accounts (free, AI Pro, AI Ultra); Google points those to its Antigravity CLI. So Gemini CLI needs an API key from [Google AI Studio](https://aistudio.google.com/apikey) with billing on. Set `GEMINI_API_KEY`, then run `gemini`, type `/auth` and choose "Use Gemini API key". Gemini's own choice in `~/.gemini/settings.json` wins over the environment.
   - Qwen Code: `npm install -g @qwen-code/qwen-code`.
   - Ollama: `winget install Ollama.Ollama`, then `ollama pull qwen3.6:35b-a3b-coding`. Agent CLIs send long prompts, so set `OLLAMA_CONTEXT_LENGTH=65536` (or at least 32768) and restart Ollama.
   - DeepSeek: set `DEEPSEEK_API_KEY` in your environment. Claude Code is already installed.
2. In your `switchyard.yaml`, set `disabled: false` on the provider.
3. Choose which roles use it. Every role already has a route on every preset (`roles.<role>.<provider>`), so `prefer` is all you need:
   - `/prefer explorer gemini` in the TUI,
   - `sy --prefer reviewer=deepseek` on the command line,
   - or `prefer: gemini` in the file.
4. Run `sy doctor`. It checks the CLI, its version, the sign-in, the environment variables the provider needs, and whether a local endpoint answers.

## Fallbacks and order

When a role's provider is at its usage limit, sy uses the role's route on the **next** provider in `routing.provider_order`. The default order is codex, claude, then the rest by name. `prefer: other` (the reviewer's default) means the first provider after the planner's, and `prefer: auto` means the one that has used the fewest tokens this session.

`only_preferred: true` keeps a provider out of all of that: it runs only the roles that name it. The local presets have it set, so a slow local model never silently takes over a strong model's job.

### Standby: a free local model when the others are nearly out

`standby: [roles]` is the exception to `only_preferred`. The provider takes those roles' work when the provider chosen for it is at its limit, or has reported `routing.switch_at_utilization` (90% by default) of its limit used, and no other provider has more room. The decision log shows rule `standby`, for example `codex at 92% of its limit, no other provider has room -> qwen, standing by for explorer`.

- `qwen` (Qwen Code on Ollama) stands by for `explorer` and `researcher`. It has tools, so it can read the repo, and it runs those roles in plan mode.
- `ollama-run` (a plain model) stands by for the `judge`. It sees only the prompt, so it suits closed questions, not exploring.

Turning one on (`disabled: false`) is enough; `standby: []` turns the standby off. Writing roles can stand by only on a provider that can write. A local model is slow (one explorer turn can take minutes on a model that does not fit in VRAM), and Ollama answers one request at a time by default, so parallel explorers queue.

## Adding your own

Any provider name works. Give it a `kind` and a `command`, and add a route to the roles that should use it:

```yaml
providers:
  lmstudio:                      # Claude Code against LM Studio
    kind: claude
    command: claude
    env:
      ANTHROPIC_BASE_URL: http://127.0.0.1:1234
      ANTHROPIC_AUTH_TOKEN: lmstudio
    only_preferred: true
    models:
      - {id: qwen3-coder-30b, tier: standard}
roles:
  explorer: {prefer: lmstudio, lmstudio: {model: qwen3-coder-30b}}
```

`env` values are added to the CLI's environment and never go on the command line. `${VAR}` is read from your environment. A run stops before the CLI starts if a variable it needs is not set. The debug log records the variable names only, never their values.

## Any other CLI: `kind: generic`

A CLI with none of the built-in protocols is described in config: the command line, how to read its output, how to resume a session and how it reports a usage limit. The prompt always goes in on stdin.

```yaml
providers:
  ollama-run:
    kind: generic
    command: ollama
    only_preferred: true
    standby: [judge]
    models: [{id: "qwen3.6:35b-a3b-coding", tier: standard}]
    generic:
      args: [run]
      model_args: ["{model}", --think=false, --nowordwrap, --verbose]
      no_tools: true            # a plain model: read-only by nature
      output: text              # stdout is the answer
      usage:                    # regular expressions over stdout and stderr; the (group) is the count
        input: '^prompt eval count:\s+(\d+)'
        output: '^eval count:\s+(\d+)'
roles:
  judge: {ollama-run: {model: "qwen3.6:35b-a3b-coding"}}
```

| Field | Meaning |
|---|---|
| `args` | On every run, first. `{model}` and `{effort}` are the route's (checked like every route). |
| `model_args`, `effort_args` | Added when the route has a model or an effort, e.g. `[-m, "{model}"]`. |
| `read_only_args` | Added for read-only agents. They must keep the CLI from editing files and running commands (its plan mode), because sy cannot check what an unknown CLI does. Without them, or `no_tools`, the provider gets no read-only work. |
| `write_args` | Added for writing agents. Without them the provider gets read-only work only. |
| `no_tools` | The CLI only answers the prompt: it cannot read files or run commands. |
| `resume_args` | Continue a session on a follow-up, e.g. `[--resume, "{session}"]`. Without them a follow-up starts a fresh agent with the earlier one's context. A session id goes on the command line only if it is plain (letters, digits, `. _ : @ + -`). |
| `output` | `text` (default): stdout is the answer, each line is shown as progress, and `session` and `usage` are regular expressions over stdout and stderr. `jsonl`: one JSON object per line, read with `json` rules. |
| `json` | Rules for `jsonl`. A rule applies when every `match` path has its value (`"*"` = present), and every matching rule applies. It can read `session`, `text` (`delta: true` for streamed chunks), `final`, `thinking`, `tool` and `tool_input`, `error` (`fatal: true` fails the run even with an answer), `limit: true`, and `input_tokens`, `cached_tokens`, `output_tokens`. Paths are dot-separated keys and array indexes (`message.content.0.text`); `each: <array path>` with nested `rules` reads every element (Claude-style content blocks). |
| `edit_tools` | Tool names that change files: their `file_path` or `path` is shown as an edit and listed in the result. |
| `input_excludes_cached` | The CLI's input count leaves out the cached part (sy counts input with it). |
| `limit_patterns`, `limit_exit_codes` | Added to the global `limit_patterns`; exit codes that mean "at the usage limit". The usual `limit_cooldown` applies. |

`internal/runner/testdata/generic_qwen.yaml` and `generic_gemini.yaml` describe Qwen Code and Gemini CLI in this format. The tests check that they read the recorded runs exactly like the built-in kinds: answer, session, tokens, edited files and errors. The built-in kinds stay the better choice for those two, because they also guard against a repo's own `.qwen` and `.gemini` settings. A generic provider has no such checks, so only describe CLIs you trust with your repo.

`sy doctor` runs `<command> --version` and says what a generic provider may do (for example `read-only work only; no resume`). `ollama run` downloads a model you have not pulled, so pull it first.

## Safety

- **Repo files cannot add providers or describe CLIs.** `providers` is one of the keys a repo's `.switchyard.yaml` may set only after `sy trust`. An untrusted repo can still set `prefer`, but cannot enable a disabled provider or point one at another endpoint.
- **Read-only roles stay read-only.** Gemini and Qwen run read-only roles in their `plan` mode, which removes the edit and shell tools. A recorded Qwen Code run asked to write a file in plan mode did not write it (`internal/runner/testdata/qwen_real_readonly.jsonl`).
- **Route values cannot carry commands.** Model and effort names reach the CLI's command line, which `cmd.exe` parses for npm `.cmd` shims. So a model may contain only letters, digits and `. _ : / @ + [ ] -`, and may not start with `-`. Anything else is refused, and a repo file that tries it is refused as a whole.
- **Gemini's folder trust.** Gemini CLI only honours an approval mode in a folder it trusts. A trusted folder also has its own `.gemini` settings loaded, which can run commands, and its `.env`, which can point the CLI and your Google sign-in at another endpoint. So sy passes `--skip-trust` only when the folder has neither. In a repo that has them:
  - writing agents are refused with an explanation, unless you trusted the folder in Gemini yourself (`~/.gemini/trustedFolders.json`);
  - read-only agents still run, in Gemini's default mode, which refuses edits.
- **Qwen Code's repo settings.** A repo with its own `.qwen` folder is refused for every role, because Qwen would load it and its MCP servers and discovery commands run commands.
- **Generic CLIs get only the work they declare.** A provider without `write_args` is never routed a writing step and refuses one if it gets it; one with tools but no `read_only_args` gets no read-only work. A role that prefers a provider that cannot do its work is a config error.
- **`allow_repo_settings: true`** on a provider lifts both guards. Only your own config can set it.
- **MCP servers.** These reach Qwen Code (`--mcp-config`) and `kind: claude` providers as they do Claude. Gemini CLI reads MCP servers only from its own settings, so naming `gemini` in an MCP server's `providers:` is a config error.

## Costs and limits

- Tokens are counted per provider in the TUI, `sy stats`, `sy bench` and the reports.
- The $ figure stays Claude's API-equivalent price. Claude Code also prices other backends as if they were Claude, so sy drops that number for `kind: claude` providers with their own `ANTHROPIC_BASE_URL`.
- Gemini, Qwen and the other API providers do not report how much of their limit is used. They can therefore only fall back after a limit hit, not switch before it. Limit messages are recognised by `limit_patterns`, which now also covers `RESOURCE_EXHAUSTED` (Gemini) and `insufficient balance` (DeepSeek).

## What is verified

- Qwen Code 0.24.7 and Claude Code 2.1.287 against local Ollama were recorded on Windows: an answer, an edit, plan mode refusing a write, a resume, and missing auth. These recordings are test fixtures, and `sy run` was run end to end with both. Claude Code 2.1.288 against Ollama was run again (a one-word answer) and parses the same; the DeepSeek preset uses the same `claude` CLI.
- For Gemini CLI 0.62.0, the signed-out case (exit code 41) and the refused personal-account sign-in were recorded. The stream format is taken from the CLI's source. A recording with an API key should replace those fixtures; until then treat Gemini as beta.
- DeepSeek uses the documented endpoint and variables but has not been run here, because that needs an API key.

### Recording the missing runs

Run these in any git repo. The output files go to `internal/runner/testdata/`, the same way the Codex recordings did.

```sh
# Gemini (with GEMINI_API_KEY set and chosen in `gemini` /auth)
echo "Reply with exactly: hi" | gemini --output-format stream-json --skip-trust --approval-mode plan > gemini-real-hi.jsonl
echo "Create hello.txt containing exactly hi" | gemini --output-format stream-json --skip-trust --approval-mode auto_edit > gemini-real-edit.jsonl
echo "Create blocked.txt containing x" | gemini --output-format stream-json --skip-trust --approval-mode plan > gemini-real-readonly.jsonl

# DeepSeek (with DEEPSEEK_API_KEY set)
ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic ANTHROPIC_AUTH_TOKEN=$DEEPSEEK_API_KEY \
  claude -p --output-format stream-json --verbose --model deepseek-flash <<< "Reply with exactly: hi" > deepseek-real-hi.jsonl
```
