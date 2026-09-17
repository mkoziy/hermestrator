#!/usr/bin/env bash
# Regression test for worker/ephemeral-entrypoint.sh's prune_stale_run_data:
# entries older than STALE_RUN_DATA_RETENTION_DAYS are removed, entries
# within the retention window (a fresh tick dir, an in-flight run's
# artifacts) are left alone.
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
entrypoint="$repo_root/worker/ephemeral-entrypoint.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

export EPHEMERAL_ENTRYPOINT_SOURCED_FOR_TEST=1
source "$entrypoint"

test_root="$(mktemp -d "${TMPDIR:-/tmp}/prune-stale-run-data.XXXXXX")"
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT

dir="$test_root/artifacts"
mkdir -p "$dir/stale-run" "$dir/fresh-run"
if touch -d '10 days ago' "$test_root/.touch-probe" 2>/dev/null; then
  touch -d '10 days ago' "$dir/stale-run"
  touch -d '1 hour ago' "$dir/fresh-run"
else
  # BSD touch (macOS): no relative -d, use -t with an explicit timestamp.
  touch -t "$(date -v-10d +%Y%m%d%H%M)" "$dir/stale-run"
  touch -t "$(date -v-1H +%Y%m%d%H%M)" "$dir/fresh-run"
fi
rm -f "$test_root/.touch-probe"

STALE_RUN_DATA_RETENTION_DAYS=3 prune_stale_run_data "$dir"

[[ ! -e "$dir/stale-run" ]] || fail "stale-run (10 days old) survived pruning"
[[ -d "$dir/fresh-run" ]] || fail "fresh-run (1 hour old) was pruned"

# Missing directory (e.g. HERMESTRATOR_LOG_DIR pointed elsewhere and never
# created) must not error.
prune_stale_run_data "$test_root/does-not-exist"

printf 'ok\n'
