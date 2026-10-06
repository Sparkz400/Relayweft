#!/usr/bin/env bash
# Records the demo (docs/demo/demo.gif and demo.webm) with charmbracelet/vhs.
#
# The recording runs the real rw TUI in a throwaway git repo. The `claude`
# and `codex` it starts are the scripted agent in docs/demo/agent, so the
# plan, the edits and the reviews are always the same, and no quota is used.
# Plan approval, the worktree pool, change review, the checks and undo are
# rw's own.
#
# Needs Linux or macOS with Go, git, vhs, ttyd and ffmpeg (and gifsicle,
# if it is there, to shrink the GIF). CI runs it in
# .github/workflows/demo.yml (Actions > demo > Run workflow). Run it from
# the repo root:
#
#   docs/demo/record.sh
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
work=/tmp/rw-demo # demo.tape sources $work/env.sh, so the path is fixed
rm -rf "$work"
mkdir -p "$work/bin" "$work/config/relayweft" "$work/cache" "$work/inventory"

(cd "$repo" && go build -o "$work/bin/rw" ./cmd/rw && go build -o "$work/bin/claude" ./docs/demo/agent)
cp "$work/bin/claude" "$work/bin/codex"

# rw's config: plan approval and change review on, the sample's tests as
# the checks, no desktop notifications, a day budget for the header.
cat >"$work/config/relayweft/relayweft.yaml" <<'EOF'
orchestrator:
  approve_plan: true
  review_changes: true
  max_cpu_percent: 0
  min_free_memory_mb: 0
  min_free_disk_gb: 1
verify:
  commands: ["go test ./..."]
notify:
  enabled: false
budget:
  day_usd: 5
EOF

gocache=$(go env GOCACHE) # keep Go's build cache when XDG_CACHE_HOME moves
cat >"$work/env.sh" <<EOF
export PATH="$work/bin:\$PATH"
export XDG_CONFIG_HOME="$work/config" XDG_CACHE_HOME="$work/cache" GOCACHE="$gocache"
export GIT_AUTHOR_NAME=demo GIT_AUTHOR_EMAIL=demo@example.com GIT_COMMITTER_NAME=demo GIT_COMMITTER_EMAIL=demo@example.com
export RW_NO_SETUP=1 RW_DEMO_SPEED=\${RW_DEMO_SPEED:-1}
# The TUI's colours: termenv draws none when CI is set (as on Actions).
unset CI
export COLORTERM=truecolor
export PS1='\[\e[1;36m\]~/inventory\[\e[0m\] \$ '
cd "$work/inventory"
EOF

(
  cd "$work/inventory"
  "$work/bin/claude" setup .
  git init -q -b main .
  git add -A
  git -c user.name=demo -c user.email=demo@example.com commit -qm "inventory: first version"
  GOCACHE="$gocache" go test ./... >/dev/null # warm the build cache for rw's checks
)

cd "$repo"
PATH="$work/bin:$PATH" vhs docs/demo/demo.tape
# Keep the GIF small for the README (about 3 MB at most).
if command -v gifsicle >/dev/null; then
  gifsicle -O3 --lossy=30 --colors 128 -b docs/demo/demo.gif
fi
ls -l docs/demo/demo.gif docs/demo/demo.webm
