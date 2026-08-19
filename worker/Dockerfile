# syntax=docker/dockerfile:1.7
FROM node:22.20.0-bookworm-slim

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH
ARG SWAMP_VERSION=20260809.004828.0-sha.b61c9de2
ARG RALPHEX_VERSION=1.6.1
ARG GH_VERSION=2.97.0
ARG CODEX_VERSION=0.147.0
ARG PI_VERSION=0.82.1
ARG GREMLINS_VERSION=0.6.0
ARG RALPHEX_PI_WRAPPER_SHA256=663b0cd6393fc7caad991527be362efa7e316cdf2460fb1410f95a9925573d4a

RUN apt-get update \
    && apt-get install --no-install-recommends -y bash ca-certificates curl git gosu jq openssh-client \
    && rm -rf /var/lib/apt/lists/*

RUN case "$TARGETARCH" in \
      amd64) swamp_arch=x86_64; swamp_sha=03c8133320ddd2082647958089f5c15c659aa409c4688fb6288f08ea53f35b68; ralphex_arch=amd64; ralphex_sha=47755c3baa8a45843308e59f069c83ca21ff0a7969cc1a31fb4c0a68fa7c4f5c; gh_arch=amd64; gh_sha=a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112; gremlins_arch=amd64; gremlins_sha=b02a42e47935f891c9a411d68c07e211c7082609e79c2435b67c85ee9658c538 ;; \
      arm64) swamp_arch=aarch64; swamp_sha=825d3a2b337c248b6d1ac690ac670740b4e47dca4da0e4e05c69295a4705ff52; ralphex_arch=arm64; ralphex_sha=bd073dca5a538c2263f57ff8ed2b1e18e1628047ec64aa8d1724982e2c06078b; gh_arch=arm64; gh_sha=73ea440ecad9c9e284429997ee6f93577bc6f7bc6fba357ef62c53ad8fb641a5; gremlins_arch=arm64; gremlins_sha=686542674e54559afb7e86d6994d342c19a856cfd970c315748989537936664c ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac \
    && curl --fail --location --silent --show-error \
      "https://github.com/swamp-club/swamp/releases/download/v${SWAMP_VERSION}/swamp-linux-${swamp_arch}" \
      --output /usr/local/bin/swamp \
    && echo "${swamp_sha}  /usr/local/bin/swamp" | sha256sum --check - \
    && chmod 0755 /usr/local/bin/swamp \
    && curl --fail --location --silent --show-error \
      "https://github.com/umputun/ralphex/releases/download/v${RALPHEX_VERSION}/ralphex_${RALPHEX_VERSION}_linux_${ralphex_arch}.tar.gz" \
      --output /tmp/ralphex.tar.gz \
    && echo "${ralphex_sha}  /tmp/ralphex.tar.gz" | sha256sum --check - \
    && tar --extract --gzip --file /tmp/ralphex.tar.gz --directory /usr/local/bin ralphex \
    && rm /tmp/ralphex.tar.gz \
    && curl --fail --location --silent --show-error \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${gh_arch}.tar.gz" \
      --output /tmp/gh.tar.gz \
    && echo "${gh_sha}  /tmp/gh.tar.gz" | sha256sum --check - \
    && tar --extract --gzip --file /tmp/gh.tar.gz --directory /usr/local/bin --strip-components=2 "gh_${GH_VERSION}_linux_${gh_arch}/bin/gh" \
    && rm /tmp/gh.tar.gz \
    && curl --fail --location --silent --show-error \
      "https://github.com/go-gremlins/gremlins/releases/download/v${GREMLINS_VERSION}/gremlins_${GREMLINS_VERSION}_linux_${gremlins_arch}.tar.gz" \
      --output /tmp/gremlins.tar.gz \
    && echo "${gremlins_sha}  /tmp/gremlins.tar.gz" | sha256sum --check - \
    && tar --extract --gzip --file /tmp/gremlins.tar.gz --directory /usr/local/bin gremlins \
    && rm /tmp/gremlins.tar.gz

RUN npm install --global --ignore-scripts \
      "@openai/codex@${CODEX_VERSION}" \
      "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && npm cache clean --force

RUN useradd --create-home --shell /bin/bash worker \
    && mkdir --parents /workspace /home/worker/.config /home/worker/.codex /home/worker/.pi/agent /home/worker/.swamp-worker /home/worker/.config/gh \
    && chown --recursive worker:worker /workspace /home/worker

COPY --chmod=0755 worker/entrypoint.sh /usr/local/bin/ticket-worker-entrypoint
COPY --chown=worker:worker worker/ralphex-codex/ /home/worker/.config/ralphex-codex/
COPY --chown=worker:worker worker/ralphex-pi/ /home/worker/.config/ralphex-pi/
COPY --chown=worker:worker worker/ralphex-common/agents/ /home/worker/.config/ralphex-codex/agents/
COPY --chown=worker:worker worker/ralphex-common/prompts/ /home/worker/.config/ralphex-codex/prompts/
COPY --chown=worker:worker worker/ralphex-common/agents/ /home/worker/.config/ralphex-pi/agents/
COPY --chown=worker:worker worker/ralphex-common/prompts/ /home/worker/.config/ralphex-pi/prompts/
COPY --chown=worker:worker . /workspace

RUN curl --fail --location --silent --show-error \
      "https://raw.githubusercontent.com/umputun/ralphex/v${RALPHEX_VERSION}/scripts/pi-as-claude/pi-as-claude.sh" \
      --output /home/worker/.config/ralphex-pi/scripts/pi-as-claude.sh \
    && echo "${RALPHEX_PI_WRAPPER_SHA256}  /home/worker/.config/ralphex-pi/scripts/pi-as-claude.sh" | sha256sum --check - \
    && chmod 0755 /home/worker/.config/ralphex-pi/scripts/*.sh \
    && chown --recursive worker:worker /home/worker/.config

ENV HOME=/home/worker \
    CODEX_HOME=/home/worker/.codex \
    PI_CODING_AGENT_DIR=/home/worker/.pi/agent \
    GH_CONFIG_DIR=/home/worker/.config/gh \
    SWAMP_WORKER_CACHE_DIR=/home/worker/.swamp-worker \
    SWAMP_WORKER_CONCURRENCY=1 \
    SWAMP_WORKER_LABELS=pool=coding

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/ticket-worker-entrypoint"]
CMD ["swamp", "worker", "connect"]
