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
export RUN_ARTIFACTS_DIR="${RUN_ARTIFACTS_DIR:-/var/lib/swamp-worker-artifacts}"

[[ "$RUN_ARTIFACTS_DIR" == /* ]] || {
  printf 'ERROR: RUN_ARTIFACTS_DIR must be an absolute path\n' >&2
  exit 1
}

mkdir --parents "$SWAMP_WORKER_CACHE_DIR" "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$RUN_ARTIFACTS_DIR"
chown --recursive worker:worker "$SWAMP_WORKER_CACHE_DIR" "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$RUN_ARTIFACTS_DIR"

if [[ -n "${OPENAI_API_KEY:-}" && -z "${CODEX_ACCESS_TOKEN:-}" ]]; then
  printf '%s' "$OPENAI_API_KEY" | gosu worker codex login --with-api-key
fi

# Per-owner tokens (GH_TOKEN_<OWNER>, see scripts/github-ticket-*.sh) are
# exported as GH_TOKEN only right before each script's own gh/git calls, not
# at container start, so this can't branch on GH_TOKEN being pre-set. Force
# the credential helper onto github.com regardless — gh's helper reads
# GH_TOKEN from the process env at call time, not from auth state captured
# here.
gosu worker gh auth setup-git --hostname github.com --force

exec gosu worker "$@"
