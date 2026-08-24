#!/usr/bin/env bash
# Renders retained completed-worker notes after a workflow result was lost.
set -Eeuo pipefail

: "${RUN_ARTIFACTS_DIR:=/var/lib/swamp-worker-artifacts}"
: "${VAULT_DIR:=.swamp/vault-clone}"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "$RUN_ARTIFACTS_DIR" == /* && "$RUN_ARTIFACTS_DIR" != / ]] || \
  fail "run_artifacts_dir must be an absolute path other than /"
[[ "$VAULT_DIR" != /* ]] || fail "vault_dir must be relative to the workspace"
command -v jq >/dev/null || fail "jq is required"

readonly script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
shopt -s nullglob
note_files=("$RUN_ARTIFACTS_DIR"/*/note.json)

if [[ "${#note_files[@]}" -eq 0 ]]; then
  printf 'No retained worker notes to recover\n'
  exit 0
fi

for note_file in "${note_files[@]}"; do
  artifact_dir="$(dirname "$note_file")"
  run_id="$(basename "$artifact_dir")"
  [[ "$run_id" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]] || \
    fail "artifact directory has an invalid workflow run ID: $run_id"

  repo="$(jq -er '.repo | strings' "$note_file")" || fail "invalid note payload: $note_file"
  issue_number="$(jq -er '.issue_number | numbers | floor | tostring' "$note_file")" || \
    fail "invalid note payload: $note_file"
  ralphex_config="$(jq -er '.ralphex_config | strings' "$note_file")" || \
    fail "invalid note payload: $note_file"
  [[ "$repo" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]] || fail "invalid note repo: $repo"
  [[ "$issue_number" =~ ^[1-9][0-9]*$ ]] || fail "invalid note issue number: $issue_number"
  case "$ralphex_config" in ralphex-codex|ralphex-pi) ;; *) fail "invalid note ralphex config" ;; esac

  NOTE_JSON_RAW='' \
  REPO="$repo" \
  ISSUE_NUMBER="$issue_number" \
  RALPHEX_CONFIG="$ralphex_config" \
  WORKFLOW_RUN_ID="$run_id" \
  RUN_ARTIFACTS_DIR="$RUN_ARTIFACTS_DIR" \
  VAULT_DIR="$VAULT_DIR" \
    "$script_dir/vault-write-note.sh"
done
