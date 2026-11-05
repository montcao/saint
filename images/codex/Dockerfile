FROM node:22-bookworm-slim@sha256:83f487e0a63425e5b4d146fb5e5be574bcbe1b7b843d3ebafdd95eaf7767a7e5

RUN apt-get update \
    && apt-get install -y --no-install-recommends tmux \
    && rm -rf /var/lib/apt/lists/* \
    && groupmod -n agent node \
    && usermod -l agent -d /home/agent -m node \
    && npm install -g @openai/codex@0.149.1

# Pinned on purpose; the agent must not replace itself at runtime. The package
# is installed by root and the agent runs unprivileged, so an update would fail
# anyway - this stops it being attempted.
ENV npm_config_update_notifier=false

USER agent
WORKDIR /home/agent
