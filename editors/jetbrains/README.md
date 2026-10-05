# Switchyard for JetBrains IDEs

A thin client for [Switchyard](https://github.com/sparkz400/switchyard)
(`sy`) in IntelliJ IDEA, PyCharm, GoLand, WebStorm, Rider, CLion and the
other JetBrains IDEs. It starts `sy web --client` for your project and
drives the same local API the browser UI and the VS Code extension use:

- a **Switchyard** tool window with the agent tree, the activity log and a
  prompt box to start a task, and cancel;
- plan approval: edit, add, delete and reorder the subtasks, then approve
  or reject;
- change review in the IDE's diff viewer: accept or reject each hunk or
  file, or send the changes back with feedback;
- follow-ups, undo of the last task, and notifications when a task is done
  or failed, or when sy waits for you.

Everything runs on your machine: sy listens on `127.0.0.1` only, and the
plugin talks to it with the JDK's HTTP client, through no proxy. It has no
dependencies besides the IDE.

## Requirements

- `sy` (Switchyard) with `sy web --client` support, plus the Codex and/or
  Claude Code CLIs it routes to. See the main README for installing sy.
- A JetBrains IDE 2025.2 or newer (build 252+).

If `sy` is not on your `PATH`, set it in **Settings | Tools | Switchyard**
(`C:\Users\you\go\bin\sy.exe` on Windows). Without a setting the plugin
looks on `PATH`, then in `%GOBIN%`/`~/go/bin`, Scoop's shims and WinGet's
links (Windows) or `/usr/local/bin`, `/opt/homebrew/bin` and
`~/.local/bin`. On Windows the path must be an `.exe`: sy is started
without a shell, so a `.cmd` shim cannot be used.

## Install

Build the plugin zip (see below) or take it from the `jetbrains` workflow's
artifact, then **Settings | Plugins | ⚙ | Install Plugin from Disk…** and
pick `switchyard-jetbrains-<version>.zip`. It is not on the JetBrains
Marketplace yet.

## Use

1. Open a project and the **Switchyard** tool window (right side), then
   **Start Switchyard** (or **Tools | Switchyard | Start Switchyard**).
   The project must be trusted: sy runs agents and commands in it.
2. Type a task in the prompt box and press **Run** (or Ctrl+Enter). When sy
   is not running yet, Run starts it first. While a task runs, new tasks
   are queued and run unattended after it. `@agent message` follows up.
3. The **Agents** tab shows the status, what waits for you, the queue and
   the agent tree (main agent, subtasks, reviewer, judge) live. Hover an
   agent for its last activity, files and merge result. The activity log
   is below the tree.
4. When sy asks you something, a notification and the *Waiting for you*
   node appear (double-click or Enter answers):
   - **Plan**: a dialog with the subtasks. Edit the title, kind, role,
     prompt, files and dependencies of each; add, delete and move them;
     then **Approve Plan**, **Reject Plan…** (cancels the task) or
     **Decide Later**.
   - **Changes** (with "review changes" on): see below.
   - **Budget**: go on past the limit or stop the task.
5. **Follow Up with Agent…** (toolbar, or double-click an agent) sends
   `@agent message` to a finished agent (it resumes its session) or to a
   running one (delivered when its turn ends).
6. **Undo Last Task…** shows what `sy undo` would change and asks before it
   runs `sy undo --yes`.
7. **Open in Browser** opens the full web UI with a fresh single-use link.
8. **Stop Switchyard** stops sy. sy also stops when you close the project,
   when the IDE exits, and if the IDE dies (its stdin closes).

## Reviewing changes

With review on, each agent's changes wait in the **Review** tab before they
land. Double-click a file (or a hunk) to open the diff: the left side is
the file as the agent found it, the right side with the agent's changes.
Both are read-only documents built from the patch sy sends, applied to the
file in your project. If that file differs from the agent's base (or the
patch was too large and truncated), the diff shows the hunks alone with
markers for the lines not shown.

- Tick or untick files and hunks in the Review tab.
- In the diff, click the gutter icon at a hunk to switch it, or use the
  toolbar's **Accept Hunk at Caret** / **Reject Hunk at Caret**. Rejected
  hunks are struck through. The bar above the diff says what applying
  sends and has **Accept file**, **Reject file**, **Apply selected
  changes** and **Send back with feedback…**.
- **Apply Selected Changes** sends the decision; **Reject All Changes**
  keeps them on a branch only; **Send Back with Feedback…** reruns the
  agent with your message.

Only modified text files with more than one hunk (and a complete patch)
can be split; other files are accepted or rejected as a whole, as in the
web UI.

Windows checkouts with `core.autocrlf=true` (Git for Windows' default) have
CRLF line ends while git diffs LF content. The plugin matches the hunks
with LF on both sides then, so the diff shows the whole file, not only the
hunks (the VS Code extension had this bug once; the tests here cover it).

## Settings

**Settings | Tools | Switchyard**:

| Setting | Default | |
|---|---|---|
| sy executable | empty | Path to `sy` / `sy.exe`; empty searches as above. |
| Extra arguments | empty | For `sy web --client`, one per line, e.g. `--threads` and `2`, `--provider` and `claude`, or `--demo` to try it with fake agents. Nothing is shell-quoted. |
| Notify when a plan, a change set or a budget question waits | on | |
| Notify when a task is done or failed | on | |

The settings live in the IDE's own configuration (`switchyard.xml`, not
synced between machines), never in a project: the sy path and its
arguments choose what program runs, and a cloned repository must not be
able to choose that.

## How it connects (security)

`sy web --client` opens no browser and prints one JSON line on stdout:

```json
{"switchyard":"web-client","protocol":1,"version":"…","url":"http://127.0.0.1:PORT","addr":"127.0.0.1:PORT","bootstrap":"<64 hex>","dir":"…","pid":1234}
```

