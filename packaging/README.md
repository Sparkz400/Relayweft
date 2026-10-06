# Packaging and releases

Everything needed to ship `rw`: the release workflow
(`.github/workflows/release.yml`), the Linux packages (`nfpm.yaml`), a
Scoop manifest, winget manifest templates, the Homebrew formula
(`../Formula/relayweft.rb`, from `homebrew/`) and the AUR package
(`aur/`).

Release assets are a contract. `rw update`, the manifests and the CI
install scripts all look them up by these exact names. Add new assets,
never rename existing ones, and list every asset in `checksums.txt`
(except its own signature bundle, which cannot be in the file it signs).

| Asset | Platform |
| --- | --- |
| `rw-windows-amd64.exe`, `rw-windows-arm64.exe` | Windows |
| `rw-linux-amd64`, `rw-linux-arm64` | Linux |
| `rw-darwin-amd64`, `rw-darwin-arm64` | macOS |
| `relayweft-linux-amd64.deb`, `relayweft-linux-arm64.deb` | Debian, Ubuntu (from v0.3.0) |
| `relayweft-linux-amd64.rpm`, `relayweft-linux-arm64.rpm` | Fedora, RHEL, openSUSE (from v0.3.0) |
| `relayweft-linux-amd64.apk`, `relayweft-linux-arm64.apk` | Alpine (from v0.3.0) |
| `relayweft-signing-key.asc` | GPG public key that signs the `.deb` and `.rpm` packages (once the [signing keys](#signing-keys) are set up) |
| `relayweft-apk.rsa.pub` | RSA public key that signs the `.apk` packages (the same) |
| `rw-<os>-<arch>.cdx.json` (one per binary, without `.exe`) | CycloneDX SBOM of that binary (from v0.4.0) |
| `checksums.txt` | SHA-256 of every asset above (`sha256sum` format) |
| `checksums.txt.sigstore.json` | Sigstore bundle: keyless cosign signature of `checksums.txt` (from v0.4.0) |

From v0.4.0 every asset (and `checksums.txt`) also has a build provenance
attestation, and each binary an SBOM attestation, stored by GitHub (see
[Verifying a release](#verifying-a-release)).

The names above start with v0.3.0. v0.1.0 and v0.2.0 were released as
Switchyard, and their binaries were `sy-<os>-<arch>[.exe]`. This is the
one time the names changed. v0.3.0 has no `sy-*` copies, so `sy update`
in v0.2.0 stops with "release v0.3.0 has no sy-… asset". Users of v0.2.0
install `rw` once by hand (README, "Upgrading from sy").

## Cutting a release

Before you tag:

1. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [1.2.3] - <date>`
   and put a new, empty `## [Unreleased]` above it. Update the compare
   links at the bottom: `[Unreleased]` compares `v1.2.3...HEAD`, and a new
   `[1.2.3]` line compares the previous tag with `v1.2.3`. Merge that
   change first, so the tag contains it.
2. If users must do something to upgrade, write
   `packaging/release-notes/v1.2.3.md`. The release workflow puts it above
   the generated notes. Keep it consistent with the CHANGELOG.

Then tag:

```sh
git tag v1.2.3
git push --tags
```

The `release` workflow runs `go test ./...`, builds all six binaries with
`-X main.version=1.2.3` (no leading `v`), builds the Linux packages and
an SBOM per binary, writes `checksums.txt`, attests and signs (below)
and creates the GitHub release with generated notes. A tag with a suffix
(`v1.3.0-rc1`) becomes a pre-release, which `rw update` and Scoop ignore.

Its jobs get only the permissions they need: `build` (tests, binaries,
packages, SBOMs, checksums) can only read; `attest` and `attest-sbom` get
the OIDC token (`id-token: write`) and `attestations: write`; `publish`
gets `contents: write` and runs only after the signature and every
attestation have been verified, then starts `pages.yml` for the package
repositories (`actions: write`). Besides the workflow's own `GITHUB_TOKEN`
and OIDC token, the only secrets are the two package signing keys, which
only the `build` job's "linux packages" step gets, and never in a pull
request's run. Every action is pinned to a commit,
and every downloaded tool (nfpm, cyclonedx-gomod, cosign) to a version
and checksum.

You can also start it by hand: **Actions > release > Run workflow** and
enter a version such as `1.2.3`; the tag `v1.2.3` is created on `main`.
Only a `v*` tag or `main` can publish a release, since that is the
identity users check.

### Dry run

**Run workflow** with **dry run** ticked (on any branch, any version
such as `0.4.0-dryrun.1`) does everything except publishing: tests,
build, packages, SBOMs, the real build provenance and SBOM attestations,
the keyless signature of `checksums.txt`, and all the checks below. No
tag and no release are created; the files are in the run's
`release-dist` and `release-signature` artifacts for a week. The
attestations and the signature are real (in this repository's
attestations and Sigstore's public transparency log), but name the
branch the run used, so the `cosign` check and `gh attestation verify
--source-ref` below reject them.

```sh
gh workflow run release.yml --ref my-branch -f version=0.4.0-dryrun.1 -f dry_run=true
```

A pull request that changes `release.yml`, `linux-packages.sh`,
`verify-packages.sh`, `nfpm.yaml` or `sbom.sh` runs the build, the SBOMs
and the asset checks, and signs and verifies `checksums.txt` with a
throwaway cosign key and no transparency log. Pull requests get no OIDC
token here, so they cannot attest or sign keyless; a dry run tests that.
They get no package signing keys either, so their packages are unsigned;
a dry run signs and checks them with the real keys.

After the release is published, refresh the manifests:

```sh
packaging/render-manifests.sh 1.2.3
git add packaging/scoop/rw.json Formula/relayweft.rb packaging/aur/PKGBUILD packaging/aur/.SRCINFO
git commit -m "Manifests for v1.2.3"
```

This downloads `checksums.txt` with the `gh` CLI and updates in place:
`packaging/scoop/rw.json`, `Formula/relayweft.rb` (from
`homebrew/relayweft.rb.tmpl`) and `aur/PKGBUILD` and `aur/.SRCINFO`
(from their `.tmpl` files). It writes ready-to-submit winget manifests to
`winget-out/1.2.3/` (do not commit those). Edit the templates, not the
rendered files. The `packaging` workflow checks on every pull request
that touches these files that the committed ones match their release.
It runs `manifest-release.sh` first. If the manifests' release has none of
the asset names above (the manifests on `main` between the rename and
v0.3.0 still name v0.2.0, which only has `sy-*`), and no newer release has
them either, the install and compare checks are skipped with a notice. If a
newer release has them, the check fails until the manifests are rendered
for it.

Homebrew users who tapped the repo as Switchyard have a `switchyard`
formula. `formula_renames.json` in the repo root maps it to `relayweft`,
so `brew update && brew upgrade` moves them to `relayweft` once the
formula names a Relayweft release.

## Verifying a release

From v0.4.0 a release can be checked three ways. Each is independent of
GitHub serving the right `checksums.txt`; v0.3.0 and older releases have
only `checksums.txt`.

**Build provenance** (any asset, with the [gh CLI](https://cli.github.com/)):

```sh
gh attestation verify rw-linux-amd64 --repo Sparkz400/Relayweft
```

This shows that the file was built by a workflow of this repository, and
from which commit. To insist that it is this release (a tag-pushed
release; a release started by hand on `main` has `refs/heads/main`):

```sh
gh attestation verify rw-linux-amd64 --repo Sparkz400/Relayweft \
  --signer-workflow Sparkz400/Relayweft/.github/workflows/release.yml \
  --source-ref refs/tags/v0.4.0
```

It works for every asset, including the packages, the SBOMs and
`checksums.txt`, and on an installed binary (the file `rw update` or a
package manager put in place is the release asset).

**SBOM** (a binary's dependencies, attested by the same workflow):

```sh
gh attestation verify rw-linux-amd64 --repo Sparkz400/Relayweft \
  --predicate-type https://cyclonedx.org/bom
```

The SBOM itself is the release asset `rw-linux-amd64.cdx.json`.

**Signed checksums** (with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) v3):

```sh
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/Sparkz400/Relayweft/\.github/workflows/release\.yml@refs/(tags/v[0-9][^/]*|heads/main)$'
sha256sum --ignore-missing -c checksums.txt
```

For a tag-pushed release, the exact identity works too:
`--certificate-identity https://github.com/Sparkz400/Relayweft/.github/workflows/release.yml@refs/tags/v0.4.0`.
The release workflow checks the signature against the expression above
before it publishes, so a release that does not pass it is never
published.

Scoop, winget, Homebrew and the AUR package do not change: their
manifests carry the SHA-256 from `checksums.txt`, and the package
manager checks it on install. To check provenance as well, run
`gh attestation verify` on the installed binary (for example
`$(brew --prefix)/bin/rw`, or `(Get-Command rw).Source` in PowerShell).

`rw update` checks only the SHA-256 against `checksums.txt`; after
installing a release that is signed, it prints the
`gh attestation verify` command for the installed binary. It does not
verify the signature itself: that needs Sigstore's trust root kept
current over TUF and the sigstore-go library, which would roughly triple
the modules linked into `rw`, while `gh` and `cosign` already do it.

## Linux packages

`linux-packages.sh` builds the `.deb`, `.rpm` and `.apk` packages with
[nfpm](https://nfpm.goreleaser.com/), pinned to one version and checked
against its SHA-256 before it runs. Each package installs `/usr/bin/rw`,
the README, the license and the bash, zsh and fish completion scripts
(printed by `rw completion`; zsh's goes to `vendor-completions` on Debian
and `site-functions` elsewhere), and depends on `git`. With the
[signing keys](#signing-keys) set, the release signs every package (GPG
for `.deb` and `.rpm`, the RSA key for `.apk`) and checks each signature
before anything is published. v0.3.0's packages are unsigned. From v0.4.0
they can also be checked with `gh attestation verify` or the signed
`checksums.txt` (see [Verifying a release](#verifying-a-release)).

To build and test them locally (Docker, one container at a time):

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=0.0.1" -o dist/rw-linux-amd64 ./cmd/rw
docker run --rm -v "$PWD:/src" -w /src debian:stable-slim sh -c \
  'apt-get update -qq && apt-get install -y -qq curl ca-certificates && sh packaging/linux-packages.sh 0.0.1 dist amd64'
docker run --rm -v "$PWD:/src:ro" -w /src ubuntu:24.04 sh packaging/test-install.sh deb dist/relayweft-linux-amd64.deb 0.0.1
docker run --rm -v "$PWD:/src:ro" -w /src fedora:latest sh packaging/test-install.sh rpm dist/relayweft-linux-amd64.rpm 0.0.1
docker run --rm -v "$PWD:/src:ro" -w /src alpine:latest sh packaging/test-install.sh apk dist/relayweft-linux-amd64.apk 0.0.1
docker run --rm -v "$PWD:/src:ro" -w /src archlinux:latest sh packaging/test-install.sh aur
```

To bump nfpm, change `nfpm_version` and `nfpm_sha256` in
`linux-packages.sh` (the `Linux_x86_64.tar.gz` line of that nfpm
release's `checksums.txt`).

### apt, dnf and apk repositories

The project's GitHub Pages site has an apt, an rpm and an apk repository
with the packages of the last three signed releases (once the
[signing keys](#signing-keys) are set up and Pages is on; the first
release after that is the first one in them). Set one up once; after that
the package manager updates `rw` like everything else.

Debian, Ubuntu:

```sh
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://sparkz400.github.io/Relayweft/apt/relayweft.gpg | sudo tee /etc/apt/keyrings/relayweft.gpg >/dev/null
curl -fsSL https://sparkz400.github.io/Relayweft/apt/relayweft.list | sudo tee /etc/apt/sources.list.d/relayweft.list >/dev/null
sudo apt update && sudo apt install relayweft
```

Fedora, RHEL and other dnf systems:

```sh
curl -fsSL https://sparkz400.github.io/Relayweft/rpm/relayweft.repo | sudo tee /etc/yum.repos.d/relayweft.repo >/dev/null
sudo dnf install relayweft
```

(openSUSE: `sudo zypper addrepo https://sparkz400.github.io/Relayweft/rpm/relayweft.repo`, not tested.)

Alpine:

```sh
sudo wget -qO /etc/apk/keys/relayweft-apk.rsa.pub https://sparkz400.github.io/Relayweft/apk/relayweft-apk.rsa.pub
echo https://sparkz400.github.io/Relayweft/apk | sudo tee -a /etc/apk/repositories
sudo apk update && sudo apk add relayweft
```

Then `sudo apt update && sudo apt upgrade`, `sudo dnf upgrade` or
`sudo apk upgrade` updates `rw`, and `rw update` names that command.
Signatures are checked: apt checks the repository's (`InRelease`), dnf the
repository's (`repo_gpgcheck`) and each package's (`gpgcheck`), apk the
index's and each package's. The key files are the release's
`relayweft-signing-key.asc` (apt's `relayweft.gpg` is the same key,
unarmored) and `relayweft-apk.rsa.pub`, and they are committed in
`packaging/keys/`.

Without a repository, download a package from the release:

```sh
sudo apt install ./relayweft-linux-amd64.deb
sudo dnf install ./relayweft-linux-amd64.rpm
sudo apk add --allow-untrusted ./relayweft-linux-amd64.apk
```

`repo/build-pages.sh SITE-DIR` builds the repositories into
`SITE-DIR/apt`, `SITE-DIR/rpm` and `SITE-DIR/apk`. It downloads the
packages of the last three signed releases (those with
`relayweft-signing-key.asc`; `RW_REPO_KEEP` changes the number), checks
them against `checksums.txt` and their signatures against the keys (a
package that fails is left out with a warning), and builds and signs the
metadata in pinned Debian and Alpine images (Docker). `pages.yml` builds
the site into `_site`, runs `packaging/repo/build-pages.sh _site` and
deploys it. It needs these in the step's environment:

```yaml
env:
  PACKAGE_GPG_KEY: ${{ secrets.PACKAGE_GPG_KEY }}
  PACKAGE_APK_KEY: ${{ secrets.PACKAGE_APK_KEY }}
  PACKAGE_SIGNING_REQUIRED: ${{ github.repository == 'Sparkz400/Relayweft' }}
  GH_TOKEN: ${{ github.token }}
```

Without the keys it builds nothing and says so. With
`PACKAGE_SIGNING_REQUIRED=true` and the public keys in `packaging/keys/`,
it fails instead, so the site never loses its repositories silently.
After each release the release workflow starts `pages.yml` (a release
made with `GITHUB_TOKEN` triggers no workflow), so it needs a
`workflow_dispatch` trigger.

`repo/test.sh` tests all of this with throwaway keys made for the test
(the `package-repo` job of the `packaging` workflow runs it). It signs the
packages of four versions, builds the repositories, and in Debian, Ubuntu,
Fedora and Alpine containers checks that apt, dnf and apk refuse the
repository with another key, install 0.0.2 and upgrade it to 0.0.3, and
that `rw update --check` names the upgrade command:

```sh
sh packaging/repo/test.sh   # from the repo root; needs Go and Docker
```

### Signing keys

Two key pairs sign the packages and the repositories: a GPG key (`.deb`,
`.rpm`, apt's `InRelease`, dnf's `repomd.xml`) and an RSA key (`.apk` and
`APKINDEX`). The private keys are repository secrets and have no
passphrase; the secret store protects them. To set them up, once, on a
trusted machine with `gpg`, `openssl` and the `gh` CLI:

1. Create the keys (the GPG key does not expire, so users' keyrings keep
   working; replace it if it leaks):

   ```sh
   export GNUPGHOME="$(mktemp -d)"
   mkdir -p packaging/keys
   gpg --batch --passphrase '' --quick-gen-key \
     "Relayweft packages <62327601+Sparkz400@users.noreply.github.com>" rsa4096 sign never
   gpg --armor --export-secret-keys > relayweft-signing.key
   gpg --armor --export > packaging/keys/relayweft-signing-key.asc
   openssl genrsa -out relayweft-apk.rsa 4096
   openssl rsa -in relayweft-apk.rsa -pubout -out packaging/keys/relayweft-apk.rsa.pub
   ```

2. Add the private keys as secrets of this repository:

   ```sh
   gh secret set PACKAGE_GPG_KEY --repo Sparkz400/Relayweft < relayweft-signing.key
   gh secret set PACKAGE_APK_KEY --repo Sparkz400/Relayweft < relayweft-apk.rsa
   ```

   Keep a copy of both files offline (a password manager), then delete
   them and `$GNUPGHOME`.
3. Commit `packaging/keys/relayweft-signing-key.asc` and
   `packaging/keys/relayweft-apk.rsa.pub`. From then on the release
   workflow and `build-pages.sh` fail in this repository when the secrets
   are missing or do not match these files, instead of shipping unsigned
   packages or a site without repositories.
4. Turn on Pages: **Settings > Pages > Source: GitHub Actions**.
   `pages.yml` needs the environment shown above and a `workflow_dispatch`
   trigger.

Without the secrets (forks, pull requests, before step 3) the packages
are built unsigned, with a notice, and `build-pages.sh` builds no
repository. With only one of the two secrets nothing is built: a release
is signed completely or not at all.

## Homebrew

This repo is its own tap; the formula is `Formula/relayweft.rb`
(`relayweft`, which installs `rw`; homebrew-core has neither name). It
downloads the release binary for the OS and CPU and checks its SHA-256,
so it needs no bottles. It works on macOS and on Linux (Homebrew on
Linux).

```sh
brew tap sparkz400/relayweft https://github.com/Sparkz400/Relayweft
brew install relayweft
brew upgrade relayweft
```

`brew update` fetches the committed formula, so render and commit after
each release. The formula installs the bash, zsh and fish completion with
`generate_completions_from_executable` for releases that have
`rw completion` (v0.4.0 on; the version check can go once the manifests
name v0.4.0 or later). The AUR `PKGBUILD` does the same in `package()`.

## AUR

`aur/` holds `PKGBUILD` and `.SRCINFO` for `relayweft-bin`, built
from the release's linux binaries (x86_64 and aarch64). It provides
`relayweft` and conflicts with `relayweft` and with `rw`: the AUR's
unrelated `rw` and `rw-git` packages (Sortix's blockwise I/O tool) also
install `/usr/bin/rw`. (Before the rename the package was to be called
`switchyard-cli-bin`, since the AUR has an unrelated `switchyard`; it was
never published.)

To publish it (once, then after each release):

1. Create an AUR account at <https://aur.archlinux.org/> and add your SSH
   public key under *My Account*.
2. Add this to `~/.ssh/config`:
   ```text
   Host aur.archlinux.org
     IdentityFile ~/.ssh/aur
     User aur
   ```
3. The first time, clone the empty package (this creates it on push):
   ```sh
   git clone ssh://aur@aur.archlinux.org/relayweft-bin.git
   ```
4. After `render-manifests.sh`, copy `packaging/aur/PKGBUILD` and
   `packaging/aur/.SRCINFO` into that clone, check it, commit and push:
   ```sh
   cd relayweft-bin
   makepkg -f && namcap PKGBUILD *.pkg.tar.zst   # on Arch, optional
   git add PKGBUILD .SRCINFO
   git commit -m "Update to 1.2.3"
   git push
   ```

Without an Arch machine, the `aur` job of the `packaging` workflow (or
`test-install.sh aur` in an `archlinux` container) builds and installs the
committed PKGBUILD and checks that `.SRCINFO` matches it.

## Installing with Scoop

Install straight from the manifest in this repo:

```powershell
scoop install https://raw.githubusercontent.com/sparkz400/relayweft/main/packaging/scoop/rw.json
```

Scoop installs only what the committed manifest names, so run
`render-manifests.sh` and commit after each release. The manifest also has
`checkver` and `autoupdate`, so Scoop's own tooling works too:

```powershell
& "$(scoop prefix scoop)\bin\checkver.ps1" -Dir packaging\scoop -App rw -Update
```

To make `scoop update rw` follow new releases automatically, put the
manifest in a bucket repo (for example `sparkz400/scoop-bucket` with
`bucket/rw.json`) and let the bucket's Excavator action run `checkver`:

```powershell
scoop bucket add sparkz400 https://github.com/sparkz400/scoop-bucket
scoop install sparkz400/rw
```

Scoop and winget install only `rw.exe`. For Tab completion in PowerShell
(5.1 and 7), add `rw completion powershell | Out-String | Invoke-Expression`
to `$PROFILE`; the Scoop manifest's notes say so.

## Submitting to winget

The templates in `winget/` use `{{VERSION}}`, `{{SHA256_X64}}`,
`{{SHA256_ARM64}}` and `{{RELEASE_DATE}}`; `render-manifests.sh` fills them.
The package is `Sparkz400.Relayweft`, a portable installer that exposes the
command `rw`.

Either submit the rendered files:

```powershell
winget validate --manifest winget-out\1.2.3
winget install --manifest winget-out\1.2.3      # local test (needs LocalManifestFiles enabled)
wingetcreate submit winget-out\1.2.3 --token <github-token>
```

or let `wingetcreate` build the update from the release URLs:

```powershell
wingetcreate update Sparkz400.Relayweft --version 1.2.3 --submit `
  --urls https://github.com/sparkz400/relayweft/releases/download/v1.2.3/rw-windows-amd64.exe `
         https://github.com/sparkz400/relayweft/releases/download/v1.2.3/rw-windows-arm64.exe
```

Both open a pull request against
[microsoft/winget-pkgs](https://github.com/microsoft/winget-pkgs) (put new
manifests under `manifests/s/Sparkz400/Relayweft/<version>/`). The first
submission must use `wingetcreate submit` or a manual PR, since `update`
needs an existing package.

## `rw update` and package managers

`rw update` replaces only a binary that no package manager owns. It tells
them apart by the binary's real path (after symlinks): Homebrew's
`Cellar/relayweft/<version>/bin/rw`, Scoop's `apps\rw\<version>\` (in a
`scoop` folder, `$SCOOP` or `$SCOOP_GLOBAL`), winget's
`WinGet\Packages\Sparkz400.Relayweft_<source>\`, and for a binary under
`/usr/` the package database (`dpkg-query -S`, `rpm -qf`, `pacman -Qo`,
`apk info --who-owns`, run with `LC_ALL=C`). In those cases `rw update --check`
names the package manager's command, and `rw update` prints it and
changes nothing. For a `.deb`, `.rpm` or `.apk` that is the repository's
upgrade command (`sudo apt update && sudo apt install --only-upgrade
relayweft`, `sudo dnf upgrade relayweft`, `sudo apk update && sudo apk
upgrade relayweft`) when the setup above was done
(`/etc/apt/sources.list.d/relayweft.list`, `/etc/yum.repos.d/relayweft.repo`
or a `relayweft` line in `/etc/apk/repositories`), and otherwise the
release's package and a pointer to the setup. `rw update --force` replaces the binary anyway.

## Private repository

While the repository is private, release downloads need authentication:

- `rw update` sends `GITHUB_TOKEN` or `GH_TOKEN` when set; without one the
  GitHub API answers 404. A fine-grained token with read access to
  *Contents* on this repo is enough.
- Scoop and winget cannot authenticate, so they only work once the
  repository is public (winget-pkgs also rejects URLs it cannot fetch).
  Until then, download with
  `gh release download v1.2.3 --repo sparkz400/relayweft`.
