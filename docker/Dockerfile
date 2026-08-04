# syntax=docker/dockerfile:1
FROM debian:bookworm-slim

ARG GO_VERSION=1.26.5
ARG NODE_MAJOR=24
ARG GENKIT_CLI_VERSION=1.15.5
ARG DEBIAN_FRONTEND=noninteractive

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# The PM runs GitHub and coding executors directly. Keep their runtime tools in
# the image, but do not include Hermes, its gateway, or Hermes-owned wrappers.
RUN apt-get update && apt-get install --no-install-recommends -y \
        build-essential \
        bubblewrap \
        ca-certificates \
        curl \
        git \
        gnupg \
        jq \
        python3 \
        python3-venv \
        ripgrep \
        unzip \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install --no-install-recommends -y gh \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --shell /bin/bash app

# Go stays available at runtime because executors may work on Go repositories.
RUN ARCH="$(dpkg --print-architecture)" \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz
ENV GOPATH=/home/app/go
ENV PATH=/usr/local/go/bin:/home/app/go/bin:/home/app/.npm-global/bin:/home/app/.local/bin:$PATH

# The Developer UI is diagnostic-only; it is never started by the container.
RUN ARCH="$(dpkg --print-architecture)" \
    && curl -fsSL "https://storage.googleapis.com/genkit-assets-cli/prod/linux-${ARCH}/v${GENKIT_CLI_VERSION}/genkit" -o /usr/local/bin/genkit \
    && chmod 755 /usr/local/bin/genkit

RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install --no-install-recommends -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# ralphex is an executor binary. Its configuration is supplied through the
# PM-owned runtime directories, never copied from this repository's old
# Hermes-era ralphex profiles.
RUN --mount=type=secret,id=github_token \
    ARCH="$(dpkg --print-architecture)" \
    && AUTH_HEADER=() \
    && if [ -s /run/secrets/github_token ]; then AUTH_HEADER=(-H "Authorization: Bearer $(cat /run/secrets/github_token)"); fi \
    && RELEASE_URL="$(curl -fsSL "${AUTH_HEADER[@]}" https://api.github.com/repos/umputun/ralphex/releases/latest | grep -oE 'https://github.com/umputun/ralphex/releases/download/[^\"]*linux_'"${ARCH}"'\.tar\.gz')" \
    && test -n "$RELEASE_URL" \
    && curl -fsSL "$RELEASE_URL" -o /tmp/ralphex.tar.gz \
    && tar -xzf /tmp/ralphex.tar.gz -C /tmp ralphex \
    && install -m 755 /tmp/ralphex /usr/local/bin/ralphex \
    && rm -f /tmp/ralphex.tar.gz /tmp/ralphex

ENV HOME=/home/app
ENV NPM_CONFIG_PREFIX=/home/app/.npm-global
RUN mkdir -p /data /home/app/.local/bin /home/app/.npm-global /home/app/go \
    && chown app:app /data \
    && chown -R app:app /home/app

USER app
WORKDIR /home/app

COPY --chown=app:app docker/global-gitignore /home/app/.gitignore_global
RUN git config --global core.excludesFile /home/app/.gitignore_global

RUN npm install -g @openai/codex @earendil-works/pi-coding-agent

# Keep project skills available to direct Codex and Pi invocations.
COPY --chown=app:app skills/ /opt/agent-skills/
RUN mkdir -p /home/app/.codex/skills /home/app/.pi/agent/skills \
    && cp -a /opt/agent-skills/. /home/app/.codex/skills/ \
    && cp -a /opt/agent-skills/. /home/app/.pi/agent/skills/

COPY --chown=app:app app/ /opt/pm-app/
RUN cd /opt/pm-app && go build -o /home/app/.local/bin/pm ./cmd/pm

COPY --chmod=755 docker/pm-entrypoint.sh /usr/local/bin/pm-entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/pm-entrypoint.sh"]
CMD ["pm"]
