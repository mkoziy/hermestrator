# syntax=docker/dockerfile:1
FROM debian:bookworm-slim

ARG GO_VERSION=1.26.5
ARG NODE_MAJOR=24

ENV DEBIAN_FRONTEND=noninteractive

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
