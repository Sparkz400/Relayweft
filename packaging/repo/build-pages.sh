#!/bin/sh
# Builds the signed apt, rpm and apk repositories for GitHub Pages:
#
#   packaging/repo/build-pages.sh SITE-DIR
#
#   SITE-DIR/apt/  dists/stable/ (InRelease, Release.gpg), pool/,
#                  relayweft.list, relayweft.gpg (the keyring for apt)
#   SITE-DIR/rpm/  repodata/ (repomd.xml.asc), packages/, relayweft.repo,
#                  relayweft-signing-key.asc
#   SITE-DIR/apk/  x86_64/ and aarch64/ (APKINDEX.tar.gz), relayweft-apk.rsa.pub
#
# pages.yml runs it after building the site into _site, then deploys. It
# takes the .deb, .rpm and .apk packages of the last RW_REPO_KEEP signed
# releases (those with relayweft-signing-key.asc, so from v0.4.0), checks
# them against the release's checksums.txt and their signatures against
# the keys, and builds the metadata in pinned Debian and Alpine images.
#
# Environment:
#   PACKAGE_GPG_KEY, PACKAGE_APK_KEY  the signing keys (repository secrets,
#       the same as the release's). Without them it builds nothing and
#       says so, unless PACKAGE_SIGNING_REQUIRED=true and packaging/keys/
#       has the public keys: then it fails.
#   GH_TOKEN or GITHUB_TOKEN  read access to the releases (optional for a
#       public repository; avoids the API's rate limit).
#   RW_REPO_URL     where SITE-DIR is served (https://sparkz400.github.io/Relayweft)
#   RW_REPO_KEEP    how many releases to keep (3)
#   RW_REPO_ARCHES  amd64 arm64
#   RW_REPO_SOURCE  a local folder with one folder per version (the release
#                   assets, with checksums.txt) instead of the releases; for tests
#   RW_DOCKER_ARGS  extra `docker run` arguments, such as --cpus 2
#
# Needs sh, curl, jq, sha256sum, sort -V and Docker.
set -eu

site="${1:?usage: build-pages.sh SITE-DIR}"
url="${RW_REPO_URL:-https://sparkz400.github.io/Relayweft}"
url="${url%/}"
keep="${RW_REPO_KEEP:-3}"
arches="${RW_REPO_ARCHES:-amd64 arm64}"
github="${RW_REPO_GITHUB:-Sparkz400/Relayweft}"
token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
# debian:trixie-slim and alpine:3.22 (multi-arch indexes).
debian_image="debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a"
alpine_image="alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"

notice() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::notice::$*"; else echo "notice: $*"; fi
}
warn() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning::$*"; else echo "warning: $*" >&2; fi
}
die() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::error::$*"; else echo "error: $*" >&2; fi
  exit 1
}

root="$(cd "$(dirname "$0")/../.." && pwd)"
keys_dir="${PACKAGE_KEYS_DIR:-$root/packaging/keys}"
case "$keep" in '' | *[!0-9]* | 0) die "RW_REPO_KEEP must be a number of releases, not '$keep'" ;; esac

if [ -z "${PACKAGE_GPG_KEY:-}" ] && [ -z "${PACKAGE_APK_KEY:-}" ]; then
  if [ "${PACKAGE_SIGNING_REQUIRED:-}" = true ] && [ -f "$keys_dir/relayweft-signing-key.asc" ]; then
    die "PACKAGE_GPG_KEY and PACKAGE_APK_KEY are not set, and $keys_dir has the public keys: the site would lose its package repositories"
  fi
  notice "no apt, rpm or apk repository: PACKAGE_GPG_KEY and PACKAGE_APK_KEY are not set"
  exit 0
fi
[ -n "${PACKAGE_GPG_KEY:-}" ] && [ -n "${PACKAGE_APK_KEY:-}" ] ||
  die "only one of PACKAGE_GPG_KEY and PACKAGE_APK_KEY is set; set both, or neither"
for tool in docker sha256sum sort; do
  command -v "$tool" >/dev/null 2>&1 || die "build-pages.sh needs $tool"
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/in" "$work/out"

# assets VERSION-DIR: the names to take from a release.
assets() {
  echo checksums.txt
  for arch in $arches; do
    for format in deb rpm apk; do echo "relayweft-linux-$arch.$format"; done
  done
}

