# Packaging and releases

Everything needed to ship `sy`: the release workflow
(`.github/workflows/release.yml`), a Scoop manifest and winget manifest
templates.

Release assets are a contract. `sy update`, Scoop and winget all look them
up by these exact names:

| Asset | Platform |
| --- | --- |
| `sy-windows-amd64.exe`, `sy-windows-arm64.exe` | Windows |
| `sy-linux-amd64`, `sy-linux-arm64` | Linux |
| `sy-darwin-amd64`, `sy-darwin-arm64` | macOS |
| `checksums.txt` | SHA-256 of every asset (`sha256sum` format) |

## Cutting a release

```sh
git tag v1.2.3
git push --tags
```

The `release` workflow runs `go test ./...`, builds all six binaries with
`-X main.version=1.2.3` (no leading `v`), writes `checksums.txt` and
creates the GitHub release with generated notes. A tag with a suffix
(`v1.3.0-rc1`) becomes a pre-release, which `sy update` and Scoop ignore.

You can also start it by hand: **Actions > release > Run workflow** and
enter a version such as `1.2.3`; the tag `v1.2.3` is created on the
selected branch.

After the release is published, refresh the manifests:

```sh
packaging/render-manifests.sh 1.2.3
```

This downloads `checksums.txt` with the `gh` CLI, updates
`packaging/scoop/sy.json` in place (commit it) and writes ready-to-submit
winget manifests to `winget-out/1.2.3/` (do not commit those).

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
needs an existing package. Set `License` in the locale manifest (and in
`scoop/sy.json`) once the project has one.

## Private repository

While the repository is private, release downloads need authentication:

- `sy update` sends `GITHUB_TOKEN` or `GH_TOKEN` when set; without one the
  GitHub API answers 404. A fine-grained token with read access to
  *Contents* on this repo is enough.
- Scoop and winget cannot authenticate, so they only work once the
  repository is public (winget-pkgs also rejects URLs it cannot fetch).
  Until then, download with
  `gh release download v1.2.3 --repo sparkz400/switchyard`.
