#!/bin/sh
# Installs a built package in a throwaway container and checks it. The
# packaging workflow runs it; locally, with Docker:
#
#   docker run --rm -v "$PWD:/src:ro" -w /src ubuntu:24.04 \
#     sh packaging/test-install.sh deb dist/relayweft-linux-amd64.deb 0.0.1
#
#   deb: Debian/Ubuntu   rpm: Fedora   apk: Alpine
#   aur: Arch Linux, builds packaging/aur/PKGBUILD with makepkg (the version
#        argument is unused; it installs the PKGBUILD's release)
#
# For deb, rpm and apk, build rw as an old version (0.0.1): `rw update`
# then sees a newer release and must refuse to replace the package's
# binary. That part asks the GitHub API (set GH_TOKEN to avoid its rate
# limit for anonymous calls).
set -eu

format="${1:?usage: test-install.sh deb|rpm|apk|aur [package] [version]}"
pkg="${2:-}"
version="${3:-}"
case "$pkg" in
  "" | /*) ;;
  *) pkg="./$pkg" ;; # apt and dnf take a file only with a path
esac

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

case "$format" in
  deb)
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq ca-certificates "$pkg" >/dev/null
    owner="$(dpkg-query -S /usr/bin/rw)"
    list="dpkg -L relayweft"
    remove="apt-get remove -y -qq relayweft"
    ;;
  rpm)
    dnf install -y -q "$pkg" >/dev/null
    owner="$(rpm -qf /usr/bin/rw)"
    list="rpm -ql relayweft"
    remove="dnf remove -y -q relayweft"
    ;;
  apk)
    apk add -q --allow-untrusted ca-certificates "$pkg"
    owner="$(apk info --who-owns /usr/bin/rw)"
    list="apk info -qL relayweft"
    remove="apk del -q relayweft"
    ;;
  aur)
    pacman -Syu --noconfirm --needed -q base-devel git >/dev/null # git: the package depends on it
    useradd -m builder
    work=/home/builder/pkg
    mkdir -p "$work"
    cp packaging/aur/PKGBUILD "$work/"
    chown -R builder "$work"
    # The committed .SRCINFO must be what makepkg makes of the PKGBUILD.
    su builder -c "cd $work && makepkg --printsrcinfo" > /tmp/srcinfo
    tr -d '\r' < packaging/aur/.SRCINFO | diff -u - /tmp/srcinfo ||
      fail ".SRCINFO is out of date (run packaging/render-manifests.sh)"
    # makepkg checks every download against the PKGBUILD's sha256sums.
    su builder -c "cd $work && makepkg --noconfirm"
    pacman -U --noconfirm "$work"/*.pkg.tar.zst >/dev/null
    owner="$(pacman -Qo /usr/bin/rw)"
    version="$(sed -n 's/^pkgver=//p' packaging/aur/PKGBUILD)"
    list="pacman -Qlq relayweft-bin"
    remove="pacman -R --noconfirm relayweft-bin"
    ;;
  *)
    fail "unknown format $format"
    ;;
esac

echo "owner: $owner"
got="$(rw version)"
[ "$got" = "relayweft $version" ] || fail "rw version says '$got', want 'relayweft $version'"
echo "ok: $got"
# The package's file list, not the disk: the ubuntu image drops
# /usr/share/doc on install.
files="$($list)"
echo "$files" | grep -q 'usr/share/doc/.*/README.md$' || fail "no README in the package"
echo "$files" | grep -Eq 'usr/share/(licenses/.*/LICENSE|doc/relayweft/copyright)$' ||
  fail "no license file in the package"
echo "ok: README and license"

if [ "$format" != aur ]; then
  out="$(rw update --check)" || fail "rw update --check failed: $out"
  echo "$out"
  echo "$out" | grep -q "installed with" || fail "rw update --check does not name the package manager"
  if out="$(rw update --yes 2>&1)"; then
    fail "rw update replaced the package's binary: $out"
  fi
  echo "$out" | grep -q "leaves it alone" || fail "unexpected rw update output: $out"
  [ "$(rw version)" = "relayweft $version" ] || fail "the binary changed"
  echo "ok: rw update leaves the package alone"
fi

$remove >/dev/null 2>&1 || fail "removing the package failed"
[ ! -e /usr/bin/rw ] || fail "/usr/bin/rw is still there after removing the package"
echo "ok: removed"
echo "PASS: $format"
