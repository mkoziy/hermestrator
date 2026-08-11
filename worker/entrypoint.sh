#!/usr/bin/env bash
set -Eeuo pipefail

: "${SWAMP_ORCHESTRATOR_URL:?SWAMP_ORCHESTRATOR_URL is required}"
: "${SWAMP_WORKER_TOKEN:?SWAMP_WORKER_TOKEN is required}"

export SWAMP_WORKER_CONCURRENCY="${SWAMP_WORKER_CONCURRENCY:-1}"
export SWAMP_WORKER_LABELS="${SWAMP_WORKER_LABELS:-pool=coding}"
export SWAMP_WORKER_CACHE_DIR="${SWAMP_WORKER_CACHE_DIR:-/home/worker/.swamp-worker}"
export CODEX_HOME="${CODEX_HOME:-/home/worker/.codex}"
export PI_CODING_AGENT_DIR="${PI_CODING_AGENT_DIR:-/home/worker/.pi/agent}"
export GH_CONFIG_DIR="${GH_CONFIG_DIR:-/home/worker/.config/gh}"

mkdir --parents "$SWAMP_WORKER_CACHE_DIR" "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR"
chown --recursive worker:worker "$SWAMP_WORKER_CACHE_DIR" "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR"

if [[ -n "${OPENAI_API_KEY:-}" && -z "${CODEX_ACCESS_TOKEN:-}" ]]; then
  printf '%s' "$OPENAI_API_KEY" | gosu worker codex login --with-api-key
fi

if [[ -n "${GH_TOKEN:-}" ]]; then
  gosu worker gh auth setup-git
fi

exec gosu worker "$@"
