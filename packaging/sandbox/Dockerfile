# The reference image for sy's container sandbox (docs/sandbox.md): git,
# Node.js, Claude Code and the Codex CLI. Build it once:
#
#   docker build -t switchyard-sandbox packaging/sandbox
#   docker build -t switchyard-sandbox https://github.com/Sparkz400/switchyard.git#main:packaging/sandbox
#
# Pin the CLIs to the versions `sy doctor` was tested with:
#
#   docker build -t switchyard-sandbox --build-arg CLAUDE_CODE_VERSION=2.1.288 --build-arg CODEX_VERSION=0.160.0 packaging/sandbox
#
# Your verify commands run in this image too, so add your toolchain in an
# image of your own (FROM switchyard-sandbox, then RUN apt-get install ...,
# or install Go, Python or .NET) and point sandbox.image at it.
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates ripgrep less procps \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=latest
ARG CODEX_VERSION=latest
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
 && npm cache clean --force

# sy runs the container as your user on Linux and macOS (--user) and as
# root without capabilities on Windows; HOME is a folder sy keeps per
# project (/sy/home), so nothing here needs a home of its own.
USER node
WORKDIR /work
