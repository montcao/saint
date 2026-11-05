FROM node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5

RUN apt-get update \
    && apt-get install -y --no-install-recommends tmux \
    && rm -rf /var/lib/apt/lists/* \
    && groupmod -n agent node \
    && usermod -l agent -d /home/agent -m node \
    && npm install -g @anthropic-ai/claude-code@2.1.247

# The version above is pinned on purpose, so the agent must not replace it at
# runtime. DISABLE_UPDATES blocks every update path, not just the background
# check: an agent that can fetch and execute new code inside its own sandbox is
# a supply-chain route into the guest, and it silently undoes the pin.
#
# It also removes the "auto-update failed: no write permission to npm prefix"
# error, which is the visible half of the same problem - the package is
# installed by root and the agent runs as an unprivileged user, which is
# exactly how it should be.
ENV DISABLE_UPDATES=1

USER agent
WORKDIR /home/agent
