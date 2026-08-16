#!/usr/bin/env bash
# Removes one worker run's artifacts after its vault note has been pushed.
set -Eeuo pipefail

: "${WORKFLOW_RUN_ID:?WORKFLOW_RUN_ID is required}"
: "${RUN_ARTIFACTS_DIR:=/var/lib/swamp-worker-artifacts}"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "$WORKFLOW_RUN_ID" =~ ^[[:alnum:]][[:alnum:]._-]*$ ]] || \
  fail "WORKFLOW_RUN_ID contains unsupported characters"
[[ "$RUN_ARTIFACTS_DIR" == /* && "$RUN_ARTIFACTS_DIR" != / ]] || \
  fail "RUN_ARTIFACTS_DIR must be an absolute path other than /"
command -v rm >/dev/null || fail "rm is required"

readonly artifact_dir="${RUN_ARTIFACTS_DIR}/${WORKFLOW_RUN_ID}"
[[ -e "$artifact_dir" || -L "$artifact_dir" ]] || exit 0

rm -rf -- "$artifact_dir"
printf 'Removed worker artifacts for workflow run %s\n' "$WORKFLOW_RUN_ID"
