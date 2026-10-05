#!/bin/sh
# Tests package signing and the apt, rpm and apk repositories end to end,
# with throwaway keys made for the test (never real ones):
#
#   sh packaging/repo/test.sh
#
# Run it from the repo root on a machine with Go and Docker (Linux, or Git
# Bash on Windows). It starts one container at a time:
#   1. keys      makes two GPG and RSA key pairs: A (the repository's) and B.
#   2. packages  builds the packages of 0.0.1, 0.0.2 and 0.0.3 signed with
#                A and of 0.0.9 signed with B, and checks that
#                linux-packages.sh refuses half a key pair, a key that is not
#                the committed one and a missing key when signing is required.
#   3. build-pages.sh with RW_REPO_KEEP=3 and key A: 0.0.9 does not verify and
#      0.0.1 is too old, so the repositories have 0.0.2 and 0.0.3.
#   4. clients   Debian and Ubuntu (apt), Fedora (dnf), Alpine (apk): the
#                repository with key B is refused, then with key A rw 0.0.2
#                installs and upgrades to 0.0.3, and `rw update --check`
#                names the upgrade command.
# Step 4 needs the network (the distributions' own packages, such as git,
# and the GitHub API for `rw update`; set GH_TOKEN against its rate limit).
#
# The steps run inside the containers are this script's other modes.
set -eu

work=build/repo-test
image_debian=debian:trixie-slim
docker_args="${RW_DOCKER_ARGS:---cpus 2}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# The key pairs, made in the keys step.
key() { cat "$work/keys/$1"; }
# give_back: the files a container made belong to the user who runs the
# test, so that the next run can remove them.
give_back() {
  chmod -R a+rX "$work"
  if [ -n "${HOST_UID:-}" ]; then
    chown -R "$HOST_UID:${HOST_GID:-$HOST_UID}" "$work" 2>/dev/null || echo "note: cannot chown $work"
  fi
}

