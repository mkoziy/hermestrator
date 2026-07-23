# syntax=docker/dockerfile:1
FROM debian:bookworm-slim

ARG GO_VERSION=1.26.5
ARG NODE_MAJOR=24
# Build-time only (ARG, not ENV): keeps apt-get non-interactive during the
# build without leaking DEBIAN_FRONTEND=noninteractive into the running
# container's environment (an ENV instruction here would persist into every
# container started from this image).
ARG DEBIAN_FRONTEND=noninteractive

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# base system packages + gh CLI (official apt repo)
RUN apt-get update && apt-get install --no-install-recommends -y \
        git \
        curl \
        ca-certificates \
        jq \
        ripgrep \
        bubblewrap \
        build-essential \
        ffmpeg \
        unzip \
        fzf \
        gnupg \
    && mkdir -p /etc/apt/keyrings && chmod 755 /etc/apt/keyrings \
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
ENV GOPATH=/home/app/go \
    PATH=/usr/local/go/bin:/home/app/go/bin:$PATH

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

# ralphex binary (root, before switching to the app user — installs to /usr/local/bin).
# Optional BuildKit secret "github_token" authenticates the api.github.com
# lookup to avoid GitHub's low (60/hour/IP) unauthenticated rate limit;
# anonymous lookup still works fine (and is what happens by default) when no
# secret is supplied. Deliberately a --mount=type=secret, not an ARG: ARG
# values are recorded in the image's build history/layer metadata (visible
# via `docker history --no-trunc`) even when never turned into an ENV, so a
# real GitHub token passed via ARG would leak there. A secret mount is only
# ever exposed to this one RUN's filesystem (at /run/secrets/github_token,
# tmpfs-backed) and is never written into any image layer.
RUN --mount=type=secret,id=github_token \
    ARCH="$(dpkg --print-architecture)" \
    && AUTH_HEADER=() \
    && if [ -s /run/secrets/github_token ]; then AUTH_HEADER=(-H "Authorization: Bearer $(cat /run/secrets/github_token)"); fi \
    && REL_URL="$(curl -fsSL "${AUTH_HEADER[@]}" https://api.github.com/repos/umputun/ralphex/releases/latest \
        | grep -oE "https://github.com/umputun/ralphex/releases/download/[^\"]*linux_${ARCH}\.tar\.gz")" \
    && curl -fsSL "$REL_URL" -o /tmp/ralphex.tar.gz \
    && tar -xzf /tmp/ralphex.tar.gz -C /tmp ralphex \
    && install -m 755 /tmp/ralphex /usr/local/bin/ralphex-real \
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

# Global git excludes: applies to every repo any agent (hermes/ralphex/pi/
# codex) clones or works in inside this container — not just this project's
# own .gitignore. Baked in at build time (static, not env-dependent) rather
# than in entrypoint.sh's runtime git config block.
COPY --chown=app:app docker/global-gitignore /home/app/.gitignore_global
RUN git config --global core.excludesFile /home/app/.gitignore_global

# Hermes Agent — non-interactive install; skip the interactive `hermes setup`
# wizard (it requires API keys), that runs from entrypoint.sh at container start
RUN curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash -s -- --skip-setup --non-interactive

