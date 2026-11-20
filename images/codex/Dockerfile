FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS download

COPY runtime.lock /tmp/runtime.lock

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && . /tmp/runtime.lock \
    && case "$(dpkg --print-architecture)" in \
        arm64) arch=arm64; target=aarch64-unknown-linux-musl; sha=$sha_arm64 ;; \
        amd64) arch=x64; target=x86_64-unknown-linux-musl; sha=$sha_amd64 ;; \
        *) exit 1 ;; \
    esac \
    && curl -fsSL "https://registry.npmjs.org/@openai/codex/-/codex-${version}-linux-${arch}.tgz" -o /tmp/codex.tgz \
    && printf '%s  /tmp/codex.tgz\n' "$sha" | sha256sum -c - \
    && mkdir -p /out \
    && tar -xzf /tmp/codex.tgz -C /out --strip-components=3 "package/vendor/${target}"

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --shell /bin/bash agent

# Keep the native package layout so codex finds its bundled tools.
COPY --from=download /out/ /opt/codex/
RUN ln -s /opt/codex/bin/codex /usr/local/bin/codex \
    && ln -s /opt/codex/codex-path/rg /usr/local/bin/rg
ENV PATH="/opt/codex/bin:/opt/codex/codex-path:${PATH}"

USER agent
WORKDIR /home/agent
