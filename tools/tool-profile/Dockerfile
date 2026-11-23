# Explicit developer build on an isolated Linux ARM64 builder. Never invoked by
# pane launch. Snapshot pins resolution; signed Debian metadata remains required.
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS base
RUN test "$(dpkg --print-architecture)" = arm64 \
    && rm -f /etc/apt/sources.list.d/debian.sources \
    && printf '%s\n' \
       'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20261101T000000Z bookworm main' \
       'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20261101T000000Z bookworm-security main' \
       > /etc/apt/sources.list
# The base has no CA bundle: use the system Debian keyring to authenticate the
# snapshot over HTTP for this bootstrap, then restore HTTPS for all downloads.
RUN sed -i 's|https://snapshot|http://snapshot|g' /etc/apt/sources.list \
    && apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && sed -i 's|http://snapshot|https://snapshot|g' /etc/apt/sources.list

FROM base AS download
COPY runtime.lock /tmp/runtime.lock
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && . /tmp/runtime.lock \
    && curl --fail --show-error --silent --location --proto '=https' --proto-redir '=https' \
       "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64/-/claude-code-linux-arm64-${version}.tgz" -o /tmp/claude.tgz \
    && printf '%s  /tmp/claude.tgz\n' "$sha_arm64" | sha256sum -c - \
    && mkdir /out && tar -xzf /tmp/claude.tgz -C /out --strip-components=1 package/claude \
    && printf '%s  /out/claude\n' "$sha_binary_arm64" | sha256sum -c -

FROM base AS baseline
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash git python3 python3-venv python3-pip openssh-client ripgrep curl less \
    ca-certificates ncurses-term \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home --shell /bin/bash agent
COPY --from=download /out/claude /usr/local/bin/claude
ENV DISABLE_UPDATES=1 ENABLE_CLAUDEAI_MCP_SERVERS=false LANG=C.UTF-8 TERM=xterm-256color HOME=/home/agent
USER agent
WORKDIR /home/agent

FROM base AS assemble
RUN apt-get update && apt-get install -y --no-install-recommends erofs-utils e2fsprogs python3
COPY --from=baseline / /tree/
COPY assemble.py /assemble.py
RUN --network=none python3 /assemble.py /tree /out

FROM scratch AS disks
COPY --from=assemble /out/ /
