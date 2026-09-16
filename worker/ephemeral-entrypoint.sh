#!/usr/bin/env bash
set -Eeuo pipefail

# All-in-one ephemeral pod: start the orchestrator, fire every workflow
# that has a cron trigger exactly once, run coding-worker and qa-worker
# until both drain and idle out, then exit. `swamp serve`'s own --schedule
# only ticks while the process stays up, which defeats a pod meant to run
# for a few minutes every 15 — so this fires each scheduled workflow
# directly instead of waiting for swamp's internal cron to catch up.
# Meant for a k8s CronJob — see docs/remote-worker.md "Ephemeral all-in-one
# worker".

export SWAMP_WORKER_CACHE_DIR_CODING="${SWAMP_WORKER_CACHE_DIR_CODING:-/home/worker/.swamp-worker-coding}"
export SWAMP_WORKER_CACHE_DIR_QA="${SWAMP_WORKER_CACHE_DIR_QA:-/home/worker/.swamp-worker-qa}"
export CODEX_HOME="${CODEX_HOME:-/home/worker/.codex}"
export PI_CODING_AGENT_DIR="${PI_CODING_AGENT_DIR:-/home/worker/.pi/agent}"
export GH_CONFIG_DIR="${GH_CONFIG_DIR:-/home/worker/.config/gh}"
export RUN_ARTIFACTS_DIR="${RUN_ARTIFACTS_DIR:-/var/lib/swamp-worker-artifacts}"
export SWAMP_WORKER_IDLE_TIMEOUT="${SWAMP_WORKER_IDLE_TIMEOUT:-2m}"

# workflow_name: print the `name:` declared by a workflow file.
workflow_name() {
  sed -n 's/^name: //p' "$1" | head -1
}

# has_schedule_trigger: succeed if the workflow declares `trigger.schedule`.
has_schedule_trigger() {
  awk '
    /^trigger:/ { t=1; next }
    t && /^[^ ]/ { exit }
    t && /^  schedule:/ { found=1; exit }
    END { exit(found ? 0 : 1) }
  ' "$1"
}

# trigger_input_args: print `--input\nkey=value\n` pairs (one pair per
# line, for `mapfile`) from a workflow's flat `trigger.inputs` map — the
# values swamp's own scheduler would pass when it fires this workflow.
trigger_input_args() {
  awk '
    /^trigger:/ { t=1; next }
    t && /^[^ ]/ { t=0 }
    t && /^  inputs:/ { i=1; next }
    t && i && /^  [^ ]/ { i=0 }
    t && i && /^    [a-zA-Z_][a-zA-Z0-9_]*:/ { sub(/^    /, ""); print }
  ' "$1" | while IFS= read -r line; do
    printf -- '--input\n%s=%s\n' "${line%%:*}" "${line#*: }"
  done
}

# Everything below actually runs the pod; skip it when sourced for tests.
[[ "${EPHEMERAL_ENTRYPOINT_SOURCED_FOR_TEST:-}" == 1 ]] && return 0 2>/dev/null || true

: "${VAULT_GH_TOKEN:?VAULT_GH_TOKEN is required for the moontechs vault}"
: "${SWAMP_WORKER_TOKEN_CODING:?SWAMP_WORKER_TOKEN_CODING is required}"
: "${SWAMP_WORKER_TOKEN_QA:?SWAMP_WORKER_TOKEN_QA is required}"

mkdir --parents \
  "$SWAMP_WORKER_CACHE_DIR_CODING" "$SWAMP_WORKER_CACHE_DIR_QA" \
  "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$RUN_ARTIFACTS_DIR" \
  /workspace/.swamp
chown --recursive worker:worker \
  "$SWAMP_WORKER_CACHE_DIR_CODING" "$SWAMP_WORKER_CACHE_DIR_QA" \
  "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$RUN_ARTIFACTS_DIR" \
  /workspace/.swamp

if [[ -n "${OPENAI_API_KEY:-}" && -z "${CODEX_ACCESS_TOKEN:-}" ]]; then
  printf '%s' "$OPENAI_API_KEY" | gosu worker codex login --with-api-key
fi

# The vault belongs to moontechs; @swamp/git uses the gh credential helper
# rather than the per-repository token selection in github-ticket-*.sh, so
# the orchestrator process needs its own logged-in gh identity for it.
printf '%s' "$VAULT_GH_TOKEN" | gosu worker gh auth login --hostname github.com --with-token
gosu worker gh auth setup-git --hostname github.com --force
unset VAULT_GH_TOKEN

pids=()
cleanup() {
  local pid
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait "${pids[@]}" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

gosu worker swamp serve --host 127.0.0.1 --port 9090 --trusted-hosts localhost &
pids+=("$!")

printf 'waiting for orchestrator to come up...\n' >&2
ready=0
for _ in $(seq 1 30); do
  if gosu worker swamp worker list --server ws://127.0.0.1:9090 >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
[[ "$ready" == 1 ]] || { printf 'ERROR: orchestrator did not come up in time\n' >&2; exit 1; }

shopt -s nullglob
for wf in workflows/*.yaml; do
  has_schedule_trigger "$wf" || continue
  name="$(workflow_name "$wf")"
  [[ -n "$name" ]] || { printf 'WARN: %s has no name:, skipping\n' "$wf" >&2; continue; }
  mapfile -t args < <(trigger_input_args "$wf")
  printf 'running scheduled workflow once: %s %s\n' "$name" "${args[*]:-}" >&2
  gosu worker swamp workflow run "$name" --server ws://127.0.0.1:9090 "${args[@]}" \
    || printf 'WARN: %s failed, continuing\n' "$name" >&2
done

SWAMP_ORCHESTRATOR_URL=ws://127.0.0.1:9090 \
  SWAMP_WORKER_TOKEN="$SWAMP_WORKER_TOKEN_CODING" \
  SWAMP_WORKER_LABELS=pool=coding \
  SWAMP_WORKER_CACHE_DIR="$SWAMP_WORKER_CACHE_DIR_CODING" \
  gosu worker swamp worker connect &
coding_pid=$!
pids+=("$coding_pid")

SWAMP_ORCHESTRATOR_URL=ws://127.0.0.1:9090 \
  SWAMP_WORKER_TOKEN="$SWAMP_WORKER_TOKEN_QA" \
  SWAMP_WORKER_LABELS=pool=qa \
  SWAMP_WORKER_CACHE_DIR="$SWAMP_WORKER_CACHE_DIR_QA" \
  gosu worker swamp worker connect &
qa_pid=$!
pids+=("$qa_pid")

status=0
wait "$coding_pid" || status=$?
wait "$qa_pid" || status=$?
exit "$status"
