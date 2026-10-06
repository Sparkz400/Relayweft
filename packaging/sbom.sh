#!/bin/sh
# Writes a CycloneDX SBOM (JSON) for each rw binary in a dist folder, with
# cyclonedx-gomod (pinned below and checked against its SHA-256).
#
#   packaging/sbom.sh 1.2.3 [dist]
#
# Run it from the repo root on linux/amd64 (the runner of release.yml),
# after the build. For dist/rw-<os>-<arch>[.exe] it writes
# dist/rw-<os>-<arch>.cdx.json. These names are part of the release asset
# contract (see packaging/README.md).
#
# The SBOM is read from the binary itself (the module list Go embeds in
# it, `go version -m`), so it lists exactly what was linked: the modules,
# their versions and go.sum hashes, the Go version (as the "std"
# component) and the build settings, plus the binary's own SHA-256.
# GOOS and GOARCH are set to the binary's target only so the main
# component's package URL names that target; cyclonedx-gomod takes them
# from the environment, not from the binary.
set -eu

version="${1:?usage: sbom.sh VERSION [dist]}"
version="${version#v}"
dist="${2:-dist}"

# cyclonedx-gomod from an immutable GitHub release. The hash is the
# linux_amd64 tarball's line in that release's checksums file, whose
# signature was checked once with
#   cosign verify-blob --certificate cyclonedx-gomod_1.12.0_checksums.txt.pem \
#     --signature cyclonedx-gomod_1.12.0_checksums.txt.sig \
#     --certificate-identity https://github.com/CycloneDX/cyclonedx-gomod/.github/workflows/goreleaser.yml@refs/tags/v1.12.0 \
#     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
#     cyclonedx-gomod_1.12.0_checksums.txt
# Do the same when bumping it.
cdx_version=1.12.0
cdx_sha256=004b9f5cc595b797fb5423e2ae4c97bcf0f18c712ed2faee1640b09e5efd6d15

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
tarball="cyclonedx-gomod_${cdx_version}_linux_amd64.tar.gz"
curl -fsSL --retry 3 -o "$tmp/$tarball" \
  "https://github.com/CycloneDX/cyclonedx-gomod/releases/download/v$cdx_version/$tarball"
echo "$cdx_sha256  $tmp/$tarball" | sha256sum -c -
tar -xzf "$tmp/$tarball" -C "$tmp" cyclonedx-gomod
cdx="$tmp/cyclonedx-gomod"

found=0
for bin in "$dist"/rw-*; do
  name="$(basename "$bin")"
  case "$name" in
    *.cdx.json) continue ;;
  esac
  target="${name#rw-}"
  target="${target%.exe}"
  goos="${target%-*}"
  goarch="${target#*-}"
  out="$dist/rw-$target.cdx.json"
  # go version -m (which cyclonedx-gomod runs) wants an executable file.
  chmod +x "$bin"
  echo "SBOM for $name -> $out"
  GOOS="$goos" GOARCH="$goarch" "$cdx" bin -json -std -version "v$version" -output "$out" "$bin"
  found=$((found + 1))
done
if [ "$found" -eq 0 ]; then
  echo "no rw-* binaries in $dist" >&2
  exit 1
fi
