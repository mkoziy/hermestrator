#!/usr/bin/env bash
# Manual GitHub actions worker. Invoked by the github-ticket-actions Swamp
# workflow. Clones the target repo, runs the steps declared in its own
# .swamp-actions.yml manifest directly on $BASE_BRANCH (no agent branch, no
# ralphex), and reports the outcome as a VAULT_NOTE_JSON: marker line.
set -Eeuo pipefail

: "${REPO:?REPO is required}"
: "${ISSUE_NUMBER:?ISSUE_NUMBER is required}"
: "${BASE_BRANCH:=main}"

run_root=""
cleanup_workspace=true

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
trap cleanup EXIT

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
    --arg steps_log "$(cat "$steps_log" 2>/dev/null || true)" \
    --arg started_at "$started_at" \
    --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{repo:$repo, issue_number:$issue_number, issue:$issue[0], status:$status,
      failed_step:(if $failed_step == "" then null else $failed_step end),
      exit_code:(if $exit_code == "" then null else ($exit_code | tonumber) end),
      steps_log:$steps_log, started_at:$started_at, completed_at:$completed_at}' \
  | { printf 'VAULT_NOTE_JSON:'; cat; printf '\n'; }
}

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
failed_step=""
failed_exit_code=""

for ((i = 0; i < steps_count; i++)); do
  name="$(jq -r ".steps[$i].name" "$manifest_json")"
  run="$(jq -r ".steps[$i].run" "$manifest_json")"

  printf '\n[step: %s] running\n' "$name" | tee -a "$steps_log"

  set +e
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
