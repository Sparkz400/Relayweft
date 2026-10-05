#!/usr/bin/env bash
# Builds the docs site into _site/ at the repo root.
#
#   website/build.sh          build into _site
#   website/build.sh serve    preview at http://localhost:1313/Relayweft/
#                             (after editing a page or the README, run it again)
#
# Needs Go and Hugo extended (the version in .github/workflows/pages.yml).
# It builds rw for the CLI reference unless RW_BIN names one. GEN_FLAGS
# go to website/gen, and Hugo reads its own settings from the environment
# (HUGO_BASEURL, ...).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root/website"
mkdir -p _gen

if [[ -z "${RW_BIN:-}" ]]; then
  exe=""
  [[ "$(go env GOOS)" == windows ]] && exe=.exe
  (cd "$root" && go build -o "website/_gen/rw$exe" ./cmd/rw)
  RW_BIN="$root/website/_gen/rw$exe"
fi

# shellcheck disable=SC2086 # GEN_FLAGS is a list of flags
go run ./gen -rw "$RW_BIN" ${GEN_FLAGS:-}

if [[ "${1:-}" == serve ]]; then
  exec hugo server
fi
hugo build --gc --minify --cleanDestinationDir --destination "$root/_site"
