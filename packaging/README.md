# Packaging and releases

Everything needed to ship `sy`: the release workflow
(`.github/workflows/release.yml`), the Linux packages (`nfpm.yaml`), a
Scoop manifest, winget manifest templates, the Homebrew formula
(`../Formula/switchyard.rb`, from `homebrew/`) and the AUR package
(`aur/`).

Release assets are a contract. `sy update`, the manifests and the CI
install scripts all look them up by these exact names. Add new assets,
never rename existing ones, and list every asset in `checksums.txt`.

| Asset | Platform |
| --- | --- |
| `sy-windows-amd64.exe`, `sy-windows-arm64.exe` | Windows |
| `sy-linux-amd64`, `sy-linux-arm64` | Linux |
| `sy-darwin-amd64`, `sy-darwin-arm64` | macOS |
| `switchyard-linux-amd64.deb`, `switchyard-linux-arm64.deb` | Debian, Ubuntu (from v0.3.0) |
| `switchyard-linux-amd64.rpm`, `switchyard-linux-arm64.rpm` | Fedora, RHEL, openSUSE (from v0.3.0) |
| `switchyard-linux-amd64.apk`, `switchyard-linux-arm64.apk` | Alpine (from v0.3.0) |
| `checksums.txt` | SHA-256 of every asset (`sha256sum` format) |

## Cutting a release

```sh
git tag v1.2.3
git push --tags
```

The `release` workflow runs `go test ./...`, builds all six binaries with
`-X main.version=1.2.3` (no leading `v`), builds the Linux packages,
writes `checksums.txt` and creates the GitHub release with generated
notes. A tag with a suffix (`v1.3.0-rc1`) becomes a pre-release, which
`sy update` and Scoop ignore.

You can also start it by hand: **Actions > release > Run workflow** and
enter a version such as `1.2.3`; the tag `v1.2.3` is created on the
selected branch.

After the release is published, refresh the manifests:

```sh
packaging/render-manifests.sh 1.2.3
git add packaging/scoop/sy.json Formula/switchyard.rb packaging/aur/PKGBUILD packaging/aur/.SRCINFO
git commit -m "Manifests for v1.2.3"
```

This downloads `checksums.txt` with the `gh` CLI and updates in place:
`packaging/scoop/sy.json`, `Formula/switchyard.rb` (from
`homebrew/switchyard.rb.tmpl`) and `aur/PKGBUILD` and `aur/.SRCINFO`
(from their `.tmpl` files). It writes ready-to-submit winget manifests to
`winget-out/1.2.3/` (do not commit those). Edit the templates, not the
rendered files. The `packaging` workflow checks on every pull request
that touches these files that the committed ones match their release.

## Linux packages