case "${1:-all}" in
  all)
    [ -f go.mod ] && [ -f packaging/repo/build-pages.sh ] || fail "run it from the repo root"
    rm -rf "$work"
    mkdir -p "$work/bin"
    for v in 0.0.1 0.0.2 0.0.3 0.0.9; do
      mkdir -p "$work/bin/$v"
      GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$v" \
        -o "$work/bin/$v/rw-linux-amd64" ./cmd/rw
    done
    if command -v cygpath >/dev/null 2>&1; then src="$(cygpath -m "$(pwd)")"; else src="$(pwd)"; fi
    run() {
      image="$1"
      shift
      echo "== $image: $*"
      # shellcheck disable=SC2086 # docker_args is a list of arguments
      MSYS_NO_PATHCONV=1 docker run --rm $docker_args -e GH_TOKEN -e GITHUB_ACTIONS \
        -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
        -v "$src:/src" -w /src "$image" sh packaging/repo/test.sh "$@"
    }
    run "$image_debian" keys
    run "$image_debian" packages

    site="$work/site"
    # Without keys: nothing, with a notice; with half a pair: an error;
    # without keys but required and committed: an error.
    out="$(env -u PACKAGE_GPG_KEY -u PACKAGE_APK_KEY sh packaging/repo/build-pages.sh "$site" 2>&1)" ||
      fail "build-pages.sh without keys failed: $out"
    echo "$out" | grep -q "no apt, rpm or apk repository" || fail "no notice without keys: $out"
    [ ! -e "$site" ] || fail "build-pages.sh without keys built $site"
    if out="$(PACKAGE_GPG_KEY="$(key a.gpg)" sh packaging/repo/build-pages.sh "$site" 2>&1)"; then
      fail "build-pages.sh built with only the GPG key: $out"
    fi
    if out="$(PACKAGE_SIGNING_REQUIRED=true PACKAGE_KEYS_DIR="$work/keys/committed" \
      sh packaging/repo/build-pages.sh "$site" 2>&1)"; then
      fail "build-pages.sh skipped signing although it is required: $out"
    fi
    echo "ok: build-pages.sh without keys"

    PACKAGE_GPG_KEY="$(key a.gpg)" PACKAGE_APK_KEY="$(key a.rsa)" \
      RW_REPO_SOURCE="$work/source" RW_REPO_URL=file:///site RW_REPO_ARCHES=amd64 RW_REPO_KEEP=3 \
      RW_DOCKER_ARGS="$docker_args" sh packaging/repo/build-pages.sh "$site" > "$work/build-pages.log" 2>&1 ||
      { cat "$work/build-pages.log"; fail "build-pages.sh failed"; }
    cat "$work/build-pages.log"
    grep -q "left out .*0.0.9" "$work/build-pages.log" || fail "0.0.9 (signed with key B) was not left out"
    for f in apt/dists/stable/InRelease apt/dists/stable/Release.gpg apt/relayweft.gpg apt/relayweft.list \
      apt/pool/main/r/relayweft/relayweft_0.0.2-1_amd64.deb apt/pool/main/r/relayweft/relayweft_0.0.3-1_amd64.deb \
      rpm/repodata/repomd.xml rpm/repodata/repomd.xml.asc rpm/relayweft.repo rpm/relayweft-signing-key.asc \
      rpm/packages/relayweft-0.0.2-1.x86_64.rpm rpm/packages/relayweft-0.0.3-1.x86_64.rpm \
      apk/relayweft-apk.rsa.pub apk/x86_64/APKINDEX.tar.gz apk/x86_64/relayweft-0.0.2-r1.apk apk/x86_64/relayweft-0.0.3-r1.apk; do
      [ -f "$site/$f" ] || fail "the site has no $f"
    done
    if find "$site" -name '*0.0.1*' -o -name '*0.0.9*' | grep -q .; then
      fail "the site has 0.0.1 or 0.0.9: $(find "$site" -name '*0.0.1*' -o -name '*0.0.9*')"
    fi
    grep -qx 'deb \[signed-by=/etc/apt/keyrings/relayweft.gpg\] file:///site/apt stable main' "$site/apt/relayweft.list" ||
      fail "unexpected relayweft.list: $(cat "$site/apt/relayweft.list")"
    echo "ok: the site has 0.0.2 and 0.0.3, signed"

    for c in "debian:stable deb" "ubuntu:24.04 deb" "fedora:latest rpm" "alpine:latest apk"; do
      # shellcheck disable=SC2086 # two words
      set -- $c
      echo "== $1: client $2"
      # shellcheck disable=SC2086
      MSYS_NO_PATHCONV=1 docker run --rm $docker_args -e GH_TOKEN \
        -v "$src:/src:ro" -v "$src/$site:/site:ro" -w /src "$1" sh packaging/repo/test.sh client "$2"
    done
    echo "PASS: package signing and repositories"
    ;;

  keys)
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends gnupg openssl >/dev/null
    mkdir -p "$work/keys/committed"
    for k in a b; do
      GNUPGHOME="$(mktemp -d)"
      export GNUPGHOME
      gpg --batch --quiet --passphrase '' --quick-gen-key "Relayweft test key $k <test-$k@example.invalid>" rsa3072 sign never
      gpg --batch --armor --export-secret-keys > "$work/keys/$k.gpg"
      gpg --batch --armor --export > "$work/keys/$k.asc"
      gpg --batch --export > "$work/keys/$k.keyring"
      openssl genrsa -out "$work/keys/$k.rsa" 2048 2>/dev/null
      openssl rsa -in "$work/keys/$k.rsa" -pubout -out "$work/keys/$k.rsa.pub" 2>/dev/null
    done
    # What packaging/keys/ would hold: the public keys of A.
    cp "$work/keys/a.asc" "$work/keys/committed/relayweft-signing-key.asc"
    cp "$work/keys/a.rsa.pub" "$work/keys/committed/relayweft-apk.rsa.pub"
    give_back
    echo "ok: keys A and B"
    ;;

  packages)
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      curl ca-certificates gnupg gpgv openssl rpm binutils python3 >/dev/null
    committed="$work/keys/committed"
    # build VERSION KEY: the packages of VERSION signed with KEY (a or b),
    # with checksums.txt, as a release has them.
    build() {
      d="$work/source/$1"
      mkdir -p "$d"
      cp "$work/bin/$1/rw-linux-amd64" "$d/"
      keys_dir=/nonexistent
      [ "$2" = a ] && keys_dir="$committed"
      PACKAGE_GPG_KEY="$(key "$2.gpg")" PACKAGE_APK_KEY="$(key "$2.rsa")" PACKAGE_KEYS_DIR="$keys_dir" \
        PACKAGE_SIGNING_REQUIRED=true sh packaging/linux-packages.sh "$1" "$d" amd64
      (cd "$d" && sha256sum rw-* relayweft-* > checksums.txt)
    }
    for v in 0.0.1 0.0.2 0.0.3; do build "$v" a; done
    build 0.0.9 b
    d="$work/source/0.0.3"
    for f in relayweft-signing-key.asc relayweft-apk.rsa.pub; do
      [ -f "$d/$f" ] || fail "linux-packages.sh did not write $f"
      cmp -s "$d/$f" "$committed/$f" || [ "$f" = relayweft-signing-key.asc ] || fail "$f is not key A"
    done
    sh packaging/verify-packages.sh "$committed/relayweft-signing-key.asc" "$committed/relayweft-apk.rsa.pub" \
      "$d"/relayweft-linux-amd64.deb "$d"/relayweft-linux-amd64.rpm "$d"/relayweft-linux-amd64.apk
    # The packages signed with B fail against A, each format.
    for f in "$work"/source/0.0.9/relayweft-linux-amd64.*; do
      if sh packaging/verify-packages.sh "$committed/relayweft-signing-key.asc" "$committed/relayweft-apk.rsa.pub" "$f"; then
        fail "$f (key B) verified against key A"
      fi
    done
    echo "ok: signed with A, and B's packages fail against A"

    # No half-signed sets, no other key than the committed one, and no
    # unsigned release when signing is required.
    neg="$work/negative"
    mkdir -p "$neg"
    cp "$work/bin/0.0.1/rw-linux-amd64" "$neg/"
    expect_refusal() {
      want="$1"
      shift
      if out="$(env "$@" sh packaging/linux-packages.sh 0.0.1 "$neg" amd64 2>&1)"; then
        fail "linux-packages.sh built with $*: $out"
      fi
      echo "$out" | grep -q "$want" || fail "linux-packages.sh with $*: want '$want', got: $out"
      ! ls "$neg"/relayweft-linux-* >/dev/null 2>&1 || fail "linux-packages.sh left packages behind with $*"
    }
    expect_refusal "only one of" PACKAGE_GPG_KEY="$(key a.gpg)" PACKAGE_APK_KEY=
    expect_refusal "only one of" PACKAGE_GPG_KEY= PACKAGE_APK_KEY="$(key a.rsa)"
    expect_refusal "is not the key in" PACKAGE_GPG_KEY="$(key b.gpg)" PACKAGE_APK_KEY="$(key a.rsa)" PACKAGE_KEYS_DIR="$committed"
    expect_refusal "is not the key in" PACKAGE_GPG_KEY="$(key a.gpg)" PACKAGE_APK_KEY="$(key b.rsa)" PACKAGE_KEYS_DIR="$committed"
    expect_refusal "must be signed" PACKAGE_GPG_KEY= PACKAGE_APK_KEY= PACKAGE_SIGNING_REQUIRED=true PACKAGE_KEYS_DIR="$committed"
    expect_refusal "has no secret key" PACKAGE_GPG_KEY="$(key a.asc)" PACKAGE_APK_KEY="$(key a.rsa)"
    # Without keys (a fork, a pull request): unsigned, with a notice, and
    # no public keys next to the packages.
    out="$(PACKAGE_GPG_KEY='' PACKAGE_APK_KEY='' sh packaging/linux-packages.sh 0.0.1 "$neg" amd64 2>&1)" ||
      fail "unsigned build failed: $out"
    echo "$out" | grep -q "not signed" || fail "no notice for unsigned packages: $out"
    [ ! -e "$neg/relayweft-signing-key.asc" ] || fail "an unsigned build wrote relayweft-signing-key.asc"
    for f in "$neg"/relayweft-linux-amd64.*; do
      if sh packaging/verify-packages.sh "$committed/relayweft-signing-key.asc" "$committed/relayweft-apk.rsa.pub" "$f" 2>/dev/null; then
        fail "the unsigned $f verified"
      fi
    done
    give_back
    echo "ok: linux-packages.sh refuses what it must, and unsigned packages fail verification"
    ;;

  client)
    format="${2:?usage: test.sh client deb|rpm|apk}"
    bad="$work/keys/b"
    case "$format" in
      deb)
        export DEBIAN_FRONTEND=noninteractive
        install -d -m 0755 /etc/apt/keyrings
        cp /site/apt/relayweft.list /etc/apt/sources.list.d/relayweft.list
        # Key B: apt must refuse the repository.
        cp "$bad.keyring" /etc/apt/keyrings/relayweft.gpg
        if apt-get update >/tmp/apt.log 2>&1; then
          fail "apt-get update accepted the repository signed with another key: $(cat /tmp/apt.log)"
        fi
        grep -Eq 'NO_PUBKEY|not signed|signature' /tmp/apt.log || fail "apt-get update failed otherwise: $(cat /tmp/apt.log)"
        if apt-get install -y -qq relayweft >/dev/null 2>&1; then fail "apt installed from a repository it cannot verify"; fi
        echo "ok: apt refuses the repository with another key"
        # The setup in packaging/README.md, with the site's keyring.
        cp /site/apt/relayweft.gpg /etc/apt/keyrings/relayweft.gpg
        apt-get update -qq >/tmp/apt.log 2>&1 || fail "apt-get update: $(cat /tmp/apt.log)"
        cat /tmp/apt.log
        if grep -Eq '^(W|E):' /tmp/apt.log; then fail "apt-get update warned: $(cat /tmp/apt.log)"; fi
        apt-cache policy relayweft
        apt-cache madison relayweft | grep -q '0.0.1' && fail "the repository has 0.0.1"
        apt-get install -y -qq relayweft=0.0.2-1 ca-certificates >/dev/null
        old=0.0.2
        upgrade="apt-get upgrade -y -qq"
        new_how="sudo apt update && sudo apt install --only-upgrade relayweft"
        ;;
      rpm)
        # Key B: dnf must refuse the repository.
        sed "s|^gpgkey=.*|gpgkey=file:///src/$bad.asc|" /site/rpm/relayweft.repo > /etc/yum.repos.d/relayweft.repo
        if dnf install -y -q relayweft >/tmp/dnf.log 2>&1; then
          fail "dnf installed from a repository signed with another key: $(cat /tmp/dnf.log)"
        fi
        echo "ok: dnf refuses the repository with another key ($(grep -Eio 'signature|gpg|openpgp|key' /tmp/dnf.log | head -n 1))"
        dnf clean all -q >/dev/null 2>&1 || true
        rpm -e --allmatches gpg-pubkey >/dev/null 2>&1 || true
        cp /site/rpm/relayweft.repo /etc/yum.repos.d/relayweft.repo
        grep -qx 'gpgcheck=1' /etc/yum.repos.d/relayweft.repo && grep -qx 'repo_gpgcheck=1' /etc/yum.repos.d/relayweft.repo ||
          fail "relayweft.repo does not check signatures"
        dnf install -y -q relayweft-0.0.2 >/dev/null
        dnf list --showduplicates relayweft 2>/dev/null | grep -q '0.0.1' && fail "the repository has 0.0.1"
        old=0.0.2
        upgrade="dnf upgrade -y -q relayweft"
        new_how="sudo dnf upgrade relayweft"
        ;;
      apk)
        # The real line is https://sparkz400.github.io/Relayweft/apk;
        # rw update looks for a line that names relayweft.
        ln -s /site /relayweft
        echo /relayweft/apk >> /etc/apk/repositories
        # No key, then key B: apk must refuse the index.
        apk update >/tmp/apk.log 2>&1 || true
        grep -q UNTRUSTED /tmp/apk.log || fail "apk update without the key: $(cat /tmp/apk.log)"
        if apk add -q relayweft >/dev/null 2>&1; then fail "apk installed from an untrusted repository"; fi
        cp "$bad.rsa.pub" /etc/apk/keys/relayweft-apk.rsa.pub
        apk update >/tmp/apk.log 2>&1 || true
        grep -Eq "UNTRUSTED|BAD signature" /tmp/apk.log || fail "apk update with key B: $(cat /tmp/apk.log)"
        if apk add -q relayweft >/dev/null 2>&1; then fail "apk installed from a repository signed with another key"; fi
        echo "ok: apk refuses the repository without its key and with another key"
        cp /site/apk/relayweft-apk.rsa.pub /etc/apk/keys/relayweft-apk.rsa.pub
        apk update >/tmp/apk.log 2>&1 || fail "apk update: $(cat /tmp/apk.log)"
        cat /tmp/apk.log
        grep -Eq "UNTRUSTED|BAD signature" /tmp/apk.log && fail "apk update: $(cat /tmp/apk.log)"
        apk add -q relayweft=0.0.2-r1 ca-certificates
        # As if 0.0.2 had been the latest at `apk add relayweft`: no pin.
        sed -i 's/^relayweft=.*/relayweft/' /etc/apk/world
        apk search -a relayweft | grep -q '0.0.1' && fail "the repository has 0.0.1"
        old=0.0.2
        upgrade="apk upgrade -q"
        new_how="sudo apk update && sudo apk upgrade relayweft"
        ;;
      *) fail "unknown format $format" ;;
    esac
    [ "$(rw version)" = "relayweft $old" ] || fail "installed $(rw version), want $old"
    echo "ok: installed relayweft $old from the repository"
    # rw update names the repository's upgrade command (the latest real
    # release is newer than 0.0.2).
    out="$(rw update --check)" || fail "rw update --check failed: $out"
    echo "$out"
    echo "$out" | grep -qF "$new_how" || fail "rw update --check does not name '$new_how'"
    $upgrade
    [ "$(rw version)" = "relayweft 0.0.3" ] || fail "after the upgrade: $(rw version), want 0.0.3"
    echo "ok: upgraded to relayweft 0.0.3"
    echo "PASS: $format client"
    ;;

  *)
    fail "unknown step $1"
    ;;
esac
