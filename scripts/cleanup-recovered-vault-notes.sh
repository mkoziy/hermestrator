#!/usr/bin/env bash
# Deletes retained worker notes only after the recovery workflow pushed them.
set -Eeuo pipefail

: "${RUN_ARTIFACTS_DIR:=/var/lib/swamp-worker-artifacts}"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "$RUN_ARTIFACTS_DIR" == /* && "$RUN_ARTIFACTS_DIR" != / ]] || \
  fail "run_artifacts_dir must be an absolute path other than /"
command -v rm >/dev/null || fail "rm is required"

shopt -s nullglob
note_files=("$RUN_ARTIFACTS_DIR"/*/note.json)
for note_file in "${note_files[@]}"; do
  artifact_dir="$(dirname "$note_file")"
  run_id="$(basename "$artifact_dir")"
  [[ "$run_id" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]] || \
    fail "artifact directory has an invalid workflow run ID: $run_id"
  rm -rf -- "$artifact_dir"
  printf 'Removed recovered worker artifacts for workflow run %s\n' "$run_id"
done
