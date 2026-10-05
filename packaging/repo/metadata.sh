#!/bin/sh
# Builds and signs the repository metadata. build-pages.sh runs it in two
# pinned containers; it is not meant to be run by hand.
#
#   metadata.sh deb-rpm IN-DIR OUT-DIR   (Debian: apt-ftparchive, createrepo_c, gpg)
#   metadata.sh apk IN-DIR OUT-DIR       (Alpine: apk index, abuild-sign)
#
# IN-DIR has one folder per version with relayweft-linux-<arch>.<format>,
# already checked against the release's checksums.txt. Every package is
# checked against the keys (verify-packages.sh) before it goes in; one
# that fails is left out with a warning. The keys come from
# PACKAGE_GPG_KEY and PACKAGE_APK_KEY, the URL of the site from
# RW_REPO_URL.
set -eu

mode="${1:?usage: metadata.sh deb-rpm|apk IN-DIR OUT-DIR}"
in="${2:?usage: metadata.sh deb-rpm|apk IN-DIR OUT-DIR}"
out="${3:?usage: metadata.sh deb-rpm|apk IN-DIR OUT-DIR}"
url="${RW_REPO_URL:?RW_REPO_URL is not set}"
src="$(pwd)"

warn() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning::$*"; else echo "warning: $*" >&2; fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
(umask 077 && printf '%s\n' "$PACKAGE_APK_KEY" > "$tmp/relayweft-apk.rsa")
openssl rsa -in "$tmp/relayweft-apk.rsa" -pubout -out "$tmp/relayweft-apk.rsa.pub" 2>/dev/null
GNUPGHOME="$tmp/gnupg"
export GNUPGHOME
(umask 077 && mkdir "$GNUPGHOME")
printf '%s\n' "$PACKAGE_GPG_KEY" | gpg --batch --quiet --import 2>/dev/null
gpg --batch --armor --export > "$tmp/relayweft-signing-key.asc"
sign() { gpg --batch --yes --digest-algo SHA512 "$@"; }

# verified FILE: whether FILE is signed with the keys.
verified() {
  if sh "$src/packaging/verify-packages.sh" "$tmp/relayweft-signing-key.asc" "$tmp/relayweft-apk.rsa.pub" "$1" >"$tmp/verify.out" 2>&1; then
    return 0
  fi
  warn "left out $1, its signature does not verify: $(tr '\n' ' ' < "$tmp/verify.out")"
  return 1
}

# arch_name ARCH: the rpm and apk name of a Debian/Go arch.
arch_name() {
  case "$1" in
    amd64) echo x86_64 ;;
    arm64) echo aarch64 ;;
    *) echo "$1" ;;
  esac
}

case "$mode" in
  deb-rpm)
    apt="$out/apt"
    rpm="$out/rpm"
    mkdir -p "$apt/pool/main/r/relayweft" "$rpm/packages"
    deb_arches=""
    for dir in "$in"/*/; do
      v="$(basename "$dir")"
      for f in "$dir"relayweft-linux-*.deb "$dir"relayweft-linux-*.rpm; do
        [ -f "$f" ] || continue
        verified "$f" || continue
        arch="${f##*relayweft-linux-}"
        arch="${arch%.*}"
        case "$f" in
          *.deb)
            cp "$f" "$apt/pool/main/r/relayweft/relayweft_${v}-1_${arch}.deb"
            case " $deb_arches " in *" $arch "*) ;; *) deb_arches="$deb_arches $arch" ;; esac
            ;;
          *.rpm) cp "$f" "$rpm/packages/relayweft-${v}-1.$(arch_name "$arch").rpm" ;;
        esac
      done
    done
    deb_arches="${deb_arches# }"

    # apt: dists/stable/main/binary-<arch>/Packages and a signed Release.
    cd "$apt"
    for arch in $deb_arches; do
      mkdir -p "dists/stable/main/binary-$arch"
      apt-ftparchive --arch "$arch" packages pool > "dists/stable/main/binary-$arch/Packages"
      gzip -9n < "dists/stable/main/binary-$arch/Packages" > "dists/stable/main/binary-$arch/Packages.gz"
    done
    mkdir -p dists/stable
    apt-ftparchive \
      -o APT::FTPArchive::Release::Origin=Relayweft \
      -o APT::FTPArchive::Release::Label=Relayweft \
      -o APT::FTPArchive::Release::Suite=stable \
      -o APT::FTPArchive::Release::Codename=stable \
      -o "APT::FTPArchive::Release::Architectures=$deb_arches" \
      -o APT::FTPArchive::Release::Components=main \
      -o "APT::FTPArchive::Release::Description=Relayweft (rw) from its GitHub releases" \
      release dists/stable > "$tmp/Release"
    mv "$tmp/Release" dists/stable/Release
    sign --clearsign -o dists/stable/InRelease dists/stable/Release
    sign --armor --detach-sign -o dists/stable/Release.gpg dists/stable/Release
    gpg --batch --export > relayweft.gpg
    cp "$tmp/relayweft-signing-key.asc" relayweft-signing-key.asc
    echo "deb [signed-by=/etc/apt/keyrings/relayweft.gpg] $url/apt stable main" > relayweft.list
    echo "apt: $(find pool -name '*.deb' | wc -l) packages ($deb_arches)"

    # rpm: one repository for every arch; dnf picks the right one.
    cd "$rpm"
    createrepo_c --quiet --general-compress-type gz .
    sign --armor --detach-sign -o repodata/repomd.xml.asc repodata/repomd.xml
    cp "$tmp/relayweft-signing-key.asc" relayweft-signing-key.asc
    cat > relayweft.repo <<EOF
[relayweft]
name=Relayweft
baseurl=$url/rpm
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$url/rpm/relayweft-signing-key.asc
EOF
    echo "rpm: $(find packages -name '*.rpm' | wc -l) packages"
    ;;

  apk)
    apk_out="$out/apk"
    mkdir -p "$apk_out"
    for dir in "$in"/*/; do
      v="$(basename "$dir")"
      for f in "$dir"relayweft-linux-*.apk; do
        [ -f "$f" ] || continue
        verified "$f" || continue
        arch="${f##*relayweft-linux-}"
        arch="$(arch_name "${arch%.apk}")"
        mkdir -p "$apk_out/$arch"
        cp "$f" "$apk_out/$arch/relayweft-${v}-r1.apk"
      done
    done
    # abuild-sign names the signature after the public key's file name,
    # which is what apk looks for in /etc/apk/keys.
    cp "$tmp/relayweft-apk.rsa.pub" "$apk_out/relayweft-apk.rsa.pub"
    # apk index checks each package's signature too, against this key only.
    mkdir "$tmp/keys"
    cp "$tmp/relayweft-apk.rsa.pub" "$tmp/keys/"
    n=0
    for d in "$apk_out"/*/; do
      [ -d "$d" ] || continue
      (cd "$d" && apk --keys-dir "$tmp/keys" index --quiet -o APKINDEX.tar.gz -d "Relayweft" ./*.apk &&
        abuild-sign -q -k "$tmp/relayweft-apk.rsa" -p relayweft-apk.rsa.pub APKINDEX.tar.gz)
      n=$((n + $(find "$d" -name '*.apk' | wc -l)))
    done
    echo "apk: $n packages"
    ;;

  *)
    echo "unknown mode $mode" >&2
    exit 2
    ;;
esac

# The files belong to the user who ran build-pages.sh, not to the
# container's root.
if [ -n "${HOST_UID:-}" ]; then
  chown -R "$HOST_UID:${HOST_GID:-$HOST_UID}" "$out"
fi
