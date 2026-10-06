---
title: Install
weight: 1
---

`rw` is one binary for Windows, macOS and Linux, on amd64 and arm64. It needs **Git 2.38 or newer** and at least one agent CLI (below).

## Relayweft

**Windows** with [Scoop](https://scoop.sh):

```sh
scoop install https://raw.githubusercontent.com/sparkz400/relayweft/main/packaging/scoop/rw.json
```

**winget:** the manifests are ready, but the package is not in winget-pkgs yet.

**macOS and Linux** with Homebrew:

```sh
brew tap sparkz400/relayweft https://github.com/Sparkz400/Relayweft
brew install relayweft
```

**Debian, Ubuntu, Fedora, RHEL, openSUSE, Alpine:** each release from v0.3.0 has `.deb`, `.rpm` and `.apk` packages for amd64 and arm64.

From v0.4.0, use the [signed apt, dnf or apk repository](../project/packaging.md#apt-dnf-and-apk-repositories) to receive updates through your package manager. For a standalone package, [verify the release](../project/packaging.md#verifying-a-release), then install it:

```sh
curl -fsSLO https://github.com/Sparkz400/Relayweft/releases/latest/download/relayweft-linux-amd64.deb
sudo apt install ./relayweft-linux-amd64.deb
# Fedora and friends: relayweft-linux-amd64.rpm, then sudo dnf install ./relayweft-linux-amd64.rpm
```

On Alpine, also download `relayweft-apk.rsa.pub` from the same release, then:

```sh
sudo install -m 0644 relayweft-apk.rsa.pub /etc/apk/keys/relayweft-apk.rsa.pub
sudo apk add ./relayweft-linux-amd64.apk
```

**Arch Linux:** `packaging/aur` has the PKGBUILD of the AUR package `relayweft-bin`. It is not published yet.

**Any OS, by hand:** download `rw-<os>-<arch>` (`rw-windows-amd64.exe` on Windows) from the [latest release](https://github.com/Sparkz400/Relayweft/releases/latest), check it against `checksums.txt`, and put it on your PATH as `rw`:

```sh
curl -fsSLO https://github.com/Sparkz400/Relayweft/releases/latest/download/rw-linux-amd64
curl -fsSLO https://github.com/Sparkz400/Relayweft/releases/latest/download/checksums.txt
sha256sum --ignore-missing -c checksums.txt
install -m 755 rw-linux-amd64 ~/.local/bin/rw
```

**With Go** 1.26 or newer:

```sh
go install github.com/sparkz400/relayweft/cmd/rw@latest
```

`rw version` then says `dev`. Run the same command again to update.

**From source:**

```sh
git clone https://github.com/Sparkz400/Relayweft
cd Relayweft
go build -o rw ./cmd/rw     # rw.exe on Windows
```

## Updates

`rw update` replaces a downloaded binary with the newest release, after checking it against the release's `checksums.txt`. If a package manager installed `rw`, it tells you to use that instead. The [release assets](/packaging/README.md) keep their names from release to release, so scripts can rely on them.

## Agent CLIs

Install at least one and log in. One is enough; `rw setup` tells you what is missing.

```powershell
npm install -g @anthropic-ai/claude-code   # or: irm https://claude.ai/install.ps1 | iex
claude auth login
npm install -g @openai/codex
codex login
```

Gemini CLI, Qwen Code, DeepSeek and local Ollama models are optional: see [providers](../guides/providers.md).

Use **Windows Terminal** for the full look. Legacy `conhost` gets an ASCII theme by itself (force either with `--ascii` or `--unicode`).

<!-- include README.md#upgrading-from-switchyard-sy -->
