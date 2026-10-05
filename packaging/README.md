# Packaging and releases

Everything needed to ship `rw`: the release workflow
(`.github/workflows/release.yml`), the Linux packages (`nfpm.yaml`), a
Scoop manifest, winget manifest templates, the Homebrew formula
(`../Formula/relayweft.rb`, from `homebrew/`) and the AUR package
(`aur/`).

Release assets are a contract. `rw update`, the manifests and the CI
install scripts all look them up by these exact names. Add new assets,
never rename existing ones, and list every asset in `checksums.txt`.

| Asset | Platform |
| --- | --- |
| `rw-windows-amd64.exe`, `rw-windows-arm64.exe` | Windows |
| `rw-linux-amd64`, `rw-linux-arm64` | Linux |
| `rw-darwin-amd64`, `rw-darwin-arm64` | macOS |
| `relayweft-linux-amd64.deb`, `relayweft-linux-arm64.deb` | Debian, Ubuntu (from v0.3.0) |
| `relayweft-linux-amd64.rpm`, `relayweft-linux-arm64.rpm` | Fedora, RHEL, openSUSE (from v0.3.0) |
| `relayweft-linux-amd64.apk`, `relayweft-linux-arm64.apk` | Alpine (from v0.3.0) |
| `checksums.txt` | SHA-256 of every asset (`sha256sum` format) |

The names above start with v0.3.0. v0.1.0 and v0.2.0 were released as
Switchyard, and their binaries were `sy-<os>-<arch>[.exe]`. This is the
one time the names changed. v0.3.0 has no `sy-*` copies, so `sy update`
in v0.2.0 stops with "release v0.3.0 has no sy-… asset". Users of v0.2.0
install `rw` once by hand (README, "Upgrading from sy").

## Cutting a release

```sh
git tag v1.2.3
git push --tags
```

The `release` workflow runs `go test ./...`, builds all six binaries with
`-X main.version=1.2.3` (no leading `v`), builds the Linux packages,
writes `checksums.txt` and creates the GitHub release with generated
notes. A tag with a suffix (`v1.3.0-rc1`) becomes a pre-release, which
`rw update` and Scoop ignore.

You can also start it by hand: **Actions > release > Run workflow** and
enter a version such as `1.2.3`; the tag `v1.2.3` is created on the
selected branch.

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

## Linux packages

`linux-packages.sh` builds the `.deb`, `.rpm` and `.apk` packages with
[nfpm](https://nfpm.goreleaser.com/), pinned to one version and checked
against its SHA-256 before it runs. Each package installs `/usr/bin/rw`,
the README, the license and the bash, zsh and fish completion scripts
(printed by `rw completion`; zsh's goes to `vendor-completions` on Debian
and `site-functions` elsewhere), and depends on `git`. The packages are not
signed, and there is no apt or dnf repository, so users download them from
the release:

```sh
sudo apt install ./relayweft-linux-amd64.deb
sudo dnf install ./relayweft-linux-amd64.rpm
sudo apk add --allow-untrusted ./relayweft-linux-amd64.apk
```

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
changes nothing. `rw update --force` replaces the binary anyway.

## Private repository

While the repository is private, release downloads need authentication:

- `rw update` sends `GITHUB_TOKEN` or `GH_TOKEN` when set; without one the
  GitHub API answers 404. A fine-grained token with read access to
  *Contents* on this repo is enough.
- Scoop and winget cannot authenticate, so they only work once the
  repository is public (winget-pkgs also rejects URLs it cannot fetch).
  Until then, download with
  `gh release download v1.2.3 --repo sparkz400/relayweft`.
