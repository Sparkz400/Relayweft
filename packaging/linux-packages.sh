#!/bin/sh
# Builds the .deb, .rpm and .apk packages from the linux binaries in a
# dist folder, with nfpm (pinned below and checked against its SHA-256).
#
#   packaging/linux-packages.sh 1.2.3 [dist] [arch...]
#
# Run it from the repo root on linux/amd64 (the runner of release.yml).
# It reads dist/sy-linux-<arch> and writes dist/switchyard-linux-<arch>.deb,
# .rpm and .apk; the arches default to amd64 and arm64. These names are
# part of the release asset contract (see packaging/README.md).
set -eu

version="${1:?usage: linux-packages.sh VERSION [dist] [arch...]}"
version="${version#v}"
dist="${2:-dist}"
[ $# -ge 2 ] && shift 2 || shift $#
arches="${*:-amd64 arm64}"

# nfpm from an immutable GitHub release. The hash is the Linux_x86_64
# tarball's line in that release's checksums.txt, whose signature was
# checked once with
#   cosign verify-blob --bundle checksums.txt.sigstore.json \
#     --certificate-identity-regexp '^https://github.com/goreleaser/nfpm/' \
#     --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
# Do the same when bumping it.
nfpm_version=2.47.0
nfpm_sha256=0660ca602b2d2d2ae4781a06c692b3eeb9d437ffea05b831d76e41f4a3188783

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
tarball="nfpm_${nfpm_version}_Linux_x86_64.tar.gz"
curl -fsSL --retry 3 -o "$tmp/$tarball" \
  "https://github.com/goreleaser/nfpm/releases/download/v$nfpm_version/$tarball"
echo "$nfpm_sha256  $tmp/$tarball" | sha256sum -c -
tar -xzf "$tmp/$tarball" -C "$tmp" nfpm
nfpm="$tmp/nfpm"

mkdir -p build
for arch in $arches; do
  bin="$dist/sy-linux-$arch"
  if [ ! -f "$bin" ]; then
    echo "missing $bin" >&2
    exit 1
  fi
  cp "$bin" build/sy
  chmod 755 build/sy
  for format in deb rpm apk; do
    out="$dist/switchyard-linux-$arch.$format"
    VERSION="$version" ARCH="$arch" "$nfpm" package \
      -f packaging/nfpm.yaml -p "$format" -t "$out"
  done
done
rm -f build/sy
