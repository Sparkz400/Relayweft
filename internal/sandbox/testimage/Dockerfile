# The image rw selftest and the sandbox tests run: git and a scripted
# stand-in for Claude Code (claude), no real agent CLI. Built by
# sandbox.BuildSelftestImage.
FROM alpine:3
RUN apk add --no-cache git
COPY claude /usr/local/bin/claude
RUN chmod 755 /usr/local/bin/claude
# Not root, like the reference image; on Linux rw runs it as your user.
USER 1000:1000
