#!/usr/bin/env bash
# Manual GitHub actions worker. Invoked by the github-ticket-actions Swamp
# workflow. Clones the target repo, runs the steps declared in its own
# .swamp-actions.yml manifest directly on $BASE_BRANCH (no agent branch, no
# ralphex), and reports the outcome as a VAULT_NOTE_JSON: marker line.
#
# Runs in two phases so that manifest steps (potentially attacker-controlled
# via .swamp-actions.yml in a repo that takes external PRs) never execute in
# a process whose /proc/<pid>/environ contains GH_TOKEN/CODEX_ACCESS_TOKEN/
# OPENAI_API_KEY/OPENCODE_API_KEY/SWAMP_WORKER_TOKEN. A shell `unset` does
# NOT clear /proc/<pid>/environ (it only reflects the process's env at its
# own execve()); the only way to scrub THIS process's own environ is a
# genuine execve() with a scrubbed envp, which is what `exec env -i ...`
# below does: phase 1 does all credential-needing setup (repo view/clone,
# issue fetch, manifest parse+validate) and writes what phase 2 needs to disk
# under $run_root, then re-execs itself (same PID) via `exec` into a fully
# clean environment for phase 2, which runs the manifest steps and emits the
# vault note using only on-disk state.
#
# IMPORTANT (honest limitation, do not "fix" this again at the script level):
# this re-exec closes the leak vector for THIS script's own process only. It
# does NOT close /proc/<pid>/environ readability in general: on Linux that's
# per-UID, not per-process-tree, so any co-resident same-UID process can read
# it -- and the `swamp worker` daemon that launched this script as a
# subprocess holds the same credentials in its OWN environ for the entire
# lifetime of the coding-worker container, re-exec or no re-exec. No amount
# of process hygiene inside this one script can close that. See "Security
# considerations" in docs/swamp-actions-manifest.md for the actual scope of
# what is and isn't covered, and what closing it fully would require
# (infra-level: a separate, narrowly-scoped worker pool for run-actions, or
# OS-level sandboxing of manifest step execution).
set -Eeuo pipefail

# Captured before any `cd` so the phase-1 -> phase-2 `exec ... bash "$script_path"`
# re-exec below still works when this script is invoked via a relative path
# (the workflow invokes it as `scripts/github-actions-worker.sh`, so `$0` stops
# resolving correctly the moment phase 1 does `cd "$checkout"`).
script_path="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

