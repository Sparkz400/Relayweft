#!/bin/sh
# Builds the .deb, .rpm and .apk packages from the linux binaries in a
# dist folder, with nfpm (pinned below and checked against its SHA-256).
#
#   packaging/linux-packages.sh 1.2.3 [dist] [arch...]
#
# Run it from the repo root on linux/amd64 (the runner of release.yml).
# It reads dist/rw-linux-<arch> and writes dist/relayweft-linux-<arch>.deb,
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

# Signing. With both keys set, every package is signed and checked (the
# keys come from repository secrets; packaging/README.md, "Signing keys"):
#   PACKAGE_GPG_KEY  armored GPG private key, no passphrase: .deb and .rpm
#   PACKAGE_APK_KEY  RSA private key (PEM), no passphrase: .apk
# The public keys are written next to the packages as
# relayweft-signing-key.asc and relayweft-apk.rsa.pub (release assets) and
# must match the ones in packaging/keys/ when those are there. With
# neither key set (forks, pull requests) the packages are unsigned, with a
# notice; with one, or with PACKAGE_SIGNING_REQUIRED=true and the public
# keys in packaging/keys/, nothing is built.
keys_dir="${PACKAGE_KEYS_DIR:-packaging/keys}"
notice() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::notice::$*"; else echo "notice: $*"; fi
}
die() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::error::$*"; else echo "error: $*" >&2; fi
  exit 1
}
PACKAGE_GPG_KEY_FILE=""
PACKAGE_APK_KEY_FILE=""
rm -f "$dist/relayweft-signing-key.asc" "$dist/relayweft-apk.rsa.pub"
if [ -n "${PACKAGE_GPG_KEY:-}" ] && [ -n "${PACKAGE_APK_KEY:-}" ]; then
  for tool in gpg openssl; do
    command -v "$tool" >/dev/null 2>&1 || die "signing needs $tool"
  done
  (umask 077 && printf '%s\n' "$PACKAGE_GPG_KEY" > "$tmp/gpg.key" && printf '%s\n' "$PACKAGE_APK_KEY" > "$tmp/apk.rsa")
  GNUPGHOME="$tmp/gnupg"
  export GNUPGHOME
  (umask 077 && mkdir "$GNUPGHOME")
  gpg --batch --quiet --import "$tmp/gpg.key" 2>/dev/null || die "PACKAGE_GPG_KEY is not a GPG private key"
  fpr="$(gpg --batch --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }')"
  [ -n "$fpr" ] || die "PACKAGE_GPG_KEY has no secret key"
  gpg --batch --armor --export "$fpr" > "$tmp/relayweft-signing-key.asc"
  openssl rsa -in "$tmp/apk.rsa" -pubout -out "$tmp/relayweft-apk.rsa.pub" 2>/dev/null ||
    die "PACKAGE_APK_KEY is not an RSA private key in PEM format (or has a passphrase)"
  if [ -f "$keys_dir/relayweft-signing-key.asc" ]; then
    committed="$(gpg --batch --with-colons --import-options show-only --import "$keys_dir/relayweft-signing-key.asc" 2>/dev/null |
      awk -F: '$1 == "fpr" { print $10; exit }')"
    [ "$committed" = "$fpr" ] ||
      die "PACKAGE_GPG_KEY ($fpr) is not the key in $keys_dir/relayweft-signing-key.asc (${committed:-none})"
  fi
  if [ -f "$keys_dir/relayweft-apk.rsa.pub" ]; then
    [ "$(openssl pkey -pubin -in "$keys_dir/relayweft-apk.rsa.pub" -outform DER 2>/dev/null | sha256sum)" = \
      "$(openssl pkey -pubin -in "$tmp/relayweft-apk.rsa.pub" -outform DER | sha256sum)" ] ||
      die "PACKAGE_APK_KEY is not the key in $keys_dir/relayweft-apk.rsa.pub"
  fi
  cp "$tmp/relayweft-signing-key.asc" "$tmp/relayweft-apk.rsa.pub" "$dist/"
  PACKAGE_GPG_KEY_FILE="$tmp/gpg.key"
  PACKAGE_APK_KEY_FILE="$tmp/apk.rsa"
  echo "signing with GPG key $fpr and the RSA key in relayweft-apk.rsa.pub"
elif [ -n "${PACKAGE_GPG_KEY:-}" ] || [ -n "${PACKAGE_APK_KEY:-}" ]; then
  die "only one of PACKAGE_GPG_KEY and PACKAGE_APK_KEY is set; set both to sign, or neither"
elif [ "${PACKAGE_SIGNING_REQUIRED:-}" = true ] && [ -f "$keys_dir/relayweft-signing-key.asc" ]; then
  die "PACKAGE_GPG_KEY and PACKAGE_APK_KEY are not set, and $keys_dir has the public keys: this release must be signed"
else
  notice "the Linux packages are not signed (PACKAGE_GPG_KEY and PACKAGE_APK_KEY are not set)"
fi
export PACKAGE_GPG_KEY_FILE PACKAGE_APK_KEY_FILE

tarball="nfpm_${nfpm_version}_Linux_x86_64.tar.gz"
curl -fsSL --retry 3 -o "$tmp/$tarball" \
  "https://github.com/goreleaser/nfpm/releases/download/v$nfpm_version/$tarball"
echo "$nfpm_sha256  $tmp/$tarball" | sha256sum -c -
tar -xzf "$tmp/$tarball" -C "$tmp" nfpm
nfpm="$tmp/nfpm"

mkdir -p build
# The shell completion scripts (nfpm.yaml installs them) are the same for
# every arch: print them with the binary this machine can run.
case "$(uname -m)" in
  x86_64 | amd64) host=amd64 ;;
  aarch64 | arm64) host=arm64 ;;
  *) host="" ;;
esac
if [ -z "$host" ] || [ ! -f "$dist/rw-linux-$host" ]; then
  echo "the completion scripts need $dist/rw-linux-<this machine's arch> ($(uname -m))" >&2
  exit 1
fi
mkdir -p build/completions
for shell in bash zsh fish; do
  "$dist/rw-linux-$host" completion "$shell" > "build/completions/rw.$shell"
done

for arch in $arches; do
  bin="$dist/rw-linux-$arch"
  if [ ! -f "$bin" ]; then
    echo "missing $bin" >&2
    exit 1
  fi
  cp "$bin" build/rw
  chmod 755 build/rw
  for format in deb rpm apk; do
    out="$dist/relayweft-linux-$arch.$format"
    VERSION="$version" ARCH="$arch" "$nfpm" package \
      -f packaging/nfpm.yaml -p "$format" -t "$out"
    if [ -n "$PACKAGE_GPG_KEY_FILE" ]; then
      sh packaging/verify-packages.sh "$dist/relayweft-signing-key.asc" "$dist/relayweft-apk.rsa.pub" "$out"
    fi
  done
done
rm -rf build/rw build/completions