# check_sums DIR: every package in DIR matches its line in checksums.txt.
check_sums() {
  for name in $(assets); do
    [ "$name" = checksums.txt ] && continue
    want="$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$1/checksums.txt")"
    got="$(sha256sum "$1/$name" | cut -d' ' -f1)"
    [ -n "$want" ] && [ "$want" = "$got" ] || die "$1/$name does not match its checksums.txt"
  done
}

if [ -n "${RW_REPO_SOURCE:-}" ]; then
  versions="$(for d in "$RW_REPO_SOURCE"/*/; do
    [ -f "${d}relayweft-signing-key.asc" ] && basename "$d"
  done | sort -V -r | head -n "$keep")"
  for v in $versions; do
    mkdir "$work/in/$v"
    for name in $(assets); do
      cp "$RW_REPO_SOURCE/$v/$name" "$work/in/$v/" || die "$RW_REPO_SOURCE/$v has no $name"
    done
  done
else
  for tool in curl jq; do
    command -v "$tool" >/dev/null 2>&1 || die "build-pages.sh needs $tool"
  done
  # The token goes to curl on stdin, not on its command line.
  api() {
    if [ -n "$token" ]; then
      printf 'Authorization: Bearer %s\n' "$token" |
        curl -fsSL --retry 3 -H "Accept: application/vnd.github+json" -H @- "$@"
    else
      curl -fsSL --retry 3 -H "Accept: application/vnd.github+json" "$@"
    fi
  }
  api "https://api.github.com/repos/$github/releases?per_page=100" > "$work/releases.json" ||
    die "cannot list the releases of $github"
  want="$(assets | jq -R . | jq -s .)"
  # Published releases (no drafts or pre-releases) with every package,
  # checksums.txt and the signing key, newest first.
  versions="$(jq -r --argjson want "$want" '
    .[] | select(.draft == false and .prerelease == false)
    | select([.assets[].name] as $have
        | ($want + ["relayweft-signing-key.asc"]) | all(. as $n | $have | index($n)))
    | .tag_name | ltrimstr("v")' "$work/releases.json" | sort -V -r | head -n "$keep")"
  for v in $versions; do
    mkdir "$work/in/$v"
    for name in $(assets); do
      # The asset's API URL works for private repositories too.
      asset_url="$(jq -r --arg t "v$v" --arg n "$name" \
        '.[] | select(.tag_name == $t) | .assets[] | select(.name == $n) | .url' "$work/releases.json")"
      api -H "Accept: application/octet-stream" -o "$work/in/$v/$name" "$asset_url" ||
        die "cannot download $name of v$v"
    done
  done
fi

if [ -z "$versions" ]; then
  warn "no signed release has the packages yet: the repositories are empty"
fi
for v in $versions; do
  check_sums "$work/in/$v"
done
# shellcheck disable=SC2086 # one line
echo releases: ${versions:-none}

# Docker needs host paths; on Windows (Git Bash) that is the C:/... form.
host_path() {
  if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi
}
run() {
  image="$1"
  shift
  # shellcheck disable=SC2086 # RW_DOCKER_ARGS is a list of arguments
  MSYS_NO_PATHCONV=1 docker run --rm ${RW_DOCKER_ARGS:-} \
    -e PACKAGE_GPG_KEY -e PACKAGE_APK_KEY -e GITHUB_ACTIONS \
    -e RW_REPO_URL="$url" -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    -v "$(host_path "$root"):/src:ro" -v "$(host_path "$work"):/work" -w /src \
    "$image" sh -c "$1"
}
run "$debian_image" '
  apt-get update -qq &&
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    gnupg gpgv apt-utils createrepo-c rpm binutils openssl >/dev/null &&
  sh packaging/repo/metadata.sh deb-rpm /work/in /work/out'
run "$alpine_image" '
  apk add -q abuild gnupg openssl python3 &&
  sh packaging/repo/metadata.sh apk /work/in /work/out'

mkdir -p "$site"
for d in apt rpm apk; do
  rm -rf "${site:?}/$d"
  cp -R "$work/out/$d" "$site/$d"
done
echo "built $site/apt, $site/rpm and $site/apk for $url"