fail() {
  cleanup_workspace=false
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

# Prints VAULT_NOTE_JSON:{...} the report job's comment/vault-note steps grep
# out of this step's stdout. status is "success" or "failed"; failed_step and
# exit_code are null on success.
emit_vault_note() {
  local status_label="$1"
  local failed_step_val="${2:-}"
  local exit_code_val="${3:-}"
  jq -nc \
    --arg repo "$REPO" \
    --argjson issue_number "$ISSUE_NUMBER" \
    --slurpfile issue "$issue_json" \
    --arg status "$status_label" \
    --arg failed_step "$failed_step_val" \
    --arg exit_code "$exit_code_val" \
    --rawfile steps_log "$steps_log" \
    --arg started_at "$started_at" \
    --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{repo:$repo, issue_number:$issue_number, issue:$issue[0], status:$status,
      failed_step:(if $failed_step == "" then null else $failed_step end),
      exit_code:(if $exit_code == "" then null else ($exit_code | tonumber) end),
      steps_log:$steps_log, started_at:$started_at, completed_at:$completed_at}' \
  | { printf 'VAULT_NOTE_JSON:'; cat; printf '\n'; }
}

cleanup() {
  local status=$?
  if [[ -z "$run_root" ]]; then
    :
  elif [[ "$cleanup_workspace" == true ]]; then
    rm -rf "$run_root"
  else
    printf 'Workspace preserved for diagnosis: %s\n' "$run_root" >&2
  fi
  exit "$status"
}

run_steps() {
  # Read the manifest into memory exactly once, before any step runs, and
  # iterate over that in-memory copy for the rest of the loop. $manifest_json
  # lives at $run_root/manifest.json, a copy made before step 1 runs -- but a
  # malicious manifest step's `run:` command still executes with filesystem
  # access and could overwrite that copy; re-reading the file from disk on
  # each iteration would let an early step rewrite later steps (or forge a
  # success marker) out from under phase-1's validation (TOCTOU).
  # manifest_content is never re-read from disk after this point.
  local manifest_content
  manifest_content="$(cat "$manifest_json")"

  steps_count="$(jq '.steps | length' <<<"$manifest_content")"
  failed_step=""
  failed_exit_code=""

  for ((i = 0; i < steps_count; i++)); do
    name="$(jq -r ".steps[$i].name" <<<"$manifest_content")"
    run="$(jq -r ".steps[$i].run" <<<"$manifest_content")"

    printf '\n[step: %s] running\n' "$name" | tee -a "$steps_log"

    # Manifest steps come from the target repo's own .swamp-actions.yml,
    # which is potentially untrusted (external-PR-controlled) content. This
    # process's own environment is already fully scrubbed of credentials
    # (see the re-exec in phase 1 below), and env -i strips even the
    # non-secret phase-2 bookkeeping vars (RUN_ROOT/REPO/etc.) from what the
    # step itself sees, so a step only ever gets PATH/HOME/LANG/TMPDIR.
    set +e
    env -i \
      PATH="$PATH" \
      HOME="$HOME" \
      LANG="${LANG:-C.UTF-8}" \
      TMPDIR="${TMPDIR:-/tmp}" \
      bash -c "$run" 2>&1 | while IFS= read -r line || [[ -n "$line" ]]; do
      printf '[step: %s] %s\n' "$name" "$line"
    done | tee -a "$steps_log"
    exit_code="${PIPESTATUS[0]}"
    set -e

    if [[ "$exit_code" -ne 0 ]]; then
      printf '[step: %s] failed with exit code %s\n' "$name" "$exit_code" | tee -a "$steps_log" >&2
      failed_step="$name"
      failed_exit_code="$exit_code"
      break
    fi
    printf '[step: %s] succeeded\n' "$name" | tee -a "$steps_log"
  done

  if [[ -n "$failed_step" ]]; then
    emit_vault_note failed "$failed_step" "$failed_exit_code"
    fail "step '$failed_step' failed with exit code $failed_exit_code"
  fi

  emit_vault_note success
}

if [[ "${GITHUB_ACTIONS_PHASE2:-}" == "1" ]]; then
  # Phase 2: this process was re-exec'd with a scrubbed environment (see
  # phase 1 below), so /proc/$$/environ no longer contains any credential.
  # All state needed here was written to disk by phase 1 under $RUN_ROOT.
  : "${RUN_ROOT:?RUN_ROOT is required in phase 2}"
  : "${REPO:?REPO is required in phase 2}"
  : "${ISSUE_NUMBER:?ISSUE_NUMBER is required in phase 2}"
  : "${STARTED_AT:?STARTED_AT is required in phase 2}"

  run_root="$RUN_ROOT"
  cleanup_workspace=true
  trap cleanup EXIT
  trap 'cleanup_workspace=false' ERR

  readonly checkout="${run_root}/repo"
  readonly issue_json="${run_root}/issue.json"
  readonly manifest_json="${run_root}/manifest.json"
  readonly steps_log="${run_root}/steps.log"
  started_at="$STARTED_AT"

  cd "$checkout"
  run_steps
  exit 0
fi

# Phase 1: normal (credentialed) setup. Nothing below the re-exec at the
# bottom of this branch may depend on GH_TOKEN/CODEX_ACCESS_TOKEN/
# OPENAI_API_KEY/OPENCODE_API_KEY/SWAMP_WORKER_TOKEN.

: "${REPO:?REPO is required}"
: "${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
: "${BASE_BRANCH:=main}"

run_root=""
cleanup_workspace=true
trap cleanup EXIT

[[ "$REPO" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || fail "repo must be owner/name"
[[ "$ISSUE_NUMBER" =~ ^[1-9][0-9]*$ ]] || fail "issue_number must be a positive integer"
[[ "$BASE_BRANCH" =~ ^[[:alnum:]_./-]+$ ]] || fail "base_branch contains unsupported characters"
git check-ref-format --branch "$BASE_BRANCH" >/dev/null || fail "base_branch is not a valid git branch"

command -v gh >/dev/null || fail "gh is required"
command -v git >/dev/null || fail "git is required"
command -v jq >/dev/null || fail "jq is required"
command -v yq >/dev/null || fail "yq is required"

run_root="$(mktemp -d "${TMPDIR:-/tmp}/github-actions-worker.XXXXXX")"
readonly checkout="${run_root}/repo"
readonly issue_json="${run_root}/issue.json"
readonly manifest_json="${run_root}/manifest.json"
readonly steps_log="${run_root}/steps.log"
: > "$steps_log"
# A failed run is diagnostically useful; preserve its checkout while Swamp
# retains the command's streaming log and original exit status.
trap 'cleanup_workspace=false' ERR

printf 'Validating repository %s\n' "$REPO"
gh repo view "$REPO" --json nameWithOwner >/dev/null || fail "repository does not exist or is inaccessible"

printf 'Fetching issue #%s\n' "$ISSUE_NUMBER"
gh issue view "$ISSUE_NUMBER" --repo "$REPO" \
  --json number,title,state,url,labels,body,comments >"$issue_json" || fail "issue does not exist or is inaccessible"

# Unlike github-ticket-worker.sh this flow does not fail on a closed issue:
# the poller already stripped the label before triggering this run, so the
# run was legitimately queued and should still execute.
if [[ "$(jq -r '.state' "$issue_json")" != "OPEN" ]]; then
  printf 'WARNING: issue #%s is not open, continuing anyway\n' "$ISSUE_NUMBER" >&2
fi

printf 'Cloning %s into isolated workspace\n' "$REPO"
gh repo clone "$REPO" "$checkout" -- --branch "$BASE_BRANCH" --single-branch
cd "$checkout"

readonly manifest_file="${checkout}/.swamp-actions.yml"
[[ -f "$manifest_file" ]] || fail ".swamp-actions.yml is missing at the repository root"

yq -o=json . "$manifest_file" >"$manifest_json" || fail ".swamp-actions.yml could not be parsed as YAML"

jq -e '.version == 1' "$manifest_json" >/dev/null || fail ".swamp-actions.yml: version must be 1"
jq -e '(.steps | type) == "array" and (.steps | length) > 0' "$manifest_json" >/dev/null || \
  fail ".swamp-actions.yml: steps must be a non-empty array"

steps_count="$(jq '.steps | length' "$manifest_json")"
for ((i = 0; i < steps_count; i++)); do
  jq -e ".steps[$i].name | type == \"string\" and length > 0" "$manifest_json" >/dev/null || \
    fail ".swamp-actions.yml: steps[$i].name must be a non-empty string"
  # name is printed raw into "[step: <name>] ..." stdout lines that
  # github-actions-comment.sh/vault-write-actions-note.sh later grep -m1 a
  # "^VAULT_NOTE_JSON:" marker out of. A name containing a newline could
  # inject a line that wins that race ahead of the real marker (repo content
  # is attacker-controlled if the target repo takes external PRs), so reject
  # embedded newlines here rather than let them reach stdout.
  jq -e ".steps[$i].name | test(\"\\n\") | not" "$manifest_json" >/dev/null || \
    fail ".swamp-actions.yml: steps[$i].name must not contain newlines"
  jq -e ".steps[$i].run | type == \"string\" and length > 0" "$manifest_json" >/dev/null || \
    fail ".swamp-actions.yml: steps[$i].run must be a non-empty string"
done

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Everything needed by phase 2 (manifest step execution + vault note
# emission) is now on disk under $run_root. Re-exec via a genuine execve()
# (same PID) into a fully scrubbed environment: this clears *this process's
# own* /proc/<pid>/environ of GH_TOKEN and friends -- `unset` only updates
# this shell's variable table, not the environ that execve() already wrote
# to the kernel/procfs at this process's own startup. This closes the leak
# for this specific PID, but /proc/<pid>/environ readability is per-UID, not
# per-process-tree: the `swamp worker` daemon that spawned this script also
# holds these same secrets in its own environ, for the container's entire
# life, and this re-exec does nothing about that. See "Security
# considerations" in docs/swamp-actions-manifest.md.
exec env -i \
  PATH="$PATH" \
  HOME="$HOME" \
  LANG="${LANG:-C.UTF-8}" \
  TMPDIR="${TMPDIR:-/tmp}" \
  GITHUB_ACTIONS_PHASE2=1 \
  RUN_ROOT="$run_root" \
  REPO="$REPO" \
  ISSUE_NUMBER="$ISSUE_NUMBER" \
  STARTED_AT="$started_at" \
  bash "$script_path"
