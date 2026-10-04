#!/bin/sh
# Fills the scoop manifest and the winget templates for a published release.
#
#   packaging/render-manifests.sh 1.2.3 [checksums.txt] [winget-out-dir]
#
# Without a checksums file it downloads the release's checksums.txt with
# the gh CLI (which also works for a private repo). It updates
# packaging/scoop/sy.json in place and writes the rendered winget manifests
# to winget-out-dir (default: ./winget-out/<version>), ready for a
# microsoft/winget-pkgs pull request.
set -eu

version="${1:?usage: render-manifests.sh VERSION [checksums.txt] [out-dir]}"
version="${version#v}"
here="$(cd "$(dirname "$0")" && pwd)"
sums="${2:-}"
out="${3:-winget-out/$version}"

if [ -z "$sums" ]; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  gh release download "v$version" --repo sparkz400/switchyard --pattern checksums.txt --dir "$tmp"
  sums="$tmp/checksums.txt"
fi

hash_of() {
  h="$(awk -v n="$1" '{ f=$2; sub(/^\*/, "", f); if (f == n) print $1 }' "$sums")"
  if [ -z "$h" ]; then
    echo "no checksum for $1 in $sums" >&2
    exit 1
  fi
  echo "$h"
}
x64="$(hash_of sy-windows-amd64.exe)"
arm64="$(hash_of sy-windows-arm64.exe)"
# The release's own date, not today's: rendering can happen days later.
today="$(gh release view "v$version" --repo sparkz400/switchyard --json publishedAt -q .publishedAt 2>/dev/null | cut -c1-10 || true)"
[ -n "$today" ] || today="$(date -u +%Y-%m-%d)"

# Scoop: the manifest's layout is fixed, so line-based edits are safe.
scoop="$here/scoop/sy.json"
sed -i.bak \
  -e "s|\"version\": \"[^\"]*\"|\"version\": \"$version\"|" \
  -e "s|/download/v[^/\$]*/sy-windows|/download/v$version/sy-windows|" \
  "$scoop"
# Each hash line follows its url line; replace them in order (x64, arm64).
awk -v a="$x64" -v b="$arm64" '
  /"hash": "[0-9a-fA-F]+"/ { n++; sub(/"hash": "[0-9a-fA-F]+"/, "\"hash\": \"" (n == 1 ? a : b) "\"") }
  { print }
' "$scoop" > "$scoop.tmp"
mv "$scoop.tmp" "$scoop"
rm -f "$scoop.bak"

mkdir -p "$out"
for f in "$here"/winget/*.yaml; do
  sed -e '/^# Template:/d' \
    -e "s|{{VERSION}}|$version|g" \
    -e "s|{{SHA256_X64}}|$x64|g" \
    -e "s|{{SHA256_ARM64}}|$arm64|g" \
    -e "s|{{RELEASE_DATE}}|$today|g" \
    "$f" > "$out/$(basename "$f")"
done

echo "updated $scoop"
echo "wrote winget manifests to $out"
