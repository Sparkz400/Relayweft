# The reference image for rw's container sandbox (docs/sandbox.md): git,
# Node.js, Claude Code and the Codex CLI. Build it once:
#
#   docker build -t relayweft-sandbox packaging/sandbox
#   docker build -t relayweft-sandbox https://github.com/Sparkz400/relayweft.git#main:packaging/sandbox
#
# Pin the CLIs to the versions `rw doctor` was tested with:
#
#   docker build -t relayweft-sandbox --build-arg CLAUDE_CODE_VERSION=2.1.288 --build-arg CODEX_VERSION=0.160.0 packaging/sandbox
#
# Your verify commands run in this image too, so add your toolchain in an
# image of your own (FROM relayweft-sandbox, then RUN apt-get install ...,
# or install Go, Python or .NET) and point sandbox.image at it.
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates ripgrep less procps \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=latest
ARG CODEX_VERSION=latest
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
 && npm cache clean --force

# rw runs the container as your user on Linux and macOS (--user) and as
# root without capabilities on Windows; HOME is a folder rw keeps per
# project (/rw/home), so nothing here needs a home of its own.
USER node
WORKDIR /work
