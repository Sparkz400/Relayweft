# The Relayweft demo, recorded with charmbracelet/vhs.
# docs/demo/record.sh prepares /tmp/rw-demo (rw, the scripted agents and a
# small Go project in a git repo) and then runs this file.
#
# The story: one task, plan approval with an edited step, agents working in
# parallel, change review that leaves one hunk out, done with the cost,
# then rw undo.

Output docs/demo/demo.gif
Output docs/demo/demo.webm

Require rw
Require git

Set Shell "bash"
Set FontSize 15
Set Width 1360
Set Height 800
Set Padding 14
Set Theme "Catppuccin Mocha"
Set TypingSpeed 28ms
Set Framerate 12
Set CursorBlink false

Hide
Type "source /tmp/rw-demo/env.sh && clear"
Enter
Sleep 500ms
Show

# Start the TUI and give it the task.
Type "rw"
Sleep 500ms
Enter
Wait+Screen@20s /Relayweft/
Sleep 1s
Type "Make the parser keep trailing empty fields and add a --strict flag that rejects malformed lines"
Sleep 700ms
Enter

# Plan approval: add a line to the third step's prompt, then run the plan.
Wait+Screen@30s /APPROVE THE PLAN/
Sleep 2.5s
Down
Sleep 500ms
Down
Sleep 700ms
Type "e"
Sleep 700ms
Type " Document it in the README."
Sleep 900ms
Ctrl+S
Sleep 1.2s
Enter

# Two agents work in parallel, each in its own worktree. Change review of
# the first: open parse.go's hunks, untick the stray debug print, apply.
Wait+Screen@60s /REVIEW CHANGES.*fields/
Sleep 2s
Tab
Sleep 900ms
Down
Sleep 1.2s
Space
Sleep 1.8s
Enter

# The second agent's changes: take them all.
Wait+Screen@60s /REVIEW CHANGES.*strict/
Sleep 2s
Enter

# The checks run, the reviewer approves, and the task is done with its cost.
Wait+Screen@90s /API-equivalent/
Sleep 4.5s
Type "/quit"
Sleep 400ms
Enter
Sleep 1s

# Everything the task did is one command away from gone.
Type "git status --short"
Enter
Sleep 1.5s
Type "rw undo"
Enter
Wait+Screen@20s /\[y\/N\]/
Sleep 2s
Type "y"
Enter
Wait+Screen@20s /Undo done/
Sleep 1s
Type "git status --short"
Enter
Sleep 3s
