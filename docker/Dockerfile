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

# non-root user
RUN useradd -m -s /bin/bash app
ENV HOME=/home/app

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
ENV UV_INSTALL_DIR=/usr/local/bin
RUN curl -LsSf https://astral.sh/uv/install.sh | sh

# make Go/uv-installed binaries available to the app user too
RUN mkdir -p /home/app/go && chown -R app:app /home/app/go
