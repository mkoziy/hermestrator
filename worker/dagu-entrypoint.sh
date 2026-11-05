#!/usr/bin/env bash
# Entrypoint for the dagu orchestrator container: prepares the volumes,
# logs gh/codex in, bootstraps the vault checkout, then drops to `worker`
# and execs dagu. Runs as root only until the final gosu.
set -Eeuo pipefail

export DAGU_HOME="${DAGU_HOME:-/var/lib/dagu}"
export CODEX_HOME="${CODEX_HOME:-/home/worker/.codex}"
export PI_CODING_AGENT_DIR="${PI_CODING_AGENT_DIR:-/home/worker/.pi/agent}"
export GH_CONFIG_DIR="${GH_CONFIG_DIR:-/home/worker/.config/gh}"
readonly artifacts_dir="${RUN_ARTIFACTS_DIR:-/var/lib/hermestrator/artifacts}"
readonly vault_dir="/workspace/${VAULT_DIR:-.vault-clone}"
readonly vault_repo_url="${VAULT_REPO_URL:-https://github.com/moontechs/notes.git}"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

# The vault belongs to moontechs, so it gets its own gh identity rather than
# the per-owner GH_TOKEN_<OWNER> that the ticket scripts export per call.
: "${VAULT_GH_TOKEN:?VAULT_GH_TOKEN is required}"
[[ "$artifacts_dir" == /* ]] || fail "RUN_ARTIFACTS_DIR must be an absolute path"

mkdir --parents "$DAGU_HOME" "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$artifacts_dir" "$vault_dir"
chown --recursive worker:worker "$DAGU_HOME" "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$artifacts_dir" "$vault_dir"

install --owner=worker --group=worker --mode=0644 /usr/local/share/dagu/config.yaml "$DAGU_HOME/config.yaml"

if [[ -n "${OPENAI_API_KEY:-}" && -z "${CODEX_ACCESS_TOKEN:-}" ]]; then
  printf '%s' "$OPENAI_API_KEY" | gosu worker codex login --with-api-key
fi

printf '%s' "$VAULT_GH_TOKEN" | gosu worker gh auth login --hostname github.com --with-token
gosu worker gh auth setup-git --hostname github.com --force
unset VAULT_GH_TOKEN

if [[ ! -d "$vault_dir/.git" ]]; then
  printf 'bootstrapping vault checkout at %s\n' "$vault_dir" >&2
  gosu worker git clone "$vault_repo_url" "$vault_dir"
fi

exec gosu worker "$@"