The plugin trades the single-use bootstrap for a session
(`POST /api/session`) exactly like a browser tab trades the `#b=` link
fragment, and sends the session in `X-Switchyard-Session` on every request,
including the event stream. No security check is relaxed for it: sy still
checks the `Host` header, requires a JSON body for changes and a valid
session for every `/api` route, and refuses a request with a foreign
`Origin` even with a valid session. The plugin sends no `Origin` and no
cookies, refuses any address that is not loopback, and ignores the IDE's
and the system's proxy settings, so the session never leaves the machine.
The bootstrap travels only through the pipe between the IDE and the sy it
started.

On stdin, sy answers `link` (a page link for **Open in Browser**) and
`bootstrap` (a new bootstrap, used to log in again if the session was
dropped) with one JSON line each, and stops when stdin closes.

## Build

You need a JDK 21 to run Gradle (or any JDK 17+: the build downloads a JDK
21 for compiling if none is installed). The Gradle wrapper is in the repo.

```sh
cd editors/jetbrains
./gradlew buildPlugin     # build/distributions/switchyard-jetbrains-<version>.zip
./gradlew test            # unit tests, a real sy, and the plugin in a headless IDE (see below)
./gradlew verifyPlugin    # the IntelliJ Plugin Verifier against IDEA 2025.2, 2025.3 and 2026.2
./gradlew runIde          # IntelliJ IDEA Community 2025.2 with the plugin, in a sandbox
./gradlew runLocalIde -PlocalIde="C:\Program Files\JetBrains\PyCharm 2026.2.2"   # an IDE you have installed, in a sandbox
```

The first run downloads the IDE it compiles against (about 1 GB);
`verifyPlugin` downloads the three IDEs it checks.

`./gradlew test` runs:

- unit tests for the protocol client against a fake server (Host, session,
  re-login, no proxy, the event stream and its reconnects), the sy process
  against a scripted fake sy (hello, commands, stop and kill, Windows
  argument quoting), the patch and hunk mapping (CRLF included), the plan
  editor and the task model;
- `RealSyTest`: a real `sy web --client` built from this repository
  (`go build ./cmd/sy`; `SY_EXE=<path>` uses an existing one) with a
  scripted Claude CLI as its agent (no quota is used) and its config and
  state in scratch folders. It runs tasks, edits and approves a plan,
  rejects one hunk on a CRLF checkout (Windows), checks the files, then
  follow-up, feedback, reject, cancel and stop, and that no process is
  left;
- `SyServiceIdeTest`: the same against the plugin's own service and views
  in a headless IDE (the IntelliJ Platform test framework, a project on
  disk): the Agents and Review tabs, the hunk marks and strike-through in
  the diff documents, notifications, a killed sy, and closing the project
  while an agent works.

`./gradlew uiTest` (with `SY_UI_TEST=1`) runs `SwitchyardUiTest`: the
plugin in a real IntelliJ IDEA, as its own process and window, driven
through its UI with JetBrains' IDE Starter and Driver frameworks. It
starts sy from the toolbar, types a task in the prompt box, edits the plan
in the plan dialog, unticks a hunk in the Review tab, opens the diff,
applies, checks the files and stops sy, and saves screenshots of the IDE
window to `build/reports/uiTest`. The driver clicks and types with
`java.awt.Robot` into whatever has the focus, so run it on a display of
its own: the `jetbrains` workflow's `ui` job runs it on Xvfb. It downloads
IntelliJ IDEA Community 2025.2 the first time.

Without Go and without `SY_EXE` the real-sy tests are skipped; `SY_IT_REQUIRED=1`
(CI) makes that a failure. Their scratch folders are in
`%TEMP%/sy-jetbrains-test` (`SY_JB_TEST_DIR` changes it; `SY_JB_TEST_KEEP=1`
keeps them). Your own IDE settings and sy state are not touched.

`gradle.properties` keeps the build light (`org.gradle.workers.max=4`, no
parallel projects, small heaps).

## Publishing to the JetBrains Marketplace (not done yet)

Publishing needs a JetBrains account, so it is not part of the build or
CI. The steps:

1. Sign in at <https://plugins.jetbrains.com> with a JetBrains account and
   create a vendor profile (for example `sparkz400`), the name in
   `plugin.xml`'s `<vendor>`.
2. Upload the first version by hand: **Upload plugin**, pick the zip from
   `./gradlew buildPlugin`, choose the license (MIT) and the tags. JetBrains
   reviews new plugins before they appear (usually a few working days).
   The plugin ID `io.github.sparkz400.switchyard` cannot change later.
3. Optional but recommended: sign the plugin. Create a key and a
   certificate chain (see "Plugin Signing" in the IntelliJ Platform SDK
   docs) and set `CERTIFICATE_CHAIN`, `PRIVATE_KEY` and
   `PRIVATE_KEY_PASSWORD`; `./gradlew signPlugin` then signs the zip.
4. For later versions, create a token in your Marketplace profile
   (**My Tokens**) and run `PUBLISH_TOKEN=<token> ./gradlew publishPlugin`
   after raising `pluginVersion` in `gradle.properties`. To publish from
   CI, add the token (and the signing values) as repository secrets and a
   workflow step that runs `publishPlugin`; keep that workflow manual.

## Limitations

- One sy per project window.
- The diff's "before" side is the file in your project. For a file that
  changed since the agent started (or in another repo of a multi-repo
  task), the diff shows only the hunks.
- Routing, settings, history, stats and scheduling are not in the plugin
  yet: use **Open in Browser** for those.
- Remote development (Gateway, split mode) is not tested: sy runs where the
  IDE's backend runs.
