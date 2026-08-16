#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/cleanup-run-artifacts.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

mkdir -p "$test_root/artifacts/run-123" "$test_root/artifacts/keep-me"
printf 'artifact data\n' >"$test_root/artifacts/run-123/ralphex.stdout.log"
printf 'retain this\n' >"$test_root/artifacts/keep-me/marker"

WORKFLOW_RUN_ID=run-123 \
RUN_ARTIFACTS_DIR="$test_root/artifacts" \
"$repo_root/scripts/cleanup-run-artifacts.sh" >/dev/null

[[ ! -e "$test_root/artifacts/run-123" ]]
[[ -f "$test_root/artifacts/keep-me/marker" ]]
