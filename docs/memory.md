# Inspect project memory

Open **Memory** in `rw web` or `rw app` to read, edit, add, pin and remove the
project notes used for context hand-off. Type a task into the preview box to
see which notes that task would get and why. Changes do not rewrite an agent
session already in progress.

```sh
rw memory
rw memory --json
rw memory --dir /path/to/project
rw memory --task "fix the retry loop in net/retry.go"
```

## What a task gets

With `orchestrator.handoff` on, every planner and step prompt gets:

- **Pinned conventions**, first and always: up to 8 notes you pinned. They
  never age out.
- **The two newest notes**, so a task knows what happened just before it.
- **Up to four more notes that match the task**: a note matches when the
  task shares words or identifiers with its text, or names one of its source
  files or their folder (counted double). Notes that match nothing stay out of
  the prompt.

All notes together are capped at 16 KiB per prompt. Without a task to match,
for example in `rw memory` without `--task`, the six newest notes fill the
slots.

Every note in `rw memory` shows whether it is in or out and why.

## Source files and invalidation

A finished task records the files it changed (up to 12) with a fingerprint of
their content. You can name source files for your own notes too. Agents see
the file names, so they know where to look.

When a referenced file changes, the note is marked: agents read
`a.go (changed since)`. When **all** of a note's source files have changed or
been deleted, the note no longer describes the code and leaves the prompts.
If it still holds, re-confirm it (`--refresh`, or **Re-confirm** in the
browser). This records the files as they are now, and the note comes back.

A pinned convention is never dropped for changed sources. It stays in the
prompt with a warning for agents to check it still holds, and `rw memory`
asks you to re-confirm or edit it.

Unpinned notes from tasks also age out after 60 days. Notes written before
fingerprints existed keep their file list. Only a file's deletion counts for
them until you re-confirm them.

## Editing from the CLI

Each note has an ID and the document has a revision. Copy those values when
editing from the CLI:

```sh
rw memory --revision REVISION --file new-note.txt                       # add
rw memory --revision REVISION --file rule.txt --pin --ref internal/api/errors.go
rw memory --revision REVISION --id NOTE_ID --file revised-note.txt      # new text
rw memory --revision REVISION --id NOTE_ID --pin                        # or --unpin
rw memory --revision REVISION --id NOTE_ID --ref a.go --ref b.go        # replace sources
rw memory --revision REVISION --id NOTE_ID --no-refs                    # drop sources
rw memory --revision REVISION --id NOTE_ID --refresh                    # re-confirm
rw memory --revision REVISION --id NOTE_ID --delete
```

`--ref` takes repo-relative paths or paths inside the repo, repeated or
comma-separated. Each must be an existing file. Flags combine: one call can
change a note's text, pin it and set its sources.

The revision prevents a stale editor from overwriting notes saved by another
task. Reload after a conflict and reapply your edit. Automatic notes and manual
changes use the same process lock and atomic write, so concurrent writers do
not silently discard each other's work.

At most 20 notes are stored. A new task note replaces the oldest unpinned
note, so pinned conventions are never pushed out. Manual notes can contain up
to 8192 bytes. Removing a note removes it from future context; it does not
remove text from old task logs or existing provider sessions.

The notes file is plain text in the state directory (`rw memory` prints its
path). Each note ends with optional `refs:` and `pinned: yes` lines that `rw`
maintains.

This view covers Relayweft's stored project notes. Repository instruction files,
the repository map, learned routing data and a running task's discoveries are
separate inputs. It does not inspect or change Codex or Claude's own memory.
