# syntax=docker/dockerfile:1
FROM debian:bookworm-slim

ARG GO_VERSION=1.26.5
ARG NODE_MAJOR=24

ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# base system packages + gh CLI (official apt repo)
RUN apt-get update && apt-get install --no-install-recommends -y \
        git \
        curl \
        ca-certificates \
        jq \
        ripgrep \
        build-essential \
        ffmpeg \
        unzip \
        fzf \
        gnupg \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install --no-install-recommends -y gh \
    && rm -rf /var/lib/apt/lists/*

# non-root user. HOME is intentionally NOT set globally here: this stays root's
# $HOME (/root) through the rest of the root-run install steps below, so tools
# like `uv`'s installer don't write root-owned dotfiles into /home/app. HOME is
# set to /home/app only once we actually switch to `USER app` further down.
RUN useradd -m -s /bin/bash app

# Go toolchain
RUN ARCH="$(dpkg --print-architecture)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz
ENV PATH=/usr/local/go/bin:$PATH
ENV GOPATH=/home/app/go
ENV PATH=/home/app/go/bin:$PATH

# Node.js LTS (NodeSource). Confirmed against the real Hermes installer
# (install.sh): it only installs its own managed Node under $HERMES_HOME/node
# (and rewrites npm's global prefix) when the system node fails
# node_satisfies_build (^20.19 || >=22.12). NODE_MAJOR here must stay >= 22.12
# so the installer detects this system Node as sufficient and leaves it alone.
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install --no-install-recommends -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# uv (astral) + system python3. Confirmed against the real Hermes installer:
# it always provisions its OWN Python 3.11 via `uv python install`/`uv venv`,
# regardless of any system Python present — there is no shared-Python
# collision to avoid here (unlike Node). System python3 is installed anyway
# for general availability / the project's own validation checks.
RUN apt-get update && apt-get install --no-install-recommends -y python3 python3-venv \
    && rm -rf /var/lib/apt/lists/*
RUN export UV_INSTALL_DIR=/usr/local/bin && curl -LsSf https://astral.sh/uv/install.sh | sh

# make Go/uv-installed binaries available to the app user too
RUN mkdir -p /home/app/go && chown -R app:app /home/app/go

# ralphex binary (root, before switching to the app user — installs to /usr/local/bin)
RUN ARCH="$(dpkg --print-architecture)" \
    && REL_URL="$(curl -fsSL https://api.github.com/repos/umputun/ralphex/releases/latest \
        | grep -oE "https://github.com/umputun/ralphex/releases/download/[^\"]*linux_${ARCH}\.tar\.gz")" \
    && curl -fsSL "$REL_URL" -o /tmp/ralphex.tar.gz \
    && tar -xzf /tmp/ralphex.tar.gz -C /tmp ralphex \
    && install -m 755 /tmp/ralphex /usr/local/bin/ralphex \
    && rm -f /tmp/ralphex.tar.gz /tmp/ralphex

# non-root `npm install -g` needs a writable prefix — the system Node's
# default global prefix (/usr) is root-owned
ENV NPM_CONFIG_PREFIX=/home/app/.npm-global
ENV PATH=/home/app/.npm-global/bin:/home/app/.local/bin:$PATH
RUN mkdir -p /home/app/.npm-global && chown -R app:app /home/app/.npm-global

# Debian's /etc/profile unconditionally rewrites PATH by uid for LOGIN shells
# (e.g. `bash -l`, `docker exec -it ... bash --login`), which would otherwise
# wipe out the Go/Node/npm-global/uv/.local PATH entries set via ENV above —
# those only apply to non-login shells and Docker's own RUN/CMD/ENTRYPOINT.
# A profile.d script re-appends them after that reset so login shells work too.
RUN echo 'export PATH="/home/app/.npm-global/bin:/home/app/.local/bin:/home/app/go/bin:/usr/local/go/bin:$PATH"' \
        > /etc/profile.d/00-hermes-path.sh \
    && chmod +x /etc/profile.d/00-hermes-path.sh

ENV HOME=/home/app
USER app
WORKDIR /home/app

# Hermes Agent — non-interactive install; skip the interactive `hermes setup`
# wizard (it requires API keys), that runs from entrypoint.sh at container start
RUN curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash -s -- --skip-setup --non-interactive

# Mnemosyne memory layer as an isolated uv tool (exposes `mnemosyne` on PATH via ~/.local/bin).
# Deviates from the plan's literal `[all]` extra: `all` also pulls in the `llm`
# extra (ctransformers + llama-cpp-python), which has no prebuilt wheel here and
# fails to compile from source without `cmake` (not installed, and a from-source
# llama.cpp build is slow/fragile in a container). That extra is local-LLM
# inference, not needed for this deployment (Hermes talks to LLMs via API) — so
# only the extras mnemosyne actually needs here (MCP server + embeddings +
# openclaw integration) are installed.
RUN uv tool install "mnemosyne-memory[mcp,embeddings,openclaw]"

# codex CLI (OpenAI) + pi CLI — package names verified against the npm
# registry at implementation time: @openai/codex (bin `codex`) and
# @earendil-works/pi-coding-agent (bin `pi`, matches ralphex-pi/scripts/pi-as-claude.sh)
RUN npm install -g @openai/codex @earendil-works/pi-coding-agent

# ralphex profiles: baked into the read-only image layer under
# /opt/ralphex-profiles/<name>/; ralphex-use-profile.sh (below) copies the
# selected one into ~/.config/ralphex at runtime. COPY runs with the
# builder's own privileges regardless of the current USER, so --chown=app:app
# is still honored here even though we already switched to `USER app` above.
COPY --chown=app:app ralphex/ralphex-codex/config /opt/ralphex-profiles/codex/config
COPY --chown=app:app ralphex/ralphex-pi/config /opt/ralphex-profiles/pi/config
COPY --chown=app:app ralphex/ralphex-pi/agents/ /opt/ralphex-profiles/pi/agents/
COPY --chown=app:app ralphex/ralphex-pi/prompts/ /opt/ralphex-profiles/pi/prompts/
COPY --chown=app:app ralphex/ralphex-pi/scripts/ /opt/ralphex-profiles/pi/scripts/
COPY --chown=app:app ralphex/ralphex-claude/config /opt/ralphex-profiles/claude/config
RUN chmod +x /opt/ralphex-profiles/pi/scripts/*.sh

# profile switcher: root-owned, world-executable, lives outside any profile
# so it's available before a profile has ever been selected.
COPY --chmod=755 docker/ralphex-use-profile.sh /usr/local/bin/ralphex-use-profile.sh

# HERMES_HOME: explicit persistent-state directory. Expected to be a mounted
# volume in real deployments (survives `docker restart` / a future k8s
# restart policy); entrypoint.sh restores it from HERMES_BACKUP_REPO or
# initializes it fresh on first run when empty.
ENV HERMES_HOME=/home/app/.hermes

# self-backup script + the cron job entrypoint.sh registers to run it daily
# (Task 7): see docker/hermes-backup.sh's own header for the manual-trigger
# command.
COPY --chmod=755 docker/hermes-backup.sh /usr/local/bin/hermes-backup.sh

COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
