FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS download

COPY runtime.lock /tmp/runtime.lock

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && . /tmp/runtime.lock \
    && case "$(dpkg --print-architecture)" in \
        arm64) arch=arm64; sha=$sha_arm64 ;; \
        amd64) arch=x64; sha=$sha_amd64 ;; \
        *) exit 1 ;; \
    esac \
    && curl -fsSL "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-${arch}/-/claude-code-linux-${arch}-${version}.tgz" -o /tmp/claude.tgz \
    && printf '%s  /tmp/claude.tgz\n' "$sha" | sha256sum -c - \
    && mkdir -p /out \
    && tar -xzf /tmp/claude.tgz -C /out --strip-components=1 package/claude

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git openssh-client ripgrep less python3 tmux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --shell /bin/bash agent

COPY --from=download /out/claude /usr/local/bin/claude

# The root-owned binary is pinned at build time.
ENV DISABLE_UPDATES=1

USER agent
WORKDIR /home/agent