# [decision] (Task 9 fix, found via actual volume-mount end-to-end testing —
# not anticipated when Tasks 4/6 were written): the Hermes installer puts its
# OWN application checkout + private venv (~/.hermes/hermes-agent/, ~220MB
# venv + ~1.4GB node_modules) and private uv/uvx copies (~/.hermes/bin/)
# INSIDE the very directory ($HOME/.hermes == $HERMES_HOME) that this project
# treats as pure mutable state and expects to live on an external persistent
# volume across restarts (see Task 6 / entrypoint.sh). Mounting any volume at
# $HERMES_HOME — including the intended empty-first-run case — therefore
# shadows/deletes the baked-in installation, and `hermes` fails outright.
# Fix: snapshot the as-installed
# app here, at build time, so entrypoint.sh can reseed just those two
# subtrees into $HERMES_HOME at container start if they're missing, without
# touching genuine state (config.yaml, sessions/, memories/, cron/, etc).
# Only the two installed-app subtrees are snapshotted (not the whole
# directory) to avoid needlessly doubling image size with a copy of
# never-reseeded state dirs (audio_cache/, image_cache/, etc).
RUN mkdir -p "$HOME/.hermes-seed" \
    && cp -a "$HOME/.hermes/hermes-agent" "$HOME/.hermes-seed/hermes-agent" \
    && cp -a "$HOME/.hermes/bin" "$HOME/.hermes-seed/bin"

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
# /opt/ralphex-profiles/<name>/ so Hermes or helper scripts can point
# ralphex at a concrete profile directory directly. COPY runs with the
# builder's own privileges regardless of the current USER, so --chown=app:app
# is still honored here even though we already switched to `USER app` above.
COPY --chown=app:app ralphex/ralphex-codex/config /opt/ralphex-profiles/codex/config
COPY --chown=app:app ralphex/ralphex-pi/config /opt/ralphex-profiles/pi/config
COPY --chown=app:app ralphex/ralphex-pi/agents/ /opt/ralphex-profiles/pi/agents/
COPY --chown=app:app ralphex/ralphex-pi/prompts/ /opt/ralphex-profiles/pi/prompts/
COPY --chown=app:app ralphex/ralphex-pi/scripts/ /opt/ralphex-profiles/pi/scripts/
COPY --chown=app:app ralphex/ralphex-claude/config /opt/ralphex-profiles/claude/config

# planning profiles: same executors as above, kept at the original
# (pre-effort-reduction) settings so plan creation quality is unaffected by
# the coding/task-execution profiles above having their effort tuned down.
COPY --chown=app:app ralphex/ralphex-codex-planning/config /opt/ralphex-profiles/codex-planning/config
COPY --chown=app:app ralphex/ralphex-pi-planning/config /opt/ralphex-profiles/pi-planning/config
COPY --chown=app:app ralphex/ralphex-pi-planning/agents/ /opt/ralphex-profiles/pi-planning/agents/
COPY --chown=app:app ralphex/ralphex-pi-planning/prompts/ /opt/ralphex-profiles/pi-planning/prompts/
COPY --chown=app:app ralphex/ralphex-pi-planning/scripts/ /opt/ralphex-profiles/pi-planning/scripts/
COPY --chown=app:app ralphex/ralphex-claude-planning/config /opt/ralphex-profiles/claude-planning/config
RUN chmod +x /opt/ralphex-profiles/pi/scripts/*.sh /opt/ralphex-profiles/pi-planning/scripts/*.sh

COPY --chmod=755 docker/ralphex-wrapper.sh /usr/local/bin/ralphex
COPY --chmod=755 docker/ralphex-headless-plan.sh /usr/local/bin/ralphex-headless-plan

# Agent Skills (open SKILL.md standard, shared unmodified across Claude Code,
# codex CLI, and pi): baked into the read-only image layer under
# /opt/agent-skills/<skill-name>/SKILL.md; entrypoint.sh reseeds them
# additively into codex's and pi's own native skill directories
# (~/.codex/skills/, ~/.pi/agent/skills/) at every container start, the same
# "bake once, reseed on start" idiom used for the Hermes app subtrees above.
COPY --chown=app:app skills/ /opt/agent-skills/

# HERMES_HOME: explicit persistent-state directory. Expected to be a mounted
# volume in real deployments (survives `docker restart` / a future k8s
# restart policy); entrypoint.sh initializes it fresh on first run when empty.
ENV HERMES_HOME=/home/app/.hermes
# HERMES_HOME_SEED: baked-in copy of the installed app subtrees (hermes-agent/,
# bin/ — see the RUN cp -a above) entrypoint.sh reseeds into $HERMES_HOME on
# every start if they're missing there (Task 9 fix, see that RUN's comment).
ENV HERMES_HOME_SEED=/home/app/.hermes-seed

COPY --chmod=755 docker/entrypoint.sh /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
