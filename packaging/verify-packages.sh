#!/bin/sh
# Checks that .deb, .rpm and .apk packages are signed with the given
# public keys. It fails for an unsigned package, one signed with another
# key and one changed after signing.
#
#   packaging/verify-packages.sh GPG-KEY.asc APK-KEY.rsa.pub PACKAGE...
#
# GPG-KEY.asc is the armored public key that signs the .deb and .rpm
# packages, APK-KEY.rsa.pub the RSA public key (PEM) that signs the .apk.
#
#   .deb  the debsigs "origin" signature nfpm writes (_gpgorigin) over
#         debian-binary, control.tar and data.tar; gpgv and ar.
#   .rpm  the header and package signatures; rpmkeys (the rpm package).
#   .apk  the .SIGN.RSA signature of the control part; python3 (to split
#         the package into its gzip parts) and openssl.
#
# apt does not check .deb signatures (it checks the repository's), but the
# signature still ties each .deb to the key.
set -eu

gpg_key="${1:?usage: verify-packages.sh GPG-KEY.asc APK-KEY.rsa.pub PACKAGE...}"
apk_key="${2:?usage: verify-packages.sh GPG-KEY.asc APK-KEY.rsa.pub PACKAGE...}"
shift 2
[ $# -gt 0 ] || { echo "verify-packages.sh: no packages given" >&2; exit 2; }

need() {
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || {
      echo "verify-packages.sh needs $tool" >&2
      exit 2
    }
  done
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fails=0
bad() {
  echo "BAD: $1: $2" >&2
  fails=$((fails + 1))
}

for pkg in "$@"; do
  [ -f "$pkg" ] || { bad "$pkg" "no such file"; continue; }
  case "$pkg" in
    *.deb)
      need gpg gpgv ar
      [ -f "$tmp/keyring.gpg" ] || gpg --batch --dearmor < "$gpg_key" > "$tmp/keyring.gpg"
      rm -rf "$tmp/deb" && mkdir "$tmp/deb"
      members="$(ar t "$pkg")" || { bad "$pkg" "not an ar archive"; continue; }
      if ! echo "$members" | grep -qx _gpgorigin; then
        bad "$pkg" "not signed (no _gpgorigin)"
        continue
      fi
      abs="$(cd "$(dirname "$pkg")" && pwd)/$(basename "$pkg")"
      (cd "$tmp/deb" && ar x "$abs")
      # The order of the signed data is the order in the archive.
      signed=""
      for m in $members; do
        case "$m" in debian-binary | control.tar* | data.tar*) signed="$signed $tmp/deb/$m" ;; esac
      done
      # shellcheck disable=SC2086 # the member paths have no spaces
      if cat $signed | gpgv --keyring "$tmp/keyring.gpg" "$tmp/deb/_gpgorigin" - >"$tmp/out" 2>&1; then
        echo "ok: $pkg"
      else
        bad "$pkg" "$(cat "$tmp/out")"
      fi
      ;;
    *.rpm)
      need rpmkeys
      if [ ! -d "$tmp/rpmdb" ]; then
        mkdir "$tmp/rpmdb"
        rpmkeys --dbpath "$tmp/rpmdb" --import "$gpg_key"
      fi
      # An unsigned package passes -K on its digests alone, so look for
      # the signatures: at least one (nfpm writes the header and the
      # header+payload one) must be OK, and nothing NOKEY or BAD.
      out="$(rpmkeys --dbpath "$tmp/rpmdb" -Kv "$pkg" 2>&1)" || { bad "$pkg" "$out"; continue; }
      if [ "$(echo "$out" | grep -c 'Signature, key ID [0-9a-f]*: OK$')" -ge 1 ] &&
        ! echo "$out" | grep -Eq 'NOKEY|NOTTRUSTED|BAD|NOT OK'; then
        echo "ok: $pkg"
      else
        bad "$pkg" "not signed with this key: $out"
      fi
      ;;
    *.apk)
      need python3 openssl
      rm -rf "$tmp/apk" && mkdir "$tmp/apk"
      # An .apk is three gzip streams: the signature, the control part
      # and the data. The signature is a tar with one .SIGN.RSA.<key>
      # (SHA-1) or .SIGN.RSA256.<key> file over the control stream.
      if ! python3 - "$pkg" "$tmp/apk" <<'PY'
import io, sys, tarfile, zlib
data = open(sys.argv[1], "rb").read()
parts, off = [], 0
while off < len(data) and len(parts) < 2:
    d = zlib.decompressobj(31)
    out = d.decompress(data[off:])
    if not d.eof:
        sys.exit("truncated gzip stream")
    end = len(data) - len(d.unused_data)
    parts.append((data[off:end], out))
    off = end
if len(parts) < 2:
    sys.exit("fewer than two gzip streams")
# The signature tar has no end-of-archive blocks; add them to read it.
sig = tarfile.open(fileobj=io.BytesIO(parts[0][1] + b"\0" * 1024))
names = [m.name for m in sig.getmembers()]
if len(names) != 1 or not names[0].startswith(".SIGN.RSA"):
    sys.exit("not signed: the first part has %s" % names)
name = names[0]
algo = "sha256" if name.startswith(".SIGN.RSA256.") else "sha1"
open(sys.argv[2] + "/sig", "wb").write(sig.extractfile(name).read())
open(sys.argv[2] + "/control.gz", "wb").write(parts[1][0])
open(sys.argv[2] + "/algo", "w").write(algo)
open(sys.argv[2] + "/name", "w").write(name)
PY
      then
        bad "$pkg" "cannot read the signature"
        continue
      fi
      if openssl dgst "-$(cat "$tmp/apk/algo")" -verify "$apk_key" -signature "$tmp/apk/sig" \
        "$tmp/apk/control.gz" >"$tmp/out" 2>&1; then
        echo "ok: $pkg ($(cat "$tmp/apk/name"))"
      else
        bad "$pkg" "$(cat "$tmp/out")"
      fi
      ;;
    *)
      bad "$pkg" "not a .deb, .rpm or .apk"
      ;;
  esac
done

if [ "$fails" -gt 0 ]; then
  echo "$fails package(s) failed the signature check" >&2
  exit 1
fi
