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

# vault-repo (models/@swamp/git/...) reads/writes this Git checkout — see
# scripts/recover-vault-notes.sh / vault-write-note.sh, both default to the
# same path. It must persist across pod runs (mount the same volume as
# /workspace/.swamp) and, if that volume is ever empty (first run, or a
# wiped PVC), gets bootstrapped below before any workflow that touches it.
export VAULT_DIR="${VAULT_DIR:-.swamp/vault-clone}"
export VAULT_REPO_URL="${VAULT_REPO_URL:-https://github.com/moontechs/notes.git}"

# swamp serve's own process log and each worker's `connect` log are not
# captured anywhere per-run (unlike ralphex/QA output, which swamp already
# writes under RUN_ARTIFACTS_DIR) — without this they'd only exist in the
# pod's own stdout, gone once its k8s Job/Pod history rotates out. Persist
# them on the same volume as RUN_ARTIFACTS_DIR, one subdirectory per tick.
export HERMESTRATOR_LOG_DIR="${HERMESTRATOR_LOG_DIR:-$RUN_ARTIFACTS_DIR/logs}"

# run_scheduled_workflow: fire one workflow once against the local
# orchestrator, tolerating failure (a bad run shouldn't block the others or
# the pod's own exit). $2+ are passed through as `swamp workflow run` args.
#
# There's no supported swamp mechanism to "fire every due trigger.schedule
# once" from outside swamp serve's own --schedule loop (verified: `swamp
# workflow run <name>` does NOT fall back to a workflow's trigger.inputs —
# every schema-required input must be passed explicitly, the same as any
# other manual run). So this is an explicit list, not auto-discovered from
# workflows/*.yaml — tests/ephemeral-entrypoint.sh cross-checks it against
# every workflow file that actually declares a trigger.schedule, so an
# onboarded poller with nothing wired in here fails that test instead of
# silently never running.
run_scheduled_workflow() {
  local name="$1"
  shift
  printf 'running scheduled workflow once: %s %s\n' "$name" "$*" >&2
  gosu worker swamp workflow run "$name" --server ws://127.0.0.1:9090 "$@" \
    || printf 'WARN: %s failed, continuing\n' "$name" >&2
}

# Update this alongside workflows/*.yaml: a new one-file-per-repo ticket
# poller, or a new input a poller's trigger.inputs starts requiring.
run_scheduled_workflows() {
  run_scheduled_workflow github-ticket-poller-weird-reader --input repo=moontechs/weird-reader
  run_scheduled_workflow github-ticket-poller-files-nest --input repo=moontechs/files-nest
  run_scheduled_workflow github-ticket-poller-streamberg --input repo=mkoziy/streamberg
  run_scheduled_workflow github-qa-poller --input repos=moontechs/files-nest,moontechs/weird-reader,mkoziy/streamberg
  run_scheduled_workflow vault-note-recovery
}

# Everything below actually runs the pod; skip it when sourced for tests.
[[ "${EPHEMERAL_ENTRYPOINT_SOURCED_FOR_TEST:-}" == 1 ]] && return 0 2>/dev/null || true

: "${VAULT_GH_TOKEN:?VAULT_GH_TOKEN is required for the moontechs vault}"
: "${SWAMP_WORKER_TOKEN_CODING:?SWAMP_WORKER_TOKEN_CODING is required}"
: "${SWAMP_WORKER_TOKEN_QA:?SWAMP_WORKER_TOKEN_QA is required}"

tick_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
tick_log_dir="$HERMESTRATOR_LOG_DIR/$tick_id"

mkdir --parents \
  "$SWAMP_WORKER_CACHE_DIR_CODING" "$SWAMP_WORKER_CACHE_DIR_QA" \
  "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$RUN_ARTIFACTS_DIR" \
  "$tick_log_dir" /workspace/.swamp
chown --recursive worker:worker \
  "$SWAMP_WORKER_CACHE_DIR_CODING" "$SWAMP_WORKER_CACHE_DIR_QA" \
  "$CODEX_HOME" "$PI_CODING_AGENT_DIR" "$GH_CONFIG_DIR" "$RUN_ARTIFACTS_DIR" \
  "$HERMESTRATOR_LOG_DIR" /workspace/.swamp

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

gosu worker swamp serve --host 127.0.0.1 --port 9090 --trusted-hosts localhost \
  >>"$tick_log_dir/serve.log" 2>&1 &
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

# Bootstrap the vault checkout if this is the first run on a fresh volume —
# vault-pull/-commit/-push (used by workflow-github-ticket-worker.yaml and
# workflow-vault-note-recovery.yaml) all assume $VAULT_DIR already exists.
if [[ ! -d "/workspace/$VAULT_DIR/.git" ]]; then
  printf 'bootstrapping vault checkout at %s\n' "$VAULT_DIR" >&2
  gosu worker swamp model method run vault-repo clone --server ws://127.0.0.1:9090 \
    --input "url=$VAULT_REPO_URL"
fi

run_scheduled_workflows

SWAMP_ORCHESTRATOR_URL=ws://127.0.0.1:9090 \
  SWAMP_WORKER_TOKEN="$SWAMP_WORKER_TOKEN_CODING" \
  SWAMP_WORKER_LABELS=pool=coding \
  SWAMP_WORKER_CACHE_DIR="$SWAMP_WORKER_CACHE_DIR_CODING" \
  gosu worker swamp worker connect >>"$tick_log_dir/coding-worker.log" 2>&1 &
coding_pid=$!
pids+=("$coding_pid")

SWAMP_ORCHESTRATOR_URL=ws://127.0.0.1:9090 \
  SWAMP_WORKER_TOKEN="$SWAMP_WORKER_TOKEN_QA" \
  SWAMP_WORKER_LABELS=pool=qa \
  SWAMP_WORKER_CACHE_DIR="$SWAMP_WORKER_CACHE_DIR_QA" \
  gosu worker swamp worker connect >>"$tick_log_dir/qa-worker.log" 2>&1 &
qa_pid=$!
pids+=("$qa_pid")

status=0
wait "$coding_pid" || status=$?
wait "$qa_pid" || status=$?
exit "$status"