`linux-packages.sh` builds the `.deb`, `.rpm` and `.apk` packages with
[nfpm](https://nfpm.goreleaser.com/), pinned to one version and checked
against its SHA-256 before it runs. Each package installs `/usr/bin/sy`,
the README and the license, and depends on `git`. The packages are not
signed, and there is no apt or dnf repository, so users download them from
the release:

```sh
sudo apt install ./switchyard-linux-amd64.deb
sudo dnf install ./switchyard-linux-amd64.rpm
sudo apk add --allow-untrusted ./switchyard-linux-amd64.apk
```

To build and test them locally (Docker, one container at a time):

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=0.0.1" -o dist/sy-linux-amd64 ./cmd/sy
docker run --rm -v "$PWD:/src" -w /src debian:stable-slim sh -c \
  'apt-get update -qq && apt-get install -y -qq curl ca-certificates && sh packaging/linux-packages.sh 0.0.1 dist amd64'
docker run --rm -v "$PWD:/src:ro" -w /src ubuntu:24.04 sh packaging/test-install.sh deb dist/switchyard-linux-amd64.deb 0.0.1
docker run --rm -v "$PWD:/src:ro" -w /src fedora:latest sh packaging/test-install.sh rpm dist/switchyard-linux-amd64.rpm 0.0.1
docker run --rm -v "$PWD:/src:ro" -w /src alpine:latest sh packaging/test-install.sh apk dist/switchyard-linux-amd64.apk 0.0.1
docker run --rm -v "$PWD:/src:ro" -w /src archlinux:latest sh packaging/test-install.sh aur
```

To bump nfpm, change `nfpm_version` and `nfpm_sha256` in
`linux-packages.sh` (the `Linux_x86_64.tar.gz` line of that nfpm
release's `checksums.txt`).

## Homebrew

This repo is its own tap; the formula is `Formula/switchyard.rb`
(`switchyard`, which installs `sy`; homebrew-core has neither name). It
downloads the release binary for the OS and CPU and checks its SHA-256,
so it needs no bottles. It works on macOS and on Linux (Homebrew on
Linux).

```sh
brew tap sparkz400/switchyard https://github.com/Sparkz400/Switchyard
brew install switchyard
brew upgrade switchyard
```

`brew update` fetches the committed formula, so render and commit after
each release.

## AUR

`aur/` holds `PKGBUILD` and `.SRCINFO` for `switchyard-cli-bin`, built
from the release's linux binaries (x86_64 and aarch64). The name is not
`switchyard-bin` because the AUR already has an unrelated `switchyard`
(an SMTP-to-XMPP bridge), and `-bin` names the binary build of the package
with the same base name. It provides and conflicts with `sy`.

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
   git clone ssh://aur@aur.archlinux.org/switchyard-cli-bin.git
   ```
4. After `render-manifests.sh`, copy `packaging/aur/PKGBUILD` and
   `packaging/aur/.SRCINFO` into that clone, check it, commit and push:
   ```sh
   cd switchyard-cli-bin
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
scoop install https://raw.githubusercontent.com/sparkz400/switchyard/main/packaging/scoop/sy.json
```

Scoop installs only what the committed manifest names, so run
`render-manifests.sh` and commit after each release. The manifest also has
`checkver` and `autoupdate`, so Scoop's own tooling works too:

```powershell
& "$(scoop prefix scoop)\bin\checkver.ps1" -Dir packaging\scoop -App sy -Update
```

To make `scoop update sy` follow new releases automatically, put the
manifest in a bucket repo (for example `sparkz400/scoop-bucket` with
`bucket/sy.json`) and let the bucket's Excavator action run `checkver`:

```powershell
scoop bucket add sparkz400 https://github.com/sparkz400/scoop-bucket
scoop install sparkz400/sy
```

## Submitting to winget

The templates in `winget/` use `{{VERSION}}`, `{{SHA256_X64}}`,
`{{SHA256_ARM64}}` and `{{RELEASE_DATE}}`; `render-manifests.sh` fills them.
The package is `Sparkz400.Switchyard`, a portable installer that exposes the
command `sy`.

Either submit the rendered files:

```powershell
winget validate --manifest winget-out\1.2.3
winget install --manifest winget-out\1.2.3      # local test (needs LocalManifestFiles enabled)
wingetcreate submit winget-out\1.2.3 --token <github-token>
```

or let `wingetcreate` build the update from the release URLs:

```powershell
wingetcreate update Sparkz400.Switchyard --version 1.2.3 --submit `
  --urls https://github.com/sparkz400/switchyard/releases/download/v1.2.3/sy-windows-amd64.exe `
         https://github.com/sparkz400/switchyard/releases/download/v1.2.3/sy-windows-arm64.exe
```

Both open a pull request against
[microsoft/winget-pkgs](https://github.com/microsoft/winget-pkgs) (put new
manifests under `manifests/s/Sparkz400/Switchyard/<version>/`). The first
submission must use `wingetcreate submit` or a manual PR, since `update`
needs an existing package.

## `sy update` and package managers

`sy update` replaces only a binary that no package manager owns. It tells
them apart by the binary's real path (after symlinks): Homebrew's
`Cellar`, Scoop's `scoop\apps`, winget's `WinGet\Packages`, and for a
binary under `/usr/` the package database (`dpkg-query -S`, `rpm -qf`,
`pacman -Qo`, `apk info --who-owns`). In those cases `sy update --check`
names the package manager's command, and `sy update` prints it and
changes nothing. `sy update --force` replaces the binary anyway.

## Private repository

While the repository is private, release downloads need authentication:

- `sy update` sends `GITHUB_TOKEN` or `GH_TOKEN` when set; without one the
  GitHub API answers 404. A fine-grained token with read access to
  *Contents* on this repo is enough.
- Scoop and winget cannot authenticate, so they only work once the
  repository is public (winget-pkgs also rejects URLs it cannot fetch).
  Until then, download with
  `gh release download v1.2.3 --repo sparkz400/switchyard`.
