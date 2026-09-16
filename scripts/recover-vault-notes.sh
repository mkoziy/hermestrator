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

# A single corrupt/incomplete note.json (e.g. a process killed mid-write)
# must not block recovery of every other retained note — this runs
# unattended on a cron tick, so skip-and-warn beats abort-the-batch.
overall_status=0

for note_file in "${note_files[@]}"; do
  artifact_dir="$(dirname "$note_file")"
  run_id="$(basename "$artifact_dir")"
  if [[ ! "$run_id" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]]; then
    printf 'WARN: skipping artifact directory with an invalid workflow run ID: %s\n' "$run_id" >&2
    overall_status=1
    continue
  fi

  if ! repo="$(jq -er '.repo | strings' "$note_file" 2>/dev/null)" || \
     ! issue_number="$(jq -er '.issue_number | numbers | floor | tostring' "$note_file" 2>/dev/null)" || \
     ! ralphex_config="$(jq -er '.ralphex_config | strings' "$note_file" 2>/dev/null)"; then
    printf 'WARN: skipping invalid note payload: %s\n' "$note_file" >&2
    overall_status=1
    continue
  fi
  if [[ ! "$repo" =~ ^[[:alnum:]_.-]+/[[:alnum:]_.-]+$ ]]; then
    printf 'WARN: skipping note with invalid repo: %s (%s)\n' "$repo" "$note_file" >&2
    overall_status=1
    continue
  fi
  if [[ ! "$issue_number" =~ ^[1-9][0-9]*$ ]]; then
    printf 'WARN: skipping note with invalid issue number: %s (%s)\n' "$issue_number" "$note_file" >&2
    overall_status=1
    continue
  fi
  case "$ralphex_config" in
    ralphex-codex|ralphex-pi|codex|pi) ;;
    *)
      printf 'WARN: skipping note with invalid ralphex config: %s (%s)\n' "$ralphex_config" "$note_file" >&2
      overall_status=1
      continue
      ;;
  esac

  NOTE_JSON_RAW='' \
  REPO="$repo" \
  ISSUE_NUMBER="$issue_number" \
  RALPHEX_CONFIG="$ralphex_config" \
  WORKFLOW_RUN_ID="$run_id" \
  RUN_ARTIFACTS_DIR="$RUN_ARTIFACTS_DIR" \
  VAULT_DIR="$VAULT_DIR" \
    "$script_dir/vault-write-note.sh" || overall_status=1
done

exit "$overall_status"
