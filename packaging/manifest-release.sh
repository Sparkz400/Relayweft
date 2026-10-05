#!/bin/sh
# Decides whether the packaging checks can test the committed manifests
# (Formula/relayweft.rb, packaging/aur/PKGBUILD and .SRCINFO,
# packaging/scoop/rw.json) against their release. The packaging workflow
# runs it first in each job that installs or re-renders them.
#
#   packaging/manifest-release.sh
#
# The manifests name a release and its assets by the names in
# packaging/README.md. A release from before the rename to Relayweft
# (v0.2.0 and older: sy-<os>-<arch>) has none of them, so its manifests
# cannot install anything. Then:
#   - if the latest release has the assets, the manifests are stale:
#     error (render them for that release);
#   - if no release has them yet (between the rename and the first
#     Relayweft release), the checks are skipped with a notice.
# Otherwise the checks run in full, so real drift fails them.
#
# It writes ready=true|false and version=<the manifests' version> to
# $GITHUB_OUTPUT when that is set. RELEASES_URL and LATEST_TAG override
# where the release files come from and which tag is the latest (tests).
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
root="$(dirname "$here")"
releases="${RELEASES_URL:-https://github.com/sparkz400/relayweft/releases/download}"
# The binaries the manifests install (packaging/README.md, release.yml).
assets="rw-windows-amd64.exe rw-windows-arm64.exe rw-linux-amd64 rw-linux-arm64 rw-darwin-amd64 rw-darwin-arm64"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

output() {
  echo "$1=$2"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "$1=$2" >> "$GITHUB_OUTPUT"
  fi
}

version="$(sed -n 's|.*/releases/download/v\([^/]*\)/.*|\1|p' "$root/Formula/relayweft.rb" | head -n 1)"
if [ -z "$version" ]; then
  echo "::error::Formula/relayweft.rb names no release"
  exit 1
fi
# All manifests follow one release.
scoop="$(sed -n 's|^ *"version": "\([^"]*\)".*|\1|p' "$here/scoop/rw.json" | head -n 1)"
aur="$(sed -n 's|^pkgver=||p' "$here/aur/PKGBUILD")"
if [ "$scoop" != "$version" ] || [ "$aur" != "$version" ]; then
  echo "::error::the manifests name different releases (Formula $version, scoop $scoop, PKGBUILD $aur); run packaging/render-manifests.sh for one release and commit"
  exit 1
fi
output version "$version"

# missing VERSION: prints the assets the release's checksums.txt lacks.
# A release without a checksums.txt is an error, not a skip.
missing() {
  if ! curl -fsSL --retry 3 -o "$tmp/sums-$1" "$releases/v$1/checksums.txt"; then
    echo "::error::cannot download checksums.txt of v$1 (is there a release v$1?)" >&2
    exit 1
  fi
  for a in $assets; do
    if ! awk -v n="$a" '{ f=$2; sub(/^\*/, "", f); if (f == n) found=1 } END { exit !found }' "$tmp/sums-$1"; then
      printf '%s ' "$a"
    fi
  done
}

lacks="$(missing "$version")"
lacks="${lacks% }"
latest="${LATEST_TAG:-$(gh release view --repo sparkz400/relayweft --json tagName -q .tagName)}"
latest="${latest#v}"
if [ -z "$lacks" ]; then
  echo "v$version has every asset the manifests use"
  if [ -n "$latest" ] && [ "$latest" != "$version" ]; then
    echo "::warning::the manifests are for v$version, the latest release is v$latest: run packaging/render-manifests.sh $latest and commit"
  fi
  output ready true
  exit 0
fi

if [ -n "$latest" ] && [ "$latest" != "$version" ]; then
  latest_lacks="$(missing "$latest")"
  if [ -z "$latest_lacks" ]; then
    echo "::error::the manifests are for v$version, which has no $lacks; the latest release v$latest has them: run packaging/render-manifests.sh $latest and commit"
    exit 1
  fi
fi
echo "::notice::the manifests are for v$version, which predates the asset names they use (it has no $lacks), and no newer release has them yet (latest: v$latest). Installing and comparing the manifests is skipped until the first release with them; then render the manifests for it."
output ready false
