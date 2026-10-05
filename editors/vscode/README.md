# Relayweft for VS Code

A thin VS Code client for [Relayweft](https://github.com/sparkz400/relayweft)
(`rw`). It starts `rw web --client` for your workspace folder and drives the
same local API the browser UI uses: the agent tree and status in a side
panel, the activity log in an output channel, plan approval, follow-ups,
undo, and hunk-by-hunk change review in VS Code's diff editor.

Everything runs on your machine: rw listens on `127.0.0.1` only, and the
extension talks to it with node's `http` module. It has no runtime npm
dependencies.

## Requirements

- `rw` (Relayweft) with `rw web --client` support, plus the Codex and/or
  Claude Code CLIs it routes to. See the main README for installing rw.
- VS Code 1.90 or newer.

If `rw` is not on your `PATH`, set **`relayweft.path`** to the executable
(`C:\Users\you\go\bin\rw.exe` on Windows). Without a setting the extension
looks on `PATH`, then in `%GOBIN%`/`~/go/bin`, Scoop's shims and WinGet's
links (Windows) or `/usr/local/bin` and `~/.local/bin`. On Windows the path
must be an `.exe`: rw is started without a shell, so a `.cmd` shim cannot be
used.

## Use

1. Open a project folder and click the Relayweft icon in the activity bar,
   then **Start Relayweft** (or run **Relayweft: Start**). With several
   workspace folders you pick one.
2. **Relayweft: Run Task…** (the `+` in the Agents view) asks for a task.
   While a task runs, new tasks are queued and run unattended after it.
3. The **Agents** view shows the status, what waits for you, the queue and
   the agent tree (main agent, subtasks, reviewer, judge) live. Hover an
   agent for its last activity, files and merge result. The **Relayweft**
   output channel (**Relayweft: Show Activity Log**) has the activity log.
4. When rw asks you something, a notification and the *Waiting for you*
   node appear:
   - **Plan**: approve, reject, or *Edit the plan…*, which opens the plan
     as JSON with *Approve this plan* / *Reject* CodeLens at the top.
   - **Changes** (with "review changes" on): see below.
   - **Budget**: go on past the limit or stop the task.
   - **Merge conflict**: let an agent resolve it (you then review its
     resolution like any change set) or keep the change on a branch.
5. **Relayweft: Follow Up with Agent…** (or the inline button on an agent)
   sends `@agent message` to a finished agent (it resumes its session) or
   to a running one (delivered when its turn ends).
6. **Relayweft: Undo Last Task…** shows what `rw undo` would change and
   asks before it runs `rw undo --yes`.
7. **Relayweft: Open in Browser** opens the full web UI with a fresh
   single-use link.
8. **Relayweft: Stop** stops rw. rw also stops when you close the folder,
   when VS Code closes, and if the extension host dies (its stdin closes).

## Reviewing changes

With review on, each agent's changes wait in the **Review** view before
they land. Click a file to open it in the diff editor: the left side is
the file as the agent found it, the right side with the agent's changes.
Both are read-only virtual documents built from the patch rw sends,
applied to the file in your folder. If that file differs from the agent's
base (or the patch was too large and truncated), the diff shows the hunks
alone with markers for the lines not shown.

- Check or uncheck files and hunks in the Review view.
- In the diff, use the editor title buttons (or **Accept/Reject Hunk at
  Cursor**) for the hunk under the cursor. Rejected hunks are struck
  through.
- CodeLens above each hunk toggles it. VS Code hides CodeLens in diff
  editors unless `"diffEditor.codeLens": true` is set; the other ways work
  without it.
- **Apply Selected Changes** sends the decision; **Reject All Changes**
  keeps them on a branch only; **Send Back with Feedback…** reruns the
  agent with your message.

Only modified text files with more than one hunk (and a complete patch)
can be split; other files are accepted or rejected as a whole, as in the
web UI.

## Settings

| Setting | Default | |
|---|---|---|
| `relayweft.path` | `""` | Path to `rw` / `rw.exe`; empty searches as above. This and `relayweft.args` are read from your user settings only, never from a workspace's `.vscode/settings.json`. |
| `relayweft.args` | `[]` | Extra arguments for `rw web --client`, one per item, e.g. `["--threads", "2"]`, `["--provider", "claude"]` or `["--demo"]` to try it with fake agents. Nothing is shell-quoted. |
| `relayweft.notifyApprovals` | `true` | Notify when a plan, change set, budget or merge conflict question waits. |

## How it connects (security)

`rw web --client` opens no browser and prints one JSON line on stdout:

```json
{"relayweft":"web-client","protocol":1,"version":"…","url":"http://127.0.0.1:PORT","addr":"127.0.0.1:PORT","bootstrap":"<64 hex>","dir":"…","pid":1234}
```

The extension trades the single-use bootstrap for a session
(`POST /api/session`) exactly like a browser tab trades the `#b=` link
fragment, and sends the session in `X-Relayweft-Session` on every
request, including the event stream. No security check is relaxed for it:
rw still checks the `Host` header, requires a JSON body for changes and a
valid session for every `/api` route, and refuses a request with a foreign
`Origin` even with a valid session. The extension sends no `Origin` (it is
not a web page), which rw has always treated as same-origin; that alone
grants nothing. The bootstrap travels only through the pipe between VS
Code and the rw it started.

On stdin, rw answers `link` (a page link for **Open in Browser**) and
`bootstrap` (a new bootstrap, used to log in again if the session was
dropped) with one JSON line each, and stops when stdin closes.

## Build

```sh
cd editors/vscode
npm ci           # dev dependencies only: typescript, @types/vscode, @types/node
npm run build    # tsc -> out/
npm test         # unit tests (node:test), no VS Code needed
npm run test:integration   # the extension in a real VS Code, see below
```

`npm run test:integration` downloads VS Code into `.vscode-test/` (stable;
`VSCODE_VERSION=1.90.0` for the oldest supported one), builds rw from
this repository (`go build ./cmd/rw`; `RW_EXE=<path>` uses an existing
one) and runs `src/test/integration` in that VS Code with its own user
data and extensions folders. rw runs for real (git, worktrees, change
review) in a throwaway repository, with a scripted Claude CLI as its agent
(no quota is used) and its config and state in a scratch `APPDATA`. The
suite starts rw, runs tasks, edits and approves a plan, accepts and
rejects hunks in the diff editor and checks the files in the repository,
then undo, follow-up, feedback, cancel, stop, a killed rw, and VS Code
closing while an agent works; afterwards it checks that no rw or agent
process is left. Your own VS Code profile and rw state are not touched.

To try it, open `editors/vscode` in VS Code and press F5 (Extension
Development Host), or package and install it:

```sh
npm run package                       # runs @vscode/vsce through npx; makes relayweft-<version>.vsix
code --install-extension relayweft-0.1.0.vsix
```

`npm run package` downloads `@vscode/vsce` on first use; it is not run in
CI.

## Limitations

- One rw per VS Code window (one workspace folder at a time).
- The diff's "before" side is the file in your folder. For a file that
  changed since the agent started (or in another repo of a multi-repo
  task), the diff shows only the hunks.
- Routing, settings, history, stats and scheduling are not in the
  extension yet: use **Open in Browser** for those.
